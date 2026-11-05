package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// newTestFrontend is the smallest embedded SPA: a shell, one content-hashed
// asset and the favicon route.
func newTestFrontend() http.Handler {
	return serveFrontend(fstest.MapFS{
		"index.html":           &fstest.MapFile{Data: []byte("<html>shell</html>")},
		"assets/app-abc.js":    &fstest.MapFile{Data: []byte("console.log(1)")},
		"favicon.svg":          &fstest.MapFile{Data: []byte("<svg/>")},
		"manifest.webmanifest": &fstest.MapFile{Data: []byte(`{"name":"isola"}`)},
		"icons/icon-192.png":   &fstest.MapFile{Data: []byte("png")},
	})
}

// TestServeFrontendContentTypes pins the headers Chromium's installability
// check reads. The manifest has no entry in Go's built-in mime table, so
// without registration it was sniffed and served as text/plain, and the icon
// route is separate from the SPA fallback so a typo'd icon name cannot answer
// with index.html.
func TestServeFrontendContentTypes(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "/manifest.webmanifest", want: "application/manifest+json"},
		{path: "/icons/icon-192.png", want: "image/png"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newTestFrontend().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", tc.path, rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != tc.want {
				t.Errorf("GET %s Content-Type = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
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

// TestNotFoundSeparatesAPIFromClientRoutes pins the SPA fallback: a client
// route must serve the shell so a deep link survives a reload, but an unknown
// /api path must never answer 200 with HTML — clients read that as success and
// then fail parsing the body.
func TestNotFoundSeparatesAPIFromClientRoutes(t *testing.T) {
	h := notFound(newTestFrontend())

	for _, path := range []string{"/api/nope", "/api", "/api/sessions/nope/extra"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("%s: content-type = %q, want application/json", path, ct)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: cache-control = %q, want no-store", path, rec.Header().Get("Cache-Control"))
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body is not JSON: %v (%q)", path, err, rec.Body.String())
		}
		if body.Error.Code != "not_found" {
			t.Fatalf("%s: error code = %q, want not_found", path, body.Error.Code)
		}
	}

	for _, path := range []string{"/session/abc", "/share/tok", "/run"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<html>shell</html>") {
			t.Fatalf("%s: body = %q, want the SPA shell", path, rec.Body.String())
		}
	}
}
