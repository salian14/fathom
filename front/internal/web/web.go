// Package web serves the chat page and its assets from the binary.
//
// Embedded rather than mounted: the whole UI is a few kilobytes, and shipping
// it inside the image means the Deployment has no ConfigMap to keep in step
// with the code, and no writable volume under readOnlyRootFilesystem.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var content embed.FS

// Handler serves the UI at /, with the root path mapping to index.html.
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(content, "static")
	if err != nil {
		return nil, err
	}

	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// no-store on everything. The assets are unversioned and a few KB, so
		// a stale app.js held against a newly rolled binary costs far more than
		// the handful of bytes a cache hit would save. Revisit if the page ever
		// grows enough for that trade to flip.
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	}), nil
}
