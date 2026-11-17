package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
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

// MemStore is the in-memory session store. All data is held in a map with
// periodic TTL cleanup.
type MemStore struct {
	mu       sync.RWMutex
	sessions map[uuid.UUID]*models.Session

	stopCh chan struct{}
	doneCh chan struct{}
}

// NewMemStore creates a ready-to-use in-memory store and starts the TTL reaper.
func NewMemStore() *MemStore {
	ms := &MemStore{
		sessions: make(map[uuid.UUID]*models.Session),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	go ms.reapLoop()
	log.Info().Msg("memstore: init")
	return ms
}

const reapInterval = 30 * time.Second

func (ms *MemStore) reapLoop() {
	defer close(ms.doneCh)
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ms.reap()
		case <-ms.stopCh:
			return
		}
	}
}

func (ms *MemStore) reap() {
	now := time.Now()
	ms.mu.Lock()
	defer ms.mu.Unlock()

	for id, s := range ms.sessions {
		// Remove terminated sessions older than 1 hour.
		if s.Status == models.SessionStatusTerminated && s.TerminatedAt != nil {
			if now.Sub(*s.TerminatedAt) > 1*time.Hour {
				delete(ms.sessions, id)
			}
		}
		// Remove expired sessions (past expiry + 1 hour grace).
		if s.Status == models.SessionStatusRunning && s.ExpiresAt != nil {
			if now.Sub(*s.ExpiresAt) > 1*time.Hour {
				log.Debug().Str("session_id", id.String()).Msg("memstore: reaping expired")
				delete(ms.sessions, id)
			}
		}
		// Remove provisioning sessions stuck for > 10 minutes.
		if s.Status == models.SessionStatusProvisioning || s.Status == models.SessionStatusError {
			if now.Sub(s.CreatedAt) > 10*time.Minute {
				log.Debug().Str("session_id", id.String()).Msg("memstore: reaping stuck")
				delete(ms.sessions, id)
			}
		}
	}
}

// Close stops the reaper goroutine.
func (ms *MemStore) Close() error {
	close(ms.stopCh)
	<-ms.doneCh
	return nil
}

// Ping always returns nil — memory store is always healthy.
func (ms *MemStore) Ping(ctx context.Context) error {
	return nil
}

func (ms *MemStore) CreateSession(ctx context.Context, session *models.Session) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.insertLocked(session)
	return nil
}

// CreateSessionIfCapacity is CreateSession with an admission gate: when max > 0
// and the number of active (provisioning + running) sessions has already
// reached it, the insert is refused with ErrMaxSessionsReached. The count and
// the insert share one lock, so two concurrent launches cannot both pass the
// check and overshoot the cap by one.
func (ms *MemStore) CreateSessionIfCapacity(ctx context.Context, session *models.Session, max int) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if max > 0 {
		active := 0
		for _, sess := range ms.sessions {
			if isActiveSessionStatus(sess.Status) {
				active++
			}
		}
		if active >= max {
			return ErrMaxSessionsReached
		}
	}
	ms.insertLocked(session)
	return nil
}

// insertLocked stores a copy of the session so external mutation cannot reach
// the map. CreatedAt is set when missing so the idle reaper can compute
// time-since-creation correctly (the Go zero-time is 0001-01-01, which would
// make the session look ancient). Caller must hold ms.mu.
func (ms *MemStore) insertLocked(session *models.Session) {
	copy := *session
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = time.Now()
	}
	ms.sessions[session.ID] = &copy
}

func (ms *MemStore) GetSession(ctx context.Context, id uuid.UUID) (*models.Session, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	sess, ok := ms.sessions[id]
	if !ok {
		return nil, nil
	}
	copy := *sess
	return &copy, nil
}

func (ms *MemStore) ListActiveSessionsByUser(ctx context.Context, userID uuid.UUID) ([]models.Session, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	var out []models.Session
	for _, sess := range ms.sessions {
		if sess.UserID == userID && isActiveSessionStatus(sess.Status) {
			out = append(out, *sess)
		}
	}
	return out, nil
}

func (ms *MemStore) ListActiveSessions(ctx context.Context) ([]models.Session, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	var out []models.Session
	for _, sess := range ms.sessions {
		if isActiveSessionStatus(sess.Status) {
			out = append(out, *sess)
		}
	}
	return out, nil
}

