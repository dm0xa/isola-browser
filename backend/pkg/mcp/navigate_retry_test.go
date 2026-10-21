package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

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
	st := store.NewMemBridgeStore(mem)
	if err := st.Sessions.CreateSession(context.Background(), &models.Session{
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
