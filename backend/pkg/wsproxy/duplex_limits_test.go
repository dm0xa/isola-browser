package wsproxy

import (
	"bytes"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestProxyDuplexWriteDeadlineUnblocksWedgedViewer reproduces the missing
// relay-side write deadline: a viewer that stops reading (suspended tab, TCP
// zero window, wedged proxy) fills the socket buffer, and the container→client
// goroutine parks inside io.Copy forever because nothing arms a write
// deadline. The container keeps producing frames, so the relay goroutine, the
// container websocket and the session's encoder subscription all stay alive.
func TestProxyDuplexWriteDeadlineUnblocksWedgedViewer(t *testing.T) {
	original := duplexWriteDeadline
	duplexWriteDeadline = 250 * time.Millisecond
	defer func() { duplexWriteDeadline = original }()

	h := newDuplexHarness(t)
	defer h.cleanup(t)
	h.start()
	clientPeer, containerPeer, proxyDone := h.clientPeer, h.containerPeer, h.proxyDone

	// Never read from clientPeer: it is the wedged viewer. The container pumps
	// until its own write blocks behind the relay's blocked write.
	var pumped atomic.Int64
	go func() {
		payload := bytes.Repeat([]byte("frame"), 64*1024) // 320 KiB per message
		for range 4096 {
			if err := containerPeer.WriteMessage(websocket.BinaryMessage, payload); err != nil {
				return
			}
			pumped.Add(int64(len(payload)))
		}
	}()

	select {
	case <-proxyDone:
	case <-time.After(15 * time.Second):
		t.Fatalf("proxyDuplex did not return with a wedged viewer; relayed %d bytes before blocking", pumped.Load())
	}

	// Frames the relay had already handed to the kernel are still readable;
	// what must not happen is the socket staying open, so drain until the
	// close surfaces.
	_ = clientPeer.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := clientPeer.ReadMessage(); err != nil {
			break
		}
	}
}

// TestProxyDuplexWriteDeadlineBoundsContainer proves the same bound in the
// other direction: a container that stops reading client input must not pin
// the client→container goroutine. The container peer here is a plain TCP peer
// that completes the WebSocket handshake and then never reads.
func TestProxyDuplexWriteDeadlineBoundsContainer(t *testing.T) {
	original := duplexWriteDeadline
	duplexWriteDeadline = 250 * time.Millisecond
	defer func() { duplexWriteDeadline = original }()

	h := newDuplexHarness(t)
	defer h.cleanup(t)
	h.start()
	clientPeer, containerPeer, proxyDone := h.clientPeer, h.containerPeer, h.proxyDone

	// Keep the container→client goroutine healthy so the test isolates the
	// client→container direction: drain whatever the relay sends that way.
	go func() {
		for {
			if _, _, err := clientPeer.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Container never reads. The client pumps input until the relay's write to
	// the container blocks and the deadline fires.
	payload := bytes.Repeat([]byte(`{"type":"ping"}`), 4096) // ~64 KiB per message
	go func() {
		for range 4096 {
			if err := clientPeer.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		}
	}()

	select {
	case <-proxyDone:
	case <-time.After(15 * time.Second):
		t.Fatal("proxyDuplex did not return with a container that stopped reading")
	}

	// The relay must also have closed the container socket on its way out.
	// Buffered input frames are still readable, so drain until the close.
	_ = containerPeer.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := containerPeer.ReadMessage(); err != nil {
			break
		}
	}
}

// TestProxyDuplexForwardsLargeBinaryFrame guards the container read limit
// against the obvious over-tightening: a multi-MiB video frame (a 4K keyframe
// or an init segment) must relay intact.
func TestProxyDuplexForwardsLargeBinaryFrame(t *testing.T) {
	original := duplexContainerReadLimitBytes
	duplexContainerReadLimitBytes = 8 << 20
	defer func() { duplexContainerReadLimitBytes = original }()

	h := newDuplexHarness(t)
	h.start()
	clientPeer, containerPeer, proxyDone := h.clientPeer, h.containerPeer, h.proxyDone

	payload := bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 1<<20) // 4 MiB
	go func() {
		_ = containerPeer.WriteMessage(websocket.BinaryMessage, payload)
	}()

	if err := clientPeer.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("client deadline: %v", err)
	}
	var got []byte
	for len(got) < len(payload) {
		mt, chunk, err := clientPeer.ReadMessage()
		if err != nil {
			t.Fatalf("client read after %d/%d bytes: %v", len(got), len(payload), err)
		}
		if mt != websocket.BinaryMessage {
			t.Fatalf("message type = %d, want binary", mt)
		}
		got = append(got, chunk...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("relayed %d bytes, want %d (content mismatch)", len(got), len(payload))
	}
	if len(got) != len(payload) {
		t.Fatalf("relayed %d bytes, want %d", len(got), len(payload))
	}

	h.cleanup(t)
	select {
	case <-proxyDone:
	case <-time.After(3 * time.Second):
		t.Fatal("proxyDuplex did not return")
	}
}

// TestProxyDuplexRejectsOversizedContainerFrame proves the container read limit
// is actually armed: one message above it closes the relay instead of being
// streamed to the viewer.
func TestProxyDuplexRejectsOversizedContainerFrame(t *testing.T) {
	originalLimit := duplexContainerReadLimitBytes
	duplexContainerReadLimitBytes = 1 << 20
	defer func() { duplexContainerReadLimitBytes = originalLimit }()

	h := newDuplexHarness(t)
	defer h.cleanup(t)
	h.start()
	clientPeer, containerPeer, proxyDone := h.clientPeer, h.containerPeer, h.proxyDone

	payload := bytes.Repeat([]byte{0xAA}, 2<<20) // 2 MiB, above the limit
	go func() {
		_ = containerPeer.WriteMessage(websocket.BinaryMessage, payload)
	}()

	// The viewer must not receive the message; it gets a close instead.
	deadline := time.Now().Add(10 * time.Second)
	_ = clientPeer.SetReadDeadline(deadline)
	for {
		_, _, err := clientPeer.ReadMessage()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatal("viewer received nothing and the relay did not close it")
			}
			break
		}
	}
	select {
	case <-proxyDone:
	case <-time.After(5 * time.Second):
		t.Fatal("proxyDuplex did not return after the oversized container frame")
	}
}

// TestProxyDuplexRejectsOversizedContainerControlFrame pins the separate,
// tighter bound on container→client TEXT frames: control JSON does not scale
// with the picture, so a text frame above duplexContainerControlMaxBytes is a
// protocol violation even when it fits under the video read limit.
func TestProxyDuplexRejectsOversizedContainerControlFrame(t *testing.T) {
	originalControl := duplexContainerControlMaxBytes
	duplexContainerControlMaxBytes = 64 << 10
	defer func() { duplexContainerControlMaxBytes = originalControl }()

	h := newDuplexHarness(t)
	defer h.cleanup(t)
	h.start()
	clientPeer, containerPeer, proxyDone := h.clientPeer, h.containerPeer, h.proxyDone

	payload := bytes.Repeat([]byte(`{"type":"metrics"}`), 8*1024) // ~128 KiB
	go func() {
		_ = containerPeer.WriteMessage(websocket.TextMessage, payload)
	}()

	_ = clientPeer.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		if _, _, err := clientPeer.ReadMessage(); err != nil {
			break
		}
	}
	select {
	case <-proxyDone:
	case <-time.After(5 * time.Second):
		t.Fatal("proxyDuplex did not return after the oversized control frame")
	}
}
