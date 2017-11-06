package chartmuseum

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/chartmuseum/chartmuseum/pkg/repo"

	"github.com/gin-gonic/gin"
)

var (
	objectSavedResponse        = gin.H{"saved": true}
	objectDeletedResponse      = gin.H{"deleted": true}
	notFoundErrorResponse      = gin.H{"error": "not found"}
	badExtensionErrorResponse  = gin.H{"error": "unsupported file extension"}
	alreadyExistsErrorResponse = gin.H{"error": "file already exists"}
)

type (
	packageOrProvenanceFile struct {
		filename string
		content  []byte
		field    string // file was extracted from this form field
	}
	filenameFromContentFn func([]byte) (string, error)
)

func (server *Server) getIndexFileRequestHandler(c *gin.Context) {
	if server.CacheRefreshPeriod == 0 {
		err := server.syncRepositoryIndex()
		if err != nil {
			c.JSON(500, errorResponse(err))
			return
		}
	}
	c.Data(200, repo.IndexFileContentType, server.RepositoryIndex.Raw)
}

func (server *Server) getAllChartsRequestHandler(c *gin.Context) {
	if server.CacheRefreshPeriod == 0 {
		err := server.syncRepositoryIndex()
		if err != nil {
			c.JSON(500, errorResponse(err))
			return
		}
	}
	c.JSON(200, server.RepositoryIndex.Entries)
}

func (server *Server) getChartRequestHandler(c *gin.Context) {
	name := c.Param("name")
	if server.CacheRefreshPeriod == 0 {
		err := server.syncRepositoryIndex()
		if err != nil {
			c.JSON(500, errorResponse(err))
			return
		}
	}
	chart := server.RepositoryIndex.Entries[name]
	if chart == nil {
		c.JSON(404, notFoundErrorResponse)
		return
	}
	c.JSON(200, chart)
}

func (server *Server) getChartVersionRequestHandler(c *gin.Context) {
	name := c.Param("name")
	version := c.Param("version")
	if version == "latest" {
		version = ""
	}
	if server.CacheRefreshPeriod == 0 {
		err := server.syncRepositoryIndex()
		if err != nil {
			c.JSON(500, errorResponse(err))
			return
		}
	}
	chartVersion, err := server.RepositoryIndex.Get(name, version)
	if err != nil {
		c.JSON(404, notFoundErrorResponse)
		return
	}
	c.JSON(200, chartVersion)
}

func (server *Server) deleteChartVersionRequestHandler(c *gin.Context) {
	if server.CacheRefreshPeriod > 0 {
		server.Logger.Debugw("Acquiring storage cache lock")
		server.StorageCacheLock.Lock()
		server.Logger.Debugw("Storage cache lock acquired")
		defer func() {
			server.Logger.Debugw("Releasing storage cache lock")
			server.StorageCacheLock.Unlock()
		}()
	}
	name := c.Param("name")
	version := c.Param("version")
	filename := repo.ChartPackageFilenameFromNameVersion(name, version)
	server.Logger.Debugw("Deleting package from storage",
		"package", filename,
	)
	err := server.StorageBackend.DeleteObject(filename)
	if err != nil {
		c.JSON(404, notFoundErrorResponse)
		return
	}
	provFilename := repo.ProvenanceFilenameFromNameVersion(name, version)
	server.StorageBackend.DeleteObject(provFilename) // ignore error here, may be no prov file
	if server.CacheRefreshPeriod > 0 {
		err = server.removeChartPackage(filename)
		if err != nil {
			c.JSON(500, errorResponse(err))
			return
		}
	}
	c.JSON(200, objectDeletedResponse)
}

func (server *Server) getStorageObjectRequestHandler(c *gin.Context) {
	filename := c.Param("filename")
	isChartPackage := strings.HasSuffix(filename, repo.ChartPackageFileExtension)
	isProvenanceFile := strings.HasSuffix(filename, repo.ProvenanceFileExtension)
	if !isChartPackage && !isProvenanceFile {
		c.JSON(500, badExtensionErrorResponse)
		return
	}
	object, err := server.StorageBackend.GetObject(filename)
	if err != nil {
		c.JSON(404, notFoundErrorResponse)
		return
	}
	if isProvenanceFile {
		c.Data(200, repo.ProvenanceFileContentType, object.Content)
		return
	}
	c.Data(200, repo.ChartPackageContentType, object.Content)
}

func (server *Server) extractAndValidateFormFile(req *http.Request, field string, fnFromContent filenameFromContentFn) (*packageOrProvenanceFile, int, error) {
	file, header, _ := req.FormFile(field)
	var ppf *packageOrProvenanceFile
	if file == nil || header == nil {
		return ppf, 200, nil // field is not present
	}
	buf := bytes.NewBuffer(nil)
	_, err := io.Copy(buf, file)
	if err != nil {
		return ppf, 500, err // IO error
	}
	content := buf.Bytes()
	filename, err := fnFromContent(content)
	if err != nil {
		return ppf, 400, err // validation error (bad request)
	}
	if !server.AllowOverwrite {
		_, err = server.StorageBackend.GetObject(filename)
		if err == nil {
			return ppf, 409, fmt.Errorf("%s already exists", filename) // conflict
		}
	}
	return &packageOrProvenanceFile{filename, content, field}, 200, nil
}

