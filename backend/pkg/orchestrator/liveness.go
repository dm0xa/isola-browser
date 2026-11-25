package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrSandboxExited classifies a launch failure where the sandbox container
// stopped on its own while the backend was still waiting for its streamer. The
// entrypoint is what exits: the Tor bootstrap gate fails closed after its
// deadline, or the browser process dies. A --rm container is then gone, so
// without this check the streamer wait would spend the rest of its deadline
// dialing a container that no longer exists and blame the streamer.
var ErrSandboxExited = errors.New("sandbox exited before its stream was ready")

// SandboxExitedError carries the evidence behind an ErrSandboxExited launch
// failure: which container went away, how long the wait had been running, and
// the container's last log line (the entrypoint's own reason, when docker still
// has it).
type SandboxExitedError struct {
	ContainerID string
	Elapsed     time.Duration
	LastLog     string
}

func (e *SandboxExitedError) Error() string {
	msg := fmt.Sprintf("container %s exited on its own after %v while waiting for the streamer", e.ContainerID, e.Elapsed.Round(time.Second))
	if e.LastLog != "" {
		msg += "; last container log: " + e.LastLog
	}
	return msg
}

// Unwrap exposes the sentinel so errors.Is(err, ErrSandboxExited) holds.
func (e *SandboxExitedError) Unwrap() error { return ErrSandboxExited }

// containerExited answers "is this container definitively stopped or gone".
// Only a positive answer counts: an inspect that reports Running=true, or a
// runtime that cannot answer at all, must never fail a healthy launch.
func (o *Orchestrator) containerExited(ctx context.Context, containerID string) bool {
	if containerID == "" {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	out, err := o.dockerCmd(probeCtx, "inspect", "--format", "{{.State.Running}}", containerID).CombinedOutput()
	if err == nil {
		// Docker answered: only an explicit "false" is a dead container.
		return strings.TrimSpace(string(out)) == "false"
	}
	// Docker failed. "No such container" is the --rm aftermath of an exit;
	// every other error (daemon down, context deadline) says nothing about
	// this sandbox.
	return containerAbsent(string(out))
}

// containerLastLog returns the container's last non-empty log line, trimmed to
// a log-friendly length. Best effort: a removed container returns "".
func (o *Orchestrator) containerLastLog(ctx context.Context, containerID string) string {
	if containerID == "" {
		return ""
	}
	logCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// Output(), not CombinedOutput(): on a removed container docker writes
	// "Error response from daemon: No such container" to stderr, and that daemon
	// complaint must never be mistaken for the sandbox's own last log line.
	out, err := o.dockerCmd(logCtx, "logs", "--tail", "5", containerID).Output()
	if err != nil && len(out) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		return line
	}
	return ""
}
