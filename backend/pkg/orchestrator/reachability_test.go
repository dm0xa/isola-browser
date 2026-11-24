package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
)

// TestReachabilityStreamEndpoint pins the launch verdict against the four states the
// backend can observe: published port reachable; published refused with a live
// container and an unreachable bridge (runtime publishing down); published and
// bridge refused with a dead container; and published refused with a dialable
// bridge. Both probes are injected, so no Docker daemon is involved.
func TestReachabilityStreamEndpoint(t *testing.T) {
	const (
		containerID   = "c0ffee123456"
		bridgeIP      = "172.19.0.3"
		publishedPort = 55123
		publishedCDP  = 55124
	)

	publishedAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(publishedPort))
	bridgeAddr := net.JoinHostPort(bridgeIP, strconv.Itoa(internalStreamPort))

	tests := []struct {
		name           string
		dialable       map[string]bool
		internalAlive  bool
		wantHost       string
		wantPort       int
		wantCDP        int
		wantErr        bool
		wantPublishErr bool
	}{
		{
			name:          "published address reachable keeps existing behaviour",
			dialable:      map[string]bool{publishedAddr: true},
			internalAlive: true,
			wantHost:      "127.0.0.1",
			wantPort:      publishedPort,
			wantCDP:       publishedCDP,
		},
		{
			name:           "published refused, container alive, bridge unreachable is publish-unreachable",
			dialable:       map[string]bool{},
			internalAlive:  true,
			wantErr:        true,
			wantPublishErr: true,
		},
		{
			name:          "published refused, container dead is a plain error",
			dialable:      map[string]bool{},
			internalAlive: false,
			wantErr:       true,
		},
		{
			name:          "published refused, bridge dialable uses the bridge address",
			dialable:      map[string]bool{bridgeAddr: true},
			internalAlive: false,
			wantHost:      bridgeIP,
			wantPort:      internalStreamPort,
			wantCDP:       internalCDPPort,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origDialable := dialable
			t.Cleanup(func() { dialable = origDialable })
			dialable = func(_ context.Context, hostPort string) bool {
				return tc.dialable[hostPort]
			}

			origAlive := containerStreamAliveProbe
			t.Cleanup(func() { containerStreamAliveProbe = origAlive })
			var probed string
			containerStreamAliveProbe = func(_ context.Context, _ string, id string) bool {
				probed = id
				return tc.internalAlive
			}

			o := &Orchestrator{}
			host, port, cdpPort, err := o.streamEndpoint(
				context.Background(), containerID, publishedPort, publishedCDP, bridgeIP)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("streamEndpoint() error = nil, want an error")
				}
			} else if err != nil {
				t.Fatalf("streamEndpoint() error = %v, want nil", err)
			}

			if got := errors.Is(err, ErrPublishUnreachable); got != tc.wantPublishErr {
				t.Fatalf("errors.Is(err, ErrPublishUnreachable) = %v, want %v (err: %v)", got, tc.wantPublishErr, err)
			}

			if tc.wantPublishErr {
				// The sentinel must survive wrapping, and the error must carry
				// the evidence an operator needs to act on.
				wrapped := fmt.Errorf("create session: %w", err)
				if !errors.Is(wrapped, ErrPublishUnreachable) {
					t.Fatalf("wrapped error does not match ErrPublishUnreachable")
				}
				var pubErr *PublishUnreachableError
				if !errors.As(err, &pubErr) {
					t.Fatalf("error is not *PublishUnreachableError: %T", err)
				}
				if pubErr.ContainerID != containerID || pubErr.PublishedAddr != publishedAddr || !pubErr.InternalAlive {
					t.Fatalf("unexpected PublishUnreachableError: %+v", pubErr)
				}
				if got := probed; got != containerID {
					t.Fatalf("liveness probe ran for %q, want %q", got, containerID)
				}
				for _, want := range []string{publishedAddr, "host port publishing is down", "colima restart"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error %q does not mention %q", err.Error(), want)
					}
				}
				return
			}

			if tc.wantErr {
				return
			}
			if host != tc.wantHost || port != tc.wantPort || cdpPort != tc.wantCDP {
				t.Fatalf("streamEndpoint() = (%q, %d, %d), want (%q, %d, %d)",
					host, port, cdpPort, tc.wantHost, tc.wantPort, tc.wantCDP)
			}
		})
	}
}