func (server *Server) postPackageAndProvenanceRequestHandler(c *gin.Context) {

	if server.CacheRefreshPeriod > 0 {
		server.Logger.Debugw("Acquiring storage cache lock")
		server.StorageCacheLock.Lock()
		server.Logger.Debugw("Storage cache lock acquired")
		defer func() {
			server.Logger.Debugw("Releasing storage cache lock")
			server.StorageCacheLock.Unlock()
		}()
	}

	var ppFiles []*packageOrProvenanceFile

	type fieldFuncPair struct {
		field string
		fn    filenameFromContentFn
	}

	ffp := []fieldFuncPair{
		{server.ChartPostFormFieldName, repo.ChartPackageFilenameFromContent},
		{server.ProvPostFormFieldName, repo.ProvenanceFilenameFromContent},
	}

	for _, ff := range ffp {
		ppf, status, err := server.extractAndValidateFormFile(c.Request, ff.field, ff.fn)
		if err != nil {
			c.JSON(status, errorResponse(err))
			return
		}
		if ppf != nil {
			ppFiles = append(ppFiles, ppf)
		}
	}

	if len(ppFiles) == 0 {
		c.JSON(400, errorResponse(
			fmt.Errorf("no package or provenance file found in form fields %s and %s",
				server.ChartPostFormFieldName, server.ProvPostFormFieldName)))
		return
	}

	// At this point input is presumed valid, we now proceed to store it
	var storedFiles []*packageOrProvenanceFile
	for _, ppf := range ppFiles {
		server.Logger.Debugw("Adding file to storage (form field)",
			"filename", ppf.filename,
			"field", ppf.field,
		)
		err := server.StorageBackend.PutObject(ppf.filename, ppf.content)
		if err == nil {
			storedFiles = append(storedFiles, ppf)
			if server.CacheRefreshPeriod > 0 {
				if strings.HasSuffix(ppf.filename, repo.ChartPackageFileExtension) {
					// This could probably be done more efficiently, e.g. by having PutObject
					// return its object, but for the moment it will do
					obj, err := server.StorageBackend.GetObject(ppf.filename)
					if err == nil {
						err = server.addChartPackage(obj)
					}
				}
			}
		}
		if err != nil {
			// Clean up what's already been saved
			for _, ppf := range storedFiles {
				server.StorageBackend.DeleteObject(ppf.filename)
			}
			c.JSON(500, errorResponse(err))
			return
		}
	}
	c.JSON(201, objectSavedResponse)
}

func (server *Server) postRequestHandler(c *gin.Context) {
	if c.ContentType() == "multipart/form-data" {
		server.postPackageAndProvenanceRequestHandler(c) // new route handling form-based chart and/or prov files
	} else {
		server.postPackageRequestHandler(c) // classic binary data, chart package only route
	}
}

func (server *Server) postPackageRequestHandler(c *gin.Context) {
	if server.CacheRefreshPeriod > 0 {
		server.Logger.Debugw("Acquiring storage cache lock")
		server.StorageCacheLock.Lock()
		server.Logger.Debugw("Storage cache lock acquired")
		defer func() {
			server.Logger.Debugw("Releasing storage cache lock")
			server.StorageCacheLock.Unlock()
		}()
	}
	content, err := c.GetRawData()
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	filename, err := repo.ChartPackageFilenameFromContent(content)
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	if !server.AllowOverwrite {
		_, err = server.StorageBackend.GetObject(filename)
		if err == nil {
			c.JSON(500, alreadyExistsErrorResponse)
			return
		}
	}
	server.Logger.Debugw("Adding package to storage",
		"package", filename,
	)
	err = server.StorageBackend.PutObject(filename, content)
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	if server.CacheRefreshPeriod > 0 {
		// This could probably be done more efficiently, e.g. by having PutObject
		// return its object, but for the moment it will do
		obj, err := server.StorageBackend.GetObject(filename)
		if err == nil {
			err = server.addChartPackage(obj)
		}
		if err != nil {
			c.JSON(500, errorResponse(err))
			return
		}
	}
	c.JSON(201, objectSavedResponse)
}

func (server *Server) postProvenanceFileRequestHandler(c *gin.Context) {
	content, err := c.GetRawData()
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	filename, err := repo.ProvenanceFilenameFromContent(content)
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	if !server.AllowOverwrite {
		_, err = server.StorageBackend.GetObject(filename)
		if err == nil {
			c.JSON(500, alreadyExistsErrorResponse)
			return
		}
	}
	server.Logger.Debugw("Adding provenance file to storage",
		"provenance_file", filename,
	)
	err = server.StorageBackend.PutObject(filename, content)
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	c.JSON(201, objectSavedResponse)
}

func errorResponse(err error) map[string]interface{} {
	errResp := gin.H{"error": fmt.Sprintf("%s", err)}
	return errResp
}
