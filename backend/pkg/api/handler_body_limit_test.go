package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dm0xa/isola-browser/backend/pkg/config"
)

func newBodyLimitTestHandler(t *testing.T) *Handler {
	t.Helper()
	h, _ := newCapacityTestHandler(t, &config.Config{
		BrowserType:          "chromium",
		Browsers:             map[string]config.BrowserEntry{"chromium": {Enabled: true}},
		DisableURLValidation: true,
	})
	return h
}

// errorCodeOf decodes the API error contract from a test response.
func errorCodeOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, w.Body.String())
	}
	return resp.Error.Code
}

// TestCreateSessionRejectsOversizedBody: the create endpoint is unauthenticated
// by default, so it must refuse to buffer an unbounded body.
func TestCreateSessionRejectsOversizedBody(t *testing.T) {
	t.Parallel()
	h := newBodyLimitTestHandler(t)

	body := `{"url":"https://example.com","browser_type":"chromium","padding":"` +
		strings.Repeat("a", maxJSONBodyBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.CreateSession(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", w.Code, w.Body.String())
	}
	if code := errorCodeOf(t, w); code != "payload_too_large" {
		t.Fatalf("code = %q, want payload_too_large", code)
	}
}

// TestCreateSessionAcceptsBodyUnderLimit: the cap must not reject a normal
// create request.
func TestCreateSessionAcceptsBodyUnderLimit(t *testing.T) {
	t.Parallel()
	h := newBodyLimitTestHandler(t)

	body := `{"url":"https://example.com","browser_type":"chromium"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.CreateSession(w, req)

	if w.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("normal body rejected with 413: %s", w.Body.String())
	}
}

// TestGetConfigIgnoresLargeBody: the cap belongs to the JSON-decoding handlers,
// not to read-only endpoints — GET /api/config must still answer with a body
// far larger than the limit attached.
func TestGetConfigIgnoresLargeBody(t *testing.T) {
	t.Parallel()
	h := newBodyLimitTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/config", strings.NewReader(strings.Repeat("a", 2*maxJSONBodyBytes)))
	w := httptest.NewRecorder()
	h.GetConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestMalformedBodyStillReportsInvalidRequestBody: a small but broken body keeps
// the pre-existing error code rather than being reported as oversized.
func TestMalformedBodyStillReportsInvalidRequestBody(t *testing.T) {
	t.Parallel()
	h := newBodyLimitTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"url":`))
	w := httptest.NewRecorder()
	h.CreateSession(w, req)

	if code := errorCodeOf(t, w); code != "invalid_request_body" {
		t.Fatalf("code = %q, want invalid_request_body", code)
	}
}
