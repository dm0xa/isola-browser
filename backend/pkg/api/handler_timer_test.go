package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/orchestrator"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

func newTestHandler(t *testing.T, cfg *config.Config) *Handler {
	t.Helper()
	mem := store.NewMemStore()
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock") // lazy — no docker needed
	if err != nil {
		t.Fatal(err)
	}
	return New(store.NewMemBridgeStore(mem), store.NewMemCache(), orch, cfg)
}

// TestSessionDurationTimerFlag pins the contract that session.timer: false
// yields an unlimited session (duration 0 → ActivateSession leaves expires_at
// NULL) regardless of default_min, while the default keeps today's behavior.
func TestSessionDurationTimerFlag(t *testing.T) {
	t.Parallel()

	t.Run("timer enabled uses default_min", func(t *testing.T) {
		t.Parallel()
		h := newTestHandler(t, &config.Config{SessionTimer: true, SessionDefaultMin: 10})
		if got := h.sessionDuration(); got != 10*time.Minute {
			t.Errorf("sessionDuration = %v, want 10m", got)
		}
	})

	t.Run("timer disabled is unlimited", func(t *testing.T) {
		t.Parallel()
		h := newTestHandler(t, &config.Config{SessionTimer: false, SessionDefaultMin: 10})
		if got := h.sessionDuration(); got != 0 {
			t.Errorf("sessionDuration = %v, want 0 (unlimited)", got)
		}
	})
}

// TestGetConfigExposesTimer: the viewer needs the flag to hide countdown UI
// and to know it must close the session when the owner leaves the page.
func TestGetConfigExposesTimer(t *testing.T) {
	t.Parallel()

	fetchTimer := func(t *testing.T, timer bool) bool {
		t.Helper()
		h := newTestHandler(t, &config.Config{SessionTimer: timer, DefaultTransport: "isola"})
		req := httptest.NewRequest("GET", "/api/config", nil)
		w := httptest.NewRecorder()
		h.GetConfig(w, req)
		if w.Code != 200 {
			t.Fatalf("GET /api/config = %d: %s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		v, ok := resp["timer"].(bool)
		if !ok {
			t.Fatalf("timer flag missing or not a boolean: %v", resp["timer"])
		}
		return v
	}

	if !fetchTimer(t, true) {
		t.Errorf("timer = false, want true")
	}
	if fetchTimer(t, false) {
		t.Errorf("timer = true, want false")
	}
}

func TestShareTTL(t *testing.T) {
	t.Parallel()

	t.Run("unlimited session gets the fixed ceiling", func(t *testing.T) {
		t.Parallel()
		if got := shareTTL(&models.Session{}); got != unlimitedShareTTL {
			t.Errorf("shareTTL(nil expiry) = %v, want %v", got, unlimitedShareTTL)
		}
	})

	t.Run("past expiry floors at a minute", func(t *testing.T) {
		t.Parallel()
		past := time.Now().Add(-30 * time.Second)
		if got := shareTTL(&models.Session{ExpiresAt: &past}); got != time.Minute {
			t.Errorf("shareTTL(past) = %v, want 1m", got)
		}
	})

	t.Run("remaining life is used", func(t *testing.T) {
		t.Parallel()
		future := time.Now().Add(time.Hour)
		got := shareTTL(&models.Session{ExpiresAt: &future})
		if got <= 59*time.Minute || got > time.Hour {
			t.Errorf("shareTTL(future) = %v, want ~1h", got)
		}
	})
}
