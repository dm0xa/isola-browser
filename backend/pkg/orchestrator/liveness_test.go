package orchestrator

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// TestWaitForStreamDetectsExitedSandbox pins the launch-time liveness check: a
// sandbox that exits on its own (the Tor gate fails closed, the browser dies)
// must be reported as sandbox_exited the moment docker says so, not as a
// streamer timeout after the full deadline, and a runtime that cannot answer
// must never fail an otherwise healthy launch.
func TestWaitForStreamDetectsExitedSandbox(t *testing.T) {
	// A closed listener: nothing in this test ever answers /healthz, so only the
	// liveness verdict decides the outcome.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	tests := []struct {
		name    string
		docker  string
		wantErr error
	}{
		{
			name: "container stopped",
			docker: `for a in "$@"; do
  case "$a" in
    inspect) echo false; exit 0 ;;
    logs) echo '[browser-node-electron] FATAL: Tor bootstrap failed after 45s' ;;
  esac
done
exit 0`,
			wantErr: ErrSandboxExited,
		},
		{
			name: "container removed by --rm",
			docker: `for a in "$@"; do
  case "$a" in
    inspect) echo 'Error: No such container: 81e58b080ac8' >&2; exit 1 ;;
  esac
done
exit 0`,
			wantErr: ErrSandboxExited,
		},
		{
			name: "container running",
			docker: `for a in "$@"; do
  case "$a" in
    inspect) echo true; exit 0 ;;
  esac
done
exit 0`,
			// Only an explicit "false" is dead: a healthy sandbox keeps waiting.
			wantErr: nil,
		},
		{
			name: "runtime cannot answer",
			docker: `for a in "$@"; do
  case "$a" in
    inspect) echo 'Cannot connect to the Docker daemon at unix:///x.sock' >&2; exit 1 ;;
  esac
done
exit 0`,
			// Inconclusive inspect keeps the wait running to its deadline.
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubDocker(t, tc.docker)
			orch, err := New("unix:///nonexistent/docker.sock")
			if err != nil {
				t.Fatal(err)
			}

			// A four-second budget: the exit is noticed on the first liveness tick,
			// so an exit that went unnoticed would surface as the plain deadline
			// error below instead of ErrSandboxExited, with no wall-clock assertion
			// to go flaky under load.
			err = orch.WaitForStream(context.Background(), "81e58b080ac8", "127.0.0.1", port, "isola", "chromium", 4*time.Second)

			switch {
			case tc.wantErr == nil:
				if err == nil {
					t.Fatal("WaitForStream() = nil, want a deadline error")
				}
				if errors.Is(err, ErrSandboxExited) {
					t.Fatalf("WaitForStream() = %v, want a plain timeout", err)
				}
			default:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("WaitForStream() = %v, want %v", err, tc.wantErr)
				}
			}
		})
	}
}

// TestSandboxExitedErrorNamesTheReason keeps the entrypoint's own log line on
// the failure: without it "the sandbox exited" is unactionable.
func TestSandboxExitedErrorNamesTheReason(t *testing.T) {
	err := &SandboxExitedError{
		ContainerID: "81e58b080ac8",
		Elapsed:     31 * time.Second,
		LastLog:     "FATAL: Tor bootstrap failed after 45s",
	}
	msg := err.Error()
	if !errors.Is(err, ErrSandboxExited) {
		t.Fatal("SandboxExitedError must unwrap to ErrSandboxExited")
	}
	for _, want := range []string{"81e58b080ac8", "31s", "Tor bootstrap failed"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Error() = %q, want it to contain %q", msg, want)
		}
	}
}