func isActiveSessionStatus(status string) bool {
	return status == models.SessionStatusRunning || status == models.SessionStatusProvisioning
}

func (ms *MemStore) UpdateSessionStatus(ctx context.Context, id uuid.UUID, status, containerID, containerHost string, containerPort int) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	sess, ok := ms.sessions[id]
	if !ok {
		return nil
	}
	sess.Status = status
	sess.ContainerID = containerID
	sess.ContainerHost = containerHost
	sess.ContainerPort = containerPort
	return nil
}

func (ms *MemStore) ActivateSession(ctx context.Context, id uuid.UUID, containerID, containerHost string, containerPort int, duration time.Duration) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	sess, ok := ms.sessions[id]
	if !ok {
		return ErrSessionNotProvisioning
	}
	// Activating is only meaningful while provisioning. Terminating the
	// session during container creation used to be overwritten here, which
	// resurrected the row as "running" and hid the container from the orphan
	// sweep. Report it instead so the caller stops the container.
	if sess.Status != models.SessionStatusProvisioning {
		return ErrSessionNotProvisioning
	}
	sess.Status = models.SessionStatusRunning
	sess.ContainerID = containerID
	sess.ContainerHost = containerHost
	sess.ContainerPort = containerPort
	if duration > 0 {
		exp := time.Now().Add(duration)
		sess.ExpiresAt = &exp
	}
	// Mark activity so the idle reaper doesn't terminate the session before
	// any viewer connects.
	now := time.Now()
	sess.LastActivityAt = &now
	return nil
}

func (ms *MemStore) TerminateSession(ctx context.Context, id uuid.UUID) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	sess, ok := ms.sessions[id]
	if !ok {
		return nil
	}
	now := time.Now()
	sess.Status = models.SessionStatusTerminated
	sess.TerminatedAt = &now
	return nil
}

func (ms *MemStore) ListExpiredRunningSessions(ctx context.Context) ([]models.Session, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	now := time.Now()
	var out []models.Session
	for _, sess := range ms.sessions {
		if sess.Status == models.SessionStatusRunning && sess.ExpiresAt != nil && now.After(*sess.ExpiresAt) {
			out = append(out, *sess)
		}
	}
	return out, nil
}

func (ms *MemStore) ListRunningSessions(ctx context.Context) ([]models.Session, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	var out []models.Session
	for _, sess := range ms.sessions {
		if sess.Status == models.SessionStatusRunning {
			out = append(out, *sess)
		}
	}
	return out, nil
}

func (ms *MemStore) UpdateLastActivity(ctx context.Context, id uuid.UUID) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	sess, ok := ms.sessions[id]
	if !ok {
		return nil
	}
	now := time.Now()
	sess.LastActivityAt = &now
	return nil
}

func (ms *MemStore) ExtendExpiry(ctx context.Context, id uuid.UUID, newExpiry, maxExpiry time.Time) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	sess, ok := ms.sessions[id]
	if !ok || sess.Status != models.SessionStatusRunning {
		return nil
	}
	// Cap at maxExpiry
	if newExpiry.After(maxExpiry) {
		newExpiry = maxExpiry
	}
	sess.ExpiresAt = &newExpiry
	return nil
}

func (ms *MemStore) ListIdleSessions(ctx context.Context, idleTimeout time.Duration) ([]models.Session, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	now := time.Now()
	var out []models.Session
	for _, sess := range ms.sessions {
		if sess.Status != models.SessionStatusRunning {
			continue
		}
		ref := sess.CreatedAt
		if sess.LastActivityAt != nil {
			ref = *sess.LastActivityAt
		}
		if now.Sub(ref) > idleTimeout {
			out = append(out, *sess)
		}
	}
	return out, nil
}

// SnapshotSessions returns a copy of all sessions for persistence.
func (ms *MemStore) SnapshotSessions() []models.Session {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	out := make([]models.Session, 0, len(ms.sessions))
	for _, s := range ms.sessions {
		out = append(out, *s)
	}
	return out
}

// RestoreSession re-inserts a session from a snapshot file.
func (ms *MemStore) RestoreSession(s *models.Session) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	ms.sessions[s.ID] = s
}
