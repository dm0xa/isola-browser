package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
)

func intelRequest(t *testing.T, h *Handler, sid uuid.UUID, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/sessions/" + sid.String() + "/intel"
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
	h.GetSessionIntel(w, req)
	return w
}

func TestGetSessionIntelProxiesCanonicalQuery(t *testing.T) {
	var gotPath, gotQuery string
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"host":"example.com","dns":{"example.com":{"ips":["93.184.216.34"]}}}`))
	})

	w := intelRequest(t, h, sid, token, "host=Example.COM.&mode=dns&checks=robots,securitytxt")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/intel" {
		t.Fatalf("upstream path = %q, want /intel", gotPath)
	}
	if !strings.Contains(gotQuery, "host=example.com") {
		t.Fatalf("upstream query = %q, want canonical host", gotQuery)
	}
	if strings.Contains(gotQuery, "Example") {
		t.Fatalf("host was not lowercased: %q", gotQuery)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if !strings.Contains(w.Body.String(), "93.184.216.34") {
		t.Fatalf("body not proxied: %s", w.Body.String())
	}
}

func TestGetSessionIntelRequiresOwner(t *testing.T) {
	h, sid, _ := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer must not be called without ownership")
	})
	w := intelRequest(t, h, sid, "wrong-token", "host=example.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestGetSessionIntelStartingSessionIsConflict(t *testing.T) {
	h, sid, token := graphFixtureWithStatus(t, models.SessionStatusProvisioning, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer must not be called for a starting session")
	})
	w := intelRequest(t, h, sid, token, "host=example.com")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestGetSessionIntelRejectsBadQueries(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer must not be called for an invalid query")
	})
	cases := []string{
		"",                             // nothing to act on
		"host=https://example.com",     // URL, not a host
		"host=example.com:8443",        // port
		"host=localhost",               // no dot
		"ips=not-an-ip",                // bad address
		"host=example.com&mode=scan",   // unknown mode
		"host=example.com&checks=nope", // unknown check
		"host=example.com&checks=rdap", // removed provider check: never reaches the container
		"host=example.com&checks=ct",   // ditto
		"host=example.com&checks=wayback",
		"host=example.com&bogus=1", // unknown parameter
	}
	for _, q := range cases {
		w := intelRequest(t, h, sid, token, q)
		if w.Code != http.StatusBadRequest {
			t.Errorf("query %q: status = %d, want 400 (%s)", q, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "invalid_intel_query") {
			t.Errorf("query %q: body = %s", q, w.Body.String())
		}
	}
}

func TestGetSessionIntelPassesUpstreamErrorsThrough(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"intel_disabled_by_operator"}`))
	})
	w := intelRequest(t, h, sid, token, "host=example.com&mode=dns")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want the streamer's 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "intel_disabled_by_operator") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestValidateIntelQueryAcceptsIPsOnly(t *testing.T) {
	q, err := validateIntelQuery(map[string][]string{
		"ips": {"8.8.8.8, 2001:db8::1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := q.Get("ips"); got != "8.8.8.8,2001:db8::1" {
		t.Fatalf("ips = %q", got)
	}
}
