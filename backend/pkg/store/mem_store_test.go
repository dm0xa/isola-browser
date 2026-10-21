package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
)

func TestCreateSessionIfCapacityUnlimited(t *testing.T) {
	ms := NewMemStore()
	defer ms.Close()
	st := NewMemBridgeStore(ms)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := st.Sessions.CreateSessionIfCapacity(ctx, &models.Session{
			ID:     uuid.New(),
			Status: models.SessionStatusRunning,
		}, 0); err != nil {
			t.Fatalf("max=0 (unlimited) refused session %d: %v", i, err)
		}
	}
}

func TestCreateSessionIfCapacityEnforcesCap(t *testing.T) {
	ms := NewMemStore()
	defer ms.Close()
	st := NewMemBridgeStore(ms)
	ctx := context.Background()

	// One running session fills the single slot.
	if err := st.Sessions.CreateSession(ctx, &models.Session{
		ID:     uuid.New(),
		Status: models.SessionStatusRunning,
	}); err != nil {
		t.Fatal(err)
	}

	err := st.Sessions.CreateSessionIfCapacity(ctx, &models.Session{
		ID:     uuid.New(),
		Status: models.SessionStatusProvisioning,
	}, 1)
	if !errors.Is(err, ErrMaxSessionsReached) {
		t.Fatalf("err = %v, want ErrMaxSessionsReached", err)
	}

	// The refused session must not have been inserted.
	active, _ := st.Sessions.ListActiveSessions(ctx)
	if len(active) != 1 {
		t.Fatalf("active sessions = %d, want 1 (refused insert must not persist)", len(active))
	}
}

func TestCreateSessionIfCapacityIgnoresInactive(t *testing.T) {
	ms := NewMemStore()
	defer ms.Close()
	st := NewMemBridgeStore(ms)
	ctx := context.Background()

	// Terminated/error rows do not count against the cap.
	for _, status := range []string{models.SessionStatusTerminated, models.SessionStatusError} {
		if err := st.Sessions.CreateSession(ctx, &models.Session{
			ID:     uuid.New(),
			Status: status,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.Sessions.CreateSessionIfCapacity(ctx, &models.Session{
		ID:     uuid.New(),
		Status: models.SessionStatusProvisioning,
	}, 1); err != nil {
		t.Fatalf("inactive rows counted against the cap: %v", err)
	}
}
