package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dm0xa/isola/backend/pkg/config"
	"github.com/dm0xa/isola/backend/pkg/models"
	"github.com/dm0xa/isola/backend/pkg/orchestrator"
	"github.com/dm0xa/isola/backend/pkg/store"
)

// graphFixture starts a fake streamer and a running session wired to it.
func graphFixture(t *testing.T, streamer http.HandlerFunc) (*Handler, uuid.UUID, string) {
	t.Helper()
	srv := httptest.NewServer(streamer)
	t.Cleanup(srv.Close)

	mem := store.NewMemStore()
	st := store.NewMemBridgeStore(mem)
	cache := store.NewMemCache()
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock") // lazy — no docker needed
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, cache, orch, &config.Config{SessionMaxMin: 30, SessionDefaultMin: 10})

	ctx := context.Background()
	sid := uuid.New()
	if err := st.Sessions.CreateSession(ctx, &models.Session{
		ID:        sid,
		UserID:    uuid.Nil,
		Status:    models.SessionStatusProvisioning,
		TargetURL: "https://example.com",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Sessions.ActivateSession(ctx, sid, "container-1", "127.0.0.1", 5900, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	const token = "control-token"
	if err := cache.Set(ctx, "session:"+sid.String()+":control_token", token, time.Hour); err != nil {
		t.Fatal(err)
	}
	// The streamer address the handler proxies to.
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := cache.Set(ctx, "session:"+sid.String(), addr, time.Hour); err != nil {
		t.Fatal(err)
	}
	return h, sid, token
}

func graphRequest(t *testing.T, h *Handler, sid uuid.UUID, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/sessions/" + sid.String() + "/graph.png"
	if query != "" {
		target += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if token != "" {
		req.Header.Set("X-Session-Token", token)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", sid.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	h.GetResourceGraph(w, req)
	return w
}

func TestGetResourceGraphProxiesStreamer(t *testing.T) {
	var gotPath, gotQuery string
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNGDATA"))
	})

	w := graphRequest(t, h, sid, token, "theme=light&expand=all&scale=1")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/graph.png" {
		t.Fatalf("upstream path = %q, want /graph.png", gotPath)
	}
	if gotQuery != "theme=light&expand=all&scale=1" {
		t.Fatalf("upstream query = %q", gotQuery)
	}
	if body := w.Body.String(); body != "PNGDATA" {
		t.Fatalf("body = %q", body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q", cc)
	}
}

func TestGetResourceGraphRequiresOwner(t *testing.T) {
	called := false
	h, sid, _ := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte("PNGDATA"))
	})

	w := graphRequest(t, h, sid, "attacker-guess", "")

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("streamer was called for a non-owner")
	}
}

func TestGetResourceGraphSurfacesStreamerValidation(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "theme must be dark or light", http.StatusBadRequest)
	})

	w := graphRequest(t, h, sid, token, "theme=neon")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "theme must be dark or light") {
		t.Fatalf("body does not carry the reason: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_graph_options") {
		t.Fatalf("body does not carry the stable code: %s", w.Body.String())
	}
}

func TestGetResourceGraphNeedsContainerAddress(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("PNGDATA"))
	})
	// Drop the address the orchestrator would have published.
	if err := h.cache.Del(context.Background(), "session:"+sid.String()); err != nil {
		t.Fatal(err)
	}

	w := graphRequest(t, h, sid, token, "")

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
}
