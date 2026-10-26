package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

// newStreamerService builds a Service pointing at a fake streamer address, with
// one running session in the store so ensureSession passes.
func newStreamerService(t *testing.T, sessionID, addr string) *Service {
	t.Helper()

	mem := store.NewMemStore()
	t.Cleanup(func() { mem.Close() })
	st := store.NewMemBridgeStore(mem)
	cache := store.NewMemCache()

	sid, err := uuid.Parse(sessionID)
	if err != nil {
		t.Fatalf("session id: %v", err)
	}
	if err := st.Sessions.CreateSession(context.Background(), &models.Session{
		ID: sid, Status: models.SessionStatusRunning, TargetURL: "https://example.com",
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := cache.Set(context.Background(), sessionAddrKey(sessionID), addr, time.Hour); err != nil {
		t.Fatalf("seed address: %v", err)
	}
	return &Service{deps: Deps{Store: st, Cache: cache}}
}
