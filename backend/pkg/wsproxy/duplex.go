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

// viewOnlyKeyframeWindow: a view-only connection may forward request_keyframe this often —
// each request resets the shared encoder. Relay-side because host and share are different sockets.
var viewOnlyKeyframeWindow = 30 * time.Second

// Liveness / safety guards for the isola duplex relay. These exist because a
// wedged container must never hang a viewer forever, and because a malicious
// client must not be able to force unbounded per-message allocations.
var (
	// Container→client carries the video: silence past this tears the connection down.
	duplexContainerReadDeadline = 90 * time.Second

	// Client→container is small JSON input; 12 MiB is generous while bounding a message DoS.
	duplexClientReadLimitBytes = int64(12 << 20)

	// One binary message per packet; the largest legitimate one is a keyframe (a few MiB at 4K).
	// 32 MiB keeps headroom; a naive 64 KiB limit would break video. io.Copy streams it anyway.
	duplexContainerReadLimitBytes = int64(32 << 20)

	// Container TEXT frames are control JSON whose size does not scale with the picture — tight bound.
	duplexContainerControlMaxBytes = int64(1 << 20)

	// Without this a wedged peer blocks io.Copy forever; 30s far exceeds any legitimate write.
	duplexWriteDeadline = 30 * time.Second
)

// proxyDuplex: bidirectional WS relay, streaming (no per-message buffers). Guards:
// keepalive ping, read deadlines/limits, write deadline; first exit closes both sockets.
// clipboardHook intercepts TEXT frames both ways (DLP); binary stays zero-copy.
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
	// Teardown is owned here: closeAll only closes the client socket, so shutdown
	// closes both and the caller's closeAll, once. Closing under an open
	// NextWriter is safe; the keepalive uses serialized WriteControl.
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
			// Arm before io.Copy so the deadline also covers this message's body.
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
			// TEXT from the container gets the tight control bound; binary keeps the read limit above.
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
					Msg("relay: oversized control frame")
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
		// No client read deadline: an idle viewer sends nothing for minutes; ping/pong
		// liveness is the keepalive's job. View-only drops input/clipboard/resize/bitrate
		// so a share viewer cannot steer the session; keyframe and ping still pass.
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

	// Keepalive exits via shutdown; any relay goroutine exit stops it via `done`.
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
				Msg("relay: keyframe rate-limited")
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
