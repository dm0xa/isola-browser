package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
	"github.com/google/uuid"
)

// stubDocker writes a fake `docker` CLI into a temp dir and prepends it to PATH,
// so the reconciliation can be driven without a container runtime. Each test
// supplies the script body, i.e. the runtime it wants to simulate.
func stubDocker(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// stubDockerCounting is stubDocker plus an invocation counter: the stub appends
// one byte to a file per call, so a test can wait for a known number of probes
// instead of guessing at wall-clock time. Each probe forks a shell, which is
// slow enough under load that a fixed sleep can come up short — and the health
// checker's ticker coalesces the ticks it misses, so "sleep long enough" is not
// a reliable way to guarantee N probes ran.
func stubDockerCounting(t *testing.T, body string) (probes func() int) {
	t.Helper()
	dir := t.TempDir()
	countFile := filepath.Join(dir, "count")
	script := "#!/bin/sh\nprintf x >> '" + countFile + "'\n" + body
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		b, err := os.ReadFile(countFile)
		if err != nil {
			return 0
		}
		return len(b)
	}
}

// runtimeAnswers is a runtime that reports container "alive123" as running and
// answers "no such object" for anything else — the shape the health checker
// treats as "the runtime answered; the container is not there".
const runtimeAnswers = `case "$1" in
  version) echo "27.0.0"; exit 0 ;;
esac
last=""
for a in "$@"; do last="$a"; done
case "$last" in
  alive123) echo true; exit 0 ;;
  *) echo 'Error: no such object' >&2; exit 125 ;;
esac
`

// runtimeDown is a daemon that is not reachable at all.
const runtimeDown = `echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?" >&2
exit 1
`

// seedRestored puts a session row straight into the store, as LoadSnapshot does.
func seedRestored(t *testing.T, ms *store.MemStore, status, containerID string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	exp := time.Now().Add(time.Hour)
	ms.RestoreSession(&models.Session{
		ID:          id,
		Status:      status,
		ContainerID: containerID,
		ExpiresAt:   &exp,
	})
	return id
}

func activeIDs(t *testing.T, st *store.Store) map[uuid.UUID]bool {
	t.Helper()
	sessions, err := st.Sessions.ListActiveSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[uuid.UUID]bool, len(sessions))
	for _, s := range sessions {
		out[s.ID] = true
	}
	return out
}

// A restored row whose container is gone must not survive startup: it would hold
// an admission slot against ISOLA_MAX_SESSIONS until the health checker has seen
// containerMissLimit misses (~45s), and it answers reconnects for a session that
// cannot render. A row that never recorded a container is finalized too.
func TestReconcileRestoredSessionsDropsUnbackedSessions(t *testing.T) {
	stubDocker(t, runtimeAnswers)

	ms := store.NewMemStore()
	defer ms.Close()
	st := store.NewMemBridgeStore(ms)
	cache := store.NewMemCache()
	defer cache.Close()

	alive := seedRestored(t, ms, models.SessionStatusRunning, "alive123")
	gone := seedRestored(t, ms, models.SessionStatusRunning, "gone456")
	neverProvisioned := seedRestored(t, ms, models.SessionStatusProvisioning, "")

	o, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	o.ReconcileRestoredSessions(context.Background(), st, cache)

	if ids := activeIDs(t, st); !ids[alive] {
		t.Error("session with a live container was dropped; it must survive")
	}
	if ids := activeIDs(t, st); ids[gone] {
		t.Error("session whose container is gone is still active")
	}
	if ids := activeIDs(t, st); ids[neverProvisioned] {
		t.Error("restored session that never got a container is still active")
	}

	// Finalized rows are terminated, not deleted outright (the store keeps them
	// briefly for the viewer/timer paths, then reaps them).
	if s, err := st.Sessions.GetSession(context.Background(), gone); err != nil {
		t.Fatalf("get gone session: %v", err)
	} else if s.Status != models.SessionStatusTerminated {
		t.Errorf("gone session status = %q, want %q", s.Status, models.SessionStatusTerminated)
	}
}

// If the runtime cannot be reached, nothing can be verified — every restored row
// is left for the health checker and the store reaper rather than dropped on a
// guess.
func TestReconcileRestoredSessionsKeepsStateWhenRuntimeDown(t *testing.T) {
	stubDocker(t, runtimeDown)

	ms := store.NewMemStore()
	defer ms.Close()
	st := store.NewMemBridgeStore(ms)
	cache := store.NewMemCache()
	defer cache.Close()

	alive := seedRestored(t, ms, models.SessionStatusRunning, "alive123")
	gone := seedRestored(t, ms, models.SessionStatusRunning, "gone456")

	o, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	o.ReconcileRestoredSessions(context.Background(), st, cache)

	ids := activeIDs(t, st)
	if !ids[alive] || !ids[gone] {
		t.Errorf("runtime unreachable must drop nothing; active = %v", ids)
	}
}

// No restored rows means no runtime probe at all.
func TestReconcileRestoredSessionsNoopWithoutSessions(t *testing.T) {
	stubDocker(t, runtimeDown)

	ms := store.NewMemStore()
	defer ms.Close()
	st := store.NewMemBridgeStore(ms)
	cache := store.NewMemCache()
	defer cache.Close()

	o, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	o.ReconcileRestoredSessions(context.Background(), st, cache)

	if ids := activeIDs(t, st); len(ids) != 0 {
		t.Errorf("expected no active sessions, got %v", ids)
	}
}
