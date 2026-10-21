package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Console capture: taps CDP Runtime.consoleAPICalled and
// Runtime.exceptionThrown events, buffers entries, broadcasts to viewers.
// Metadata only — argument values are stringified and capped.
// Enabled when NET_MONITOR=1 (same gate as the network monitor).

type consoleEntry struct {
	ID    int64  `json:"i"`
	Level string `json:"l"` // log, warning, error, info, debug, exception
	Text  string `json:"t"`
	Tab   int    // active browser tab at capture time (client tab strip id)
	TS    int64  `json:"ts"` // wall-clock capture time, Unix ms
}

var (
	consoleMu       sync.Mutex
	consoleBuffer   []consoleEntry
	consoleSentKeys = map[string]bool{}
	consoleSeq      int64
)

// consoleNow is injectable so dedupe tests do not depend on the wall clock.
var consoleNow = func() int64 { return time.Now().UnixMilli() }

const consoleMaxEntries = 300
const consoleMaxTextLen = 500

// consoleKey is the dedupe identity of one console line. The timestamp is part
// of it: keying only on level|text|tab silently dropped a *repeat* of the same
// line (a loop logging "tick" rendered once), while the same line from two
// tabs is still distinct. The tab is included so the pre-strip rewrite in
// tabs.go can carry the rewritten entry over without re-adding it.
func consoleKey(e consoleEntry) string {
	return fmt.Sprintf("%d|%s|%s|%d", e.TS, e.Level, e.Text, e.Tab)
}

// consoleTap processes CDP Runtime events for console capture.
// Called from the capture client's read loop; `tab` is the tab the event's
// target belongs to.
func consoleTap(method string, params json.RawMessage, tab int) {
	var p struct {
		Type string `json:"type"`
		Args []struct {
			Value json.RawMessage `json:"value"`
		} `json:"args"`
		ExceptionDetails struct {
			Text        string `json:"text"`
			Description string `json:"description"`
		} `json:"exceptionDetails"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}

	var level, text string
	switch method {
	case "Runtime.consoleAPICalled":
		level = p.Type
		if level == "" {
			level = "log"
		}
		var parts []string
		for _, a := range p.Args {
			s := strings.TrimSpace(string(a.Value))
			if len(s) > 200 {
				s = s[:200] + "…"
			}
			parts = append(parts, s)
		}
		text = strings.Join(parts, " ")
		if len(text) > consoleMaxTextLen {
			text = text[:consoleMaxTextLen] + "…"
		}
	case "Runtime.exceptionThrown":
		level = "exception"
		text = p.ExceptionDetails.Description
		if text == "" {
			text = p.ExceptionDetails.Text
		}
		if len(text) > consoleMaxTextLen {
			text = text[:consoleMaxTextLen] + "…"
		}
	default:
		return
	}

	entry := consoleEntry{Level: level, Text: text, Tab: tab, TS: consoleNow()}

	consoleMu.Lock()
	entry.ID = consoleSeq
	consoleSeq++
	key := consoleKey(entry)
	if consoleSentKeys[key] {
		consoleMu.Unlock()
		return
	}
	consoleSentKeys[key] = true
	consoleBuffer = append(consoleBuffer, entry)
	if len(consoleBuffer) > consoleMaxEntries {
		// Evict the dedupe key with the line; the map is bounded by the buffer
		// instead of growing for the session's lifetime.
		delete(consoleSentKeys, consoleKey(consoleBuffer[0]))
		consoleBuffer = consoleBuffer[1:]
	}
	consoleMu.Unlock()

	broadcastToInputWS(consoleWire(entry))
}

// consoleWire is the viewer shape of one console line. The optional id gives
// the viewer a stable identity: re-publishing the same line with a corrected
// tab must fold into the existing row, while two lines in two tabs stay
// distinct even when they share text and millisecond.
func consoleWire(e consoleEntry) map[string]any {
	return map[string]any{"type": "console", "i": e.ID, "l": e.Level, "t": e.Text, "tb": e.Tab, "ts": e.TS}
}

// replayConsoleTo sends the console buffer to a newly attached viewer.
func replayConsoleTo(fn func(map[string]any)) {
	consoleMu.Lock()
	entries := make([]consoleEntry, len(consoleBuffer))
	copy(entries, consoleBuffer)
	consoleMu.Unlock()
	for _, e := range entries {
		fn(consoleWire(e))
	}
}

// isConsoleEventType filters CDP events for the console tap.
func isConsoleEventType(method string) bool {
	return strings.HasPrefix(method, "Runtime.consoleAPICalled") ||
		strings.HasPrefix(method, "Runtime.exceptionThrown")
}

var _ = fmt.Sprintf // keep fmt import if unused elsewhere
