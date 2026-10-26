package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/api"
	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/orchestrator"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

// TestAbandonSessionTerminatesAfterCancelledContext guards the create_session
// failure path: the caller never received the id, so an abandoned session can
// only be closed here — and it must still close when the caller's context is the
// thing that failed.
func TestAbandonSessionTerminatesAfterCancelledContext(t *testing.T) {
	t.Parallel()

	mem := store.NewMemStore()
	t.Cleanup(func() { mem.Close() })
	st := store.NewMemBridgeStore(mem)
	cache := store.NewMemCache()
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock") // lazy — no docker needed
	if err != nil {
		t.Fatal(err)
	}

	id := uuid.New()
	if err := st.Sessions.CreateSession(context.Background(), &models.Session{
		ID: id, Status: models.SessionStatusProvisioning, TargetURL: "https://example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Set(context.Background(), controlTokenKey(id.String()), "owner-token", time.Hour); err != nil {
		t.Fatal(err)
	}

	svc := &Service{deps: Deps{
		API:   api.New(st, cache, orch, &config.Config{}),
		Store: st,
		Cache: cache,
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.abandonSession(ctx, id.String())

	row, err := st.Sessions.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != models.SessionStatusTerminated {
		t.Fatalf("status = %s, want terminated: an abandoned session must not be left running", row.Status)
	}
}
