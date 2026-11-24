package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// inputWSWriteDeadline bounds a single JSON write to a viewer's input socket.
// Writes there happen from the capture pump (network/console fan-out), so a
// stalled reader must fail fast instead of freezing capture for the session.
const inputWSWriteDeadline = 10 * time.Second

// handleInputWS accepts JSON input events on a dedicated WebSocket so they
// don't share the video stream connection (avoids head-of-line blocking).
// It also serves control requests that need a reply — get_selection (read
// the remote selection so the viewer's context-menu Copy can put it on the
// host clipboard) and the tab operations, which are forwarded to the
// electron runner's control socket and broadcast to all viewers.
func handleInputWS(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "websocket required", http.StatusBadRequest)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("input: ws upgrade: %v", err)
		return
	}
	defer conn.Close()
	log.Printf("input: ws viewer")

	var wmu sync.Mutex
	writeJSON := func(msg map[string]any) {
		wmu.Lock()
		defer wmu.Unlock()
		// This runs on the CDP capture read goroutine (network/console events),
		// and the relay only bounds a write from its own side. Without a deadline
		// here a viewer that stops draining parks the capture pump and freezes
		// network capture for the whole session.
		_ = conn.SetWriteDeadline(time.Now().Add(inputWSWriteDeadline))
		_ = conn.WriteJSON(msg)
	}
	unregister := registerTabListener(writeJSON)
	replayNetTo(writeJSON)
	replayConsoleTo(writeJSON)
	replayCookiesTo(writeJSON)
	defer unregister()

	// select_range/get_selection run Runtime.evaluate which can block for
	// seconds on a busy page. They used to run inline in this read loop,
	// stalling every input event queued behind them (a press/release pair
	// stretched across seconds turns into a long-press). They now run on
	// workers; select_range keeps a small ordered queue (drop-oldest — it's
	// transient drag feedback) so selections apply in order without
	// blocking the loop.
	selCh := make(chan [4]int, 8)
	done := make(chan struct{})
	defer close(done)

	// Liveness keepalive. While the viewer sits still this handler writes
	// nothing at all, and the backend relay arms a read deadline on exactly
	// that direction (wsproxy.duplexContainerReadDeadline, 90s): after ~90s
	// of container silence it tears the healthy input channel down. The
	// viewer used to have no reconnect, so every tab control (tab_new,
	// tab_switch, "open link in new tab") silently became a no-op for the
	// rest of the session. One tiny message per interval keeps the relay
	// alive; viewers ignore unknown message types.
	go func() {
		interval := time.Duration(parseIntEnv("ISOLA_INPUT_KEEPALIVE_MS", 20000)) * time.Millisecond
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				wmu.Lock()
				// Re-arm the deadline for this write too. writeJSON does it for
				// event replies, but the keepalive goroutine used to write with
				// the last deadline still armed: after ~10 s of container-side
				// silence the write failed, and gorilla's sticky write error
				// silently killed every later push (tabs, network, clipboard,
				// selection) until the relay closed the channel at 90 s.
				_ = conn.SetWriteDeadline(time.Now().Add(inputWSWriteDeadline))
				err := conn.WriteJSON(map[string]any{"type": "keepalive"})
				wmu.Unlock()
				if err != nil {
					log.Printf("input: keepalive write: %v", err)
					return
				}
			}
		}
	}()

	go func() {
		for {
			select {
			case sr := <-selCh:
				if cdpClient == nil || !cdpClient.Available() {
					continue
				}
				expr := fmt.Sprintf(`(function(){
					var s = document.caretRangeFromPoint(%d,%d);
					var e = document.caretRangeFromPoint(%d,%d);
					if (!s || !e) return "no-range";
					var sel = window.getSelection();
					sel.removeAllRanges();
					var r = document.createRange();
					r.setStart(s.startContainer, s.startOffset);
					r.setEnd(e.startContainer, e.endOffset);
					sel.addRange(r);
					return sel.toString();
				})()`, sr[0], sr[1], sr[2], sr[3])
				text, err := cdpClient.evaluateString(expr)
				if err != nil {
					log.Printf("input: select_range: %v", err)
				} else {
					log.Printf("input: select_range %d chars", len(text))
				}
			case <-done:
				return
			}
		}
	}()

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if msgType != websocket.TextMessage || len(data) == 0 || data[0] != '{' {
			continue
		}
		var base struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &base) != nil {
			continue
		}

		// Copy/cut: dispatch the chord, then mirror whatever the sandbox just
		// put on its clipboard to this viewer (it writes it to the host
		// clipboard). Read on a worker — Runtime.evaluate can block for a moment
		// on a busy page, and the input loop must keep flowing.
		if base.Type == "chord" {
			var chord struct {
				Combo string `json:"combo"`
			}
			if json.Unmarshal(data, &chord) == nil && chordWritesClipboard(chord.Combo) {
				handleInputEvent(data)
				go syncSandboxClipboard(writeJSON)
				continue
			}
		}
		switch base.Type {
		case "select_range":
			// Drag-selection fallback: set the text selection directly via
			// the DOM API instead of replaying CDP mouse events (which
			// don't produce selection in Electron's offscreen mode).
			var sr struct {
				X1, Y1, X2, Y2 int
			}
			if json.Unmarshal(data, &sr) == nil {
				select {
				case selCh <- [4]int{sr.X1, sr.Y1, sr.X2, sr.Y2}:
				default: // queue full: drop stale drag feedback
				}
			}
		case "get_selection":
			go func() {
				text := ""
				if cdpClient != nil && cdpClient.Available() {
					t, err := cdpClient.evaluateString(`(function(){
						var s = window.getSelection().toString();
						if (s) return s;
						var el = document.activeElement;
						if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA')) {
							try { return el.value.substring(el.selectionStart, el.selectionEnd); } catch (e) {}
						}
						return "";
					})()`)
					if err != nil {
						log.Printf("input: get_selection: %v", err)
					} else {
						text = t
					}
				}
				writeJSON(map[string]any{"type": "selection", "text": text})
			}()
		case "select_all":
			var ev inputEvent
			ev.Type = "chord"
			ev.Combo = "ctrl+a"
			handleInputEvent(mustJSON(ev))
		case "tab_new", "tab_switch", "tab_close", "tab_navigate", "tab_back", "tab_reload", "tab_recover", "get_tabs":
			forwardTabCommand(data)
		default:
			handleInputEvent(data)
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
