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

func networkRequest(t *testing.T, h *Handler, sid uuid.UUID, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/sessions/" + sid.String() + "/network"
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
	h.GetSessionNetwork(w, req)
	return w
}

const networkDumpFixture = `{"entries":[
	{"url":"https://www.example.com/","method":"GET","type":"Document","status":200,"tls":{"security_state":"insecure"},"tracker":{"name":"example-analytics"}},
	{"url":"https://cdn.other.test/a.js","method":"GET","type":"Script","status":200},
	{"url":"https://evil.test/?ref=example.com","method":"GET","type":"Script","status":302,"redirects":[{"url":"https://evil.test/x"}]}
]}`

func TestGetSessionNetworkProxiesDump(t *testing.T) {
	var gotPath string
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(networkDumpFixture))
	})

	w := networkRequest(t, h, sid, token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/network" {
		t.Fatalf("upstream path = %q, want /network", gotPath)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	// Unfiltered dump must be passed through byte for byte: headers, TLS verdicts
	// and tracker labels are the reason a caller fetches it.
	if w.Body.String() != networkDumpFixture {
		t.Fatalf("body not passed through: %s", w.Body.String())
	}
}

func TestGetSessionNetworkFilters(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(networkDumpFixture))
	})

	w := networkRequest(t, h, sid, token, "type=Script&host_contains=other&url_contains=.js&limit=1")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"count":1`) {
		t.Fatalf("count = want 1: %s", body)
	}
	if !strings.Contains(body, "cdn.other.test/a.js") || strings.Contains(body, "evil.test") {
		t.Fatalf("filters not applied: %s", body)
	}
}

func TestGetSessionNetworkRequiresOwner(t *testing.T) {
	h, sid, _ := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer must not be called without ownership")
	})
	w := networkRequest(t, h, sid, "wrong-token", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestGetSessionNetworkStartingSessionIsConflict(t *testing.T) {
	h, sid, token := graphFixtureWithStatus(t, models.SessionStatusProvisioning, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer must not be called for a starting session")
	})
	w := networkRequest(t, h, sid, token, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "container_not_ready") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestGetSessionNetworkRejectsBadQuery(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("streamer must not be called for an invalid query")
	})
	for _, q := range []string{
		"limit=0",
		"limit=1001",
		"limit=ten",
		"type=" + strings.Repeat("x", networkMaxTypeLen+1),
		"host_contains=" + strings.Repeat("x", networkMaxHostLen+1),
		"url_contains=" + strings.Repeat("x", networkMaxURLLen+1),
	} {
		w := networkRequest(t, h, sid, token, q)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", q, w.Code)
		}
		if !strings.Contains(w.Body.String(), "invalid_network_query") {
			t.Fatalf("%s: body = %s", q, w.Body.String())
		}
	}
}

func TestGetSessionNetworkRejectsOversizedDump(t *testing.T) {
	h, sid, token := graphFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"entries":[]}`))
		_, _ = w.Write([]byte(strings.Repeat(" ", networkMaxResponseBytes)))
	})
	w := networkRequest(t, h, sid, token, "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "network_failed") {
		t.Fatalf("body = %s", w.Body.String())
	}
}
