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
