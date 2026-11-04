package wsproxy

import (
	"bytes"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// duplexHarness is a proxyDuplex pair wired the way the real handler wires it:
// closeAll closes `done` and the *server side* of the client socket only. The
// container socket is owned by the caller (in production by the
// `defer containerWS.Close()` in HandleWebSocket) and is deliberately NOT part
// of closeAll, so a relay that needs the container socket closed to unblock
// itself has to do it on its own.
//
// Setup is split from start so a test can take its goroutine baseline after the
// httptest servers exist but before the relay's three goroutines do.
type duplexHarness struct {
	clientPeer    *websocket.Conn
	containerPeer *websocket.Conn
	proxyDone     chan struct{}
	clientConn    *websocket.Conn
	containerConn *websocket.Conn
	done          chan struct{}
	closeAll      func()
	cleanupClient func()
	cleanupCont   func()
}

func newDuplexHarness(t *testing.T) *duplexHarness {
	t.Helper()
	h := &duplexHarness{}
	h.clientConn, h.clientPeer, h.cleanupClient = newWebSocketPair(t)
	h.containerConn, h.containerPeer, h.cleanupCont = newWebSocketPair(t)
	h.done = make(chan struct{})
	var closeOnce sync.Once
	h.closeAll = func() {
		closeOnce.Do(func() {
			close(h.done)
			_ = h.clientConn.Close()
		})
	}
	h.proxyDone = make(chan struct{})
	return h
}

func (h *duplexHarness) start() {
	go func() {
		defer close(h.proxyDone)
		proxyDuplex(h.clientConn, h.containerConn, "sess-lifecycle", h.closeAll, h.done,
			func(string) {}, new(int64), new(int64), nil, false)
	}()
}

func (h *duplexHarness) cleanup(t *testing.T) {
	t.Helper()
	h.closeAll()
	_ = h.containerConn.Close()
	select {
	case <-h.proxyDone:
	case <-time.After(3 * time.Second):
		t.Error("proxyDuplex did not return")
	}
	h.cleanupClient()
	h.cleanupCont()
}

// waitGoroutinesSettle waits until the process goroutine count drops back to at
// most baseline, polling because a closed TCP connection's reader goroutine
// exits asynchronously.
func waitGoroutinesSettle(t *testing.T, baseline int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		runtime.GC()
		n := runtime.NumGoroutine()
		if n <= baseline {
			return n
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutine leak: %d goroutines still alive, baseline %d\n%s", n, baseline, buf)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestProxyDuplexClientDeathReleasesContainerRead is the reproduction for the
// relay's asymmetric teardown: when the viewer goes away, the client→container
// goroutine returns, closeAll() closes `done` and the client socket, and the
// keepalive stops — but the container→client goroutine is parked in
// container.NextReader() waiting for the next video frame, and the container
// socket is only closed after proxyDuplex returns. wg.Wait() therefore blocks
// for the full duplexContainerReadDeadline (90s) with a dead viewer, pinning a
// relay goroutine and the container websocket.
func TestProxyDuplexClientDeathReleasesContainerRead(t *testing.T) {
	h := newDuplexHarness(t)
	defer h.cleanup(t)
	baseline := runtime.NumGoroutine()
	h.start()
	clientPeer, containerPeer, proxyDone := h.clientPeer, h.containerPeer, h.proxyDone

	// Put a frame in flight so the relay is inside its streaming loop, not
	// merely parked on the first read.
	if err := containerPeer.WriteMessage(websocket.BinaryMessage, []byte("frame")); err != nil {
		t.Fatalf("container write: %v", err)
	}
	if err := clientPeer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("client deadline: %v", err)
	}
	if _, _, err := clientPeer.ReadMessage(); err != nil {
		t.Fatalf("client did not receive the relayed frame: %v", err)
	}

	// The viewer dies. Nothing else happens: the container stays up and silent,
	// exactly like a healthy streamer with no new frames to send.
	_ = clientPeer.Close()

	select {
	case <-proxyDone:
	case <-time.After(3 * time.Second):
		t.Fatal("proxyDuplex did not return after the client died; the container read is still pinning it")
	}

	// Both sockets must be closed once the relay is done, not one of them.
	if err := containerPeer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("container deadline: %v", err)
	}
	if _, _, err := containerPeer.ReadMessage(); err == nil {
		t.Fatal("container socket still open after proxyDuplex returned")
	}

	waitGoroutinesSettle(t, baseline, 3*time.Second)
}

// TestProxyDuplexContainerDeathClosesClient is the mirror case: the container
// dies while the relay is holding an open writer into it (a multi-flush input
// message), so the viewer socket must be closed and every relay goroutine must
// exit.
func TestProxyDuplexContainerDeathClosesClient(t *testing.T) {
	h := newDuplexHarness(t)
	defer h.cleanup(t)
	baseline := runtime.NumGoroutine()
	h.start()
	clientPeer, containerPeer, proxyDone := h.clientPeer, h.containerPeer, h.proxyDone

	payload := bytes.Repeat([]byte(`{"type":"ping"}`), 64*1024) // ~1 MiB, many flushes
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		w, err := clientPeer.NextWriter(websocket.TextMessage)
		if err != nil {
			return
		}
		for off := 0; off < len(payload); off += 4096 {
			end := min(off+4096, len(payload))
			if _, err := w.Write(payload[off:end]); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		_ = w.Close()
	}()

	// Let the relay enter the message and hold the writer, then kill the
	// container.
	time.Sleep(50 * time.Millisecond)
	_ = containerPeer.Close()

	select {
	case <-proxyDone:
	case <-time.After(3 * time.Second):
		t.Fatal("proxyDuplex did not return after a mid-frame container death")
	}
	select {
	case <-writerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("client writer did not unblock; the client socket is still open")
	}

	// Frames the relay had already handed to the kernel are still readable;
	// the close must surface once they are drained.
	_ = clientPeer.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := clientPeer.ReadMessage(); err != nil {
			break
		}
	}

	waitGoroutinesSettle(t, baseline, 3*time.Second)
}

// TestProxyDuplexTeardownMidFrame closes the viewer while the relay is holding
// an open client NextWriter across a multi-flush frame. That is the window the
// old code left unprotected: shutdown must close the socket out from under the
// in-flight io.Copy without panicking, and every goroutine must exit.
func TestProxyDuplexTeardownMidFrame(t *testing.T) {
	h := newDuplexHarness(t)
	defer h.cleanup(t)
	baseline := runtime.NumGoroutine()
	h.start()
	clientPeer, containerPeer, proxyDone := h.clientPeer, h.containerPeer, h.proxyDone

	payload := bytes.Repeat([]byte("isola"), 256*1024) // 1.25 MiB, many flushes
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		w, err := containerPeer.NextWriter(websocket.BinaryMessage)
		if err != nil {
			return
		}
		for off := 0; off < len(payload); off += 4096 {
			end := min(off+4096, len(payload))
			if _, err := w.Write(payload[off:end]); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		_ = w.Close()
	}()

	// Let the relay enter the frame and hold the writer, then kill the viewer.
	time.Sleep(50 * time.Millisecond)
	_ = clientPeer.Close()

	select {
	case <-proxyDone:
	case <-time.After(3 * time.Second):
		t.Fatal("proxyDuplex did not return after a mid-frame client death")
	}
	select {
	case <-writerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("container writer did not unblock; the container socket is still open")
	}

	waitGoroutinesSettle(t, baseline, 3*time.Second)
}
