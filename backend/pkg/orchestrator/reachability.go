package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// ErrPublishUnreachable classifies a launch failure where the container
// runtime has stopped forwarding published ports: the mapping still exists in
// inspect, but the host cannot dial it. The sandbox may be perfectly alive, so
// this must not be reported as a dead container.
var ErrPublishUnreachable = errors.New("container host port publishing is down")

// PublishUnreachableError carries the evidence behind an ErrPublishUnreachable
// launch failure: which published address refused, and whether the container's
// own streamer answers on its loopback.
type PublishUnreachableError struct {
	ContainerID   string
	PublishedAddr string
	InternalAlive bool
	Err           error
}

func (e *PublishUnreachableError) Error() string {
	verdict := "the container's streamer did not answer on its loopback either"
	if e.InternalAlive {
		verdict = "the container's streamer is alive on its loopback"
	}
	msg := fmt.Sprintf(
		"container %s: published address %q is not reachable from the backend; host port publishing is down; restart the container runtime (e.g. colima restart); %s",
		e.ContainerID, e.PublishedAddr, verdict)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap exposes the sentinel so errors.Is(err, ErrPublishUnreachable) holds.
func (e *PublishUnreachableError) Unwrap() error { return ErrPublishUnreachable }

const (
	// internalStreamPort / internalCDPPort are the streamer's in-container
	// listeners. They are only usable from the host through the bridge IP.
	internalStreamPort = 5900
	internalCDPPort    = 9223

	dialProbeTimeout     = 500 * time.Millisecond
	livenessProbeTimeout = 3 * time.Second
)

// dialable reports whether this process can open a TCP connection to hostPort.
// A var so tests can drive published-port and bridge reachability without a
// container runtime.
var dialable = func(ctx context.Context, hostPort string) bool {
	if err := ctx.Err(); err != nil {
		return false
	}
	conn, err := net.DialTimeout("tcp", hostPort, dialProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// containerStreamAliveProbe asks the container itself whether its streamer is
// listening, bypassing the runtime's port publishing. A var so tests can inject
// the in-container verdict.
var containerStreamAliveProbe = inContainerStreamAlive

// inContainerStreamAlive runs a bash /dev/tcp request against the streamer's
// /healthz on the container's own loopback and treats a 200 status line as
// alive. Docker exec works even when published ports are not forwarded.
func inContainerStreamAlive(ctx context.Context, dockerHost, containerID string) bool {
	ctx, cancel := context.WithTimeout(ctx, livenessProbeTimeout)
	defer cancel()
	const script = `exec 3<>/dev/tcp/127.0.0.1/5900 && printf "GET /healthz HTTP/1.0\r\n\r\n" >&3 && head -1 <&3`
	args := []string{"exec", containerID, "bash", "-c", script}
	if dockerHost != "" && dockerHost != "unix:///var/run/docker.sock" {
		args = append([]string{"-H", dockerHost}, args...)
	}
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), " 200 ")
}

// streamEndpoint decides which address this backend process can use to reach a
// freshly created container's streamer. A published mapping in inspect is not
// proof of reachability, and the bridge IP is not routable from a host-run
// backend, so both are dialed before either is committed. When neither answers
// the container is probed from the inside: alive means the runtime stopped
// publishing (ErrPublishUnreachable), dead means the sandbox itself failed.
func (o *Orchestrator) streamEndpoint(ctx context.Context, containerID string, publishedPort, publishedCDPPort int, bridgeIP string) (host string, streamPort, cdpPort int, err error) {
	publishedAddr := "none"
	if publishedPort > 0 {
		publishedAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(publishedPort))
		if dialable(ctx, publishedAddr) {
			return "127.0.0.1", publishedPort, publishedCDPPort, nil
		}
		log.Info().Str("container_id", containerID).Int("stream_port", publishedPort).
			Msg("published port unreachable, checking browser-net")
	}

	bridgeAddr := "none"
	if bridgeIP != "" {
		bridgeAddr = net.JoinHostPort(bridgeIP, strconv.Itoa(internalStreamPort))
		if dialable(ctx, bridgeAddr) {
			return bridgeIP, internalStreamPort, internalCDPPort, nil
		}
	}

	if containerStreamAliveProbe(ctx, o.host, containerID) {
		return "", 0, 0, &PublishUnreachableError{
			ContainerID:   containerID,
			PublishedAddr: publishedAddr,
			InternalAlive: true,
			Err:           fmt.Errorf("published %s and bridge %s both refused", publishedAddr, bridgeAddr),
		}
	}
	return "", 0, 0, fmt.Errorf(
		"container %s: stream endpoint unreachable (published %s, bridge %s); sandbox is not alive",
		containerID, publishedAddr, bridgeAddr)
}
