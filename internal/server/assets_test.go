package server

import (
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestPublicAssetCache(t *testing.T) {
	const immutable = "public, max-age=31536000, immutable"
	files := publicAssets(fstest.MapFS{
		"assets/index-Ab12_C-8.js":   {Data: []byte("export {};")},
		"assets/theme-9djE5w_3.css":  {Data: []byte("body{}")},
		"assets/font-a1234567.woff2": {Data: []byte("wOF2")},
		"assets/plain.js":            {Data: []byte("export {};")},
	})
	for _, tc := range []struct {
		method, path, byteRange string
		status                  int
		cache                   string
	}{
		{"GET", "/assets/index-Ab12_C-8.js", "", 200, immutable},
		{"HEAD", "/assets/theme-9djE5w_3.css", "", 200, immutable},
		{"GET", "/assets/font-a1234567.woff2", "bytes=0-1", 206, immutable},
		{"GET", "/assets/index-Ab12_C-8.js", "bytes=99-100", 416, "no-store"},
		{"GET", "/assets/missing-Ab12_C-8.js", "", 404, "no-store"},
		{"GET", "/assets/plain.js", "", 200, "no-store"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.byteRange != "" {
			r.Header.Set("Range", tc.byteRange)
		}
		w := httptest.NewRecorder()
		w.Header().Set("Cache-Control", "no-store") // server-wide default
		files.ServeHTTP(w, r)
		res := w.Result()
		res.Body.Close()
		if res.StatusCode != tc.status || res.Header.Get("Cache-Control") != tc.cache {
			t.Errorf("%s %s %s: got %d, Cache-Control %q", tc.method, tc.path, tc.byteRange, res.StatusCode, res.Header.Get("Cache-Control"))
		}
	}
}
