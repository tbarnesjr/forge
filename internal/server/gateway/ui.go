package gateway

import (
	"io/fs"
	"log"
	"net/http"
	"strings"

	"github.com/jelmersnoeck/forge/web"
)

// RegisterUI mounts the embedded SPA at the given path prefix.
// If the embedded assets are empty or contain only .gitkeep, this is a no-op.
func RegisterUI(mux *http.ServeMux, pathPrefix string) {
	// Get the dist/ sub-filesystem
	distFS, err := fs.Sub(web.Assets, "dist")
	if err != nil {
		log.Printf("[gateway] UI assets not available: %v", err)
		return
	}

	// Check if there's an index.html — if not, the UI hasn't been built
	if _, err := fs.Stat(distFS, "index.html"); err != nil {
		log.Printf("[gateway] UI not built (no index.html in dist/), skipping UI routes")
		return
	}

	// Ensure path prefix has leading slash, no trailing
	pathPrefix = "/" + strings.Trim(pathPrefix, "/")

	fileServer := http.FileServer(http.FS(distFS))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip the prefix from the path
		path := strings.TrimPrefix(r.URL.Path, pathPrefix)
		if path == "" {
			path = "/"
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}

		// Set cache headers based on file type
		if path == "/" || path == "/index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		} else if strings.Contains(path, ".") {
			// Hashed assets get immutable cache
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}

		// Try to serve the exact file. If it doesn't exist, serve index.html (SPA fallback).
		if path != "/" {
			// Check if file exists in the embedded FS
			cleanPath := strings.TrimPrefix(path, "/")
			if _, err := fs.Stat(distFS, cleanPath); err == nil {
				// File exists — serve it
				r.URL.Path = path
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// Serve index.html for SPA routing
		r.URL.Path = "/index.html"
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})

	// Register routes
	mux.Handle(pathPrefix+"/", http.StripPrefix(pathPrefix, handler))
	mux.Handle(pathPrefix, http.RedirectHandler(pathPrefix+"/", http.StatusMovedPermanently))

	log.Printf("[gateway] UI mounted at %s/", pathPrefix)
}
