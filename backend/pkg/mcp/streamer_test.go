package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

func TestGetNetworkCallsStreamer(t *testing.T) {
	longURL := "https://example.com/" + strings.Repeat("a", 350)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/network" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries": []map[string]any{{"url": longURL, "method": "GET"}},
		})
	}))
	defer srv.Close()

	cache := store.NewMemCache()
	sessionID := uuid.New().String()
	_ = cache.Set(context.Background(), sessionAddrKey(sessionID), srv.Listener.Addr().String(), 0)

	mem := store.NewMemStore()
	st := store.NewMemBridgeStore(mem)
	sess := models.Session{ID: uuid.MustParse(sessionID), Status: models.SessionStatusRunning, TargetURL: "https://example.com"}
	if err := st.Sessions.CreateSession(context.Background(), &sess); err != nil {
		t.Fatal(err)
	}

	svc := &Service{deps: Deps{Store: st, Cache: cache}}
	body, _, err := svc.streamerGET(context.Background(), sessionID, "/network")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Entries []struct {
			URL string `json:"url"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Entries) != 1 || len(parsed.Entries[0].URL) < 350 {
		t.Fatalf("unexpected network body: %s", body)
	}
}

// TestToolGetNetworkRejectsOversizedResponse guards the payload cap: a huge
// window must produce an actionable error instead of megabytes of text in one
// JSON-RPC reply.
func TestToolGetNetworkRejectsOversizedResponse(t *testing.T) {
	t.Parallel()

	entries := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		entries = append(entries, `{"url":"https://example.com/app.js","type":"Script"}`)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/network" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"entries":[` + strings.Join(entries, ",") + `]}`))
	}))
	defer srv.Close()

	id := uuid.New().String()
	svc := newStreamerService(t, id, srv.Listener.Addr().String())
	svc.networkMaxBytes = 64

	_, _, err := svc.toolGetNetwork(context.Background(), nil, &getNetworkParams{SessionID: id})
	if err == nil {
		t.Fatal("expected the oversized network log to be refused")
	}
	if !strings.Contains(err.Error(), "limit=200") {
		t.Fatalf("error must tell the agent how to shrink the query: %v", err)
	}
}
