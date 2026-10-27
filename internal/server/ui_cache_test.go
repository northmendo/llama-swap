package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// serveUITestFS is a minimal embedded-UI stand-in: an index.html plus one
// hash-named asset, mirroring what ui_dist contains after a vite build.
func serveUITestFS() http.FileSystem {
	return http.FS(fstest.MapFS{
		"index.html":                  {Data: []byte("<html>ui</html>")},
		"assets/index-BlTfv52S.js":    {Data: []byte("/*bundle*/")},
		"assets/index-BlTfv52S.js.br":  {Data: []byte("br-bundle")},
	})
}

func TestServeUICacheHeaders(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		acceptBr string
		want     string
	}{
		{"index revalidates", "/ui/", "", "no-cache"},
		{"explicit index revalidates", "/ui/index.html", "", "no-cache"},
		{"hashed asset is immutable", "/ui/assets/index-BlTfv52S.js", "", "public, max-age=31536000, immutable"},
		{"hashed asset immutable via brotli", "/ui/assets/index-BlTfv52S.js", "br", "public, max-age=31536000, immutable"},
		{"SPA fallback revalidates", "/ui/some/route", "", "no-cache"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.acceptBr != "" {
				r.Header.Set("Accept-Encoding", tc.acceptBr)
			}
			serveUI(serveUITestFS(), w, r)
			if got := w.Header().Get("Cache-Control"); got != tc.want {
				t.Errorf("Cache-Control = %q, want %q", got, tc.want)
			}
		})
	}
}
