package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/orchestrator"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

// TestGetSessionNeverLeaksControlTokenToNonOwner guards the anonymous
// re-issue path: a caller who knows the session id but presents a wrong (or
// stale) token must not receive the live control token. The recovery branch
// must only fire when the token is genuinely gone, because "knowing a session
// id is never sufficient" (docs/security.md).
func TestGetSessionNeverLeaksControlTokenToNonOwner(t *testing.T) {
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
		UserID:    uuid.Nil, // anonymous session
		Status:    models.SessionStatusProvisioning,
		TargetURL: "https://example.com",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Sessions.ActivateSession(ctx, sid, "container-1", "127.0.0.1", 5900, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	const controlToken = "the-real-control-token"
	if err := cache.Set(ctx, "session:"+sid.String()+":control_token", controlToken, time.Hour); err != nil {
		t.Fatal(err)
	}

	get := func(presentedToken string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil)
		if presentedToken != "" {
			req.Header.Set("X-Session-Token", presentedToken)
		}
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", sid.String())
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

		w := httptest.NewRecorder()
		h.GetSession(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GetSession status = %d: %s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	if got, ok := get(controlToken)["session_token"]; !ok || got != controlToken {
		t.Fatalf("owner token = %v, want %q", got, controlToken)
	}
	if got, ok := get("attacker-guess")["session_token"]; ok {
		t.Fatalf("non-owner received session_token %v", got)
	}
	if got, ok := get("stale-token-from-old-session")["session_token"]; ok {
		t.Fatalf("stale token received session_token %v", got)
	}
}
