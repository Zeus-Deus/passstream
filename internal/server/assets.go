package server

import (
	"io/fs"
	"net/http"
	"regexp"
)

// Vite includes an eight-character content hash in its generated asset names.
var fingerprintedAsset = regexp.MustCompile(`^/assets/[^/]+-[A-Za-z0-9_-]{8}\.(js|css|woff2)$`)

func publicAssets(dist fs.FS) http.Handler {
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && fingerprintedAsset.MatchString(r.URL.Path) {
			w = immutableAssetWriter{w}
		}
		files.ServeHTTP(w, r)
	})
}

type immutableAssetWriter struct{ http.ResponseWriter }

func (w immutableAssetWriter) WriteHeader(status int) {
	switch status {
	case http.StatusOK, http.StatusPartialContent, http.StatusNotModified:
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		// An old URL can 404 during a deployment. Never cache that error.
		w.Header().Set("Cache-Control", "no-store")
	}
	w.ResponseWriter.WriteHeader(status)
}
