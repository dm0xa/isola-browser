package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/dm0xa/isola-browser/backend/pkg/api"
	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

func TestMCPNotMountedWhenDisabled(t *testing.T) {
	r := chi.NewRouter()
	cfg := &config.Config{MCPEnabled: false, MCPTokens: []string{"secret"}}
	Mount(r, cfg, Deps{API: api.New(store.NewMemBridgeStore(store.NewMemStore()), store.NewMemCache(), nil, cfg)})
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when disabled", w.Code)
	}
}

func TestMCPUnauthorizedWithoutBearer(t *testing.T) {
	r := chi.NewRouter()
	cfg := &config.Config{MCPEnabled: true, MCPTokens: []string{"mount-test-token-32-characters-min"}}
	st := store.NewMemBridgeStore(store.NewMemStore())
	Mount(r, cfg, Deps{API: api.New(st, store.NewMemCache(), nil, cfg)})
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestMCPAcceptsAnyConfiguredToken(t *testing.T) {
	r := chi.NewRouter()
	const first = "mount-test-token-32-characters-aaa"
	const second = "mount-test-token-32-characters-bbb"
	cfg := &config.Config{MCPEnabled: true, MCPTokens: []string{first, second}}
	st := store.NewMemBridgeStore(store.NewMemStore())
	Mount(r, cfg, Deps{API: api.New(st, store.NewMemCache(), nil, cfg)})
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+second)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("second token rejected with 401")
	}
	if w.Code == http.StatusNotFound {
		t.Fatalf("status = 404, /mcp should be mounted")
	}
}
