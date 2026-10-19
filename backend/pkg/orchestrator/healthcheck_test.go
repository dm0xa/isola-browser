package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/dm0xa/isola/backend/pkg/models"
	"github.com/dm0xa/isola/backend/pkg/store"
)

// TestRuntimeUnreachable pins the discriminator used by the health checker to
// tell "the runtime could not be reached" (skip this round) from "the runtime
// answered, the container is not there" (count a miss towards finalization).
//
// Both cases exit non-zero, so the exit code cannot be used, and the strings
// below are the real ones observed from the two CLIs:
//   - `podman inspect` on a removed container: `Error: no such object: "..."`,
//     exit 125.
//   - `docker -H <podman socket> inspect` on a removed container: the compat
//     API tries to read the bare hex ID as an image name and fails with an
//     image-name normalization error, exit 1. Missing this string is what let a
//     removed container look like an unreachable daemon and leak the session.
func TestRuntimeUnreachable(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "empty output",
			output: "",
			want:   false,
		},
		{
			name:   "podman: removed container",
			output: "Error: no such object: \"033214aab4ab171c54a82427eb6ad346ff78b47655c9831e4a9e008267737b09\"\n",
			want:   false,
		},
		{
			name:   "docker cli over podman compat api: removed container",
			output: `Error response from daemon: normalizing image: normalizing name "033214aab4ab171c54a82427eb6ad346ff78b47655c9831e4a9e008267737b09" (orig: "033214aab4ab171c54a82427eb6ad346ff78b47655c9831e4a9e008267737b09") for compat API: invalid repository name (033214aab4ab171c54a82427eb6ad346ff78b47655c9831e4a9e008267737b09), cannot specify 64-byte hexadecimal strings`,
			want:   false,
		},
		{
			name:   "missing socket",
			output: "failed to connect to the docker API at unix:///tmp/definitely-no-socket.sock; check if the path is correct and if the daemon is running: dial unix /tmp/definitely-no-socket.sock: connect: no such file or directory",
			want:   true,
		},
		{
			name:   "daemon down",
			output: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
			want:   true,
		},
		{
			name:   "connection refused",
			output: "Error: failed to connect: dial tcp 127.0.0.1:2375: connect: connection refused",
			want:   true,
		},
		{
			name:   "probe timed out",
			output: "error during connect: context deadline exceeded",
			want:   true,
		},
		{
			name:   "case insensitive",
			output: "FAILED TO CONNECT TO THE DOCKER API AT UNIX:///tmp/x.sock",
			want:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := runtimeUnreachable(tc.output); got != tc.want {
				t.Errorf("runtimeUnreachable(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// TestContainerAbsent is the other half of the discriminator, and the one the
// health checker now requires before it counts a miss. Note the first three
// cases: they are all failures that runtimeUnreachable deliberately reports as
// false, so before this existed they fell through to "the runtime answered: the
// container is dead" and finalized live sessions.
func TestContainerAbsent(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "empty output — says nothing about the container",
			output: "",
			want:   false,
		},
		{
			name:   "probe timed out — a killed process writes no output",
			output: "signal: killed",
			want:   false,
		},
		{
			name:   "daemon error we do not model",
			output: "Error response from daemon: something new",
			want:   false,
		},
		{
			name:   "podman: removed container",
			output: "Error: no such object: \"033214aab4ab171c54a82427eb6ad346ff78b47655c9831e4a9e008267737b09\"\n",
			want:   true,
		},
		{
			name:   "docker: removed container",
			output: "Error response from daemon: No such container: isola-0e30b942-d68\n",
			want:   true,
		},
		{
			name:   "docker cli over podman compat api: removed container",
			output: `Error response from daemon: normalizing image: normalizing name "033214aab4ab171c54a82427eb6ad346ff78b47655c9831e4a9e008267737b09" (orig: "033214aab4ab171c54a82427eb6ad346ff78b47655c9831e4a9e008267737b09") for compat API: invalid repository name (033214aab4ab171c54a82427eb6ad346ff78b47655c9831e4a9e008267737b09), cannot specify 64-byte hexadecimal strings`,
			want:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := containerAbsent(tc.output); got != tc.want {
				t.Errorf("containerAbsent(%q) = %v, want %v", tc.output, got, tc.want)
			}
			if runtimeUnreachable(tc.output) && tc.want {
				t.Errorf("output %q is both unreachable and absent", tc.output)
			}
		})
	}
}

// waitFor polls cond until it holds or the deadline passes. It is how these
// tests synchronise with the health checker's goroutine without depending on
// how fast the machine forks a shell process.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestHealthCheckerSkipsInconclusiveProbe is the regression test for the
// session-killing bug: `docker inspect` failing for a reason that says nothing
// about the container. Three of those in a row used to finalize a live session
// while the daemon still reported its container Up — the viewer then sat on
// "Reconnecting" forever because the session it belonged to had been deleted.
func TestHealthCheckerSkipsInconclusiveProbe(t *testing.T) {
	// Non-zero exit, no output at all: neither a connectivity signature nor a
	// statement that the container is missing.
	probes := stubDockerCounting(t, "exit 1\n")

	ms := store.NewMemStore()
	defer ms.Close()
	st := store.NewMemBridgeStore(ms)
	cache := store.NewMemCache()
	defer cache.Close()
	id := seedRestored(t, ms, models.SessionStatusRunning, "033214aab4ab171c54a82427eb6ad346ff78b47655c9831e4a9e008267737b09")

	o, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.StartHealthChecker(ctx, st, cache, 2*time.Millisecond)

	// Wait until well more than containerMissLimit probes have run, so the test
	// fails for the right reason if too few ever do.
	waitFor(t, "the health checker to probe past the miss limit", func() bool {
		return probes() > containerMissLimit+2
	})

	if ids := activeIDs(t, st); !ids[id] {
		t.Fatal("an inconclusive probe finalized a live session; it must be skipped, never counted as a miss")
	}
}

// TestHealthCheckerFinalizesAbsentContainer keeps the inverse honest: a probe
// that positively reports the container is not there must still finalize, or a
// genuinely dead session leaks its row and its admission slot.
func TestHealthCheckerFinalizesAbsentContainer(t *testing.T) {
	// The podman-compat signature for a removed container, minus the embedded
	// double quotes: `printf` keeps the text intact without shell quoting games.
	stubDockerCounting(t, "printf 'Error response from daemon: normalizing image: normalizing name 033214aa for compat API: invalid repository name (033214aa), cannot specify 64-byte hexadecimal strings\\n' >&2\nexit 1\n")

	ms := store.NewMemStore()
	defer ms.Close()
	st := store.NewMemBridgeStore(ms)
	cache := store.NewMemCache()
	defer cache.Close()
	id := seedRestored(t, ms, models.SessionStatusRunning, "033214aab4ab171c54a82427eb6ad346ff78b47655c9831e4a9e008267737b09")

	o, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.StartHealthChecker(ctx, st, cache, 2*time.Millisecond)

	// Finalizing needs containerMissLimit consecutive misses, so reaching this
	// state proves the checker counted them.
	waitFor(t, "a container-absent probe to finalize the session", func() bool {
		ids := activeIDs(t, st)
		return !ids[id]
	})
}
