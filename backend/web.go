package main

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:web/dist
var frontendFiles embed.FS

// Path: GET / (Vue frontend and client-side routes)
// applicationHandler serves the API routes and the embedded Vue application.
// apiHandler is the existing handler for health, authentication, and Codex APIs.
func applicationHandler(apiHandler http.Handler) http.Handler {
	staticFiles, err := fs.Sub(frontendFiles, "web/dist")
	if err != nil {
		panic("open embedded frontend files: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(staticFiles))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// API paths stay on the existing Go handlers; all other paths belong to
		// the Vue app, including client-side routes such as /agents/abc.
		if isAPIPath(r.URL.Path) {
			apiHandler.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}

		cleanPath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if cleanPath != "." {
			if _, err := fs.Stat(staticFiles, cleanPath); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
			// Missing assets should remain 404s rather than returning HTML with
			// a successful status, which would hide broken frontend builds.
			if strings.Contains(path.Base(cleanPath), ".") {
				http.NotFound(w, r)
				return
			}
		}
		http.ServeFileFS(w, r, staticFiles, "index.html")
	})
}

// isAPIPath reports whether path belongs to a backend endpoint. path is the
// URL path from the incoming HTTP request.
func isAPIPath(path string) bool {
	return path == "/healthz" || path == "/readyz" ||
		strings.HasPrefix(path, "/auth/") || strings.HasPrefix(path, "/codex/")
}
