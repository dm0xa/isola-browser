package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
)

func seedRequest(t *testing.T, h *Handler, sid uuid.UUID, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid.String()+"/seed", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Session-Token", token)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", sid.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	h.ApplySessionSeed(w, req)
	return w
}

// A cookie without a domain gets the session target's origin, the streamer's
// counts come back untouched, and no secret is echoed or cached.
func TestApplySessionSeedForwardsAndDerivesDefaults(t *testing.T) {
	var gotPath string
	var gotSeed models.SessionSeed
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotSeed); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cookies":1,"headers":0,"storage":1,"reloaded":true}`))
	})

	w := seedRequest(t, h, sid, token,
		`{"cookies":[{"name":"sid","value":"abc"}],"storage":[{"origin":"","items":{"token":"t"}}],"reload":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/seed" {
		t.Fatalf("upstream path = %q, want /seed", gotPath)
	}
	if len(gotSeed.Cookies) != 1 || gotSeed.Cookies[0].URL != "https://example.com" {
		t.Fatalf("upstream cookie = %+v, want url https://example.com", gotSeed.Cookies)
	}
	if len(gotSeed.Storage) != 1 || gotSeed.Storage[0].Origin != "" {
		t.Fatalf("upstream storage = %+v, want the empty origin passed through (the streamer resolves it to the live page)", gotSeed.Storage)
	}
	if !gotSeed.Reload {
		t.Fatal("reload flag was dropped")
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var res models.SeedResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("response not json: %v", err)
	}
	if res.Cookies != 1 || !res.Reloaded {
		t.Fatalf("result = %+v, want the streamer's counts", res)
	}
	if strings.Contains(w.Body.String(), "abc") || strings.Contains(w.Body.String(), `"t"`) {
		t.Fatalf("response leaked a value: %s", w.Body.String())
	}
}

func TestApplySessionSeedRequiresOwner(t *testing.T) {
	h, sid, _ := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer must not be called without ownership")
	})
	if w := seedRequest(t, h, sid, "", `{"cookies":[{"name":"a","value":"b"}]}`); w.Code != http.StatusForbidden {
		t.Fatalf("no token: status = %d, want 403", w.Code)
	}
	if w := seedRequest(t, h, sid, "wrong", `{"cookies":[{"name":"a","value":"b"}]}`); w.Code != http.StatusForbidden {
		t.Fatalf("wrong token: status = %d, want 403", w.Code)
	}
}

func TestApplySessionSeedProvisioningIsConflict(t *testing.T) {
	h, sid, token := graphFixtureWithStatus(t, models.SessionStatusProvisioning, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer must not be called for a starting session")
	})
	if w := seedRequest(t, h, sid, token, `{"reload":true}`); w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

// A typo in a cookie attribute must be a 400, not a silently dropped
// http_only.
func TestApplySessionSeedRejectsUnknownFieldsAndEmpties(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer must not be called for an invalid payload")
	})
	for _, body := range []string{
		`{"cookies":[{"name":"a","value":"b","httpOnly":true}]}`,
		`{}`,
		`{"cookies":[{"name":"","value":"b"}]}`,
		`{"bogus":1}`,
		`{"reload":true}{"reload":true}`,
	} {
		w := seedRequest(t, h, sid, token, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400 (%s)", body, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "invalid_seed") {
			t.Errorf("body %s: response = %s", body, w.Body.String())
		}
	}
}

func TestApplySessionSeedPayloadLimitsAndEmptyBody(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer must not be called for an oversized or empty body")
	})

	// A body over maxJSONBodyBytes must be 413, not a generic 400.
	oversize := `{"cookies":[{"name":"a","value":"` + strings.Repeat("x", maxJSONBodyBytes) + `"}]}`
	w := seedRequest(t, h, sid, token, oversize)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: status = %d, want 413 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "payload_too_large") {
		t.Fatalf("oversize: response = %s, want payload_too_large", w.Body.String())
	}

	// An empty body names the problem instead of echoing "EOF".
	w = seedRequest(t, h, sid, token, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty body: status = %d, want 400", w.Code)
	}
	if strings.Contains(w.Body.String(), "EOF") {
		t.Fatalf("empty body: response = %s, want an actionable message", w.Body.String())
	}

	// No-store applies to errors too (docs promise it for the endpoint).
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("error Cache-Control = %q, want no-store", got)
	}
}

// A running session whose container address is gone (crash, reaped container)
// must answer 409 container_not_ready, not try the streamer.
func TestApplySessionSeedMissingContainerIsConflict(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer must not be called without a container address")
	})
	if err := h.cache.Del(context.Background(), "session:"+sid.String()); err != nil {
		t.Fatal(err)
	}
	w := seedRequest(t, h, sid, token, `{"cookies":[{"name":"a","value":"b"}]}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "container_not_ready") {
		t.Fatalf("response = %s, want container_not_ready", w.Body.String())
	}
}

// A reload on its own is legitimate: the page state was seeded by an earlier
// call and the caller now wants it picked up.
func TestApplySessionSeedAllowsReloadOnly(t *testing.T) {
	var got models.SessionSeed
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cookies":0,"headers":0,"storage":0,"reloaded":true}`))
	})

	w := seedRequest(t, h, sid, token, `{"reload":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if !got.Reload {
		t.Fatal("reload flag was dropped")
	}
}

func TestApplySessionSeedMapsUpstreamErrors(t *testing.T) {
	cases := []struct {
		name       string
		upstream   int
		upstreamIn string
		wantStatus int
		wantCode   string
	}{
		{"invalid payload", http.StatusBadRequest, "cookies[0]: invalid path", http.StatusBadRequest, "invalid_seed"},
		{"origin mismatch", http.StatusConflict, "page origin does not match: page is on https://other.example", http.StatusConflict, "seed_origin_mismatch"},
		{"no cdp", http.StatusServiceUnavailable, "cdp not available", http.StatusBadGateway, "seed_unavailable"},
		{"timeout", http.StatusGatewayTimeout, "seed timed out", http.StatusGatewayTimeout, "seed_timeout"},
		{"cdp failure", http.StatusBadGateway, "Network.setExtraHTTPHeaders: cdp error 1: nope", http.StatusBadGateway, "seed_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tc.upstreamIn, tc.upstream)
			})
			w := seedRequest(t, h, sid, token, `{"cookies":[{"name":"a","value":"b"}]}`)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantCode) {
				t.Fatalf("response = %s, want code %s", w.Body.String(), tc.wantCode)
			}
		})
	}
}
