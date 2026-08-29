package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// dist holds the built UI. `make ui` runs the Vite build in ui/ and syncs
// ui/dist into internal/web/dist, OVERWRITING the placeholder index.html
// committed here. That placeholder exists so `go build ./...` works on a clean
// checkout without a Node toolchain; everything else in internal/web/dist is
// gitignored (DESIGN section 16.1).
//
//go:embed all:dist
var dist embed.FS

// assetCacheControl is applied to content-hashed asset paths, which by
// construction never change contents under a given name.
const assetCacheControl = "public, max-age=31536000, immutable"

// Handler returns the UI handler rooted at the embedded dist directory.
func Handler() (http.Handler, error) {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	files := http.FileServerFS(root)
	return &spaHandler{root: root, files: files}, nil
}

type spaHandler struct {
	root  fs.FS
	files http.Handler
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		h.serveIndex(w, r)
		return
	}
	if f, err := h.root.Open(name); err == nil {
		f.Close()
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", assetCacheControl)
		}
		h.files.ServeHTTP(w, r)
		return
	}
	// Unknown path. A client-side route reloaded in the browser asks for HTML
	// and gets the shell; anything else is a genuine 404.
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.NotFound(w, r)
		return
	}
	h.serveIndex(w, r)
}

func (h *spaHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	b, err := fs.ReadFile(h.root, "index.html")
	if err != nil {
		http.Error(w, "ui not built", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
