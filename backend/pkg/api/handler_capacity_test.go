package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/orchestrator"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

func newCapacityTestHandler(t *testing.T, cfg *config.Config) (*Handler, *store.Store) {
	t.Helper()
	mem := store.NewMemStore()
	t.Cleanup(func() { mem.Close() })
	st := store.NewMemBridgeStore(mem)
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock") // lazy — no docker needed
	if err != nil {
		t.Fatal(err)
	}
	return New(st, store.NewMemCache(), orch, cfg), st
}

// TestCreateSessionRefusedAtMaxSessions: once ISOLA_MAX_SESSIONS active sessions
// exist, POST /api/sessions answers 429 with the max_sessions_reached code.
func TestCreateSessionRefusedAtMaxSessions(t *testing.T) {
	h, st := newCapacityTestHandler(t, &config.Config{
		BrowserType:          "chromium",
		Browsers:             map[string]config.BrowserEntry{"chromium": {Enabled: true}},
		DisableURLValidation: true,
		MaxSessions:          1,
	})

	// Seed one active session directly: a row created through the handler would
	// be raced by its own provisioning goroutine (no docker in tests).
	if err := st.Sessions.CreateSession(context.Background(), &models.Session{
		ID:        uuid.New(),
		Status:    models.SessionStatusRunning,
		TargetURL: "https://example.com",
	}); err != nil {
		t.Fatal(err)
	}

	body := `{"url":"https://example.com","browser_type":"chromium"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.CreateSession(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error.Code != "max_sessions_reached" {
		t.Fatalf("code = %q, want max_sessions_reached", resp.Error.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}
}

// TestGetConfigReportsMaxSessions: the client reads the cap from /api/config to
// explain a 429 (0 = unlimited).
func TestGetConfigReportsMaxSessions(t *testing.T) {
	h, _ := newCapacityTestHandler(t, &config.Config{
		BrowserType: "chromium",
		Browsers:    map[string]config.BrowserEntry{"chromium": {Enabled: true}},
		MaxSessions: 5,
	})

	w := httptest.NewRecorder()
	h.GetConfig(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		MaxSessions int `json:"max_sessions"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.MaxSessions != 5 {
		t.Fatalf("max_sessions = %d, want 5", resp.MaxSessions)
	}
}

// TestCreateSessionAllowsConcurrentAnonymousSessions: a second launch must not
// tear down the first — multisession — so the earlier session row survives.
func TestCreateSessionAllowsConcurrentAnonymousSessions(t *testing.T) {
	h, st := newCapacityTestHandler(t, &config.Config{
		BrowserType:          "chromium",
		Browsers:             map[string]config.BrowserEntry{"chromium": {Enabled: true}},
		DisableURLValidation: true,
	})

	body := `{"url":"https://example.com","browser_type":"chromium"}`
	var firstID uuid.UUID
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.CreateSession(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("create %d = %d: %s", i, w.Code, w.Body.String())
		}
		var resp struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		id, err := uuid.Parse(resp.ID)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstID = id
		}
	}

	row, err := st.Sessions.GetSession(context.Background(), firstID)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatal("first session row disappeared after a second launch")
	}
	if row.Status == models.SessionStatusTerminated {
		t.Fatal("second launch terminated the first session — multisession is off")
	}
}
