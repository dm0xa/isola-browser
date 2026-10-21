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
	Level string `json:"l"` // log, warning, error, info, debug, exception
	Text  string `json:"t"`
	Tab   int    // active browser tab at capture time (client tab strip id)
	TS    int64  `json:"ts"` // wall-clock capture time, Unix ms
}

var (
	consoleMu       sync.Mutex
	consoleBuffer   []consoleEntry
	consoleSentKeys = map[string]bool{}
)

const consoleMaxEntries = 300
const consoleMaxTextLen = 500

// consoleKey is the dedupe identity of one console line. It includes the tab so
// the same text logged in two tabs is kept, and is shared with the pre-strip
// rewrite in tabs.go so a rewritten entry stays deduped.
func consoleKey(e consoleEntry) string {
	return e.Level + "|" + e.Text + "|" + fmt.Sprint(e.Tab)
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

	entry := consoleEntry{Level: level, Text: text, Tab: tab, TS: time.Now().UnixMilli()}

	consoleMu.Lock()
	key := consoleKey(entry)
	if consoleSentKeys[key] {
		consoleMu.Unlock()
		return
	}
	consoleSentKeys[key] = true
	consoleBuffer = append(consoleBuffer, entry)
	if len(consoleBuffer) > consoleMaxEntries {
		consoleBuffer = consoleBuffer[1:]
	}
	consoleMu.Unlock()

	broadcastToInputWS(map[string]any{"type": "console", "l": entry.Level, "t": entry.Text, "tb": entry.Tab, "ts": entry.TS})
}

// replayConsoleTo sends the console buffer to a newly attached viewer.
func replayConsoleTo(fn func(map[string]any)) {
	consoleMu.Lock()
	entries := make([]consoleEntry, len(consoleBuffer))
	copy(entries, consoleBuffer)
	consoleMu.Unlock()
	for _, e := range entries {
		fn(map[string]any{"type": "console", "l": e.Level, "t": e.Text, "tb": e.Tab, "ts": e.TS})
	}
}

// isConsoleEventType filters CDP events for the console tap.
func isConsoleEventType(method string) bool {
	return strings.HasPrefix(method, "Runtime.consoleAPICalled") ||
		strings.HasPrefix(method, "Runtime.exceptionThrown")
}

var _ = fmt.Sprintf // keep fmt import if unused elsewhere
