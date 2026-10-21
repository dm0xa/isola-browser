package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	return graphFixtureWithStatus(t, models.SessionStatusRunning, streamer)
}

// graphFixtureWithStatus is graphFixture with control over the session row's
// status, so the "still starting" contract can be exercised.
func graphFixtureWithStatus(t *testing.T, status string, streamer http.HandlerFunc) (*Handler, uuid.UUID, string) {
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
	if status == models.SessionStatusRunning {
		if err := st.Sessions.ActivateSession(ctx, sid, "container-1", "127.0.0.1", 5900, 10*time.Minute); err != nil {
			t.Fatal(err)
		}
	} else if status != models.SessionStatusProvisioning {
		if err := st.Sessions.UpdateSessionStatus(ctx, sid, status, "", "", 0); err != nil {
			t.Fatal(err)
		}
	}
	const token = "control-token"
	// Finalization deletes the control token along with the container, so only
	// a live session (starting or running) has one.
	if status == models.SessionStatusRunning || status == models.SessionStatusProvisioning {
		if err := cache.Set(ctx, "session:"+sid.String()+":control_token", token, time.Hour); err != nil {
			t.Fatal(err)
		}
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

	w := graphRequest(t, h, sid, token, "theme=light&expand=all&scale=1&bogus=1")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/graph.png" {
		t.Fatalf("upstream path = %q, want /graph.png", gotPath)
	}
	q, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("upstream query %q: %v", gotQuery, err)
	}
	if q.Get("theme") != "light" || q.Get("expand") != "all" || q.Get("scale") != "1" {
		t.Fatalf("upstream query = %q", gotQuery)
	}
	if q.Get("bogus") != "" {
		t.Fatalf("unknown parameter forwarded: %q", gotQuery)
	}
	if q.Get("direction") != "horizontal" || q.Get("width") != "1400" || q.Get("height") != "900" {
		t.Fatalf("defaults not materialised: %q", gotQuery)
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
	if cl := w.Header().Get("Content-Length"); cl != "7" {
		t.Fatalf("content-length = %q, want 7", cl)
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

// TestGetResourceGraphRejectsInvalidOptions proves the control plane validates
// before proxying: no request reaches the renderer.
func TestGetResourceGraphRejectsInvalidOptions(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{"theme=neon", "theme must be dark or light"},
		{"scale=99", "scale must be 1-4"},
		{"scale=-1", "scale must be 1-4"},
		{"width=10", "width must be 320-8000"},
		{"height=9000", "height must be 320-8000"},
		{"expand=nope", "expand must be origin, all or collapsed"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			called := false
			h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
				called = true
			})

			w := graphRequest(t, h, sid, token, tc.query)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("body does not carry %q: %s", tc.want, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "invalid_graph_options") {
				t.Fatalf("body does not carry the stable code: %s", w.Body.String())
			}
			if called {
				t.Fatal("invalid options reached the streamer")
			}
		})
	}
}

// TestGetResourceGraphSurfacesUpstreamValidation keeps the old pass-through
// contract for anything the renderer alone rejects.
func TestGetResourceGraphSurfacesUpstreamValidation(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "theme must be dark or light", http.StatusBadRequest)
	})

	w := graphRequest(t, h, sid, token, "theme=light")

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

func TestGetResourceGraphMapsRendererFailure(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "renderer exploded", http.StatusInternalServerError)
	})

	w := graphRequest(t, h, sid, token, "")

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "graph_render_failed") {
		t.Fatalf("body does not carry graph_render_failed: %s", w.Body.String())
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

// TestGetResourceGraphStartingSessionIsConflict pins the documented contract:
// a session that exists and is owned, but is still starting, answers 409.
func TestGetResourceGraphStartingSessionIsConflict(t *testing.T) {
	h, sid, token := graphFixtureWithStatus(t, models.SessionStatusProvisioning, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer was called for a starting session")
	})

	w := graphRequest(t, h, sid, token, "")

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "container_not_ready") {
		t.Fatalf("body does not carry container_not_ready: %s", w.Body.String())
	}
}

func TestGetResourceGraphTerminatedSessionIsNotFound(t *testing.T) {
	h, sid, _ := graphFixtureWithStatus(t, models.SessionStatusTerminated, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer was called for a terminated session")
	})

	// The control token is gone with the container; a finished session still
	// answers 404, not 403.
	w := graphRequest(t, h, sid, "stale-token", "")

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

// TestGetResourceGraphStartingSessionRequiresOwner keeps the 409 from leaking
// session state to a caller that cannot prove ownership.
func TestGetResourceGraphStartingSessionRequiresOwner(t *testing.T) {
	h, sid, _ := graphFixtureWithStatus(t, models.SessionStatusProvisioning, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer was called for a starting session")
	})

	w := graphRequest(t, h, sid, "attacker-guess", "")

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
}

// TestGetResourceGraphTruncatedBody proves a short upstream body never becomes
// a successful response with a partial PNG.
func TestGetResourceGraphTruncatedBody(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("PNG"))
	})

	w := graphRequest(t, h, sid, token, "")

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "graph_render_failed") {
		t.Fatalf("body does not carry graph_render_failed: %s", w.Body.String())
	}
}

func TestGetResourceGraphOversizeBody(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(strings.Repeat("x", 64)))
	})
	h.maxGraphBytes = 16

	w := graphRequest(t, h, sid, token, "")

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "graph_render_failed") {
		t.Fatalf("body does not carry graph_render_failed: %s", w.Body.String())
	}
}

func TestGetResourceGraphBusy(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		_, _ = w.Write([]byte("PNGDATA"))
	})
	h.maxGraphInFlight = 1

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- graphRequest(t, h, sid, token, "") }()
	<-entered

	w := graphRequest(t, h, sid, token, "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "graph_render_busy") {
		t.Fatalf("body does not carry graph_render_busy: %s", w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Fatal("missing Retry-After header")
	}

	close(release)
	if first := <-done; first.Code != http.StatusOK {
		t.Fatalf("first render = %d: %s", first.Code, first.Body.String())
	}
}

func TestGetResourceGraphTimeout(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("PNGDATA"))
	})
	h.graphRenderTimeout = 40 * time.Millisecond

	w := graphRequest(t, h, sid, token, "")

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "graph_render_timeout") {
		t.Fatalf("body does not carry graph_render_timeout: %s", w.Body.String())
	}
}
