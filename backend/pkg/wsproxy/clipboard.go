package wsproxy

import (
	"io"
	"time"

	"github.com/gorilla/websocket"
)

// ClipboardHook inspects (and may rewrite or block) clipboard sync messages
// crossing the isola relay. direction is "in" (client→browser paste) or
// "out" (browser→client copy). Return values:
//
//	text, nil  — forward, using the returned text (may differ from input)
//	"", nil    — drop the message entirely
//	"", err    — drop the message; err is the reason (for audit)
//
// The hook fires only for TEXT WebSocket frames; binary video frames never
// reach it. A nil hook (the default) leaves the relay byte-for-byte
// transparent — the zero-copy io.Copy path is preserved.
type ClipboardHook func(sessionID, direction, text string) (string, error)

// clipboardMessageLimit caps a scannable text message. Clipboard syncs are
// small JSON envelopes around text; anything larger is not clipboard traffic
// and passes unscanned (the relay's client read limit is far higher).
const clipboardMessageLimit = 512 * 1024

// looksLikeClipboard is a cheap prefilter over the message head so ordinary
// input events (every keystroke is a text frame) skip JSON parsing.
func looksLikeClipboard(data []byte) bool {
	if len(data) > clipboardMessageLimit {
		return false
	}
	head := data
	if len(head) > 96 {
		head = head[:96]
	}
	return containsFold(string(head), `"clipboard_sync"`) || containsFold(string(head), `"clipboard_set"`)
}

func containsFold(s, sub string) bool {
	n := len(s)
	m := len(sub)
	if m == 0 || m > n {
		return false
	}
	for i := 0; i+m <= n; i++ {
		match := true
		for j := 0; j < m; j++ {
			a, b := s[i+j], sub[j]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// interceptClipboard applies the hook to one buffered text message. ok=false
// means drop; otherwise returns the payload to forward. Non-clipboard text
// messages pass through untouched.
func interceptClipboard(hook ClipboardHook, sessionID, direction string, data []byte) ([]byte, bool) {
	if !looksLikeClipboard(data) {
		return data, true
	}
	text, err := hook(sessionID, direction, string(data))
	if err != nil {
		return nil, false
	}
	if text == "" {
		return nil, false
	}
	return []byte(text), true
}

// relayTextWithHook drains one buffered text message, applies the hook, and
// forwards the result. Only called when a hook is installed — the hook-nil
// path stays on the zero-copy io.Copy relay in duplex.go. Returns false only
// on transport failure (a policy drop returns true; the connection lives on).
func relayTextWithHook(dst *websocket.Conn, r io.Reader, hook ClipboardHook, sessionID, direction string, bytesCounter *int64) bool {
	data, err := io.ReadAll(io.LimitReader(r, clipboardMessageLimit))
	if err != nil {
		return false
	}
	data, ok := interceptClipboard(hook, sessionID, direction, data)
	if !ok || len(data) == 0 {
		return true // dropped by policy — connection stays up
	}
	_ = dst.SetWriteDeadline(time.Now().Add(duplexWriteDeadline))
	w, err := dst.NextWriter(websocket.TextMessage)
	if err != nil {
		return false
	}
	if _, err := w.Write(data); err != nil {
		return false
	}
	if err := w.Close(); err != nil {
		return false
	}
	*bytesCounter += int64(len(data))
	return true
}
