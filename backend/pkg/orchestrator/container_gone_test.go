package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

// TestFinalizeIfContainerGone pins the relay's death verdict against the three
// runtime answers it can get. The relay already knows the streamer endpoint is
// unreachable; only a positive "this container is stopped or gone" may destroy
// the session, because a runtime that cannot answer (or reports it running)
// says nothing about this sandbox and a false finalize costs the user their work.
func TestFinalizeIfContainerGone(t *testing.T) {
	tests := []struct {
		name         string
		docker       string
		wantFinalize bool
		wantStatus   string
	}{
		{
			name: "container gone",
			docker: `for a in "$@"; do
  case "$a" in
    inspect) echo 'Error: no such object: "0b132d730c9b"' >&2; exit 1 ;;
    stop|rm) exit 0 ;;
  esac
done
exit 1`,
			wantFinalize: true,
			wantStatus:   models.SessionStatusTerminated,
		},
		{
			name: "container still running",
			docker: `for a in "$@"; do
  case "$a" in
    inspect) echo true; exit 0 ;;
    stop|rm) exit 0 ;;
  esac
done
exit 1`,
			wantFinalize: false,
			wantStatus:   models.SessionStatusRunning,
		},
		{
			name: "runtime unreachable",
			docker: `for a in "$@"; do
  case "$a" in
    inspect) echo 'Cannot connect to the Docker daemon at unix:///x.sock. Is the docker daemon running?' >&2; exit 1 ;;
    stop|rm) exit 0 ;;
  esac
done
exit 1`,
			wantFinalize: false,
			wantStatus:   models.SessionStatusRunning,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubDocker(t, tc.docker)

			ctx := context.Background()
			st := store.NewMemBridgeStore(store.NewMemStore())
			cache := store.NewMemCache()
			orch, err := New("unix:///nonexistent/docker.sock")
			if err != nil {
				t.Fatal(err)
			}

			sid := uuid.New()
			if err := st.Sessions.CreateSession(ctx, &models.Session{
				ID:        sid,
				UserID:    uuid.Nil,
				Status:    models.SessionStatusProvisioning,
				TargetURL: "https://example.com",
				CreatedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.Sessions.ActivateSession(ctx, sid, "0b132d730c9b", "127.0.0.1", 5900, 10*time.Minute); err != nil {
				t.Fatal(err)
			}

			if got := orch.FinalizeIfContainerGone(ctx, st, cache, sid); got != tc.wantFinalize {
				t.Fatalf("FinalizeIfContainerGone() = %v, want %v", got, tc.wantFinalize)
			}

			sess, err := st.Sessions.GetSession(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			if sess.Status != tc.wantStatus {
				t.Fatalf("session status = %q, want %q", sess.Status, tc.wantStatus)
			}
			if _, ok := orch.reaping.Load(sid.String()); ok {
				t.Fatal("the per-session reap guard was left behind")
			}
		})
	}
}
