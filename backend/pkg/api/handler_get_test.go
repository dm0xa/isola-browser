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

// TestGetSessionRequiresOwnerToken guards "knowing a session id is never
// sufficient" (docs/security.md): an anonymous or wrongly-authenticated read is
// refused instead of downgraded to a view-only share link, and the live control
// token is only ever returned to its owner.
func TestGetSessionRequiresOwnerToken(t *testing.T) {
	mem := store.NewMemStore()
	st := mem
	cache := store.NewMemCache()
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock") // lazy — no docker needed
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, cache, orch, &config.Config{SessionMaxMin: 30, SessionDefaultMin: 10})

	ctx := context.Background()
	sid := uuid.New()
	if err := st.CreateSession(ctx, &models.Session{
		ID:        sid,
		UserID:    uuid.Nil, // anonymous session
		Status:    models.SessionStatusProvisioning,
		TargetURL: "https://example.com",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ActivateSession(ctx, sid, "container-1", "127.0.0.1", 5900, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	const controlToken = "the-real-control-token"
	if err := cache.Set(ctx, "session:"+sid.String()+":control_token", controlToken, time.Hour); err != nil {
		t.Fatal(err)
	}

	get := func(presentedToken string) (int, map[string]any) {
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
		var body map[string]any
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode body (status %d): %v", w.Code, err)
		}
		return w.Code, body
	}

	if code, body := get(controlToken); code != http.StatusOK {
		t.Fatalf("owner status = %d: %v", code, body)
	} else if got, ok := body["session_token"]; !ok || got != controlToken {
		t.Fatalf("owner token = %v, want %q", got, controlToken)
	}
	// An authenticated API read counts as activity (REST-only clients must not
	// be idle-reaped while they are polling).
	row, err := st.GetSession(ctx, sid)
	if err != nil || row == nil || row.LastActivityAt == nil {
		t.Fatalf("last activity not recorded: row=%v err=%v", row, err)
	}
	if time.Since(*row.LastActivityAt) > time.Minute {
		t.Fatalf("last activity = %v, want recent", row.LastActivityAt)
	}

	for _, presented := range []string{"", "attacker-guess", "stale-token-from-old-session"} {
		code, body := get(presented)
		if code != http.StatusForbidden {
			t.Fatalf("GetSession with token %q status = %d, want 403: %v", presented, code, body)
		}
		if _, ok := body["session_token"]; ok {
			t.Fatalf("GetSession with token %q leaked session_token", presented)
		}
		if _, ok := body["share_url"]; ok {
			t.Fatalf("GetSession with token %q minted a share_url", presented)
		}
		if _, ok := body["container_host"]; ok {
			t.Fatalf("GetSession with token %q leaked the container address", presented)
		}
	}

	// Genuine token recovery: the stored control token is gone (restart) and the
	// caller presents *a* token — issue a fresh one rather than locking the
	// browser out of its own anonymous session.
	if err := cache.Del(ctx, "session:"+sid.String()+":control_token"); err != nil {
		t.Fatal(err)
	}
	code, body := get("token-from-before-the-restart")
	if code != http.StatusOK {
		t.Fatalf("recovery status = %d: %v", code, body)
	}
	recovered, _ := body["session_token"].(string)
	if recovered == "" || recovered == controlToken {
		t.Fatalf("recovery token = %q, want a freshly issued token", recovered)
	}

	// A terminal row stays readable by its owner: finalization keeps the control
	// token so the viewer can show "Session has ended" instead of "token
	// required", and a failed launch does not look like token theft.
	if err := st.TerminateSession(ctx, sid); err != nil {
		t.Fatal(err)
	}
	code, body = get(recovered)
	if code != http.StatusOK {
		t.Fatalf("terminated owner GET = %d: %v", code, body)
	}
	if body["status"] != models.SessionStatusTerminated {
		t.Fatalf("terminated owner GET status = %v, want terminated", body["status"])
	}
}
