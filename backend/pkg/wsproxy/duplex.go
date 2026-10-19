package wsproxy

import (
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// viewOnlyControlMaxBytes caps JSON a view-only client may send. Stream
// control messages (ping, request_keyframe) are tiny; anything larger is
// treated as input and dropped.
const viewOnlyControlMaxBytes = 4096

// Liveness / safety guards for the isola duplex relay. These exist because a
// wedged container must never hang a viewer forever, and because a malicious
// client must not be able to force unbounded per-message allocations.
var (
	// duplexContainerReadDeadline bounds how long the relay waits for the
	// next message from the container. The container→client path carries the
	// video segments; if the streaming container stops producing them, the
	// connection is torn down instead of blocking the viewer indefinitely.
	duplexContainerReadDeadline = 90 * time.Second

	// duplexClientReadLimitBytes caps the size of any single message the
	// client may send. The video path is container→client only and
	// client→container messages are small JSON input events, so 12 MiB is
	// generous while still preventing an unbounded-message DoS.
	duplexClientReadLimitBytes = int64(12 << 20)
)

// proxyDuplex performs a bidirectional WebSocket relay between the client
// and the container. No SDP, no ICE, no candidate rewriting. Used for isola
// transport mode.
//
// The relay uses gorilla's NextReader/NextWriter + io.Copy streaming pattern
// so that multi-MB video frames are copied without allocating a buffer the
// size of every message. A ping keepalive on the client, a read deadline on
// the container, and a client read limit provide liveness and DoS guards.
//
// clipboardHook, when non-nil, intercepts TEXT frames on both directions for
// clipboard policy (DLP). Text messages then take a buffered path; binary
// video frames always stay on the zero-copy path below.
func proxyDuplex(
	client, container *websocket.Conn,
	sessionID string,
	closeAll func(),
	done chan struct{},
	trackActivity func(string),
	bytesIn, bytesOut *int64,
	clipboardHook ClipboardHook,
	viewOnly bool,
) {
	var wg sync.WaitGroup
	wg.Add(2)

	// Container → Client
	go func() {
		defer wg.Done()
		defer closeAll()
		for {
			// Refresh the read deadline before every message read. NextReader
			// returns a reader whose payload is still unread by io.Copy, so
			// arming the deadline here also covers the body read; arming it
			// after io.Copy would leave the next message's body unprotected.
			_ = container.SetReadDeadline(time.Now().Add(duplexContainerReadDeadline))

			mt, r, err := container.NextReader()
			if err != nil {
				return
			}
			var n int64
			if clipboardHook != nil && mt == websocket.TextMessage {
				if !relayTextWithHook(client, r, clipboardHook, sessionID, "out", bytesOut) {
					return
				}
				trackActivity(sessionID)
				continue
			}
			w, err := client.NextWriter(mt)
			if err != nil {
				return
			}
			n, err = io.Copy(w, r)
			if err != nil {
				return
			}
			if err := w.Close(); err != nil {
				return
			}
			atomic.AddInt64(bytesOut, n)
			trackActivity(sessionID)
		}
	}()

	// Client → Container
	go func() {
		defer wg.Done()
		defer closeAll()
		client.SetReadLimit(duplexClientReadLimitBytes)
		// View-only enforcement, server-side. Input, clipboard, resize and
		// ABR bitrate steps must not reach the container: those would let a
		// share/shadow viewer steer the session or restart the shared
		// encoder at their own ladder. Stream control is different —
		// request_keyframe is how a late joiner (and a decoder that dropped
		// an IDR) resyncs, and ping is the RTT probe. Drain everything else
		// so video keeps flowing.
		if viewOnly {
			relayViewOnlyClient(client, container, sessionID, trackActivity, bytesIn)
			return
		}
		for {
			mt, r, err := client.NextReader()
			if err != nil {
				return
			}
			if clipboardHook != nil && mt == websocket.TextMessage {
				if !relayTextWithHook(container, r, clipboardHook, sessionID, "in", bytesIn) {
					return
				}
				trackActivity(sessionID)
				continue
			}
			w, err := container.NextWriter(mt)
			if err != nil {
				return
			}
			n, err := io.Copy(w, r)
			if err != nil {
				return
			}
			if err := w.Close(); err != nil {
				return
			}
			atomic.AddInt64(bytesIn, n)
			trackActivity(sessionID)
		}
	}()

	// Ping keepalive on the client connection. It calls closeAll() on exit,
	// which closes `done` and both sockets, unblocking the relay goroutines;
	// conversely, when either relay goroutine exits it closes `done` so the
	// keepalive stops. Adding it to the WaitGroup makes wg.Wait() block until
	// it exits (no early return, no deadlock).
	wg.Add(1)
	go keepalivePing(client, &wg, closeAll, done, trackActivity, sessionID)

	wg.Wait()
}

// relayViewOnlyClient reads the view-only client's socket and forwards only
// stream-control JSON (ping, request_keyframe) to the container. Everything
// else — input, clipboard, ABR, binary — is discarded so video keeps flowing.
func relayViewOnlyClient(
	client, container *websocket.Conn,
	sessionID string,
	trackActivity func(string),
	bytesIn *int64,
) {
	for {
		mt, r, err := client.NextReader()
		if err != nil {
			return
		}
		if mt != websocket.TextMessage {
			_, _ = io.Copy(io.Discard, r)
			continue
		}
		payload, err := io.ReadAll(io.LimitReader(r, viewOnlyControlMaxBytes+1))
		if err != nil {
			return
		}
		if int64(len(payload)) > viewOnlyControlMaxBytes {
			_, _ = io.Copy(io.Discard, r)
			continue
		}
		if !viewOnlyControlAllowed(payload) {
			continue
		}
		w, err := container.NextWriter(websocket.TextMessage)
		if err != nil {
			return
		}
		if _, err := w.Write(payload); err != nil {
			return
		}
		if err := w.Close(); err != nil {
			return
		}
		atomic.AddInt64(bytesIn, int64(len(payload)))
		trackActivity(sessionID)
	}
}

// viewOnlyControlAllowed reports whether a view-only client text frame is
// stream control that must reach the encoder, rather than input that must not.
func viewOnlyControlAllowed(payload []byte) bool {
	var ctrl struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &ctrl) != nil {
		return false
	}
	switch ctrl.Type {
	case "ping", "request_keyframe":
		return true
	default:
		return false
	}
}
