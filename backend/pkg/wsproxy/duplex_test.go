package wsproxy

import (
	"bytes"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestProxyDuplexKeepalivePingDoesNotRaceRelay guards against the production
// panic "concurrent write to websocket connection".
//
// keepalivePing used to call SetWriteDeadline + WriteMessage on the client
// connection while the container→client relay goroutine held an open
// NextWriter across io.Copy. None of those methods acquire the connection
// mutex (only flushFrame→write and WriteControl do), so a ping landing inside
// a flush window panicked inside proxyDuplex and killed the whole backend
// process — dropping every viewer socket and surfacing as the frontend's
// "Reconnecting to session" overlay.
func TestProxyDuplexKeepalivePingDoesNotRaceRelay(t *testing.T) {
	originalInterval := keepalivePingInterval
	keepalivePingInterval = time.Millisecond
	defer func() { keepalivePingInterval = originalInterval }()

	clientConn, browserConn, cleanupClient := newWebSocketPair(t)
	defer cleanupClient()

	containerConn, containerPeer, cleanupContainer := newWebSocketPair(t)
	defer cleanupContainer()

	done := make(chan struct{})
	var closeOnce sync.Once
	closeAll := func() {
		closeOnce.Do(func() {
			close(done)
			_ = browserConn.Close()
			_ = containerPeer.Close()
		})
	}

	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		proxyDuplex(clientConn, containerConn, "sess-duplex", closeAll, done, func(string) {}, new(int64), new(int64), nil, false)
	}()

	// Dribble a single large binary message so the relay keeps an open client
	// writer across many flush windows while the pings fire.
	payload := bytes.Repeat([]byte("isola"), 64*1024) // 320 KiB
	go func() {
		w, err := containerPeer.NextWriter(websocket.BinaryMessage)
		if err != nil {
			return
		}
		for off := 0; off < len(payload); off += 4096 {
			end := off + 4096
			if end > len(payload) {
				end = len(payload)
			}
			if _, err := w.Write(payload[off:end]); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		_ = w.Close()
	}()

	if err := browserConn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("browser SetReadDeadline: %v", err)
	}
	var got []byte
	for len(got) < len(payload) {
		mt, chunk, err := browserConn.ReadMessage()
		if err != nil {
			t.Fatalf("browser ReadMessage after %d/%d bytes: %v", len(got), len(payload), err)
		}
		if mt != websocket.BinaryMessage {
			t.Fatalf("message type = %d, want %d", mt, websocket.BinaryMessage)
		}
		got = append(got, chunk...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("relayed payload corrupted: got %d bytes, want %d", len(got), len(payload))
	}

	closeAll()
	select {
	case <-proxyDone:
	case <-time.After(3 * time.Second):
		t.Fatal("proxyDuplex did not return")
	}
}

// TestProxyDuplexKeepaliveRefreshesActivity guards the idle-reaper decision: a
// viewer watching a static page receives no frames at all (the encoder omits
// frames identical to the last one), so payload-driven activity stalls and the
// idle reaper would tear down a sandbox that still has a viewer attached.
// Activity therefore has to come from the keepalive ping.
func TestProxyDuplexKeepaliveRefreshesActivity(t *testing.T) {
	originalInterval := keepalivePingInterval
	keepalivePingInterval = time.Millisecond
	defer func() { keepalivePingInterval = originalInterval }()

	clientConn, browserConn, cleanupClient := newWebSocketPair(t)
	defer cleanupClient()

	containerConn, containerPeer, cleanupContainer := newWebSocketPair(t)
	defer cleanupContainer()

	done := make(chan struct{})
	var closeOnce sync.Once
	closeAll := func() {
		closeOnce.Do(func() {
			close(done)
			_ = browserConn.Close()
			_ = containerPeer.Close()
		})
	}

	var mu sync.Mutex
	var tracked []string
	track := func(sid string) {
		mu.Lock()
		tracked = append(tracked, sid)
		mu.Unlock()
	}

	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		proxyDuplex(clientConn, containerConn, "sess-idle", closeAll, done, track, new(int64), new(int64), nil, false)
	}()

	// No relayed messages at all: the pings alone must keep activity fresh.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(tracked)
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("activity refreshed only %d times with no payload; the keepalive must refresh it", n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	for _, sid := range tracked {
		if sid != "sess-idle" {
			t.Errorf("tracked %q, want the session id", sid)
		}
	}
	mu.Unlock()

	closeAll()
	select {
	case <-proxyDone:
	case <-time.After(3 * time.Second):
		t.Fatal("proxyDuplex did not return")
	}
}

func TestViewOnlyControlType(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{"type":"ping","ts":1}`, "ping"},
		{`{"type":"request_keyframe"}`, "request_keyframe"},
		{`{"type":"set_bitrate","kbps":500}`, ""},
		{`{"type":"input","x":1}`, ""},
		{`{"type":"clipboard_sync","text":"x"}`, ""},
		{`not json`, ""},
	}
	for _, tc := range cases {
		if got := viewOnlyControlType([]byte(tc.in)); got != tc.want {
			t.Errorf("viewOnlyControlType(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestProxyDuplexViewOnlyForwardsStreamControl pins the share-viewer contract:
// request_keyframe and ping must reach the encoder (late joiners resync on
// them) while input must not.
func TestProxyDuplexViewOnlyForwardsStreamControl(t *testing.T) {
	clientConn, browserConn, cleanupClient := newWebSocketPair(t)
	defer cleanupClient()

	containerConn, containerPeer, cleanupContainer := newWebSocketPair(t)
	defer cleanupContainer()

	done := make(chan struct{})
	var closeOnce sync.Once
	closeAll := func() {
		closeOnce.Do(func() {
			close(done)
			_ = browserConn.Close()
			_ = containerPeer.Close()
		})
	}

	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		proxyDuplex(clientConn, containerConn, "sess-viewonly", closeAll, done, func(string) {}, new(int64), new(int64), nil, true)
	}()

	if err := browserConn.WriteMessage(websocket.TextMessage, []byte(`{"type":"request_keyframe"}`)); err != nil {
		t.Fatalf("send request_keyframe: %v", err)
	}
	if err := containerPeer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("container deadline: %v", err)
	}
	_, payload, err := containerPeer.ReadMessage()
	if err != nil {
		t.Fatalf("container did not receive request_keyframe: %v", err)
	}
	if string(payload) != `{"type":"request_keyframe"}` {
		t.Fatalf("container payload = %q, want request_keyframe", payload)
	}

	if err := browserConn.WriteMessage(websocket.TextMessage, []byte(`{"type":"input","x":1}`)); err != nil {
		t.Fatalf("send input: %v", err)
	}
	if err := containerPeer.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("container drop deadline: %v", err)
	}
	if _, extra, err := containerPeer.ReadMessage(); err == nil {
		t.Fatalf("container received dropped input: %q", extra)
	}

	closeAll()
	select {
	case <-proxyDone:
	case <-time.After(3 * time.Second):
		t.Fatal("proxyDuplex did not return")
	}
}

// TestViewOnlyKeyframeAllowed pins the relay's per-connection arithmetic: one
// forward per window, and a refusal that does not move the window.
func TestViewOnlyKeyframeAllowed(t *testing.T) {
	original := viewOnlyKeyframeWindow
	viewOnlyKeyframeWindow = 100 * time.Millisecond
	defer func() { viewOnlyKeyframeWindow = original }()

	var last time.Time
	now := time.Now()
	if !viewOnlyKeyframeAllowed(&last, now) {
		t.Fatal("the first request on a connection must be forwarded")
	}
	if viewOnlyKeyframeAllowed(&last, now.Add(viewOnlyKeyframeWindow-time.Millisecond)) {
		t.Fatal("a second request inside the window must be dropped")
	}
	if !last.Equal(now) {
		t.Fatal("a dropped request moved the window")
	}
	if !viewOnlyKeyframeAllowed(&last, now.Add(viewOnlyKeyframeWindow)) {
		t.Fatal("a request at the window boundary must be forwarded")
	}
}

// readContainerText reads the next container-bound text frame, or reports
// that nothing arrived inside timeout.
func readContainerText(t *testing.T, peer *websocket.Conn, timeout time.Duration) (string, bool) {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("container deadline: %v", err)
	}
	_, payload, err := peer.ReadMessage()
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return "", false
		}
		t.Fatalf("container read: %v", err)
	}
	return string(payload), true
}

func writeClientText(t *testing.T, conn *websocket.Conn, payload string) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatalf("client write %s: %v", payload, err)
	}
}

func startDuplexForTest(t *testing.T, viewOnly bool) (client, containerPeer *websocket.Conn, cleanup func(), proxyDone chan struct{}) {
	t.Helper()
	clientConn, browserConn, cleanupClient := newWebSocketPair(t)
	containerConn, peer, cleanupContainer := newWebSocketPair(t)

	done := make(chan struct{})
	var closeOnce sync.Once
	closeAll := func() {
		closeOnce.Do(func() {
			close(done)
			_ = browserConn.Close()
			_ = peer.Close()
		})
	}

	proxyDone = make(chan struct{})
	go func() {
		defer close(proxyDone)
		proxyDuplex(clientConn, containerConn, "sess-budget", closeAll, done, func(string) {}, new(int64), new(int64), nil, viewOnly)
	}()

	cleanup = func() {
		closeAll()
		select {
		case <-proxyDone:
		case <-time.After(3 * time.Second):
			t.Error("proxyDuplex did not return")
		}
		cleanupClient()
		cleanupContainer()
	}
	return browserConn, peer, cleanup, proxyDone
}

// TestProxyDuplexViewOnlyKeyframeBudget pins the relay-side share-link guard:
// a view-only connection gets one forwarded request_keyframe per window, while
// ping keeps flowing unrestricted.
func TestProxyDuplexViewOnlyKeyframeBudget(t *testing.T) {
	original := viewOnlyKeyframeWindow
	viewOnlyKeyframeWindow = 150 * time.Millisecond
	defer func() { viewOnlyKeyframeWindow = original }()

	client, containerPeer, cleanup, _ := startDuplexForTest(t, true)
	defer cleanup()

	writeClientText(t, client, `{"type":"request_keyframe"}`)
	if got, ok := readContainerText(t, containerPeer, 2*time.Second); !ok || got != `{"type":"request_keyframe"}` {
		t.Fatalf("first request_keyframe not forwarded: %q ok=%v", got, ok)
	}

	// Inside the window the request is dropped. Prove it by ordering instead
	// of a timed-out read: gorilla poisons a connection after a read deadline,
	// so the ping that follows must be the very next frame the container sees.
	writeClientText(t, client, `{"type":"request_keyframe"}`)
	writeClientText(t, client, `{"type":"ping","ts":1}`)
	if got, ok := readContainerText(t, containerPeer, 2*time.Second); !ok || got != `{"type":"ping","ts":1}` {
		t.Fatalf("container saw %q ok=%v after the dropped request; want ping next", got, ok)
	}

	// Past the window the connection may ask again.
	time.Sleep(viewOnlyKeyframeWindow + 50*time.Millisecond)
	writeClientText(t, client, `{"type":"request_keyframe"}`)
	if got, ok := readContainerText(t, containerPeer, 2*time.Second); !ok || got != `{"type":"request_keyframe"}` {
		t.Fatalf("request_keyframe after the window not forwarded: %q ok=%v", got, ok)
	}
}

// TestProxyDuplexControlKeyframesUnrestricted keeps the relay budget off the
// normal control path: a non-view-only socket still forwards every request.
func TestProxyDuplexControlKeyframesUnrestricted(t *testing.T) {
	original := viewOnlyKeyframeWindow
	viewOnlyKeyframeWindow = time.Hour
	defer func() { viewOnlyKeyframeWindow = original }()

	client, containerPeer, cleanup, _ := startDuplexForTest(t, false)
	defer cleanup()

	for i := 0; i < 2; i++ {
		writeClientText(t, client, `{"type":"request_keyframe"}`)
		if got, ok := readContainerText(t, containerPeer, 2*time.Second); !ok || got != `{"type":"request_keyframe"}` {
			t.Fatalf("control request_keyframe %d not forwarded: %q ok=%v", i+1, got, ok)
		}
	}
}
