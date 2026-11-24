package app

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// newTestFrontend is the smallest embedded SPA: a shell, one content-hashed
// asset and the favicon route.
func newTestFrontend() http.Handler {
	big := strings.Repeat("console.log('isola');\n", 256)
	return serveFrontend(fstest.MapFS{
		"index.html":           &fstest.MapFile{Data: []byte("<html>shell</html>")},
		"assets/app-abc.js":    &fstest.MapFile{Data: []byte("console.log(1)")},
		"assets/big-abc.js":    &fstest.MapFile{Data: []byte(big)},
		"fonts/brand.ttf":      &fstest.MapFile{Data: []byte(big)},
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

// TestServeFrontendCompressesLargeStaticFiles pins the binary-install path:
// without nginx, the embedded JS/font were shipped raw (819 KB + 419 KB).
func TestServeFrontendCompressesLargeStaticFiles(t *testing.T) {
	for _, tc := range []struct{ path, wantType string }{
		{"/assets/big-abc.js", "text/javascript"},
		{"/fonts/brand.ttf", "font/ttf"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			rec := httptest.NewRecorder()
			newTestFrontend().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip", got)
			}
			if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
				t.Fatalf("Vary = %q", got)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.wantType) {
				t.Fatalf("Content-Type = %q, want %s…", ct, tc.wantType)
			}
			zr, err := gzip.NewReader(rec.Body)
			if err != nil {
				t.Fatalf("gzip reader: %v", err)
			}
			body, err := io.ReadAll(zr)
			if err != nil {
				t.Fatalf("gunzip: %v", err)
			}
			if !strings.Contains(string(body), "isola") {
				t.Fatalf("uncompressed body is wrong: %q", body)
			}
		})
	}
}

func TestServeFrontendPlainWhenClientDoesNotAcceptGzip(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestFrontend().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/big-abc.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want none", got)
	}
	if !strings.Contains(rec.Body.String(), "isola") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestServeFrontendFontsAreCacheable(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestFrontend().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fonts/brand.ttf", nil))
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=604800" {
		t.Fatalf("Cache-Control = %q, want public, max-age=604800", got)
	}
}
