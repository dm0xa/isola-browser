package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// newTestFrontend is the smallest embedded SPA: a shell, one content-hashed
// asset and the favicon route.
func newTestFrontend() http.Handler {
	return serveFrontend(fstest.MapFS{
		"index.html":        &fstest.MapFile{Data: []byte("<html>shell</html>")},
		"assets/app-abc.js": &fstest.MapFile{Data: []byte("console.log(1)")},
		"favicon.svg":       &fstest.MapFile{Data: []byte("<svg/>")},
	})
}

// TestServeFrontendCachePolicy pins the headers the browser needs to pick up a
// new build. The regression: http.FileServer over an embed.FS sent neither
// validators nor Cache-Control, so a browser could keep running the previous
// bundle after an upgrade and a frontend fix would appear to do nothing.
func TestServeFrontendCachePolicy(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		status int
		want   string
	}{
		{
			name:   "content-hashed asset may be cached forever",
			path:   "/assets/app-abc.js",
			status: http.StatusOK,
			want:   "public, max-age=31536000, immutable",
		},
		{
			name:   "the app shell must be revalidated",
			path:   "/",
			status: http.StatusOK,
			want:   "no-cache",
		},
		{
			name:   "favicon follows the shell",
			path:   "/favicon.svg",
			status: http.StatusOK,
			want:   "no-cache",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newTestFrontend().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rec.Code != tc.status {
				t.Fatalf("GET %s = %d, want %d", tc.path, rec.Code, tc.status)
			}
			if got := rec.Header().Get("Cache-Control"); got != tc.want {
				t.Errorf("GET %s Cache-Control = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
