package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/orchestrator"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

// TestCreateSessionPersistsRowAndControlToken guards against the
// insert/cache-write calls being dropped from the handler again: a 202
// response MUST correspond to a persisted session row plus a stored control
// token (without the token in cache, nobody — not even the owner — can
// connect to or control the session).
func TestCreateSessionPersistsRowAndControlToken(t *testing.T) {
	mem := store.NewMemStore()
	st := mem
	cache := store.NewMemCache()
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock") // lazy — no docker needed
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		BrowserType:          "chromium",
		Browsers:             map[string]config.BrowserEntry{"chromium": {Enabled: true}},
		Resolutions:          []config.OptionEntry{{Value: "1280x720", Label: "720p"}},
		SessionMaxMin:        30,
		DisableURLValidation: true,
	}
	h := New(st, cache, orch, cfg)

	body := `{"url":"https://example.com","browser_type":"chromium","resolution":"1280x720"}`
	req := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.CreateSession(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		ID           string `json:"id"`
		SessionToken string `json:"session_token"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}

	// Row persisted?
	sid, err := uuid.Parse(resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.GetSession(req.Context(), sid)
	if err != nil || row == nil {
		t.Fatalf("session row missing after 202: err=%v", err)
	}
	if row.Status != "provisioning" {
		t.Fatalf("status = %q", row.Status)
	}

	// Control token stored in cache (owner can connect)?
	got, err := cache.Get(req.Context(), "session:"+resp.ID+":control_token")
	if err != nil || got == "" {
		t.Fatalf("control token missing from cache: err=%v", err)
	}
	if got != resp.SessionToken {
		t.Fatal("cache token differs from the token handed to the client")
	}
}

func TestCreateSessionRequiresAPITokenWhenConfigured(t *testing.T) {
	const apiToken = "create-gate-token-32-characters-min!!"
	mem := store.NewMemStore()
	st := mem
	cache := store.NewMemCache()
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		BrowserType:          "chromium",
		Browsers:             map[string]config.BrowserEntry{"chromium": {Enabled: true}},
		DisableURLValidation: true,
		APITokens:            []string{apiToken},
	}
	h := New(st, cache, orch, cfg)

	body := `{"url":"https://example.com","browser_type":"chromium"}`
	req := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.CreateSession(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("without bearer: status = %d, want 401", w.Code)
	}

	req2 := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+apiToken)
	w2 := httptest.NewRecorder()
	h.CreateSession(w2, req2)
	if w2.Code != http.StatusAccepted {
		t.Fatalf("with bearer: status = %d: %s", w2.Code, w2.Body.String())
	}
}

func TestCreateSessionAcceptsAnyConfiguredAPIToken(t *testing.T) {
	const first = "create-gate-token-32-characters-aa"
	const second = "create-gate-token-32-characters-bb"
	mem := store.NewMemStore()
	st := mem
	cache := store.NewMemCache()
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		BrowserType:          "chromium",
		Browsers:             map[string]config.BrowserEntry{"chromium": {Enabled: true}},
		DisableURLValidation: true,
		APITokens:            []string{first, second},
	}
	h := New(st, cache, orch, cfg)
	body := `{"url":"https://example.com","browser_type":"chromium"}`

	req := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+second)
	w := httptest.NewRecorder()
	h.CreateSession(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("second token: status = %d: %s", w.Code, w.Body.String())
	}
}

// TestCreateSessionInternalCallerBypassesAPITokenGate pins the contract the MCP
// server relies on: an in-process caller that already authenticated on /mcp is
// not rejected by the REST api.token gate. The marker lives in the request
// context, so a network client cannot forge it with a header.
func TestCreateSessionInternalCallerBypassesAPITokenGate(t *testing.T) {
	mem := store.NewMemStore()
	st := mem
	cache := store.NewMemCache()
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		BrowserType:          "chromium",
		Browsers:             map[string]config.BrowserEntry{"chromium": {Enabled: true}},
		DisableURLValidation: true,
		APITokens:            []string{"create-gate-token-32-characters-min!!"},
	}
	h := New(st, cache, orch, cfg)
	body := `{"url":"https://example.com","browser_type":"chromium"}`

	req := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(body))
	req = req.WithContext(WithInternalSessionCreate(req.Context()))
	w := httptest.NewRecorder()
	h.CreateSession(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("internal caller: status = %d: %s", w.Code, w.Body.String())
	}

	// A header that merely claims to be internal must still hit the gate.
	req2 := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(body))
	req2.Header.Set("X-Isola-Internal", "true")
	w2 := httptest.NewRecorder()
	h.CreateSession(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("forged header: status = %d, want 401", w2.Code)
	}
}

// A launch failure must persist a stable reason on the row and expose it as
// error_code on GET /api/sessions/{id}: a user seeing status error needs to know
// whether the sandbox died, the stream never came up, or the host publish path
// broke (the last is not the container's fault).
func TestCreateSessionFailurePersistsErrorCode(t *testing.T) {
	mem := store.NewMemStore()
	cache := store.NewMemCache()
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock") // fails fast, no docker needed
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		BrowserType:          "chromium",
		Browsers:             map[string]config.BrowserEntry{"chromium": {Enabled: true}},
		Resolutions:          []config.OptionEntry{{Value: "1280x720", Label: "720p"}},
		DisableURLValidation: true,
	}
	h := New(mem, cache, orch, cfg)

	body := `{"url":"https://example.com","browser_type":"chromium","resolution":"1280x720"}`
	req := httptest.NewRequest("POST", "/api/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.CreateSession(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		ID           string `json:"id"`
		SessionToken string `json:"session_token"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	sid, err := uuid.Parse(resp.ID)
	if err != nil {
		t.Fatal(err)
	}

	var row *models.Session
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		row, err = mem.GetSession(context.Background(), sid)
		if err == nil && row != nil && row.Status == models.SessionStatusError {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if row == nil || row.Status != models.SessionStatusError {
		t.Fatalf("session row = %+v, want status error", row)
	}
	if row.ErrorCode != "create_failed" {
		t.Fatalf("error_code = %q, want create_failed", row.ErrorCode)
	}

	greq := httptest.NewRequest(http.MethodGet, "/api/sessions/"+resp.ID, nil)
	greq.Header.Set("X-Session-Token", resp.SessionToken)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", resp.ID)
	greq = greq.WithContext(context.WithValue(greq.Context(), chi.RouteCtxKey, rctx))
	gw := httptest.NewRecorder()
	h.GetSession(gw, greq)
	if gw.Code != http.StatusOK {
		t.Fatalf("get session = %d: %s", gw.Code, gw.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(gw.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["error_code"] != "create_failed" {
		t.Fatalf("GET error_code = %v, want create_failed", got["error_code"])
	}
}

// The reason mapping is the contract clients key off: a publish-path failure is
// its own code, every other orchestrator failure is create_failed.
func TestSessionCreateErrorCode(t *testing.T) {
	t.Parallel()
	publishErr := &orchestrator.PublishUnreachableError{ContainerID: "c1", PublishedAddr: "127.0.0.1:5900", InternalAlive: true}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"publish unreachable", publishErr, "sandbox_publish_unreachable"},
		{"wrapped publish unreachable", fmt.Errorf("create: %w", publishErr), "sandbox_publish_unreachable"},
		{"other", errors.New("docker run failed"), "create_failed"},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sessionCreateErrorCode(tc.err); got != tc.want {
				t.Fatalf("sessionCreateErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
