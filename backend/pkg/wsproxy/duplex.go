package wsproxy

import (
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

// viewOnlyControlMaxBytes caps JSON a view-only client may send. Stream
// control messages (ping, request_keyframe) are tiny; anything larger is
// treated as input and dropped.
const viewOnlyControlMaxBytes = 4096

// viewOnlyKeyframeWindow is how often a view-only connection may forward a
// request_keyframe to its container. Every such request replaces the session's
// encoder run and resets every viewer of it, so one share-link watcher must not
// be able to restart the host's encoder back-to-back. This is the relay's own
// limit: the streamer budgets per subscription too, but a share watch and the
// host are different websockets and only the relay can tell them apart. It is
// a var so tests can shrink the window.
var viewOnlyKeyframeWindow = 30 * time.Second

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

	// duplexContainerReadLimitBytes caps the size of any single message the
	// container may send. The relay streams container→client messages through
	// io.Copy (32 KiB at a time) instead of buffering them, so this is not a
	// memory bound for the relay — it is a bound on what one frame can cost the
	// viewer and the wire. The streamer writes one binary message per packet
	// (an fMP4 fragment or one Annex-B access unit), so the largest legitimate
	// message is a single keyframe: well under 1 MiB at 1080p and a few MiB at
	// 4K. 32 MiB leaves an order of magnitude of headroom while still refusing
	// a runaway message; a naive small limit (say 64 KiB) would break video.
	duplexContainerReadLimitBytes = int64(32 << 20)

	// duplexContainerControlMaxBytes bounds the TEXT frames the container may
	// send. Every container→client text frame is control/status JSON
	// (stream_init, metrics, pong, analysis events) whose size does not scale
	// with the picture, so it can be bounded tightly even though the binary
	// video frames cannot.
	duplexContainerControlMaxBytes = int64(1 << 20)

	// duplexWriteDeadline bounds a single message write on either socket. The
	// streamer already sets 10s per write; without an equivalent the relay's
	// io.Copy into a wedged viewer (or a container that stops reading input)
	// blocks forever and wg.Wait never returns. The value is far above a
	// legitimate write: the largest frame is a few MiB, and a viewer that
	// cannot take that inside 30s has already been downshifted by ABR.
	duplexWriteDeadline = 30 * time.Second
)

// proxyDuplex performs a bidirectional WebSocket relay between the client
// and the container. No SDP, no ICE, no candidate rewriting. Used for isola
// transport mode.
//
// The relay uses gorilla's NextReader/NextWriter + io.Copy streaming pattern
// so that multi-MB video frames are copied without allocating a buffer the
// size of every message. Liveness and DoS guards: a ping keepalive on the
// client, read deadlines and read limits on both sockets (the container's
// binary limit is generous enough for a 4K keyframe; its TEXT frames get a
// tighter one), and a write deadline on every relayed message so a peer that
// stops reading cannot pin a goroutine. Whichever goroutine exits first closes
// both sockets (see shutdown below).
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
	// Teardown is owned here, not by the caller. Each of the three goroutines
	// below is parked on a socket: the container→client loop on
	// container.NextReader, the client→container loop on client.NextReader or
	// on a write into the container, the keepalive on its ticker. closeAll
	// closes only the client socket, so a viewer that goes away leaves the
	// container→client loop waiting for the next video frame — up to
	// duplexContainerReadDeadline — and wg.Wait holds a relay goroutine plus
	// the container websocket long after the session has no viewer. shutdown
	// closes both sockets and then runs the caller's closeAll, and sync.Once
	// makes it safe for all three goroutines to reach it (closeAll is not
	// required to be idempotent, and `done` is closed exactly once).
	//
	// Closing a socket while a NextWriter is open is safe: Conn.Close closes
	// the underlying net.Conn, so the in-flight flush returns an error rather
	// than panicking. Only one goroutine writes each socket, and the keepalive
	// uses WriteControl, which gorilla serializes, so there is no
	// concurrent-writer panic either.
	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			_ = client.Close()
			_ = container.Close()
			closeAll()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Container → Client
	go func() {
		defer wg.Done()
		defer shutdown()
		container.SetReadLimit(duplexContainerReadLimitBytes)
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
			// A TEXT frame from the container is control/status JSON; its size
			// does not scale with the picture, so it gets the tight control
			// bound while binary video stays on the streaming path below (the
			// read limit above is its only bound).
			var body io.Reader = r
			if mt == websocket.TextMessage {
				body = io.LimitReader(r, duplexContainerControlMaxBytes+1)
			}
			// Bound the write to the viewer. Without this a viewer that stops
			// reading pins this goroutine in io.Copy forever, holding the
			// container socket and its encoder subscription.
			_ = client.SetWriteDeadline(time.Now().Add(duplexWriteDeadline))
			w, err := client.NextWriter(mt)
			if err != nil {
				return
			}
			n, err = io.Copy(w, body)
			if err != nil {
				return
			}
			if mt == websocket.TextMessage && n > duplexContainerControlMaxBytes {
				log.Warn().
					Str("session_id", sessionID).
					Int64("bytes", n).
					Msg("relay: oversized control frame from container; closing")
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
		defer shutdown()
		client.SetReadLimit(duplexClientReadLimitBytes)
		// There is deliberately no read deadline on the client: a viewer
		// watching a static page sends nothing for minutes at a time, and
		// ping/pong liveness is the keepalive's job (its WriteControl has its
		// own 10s deadline, and a dead viewer makes it fail). A read deadline
		// here would tear down healthy idle sessions.
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
			// Bound the write into the container too: a wedged streamer must
			// not pin this goroutine, and with it the client socket, forever.
			_ = container.SetWriteDeadline(time.Now().Add(duplexWriteDeadline))
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

	// Ping keepalive on the client connection. It calls shutdown on exit, so a
	// dead viewer detected here tears down both sockets and `done`; conversely
	// either relay goroutine exiting closes `done` so the keepalive stops.
	// Adding it to the WaitGroup makes wg.Wait() block until it exits (no early
	// return, no goroutine left behind).
	wg.Add(1)
	go keepalivePing(client, &wg, shutdown, done, trackActivity, sessionID)

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
	// Per-connection keyframe budget. ping is not budgeted: it is an RTT probe,
	// cheap, and its rate is what keeps a static session alive.
	var lastKeyframeAt time.Time
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
		ctrlType := viewOnlyControlType(payload)
		if ctrlType == "" {
			continue
		}
		if ctrlType == "request_keyframe" && !viewOnlyKeyframeAllowed(&lastKeyframeAt, time.Now()) {
			log.Warn().
				Str("session_id", sessionID).
				Dur("window", viewOnlyKeyframeWindow).
				Msg("view-only request_keyframe dropped: one per window")
			continue
		}
		_ = container.SetWriteDeadline(time.Now().Add(duplexWriteDeadline))
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

// viewOnlyControlType returns the control message type of a view-only client
// text frame when that frame is stream control that must reach the encoder,
// and "" for input that must not.
func viewOnlyControlType(payload []byte) string {
	var ctrl struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &ctrl) != nil {
		return ""
	}
	switch ctrl.Type {
	case "ping", "request_keyframe":
		return ctrl.Type
	default:
		return ""
	}
}

// viewOnlyKeyframeAllowed reports whether a view-only connection may forward a
// request_keyframe now, and records the forward when it may. The caller holds
// the per-connection state, so concurrent connections never share a budget.
func viewOnlyKeyframeAllowed(last *time.Time, now time.Time) bool {
	if !last.IsZero() && now.Sub(*last) < viewOnlyKeyframeWindow {
		return false
	}
	*last = now
	return true
}
