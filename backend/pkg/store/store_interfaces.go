package store

import (
	"context"
	"errors"
	"time"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/google/uuid"
)

// ErrMaxSessionsReached is returned by CreateSessionIfCapacity when the
// configured concurrent-session cap is already in use.
var ErrMaxSessionsReached = errors.New("max sessions reached")

// ErrSessionNotProvisioning is returned by ActivateSession when the row is no
// longer in the provisioning state. That happens when the owner deletes the
// session while its container is still being created: the activation must not
// resurrect a terminated row, and the caller must tear down the container it
// just started instead of leaving an untracked sandbox behind.
var ErrSessionNotProvisioning = errors.New("session is not provisioning")

// SessionStoreInterface is the session method set consumed by the handler,
// proxy, and orchestrator.
type SessionStoreInterface interface {
	CreateSession(ctx context.Context, session *models.Session) error
	// CreateSessionIfCapacity inserts the session only when fewer than max
	// active (provisioning + running) sessions exist, checked and inserted
	// atomically so concurrent launches cannot overshoot the cap. max <= 0
	// means unlimited. Returns ErrMaxSessionsReached at capacity.
	CreateSessionIfCapacity(ctx context.Context, session *models.Session, max int) error
	GetSession(ctx context.Context, id uuid.UUID) (*models.Session, error)
	ListActiveSessionsByUser(ctx context.Context, userID uuid.UUID) ([]models.Session, error)
	ListActiveSessions(ctx context.Context) ([]models.Session, error)
	UpdateSessionStatus(ctx context.Context, id uuid.UUID, status, containerID, containerHost string, containerPort int) error
	ActivateSession(ctx context.Context, id uuid.UUID, containerID, containerHost string, containerPort int, duration time.Duration) error
	TerminateSession(ctx context.Context, id uuid.UUID) error
	ListExpiredRunningSessions(ctx context.Context) ([]models.Session, error)
	ListRunningSessions(ctx context.Context) ([]models.Session, error)
	UpdateLastActivity(ctx context.Context, id uuid.UUID) error
	ExtendExpiry(ctx context.Context, id uuid.UUID, newExpiry, maxExpiry time.Time) error
	ListIdleSessions(ctx context.Context, idleTimeout time.Duration) ([]models.Session, error)
}
