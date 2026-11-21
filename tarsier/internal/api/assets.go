package api

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"github.com/dale-tomson/Tarsier/web"
)

type assetFile struct {
	contentType  string
	etag         string
	cacheControl string
	content      []byte
}

// AssetsHandler serves embedded web assets with precomputed ETags, Cache-Control, and SPA fallback.
type AssetsHandler struct {
	files     map[string]*assetFile
	indexFile *assetFile
}

// NewAssetsHandler scans and indexes embedded files from web.Assets at startup.
func NewAssetsHandler() (*AssetsHandler, error) {
	files := make(map[string]*assetFile)

	err := fs.WalkDir(web.Assets, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}

		data, readErr := web.Assets.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("read embedded asset %s: %w", p, readErr)
		}

		h := sha256.Sum256(data)
		etag := fmt.Sprintf("\"%x\"", h[:])

		var ctype string
		ext := strings.ToLower(filepath.Ext(p))
		switch ext {
		case ".html":
			ctype = "text/html; charset=utf-8"
		case ".css":
			ctype = "text/css; charset=utf-8"
		case ".js":
			ctype = "application/javascript; charset=utf-8"
		case ".svg":
			ctype = "image/svg+xml"
		case ".png":
			ctype = "image/png"
		case ".ico":
			ctype = "image/x-icon"
		case ".json":
			ctype = "application/json; charset=utf-8"
		default:
			ctype = mime.TypeByExtension(ext)
			if ctype == "" {
				ctype = "application/octet-stream"
			}
		}

		var cacheControl string
		if strings.HasPrefix(p, "static/vendor/") {
			cacheControl = "public, max-age=31536000, immutable"
		} else if strings.HasPrefix(p, "static/") {
			cacheControl = "public, max-age=300"
		} else {
			cacheControl = "no-cache"
		}

		normalizedPath := "/" + strings.TrimPrefix(path.Clean(p), "/")
		files[normalizedPath] = &assetFile{
			contentType:  ctype,
			etag:         etag,
			cacheControl: cacheControl,
			content:      data,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	index, ok := files["/index.html"]
	if !ok {
		return nil, fmt.Errorf("web.Assets is missing required index.html")
	}

	return &AssetsHandler{
		files:     files,
		indexFile: index,
	}, nil
}

// ServeStatic handles GET /static/* requests.
func (h *AssetsHandler) ServeStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	cleanPath := path.Clean(r.URL.Path)
	file, ok := h.files[cleanPath]
	if !ok {
		http.NotFound(w, r)
		return
	}

	h.serveFile(w, r, file)
}

// ServeSPA handles root `/` and fallback client SPA routes by serving index.html.
func (h *AssetsHandler) ServeSPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	h.serveFile(w, r, h.indexFile)
}

func (h *AssetsHandler) serveFile(w http.ResponseWriter, r *http.Request, file *assetFile) {
	w.Header().Set("Content-Type", file.contentType)
	w.Header().Set("Cache-Control", file.cacheControl)
	w.Header().Set("ETag", file.etag)

	if matchesETag(r.Header.Get("If-None-Match"), file.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(file.content)
}

func matchesETag(clientHeader, serverETag string) bool {
	if clientHeader == "" {
		return false
	}
	serverRaw := strings.TrimPrefix(serverETag, "W/")
	for _, part := range strings.Split(clientHeader, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "W/")
		if part == "*" || part == serverRaw {
			return true
		}
	}
	return false
}
