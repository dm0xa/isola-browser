package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

func TestNavigateStreamerRetries503(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/navigate" {
			http.NotFound(w, r)
			return
		}
		if attempts.Add(1) < 3 {
			http.Error(w, "runner ctrl socket not connected", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cache := store.NewMemCache()
	sessionID := uuid.New().String()
	_ = cache.Set(context.Background(), sessionAddrKey(sessionID), srv.Listener.Addr().String(), 0)
	mem := store.NewMemStore()
	st := mem
	if err := st.CreateSession(context.Background(), &models.Session{
		ID: uuid.MustParse(sessionID), Status: models.SessionStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{deps: Deps{Store: st, Cache: cache}}
	if err := svc.navigateStreamer(context.Background(), sessionID, "https://example.com/"); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
}

// TestNavigateStreamerRetriesTimeout guards the per-attempt deadline: a streamer
// that stops answering must not end the navigation, because the retry loop still
// has budget left.
func TestNavigateStreamerRetriesTimeout(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/navigate" {
			http.NotFound(w, r)
			return
		}
		if attempts.Add(1) == 1 {
			time.Sleep(150 * time.Millisecond) // longer than the attempt budget
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	id := uuid.New().String()
	svc := newStreamerService(t, id, srv.Listener.Addr().String())
	svc.navigateAttempt = 40 * time.Millisecond

	if err := svc.navigateStreamer(context.Background(), id, "https://example.com/"); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2 (first attempt timed out)", attempts.Load())
	}
}

// TestDocumentSeenIgnoresEarlierDocuments guards navigate's document_seen: the
// network log is cumulative, so a Document captured before the navigation must
// not make a failed load look successful.
func TestDocumentSeenIgnoresEarlierDocuments(t *testing.T) {
	var entries atomic.Value
	entries.Store([]map[string]any{
		{"id": "d1", "ts": 1000, "url": "https://example.com/", "type": "Document", "status": 200},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/network" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries.Load()})
	}))
	defer srv.Close()

	id := uuid.New().String()
	svc := newStreamerService(t, id, srv.Listener.Addr().String())
	seen := svc.snapshotNetworkKeys(context.Background(), id)
	if len(seen) != 1 {
		t.Fatalf("baseline = %v, want one entry", seen)
	}

	// Only the old document plus a *failed* new one (status 0): not a load.
	entries.Store([]map[string]any{
		{"id": "d1", "ts": 1000, "url": "https://example.com/", "type": "Document", "status": 200},
		{"id": "d2", "ts": 2000, "url": "https://qa-nonexistent.invalid/", "type": "Document", "status": 0, "failed": true},
	})
	if got := svc.waitForDocumentNetwork(context.Background(), id, seen, 500*time.Millisecond); got {
		t.Fatal("document_seen = true for a failed navigation")
	}

	// A new successful Document is this navigation's result.
	entries.Store([]map[string]any{
		{"id": "d1", "ts": 1000, "url": "https://example.com/", "type": "Document", "status": 200},
		{"id": "d3", "ts": 3000, "url": "https://example.org/", "type": "Document", "status": 200},
	})
	if got := svc.waitForDocumentNetwork(context.Background(), id, seen, 2*time.Second); !got {
		t.Fatal("document_seen = false for a new successful document")
	}
}
