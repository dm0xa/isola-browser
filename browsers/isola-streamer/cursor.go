package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Cursor-shape feedback: the offscreen capture has no OS cursor, so the
// viewer synthesizes the shape (arrow / I-beam / hand) from the page's
// computed cursor style at the pointer position. Queries are debounced,
// single-flight, and broadcast to viewers only on change.

var (
	cursorMu       sync.Mutex
	lastCursorTry  time.Time
	lastCursorX    int
	lastCursorY    int
	lastCursorSent string
)

var cursorInFlight atomic.Bool

// Cursor-probe backoff. On a page whose renderer is wedged, every mousemove
// used to fire another Runtime.evaluate that then sat in the renderer's main
// thread for its full 3s timeout — and logged a line either way. That is
// self-inflicted load on the exact thread that is stuck (and 1000+ log lines
// per minute). Failures now back off exponentially and silence themselves;
// they also feed the renderer-health signal the UI shows.
var (
	cursorBackoffUntil atomic.Int64 // unix-nano
	cursorFailLogged   atomic.Bool
)

const (
	cursorBackoffBase     = 500 * time.Millisecond
	cursorBackoffMax      = 20 * time.Second
	cursorMaxBackoffShift = 5
)

var knownCursorShapes = map[string]bool{
	"default": true, "pointer": true, "text": true, "crosshair": true,
	"move": true, "not-allowed": true, "ew-resize": true, "ns-resize": true,
	"grab": true, "grabbing": true, "col-resize": true, "row-resize": true,
	"wait": true, "progress": true, "none": true,
}

// maybeQueryCursor is called on every mousemove before dispatch; it debounces
// and fires a single-flight CDP probe of the computed cursor style and hover
// link at the pointer position, broadcasting the result on change.
func maybeQueryCursor(x, y int) {
	if cdpClient == nil || !cdpClient.Available() {
		return
	}
	if until := cursorBackoffUntil.Load(); until != 0 && time.Now().UnixNano() < until {
		return
	}
	cursorMu.Lock()
	moved := x != lastCursorX || y != lastCursorY
	since := time.Since(lastCursorTry)
	if cursorInFlight.Load() {
		cursorMu.Unlock()
		return
	}
	if !moved && since < 300*time.Millisecond {
		cursorMu.Unlock()
		return
	}
	if since < 80*time.Millisecond {
		cursorMu.Unlock()
		return
	}
	lastCursorTry = time.Now()
	lastCursorX, lastCursorY = x, y
	cursorMu.Unlock()
	if !cursorInFlight.CompareAndSwap(false, true) {
		return
	}
	expr := fmt.Sprintf(`(function(){
		var el = document.elementFromPoint(%s,%s);
		if (!el) return JSON.stringify({c:"default",u:""});
		var cur = getComputedStyle(el).cursor;
		if (cur === "auto") cur = "default";
		var a = el.closest ? el.closest("a[href]") : null;
		return JSON.stringify({c: cur, u: a ? a.href : ""});
	})()`, strconv.Itoa(x), strconv.Itoa(y))
	go func() {
		defer cursorInFlight.Store(false)
		out, err := cdpClient.evaluateString(expr)
		if err != nil {
			noteProbeFailure()
			streak := probeFailStreak.Load()
			shift := streak - 1
			if shift > cursorMaxBackoffShift {
				shift = cursorMaxBackoffShift
			}
			if shift < 0 {
				shift = 0
			}
			backoff := cursorBackoffBase << uint(shift)
			if backoff > cursorBackoffMax {
				backoff = cursorBackoffMax
			}
			cursorBackoffUntil.Store(time.Now().Add(backoff).UnixNano())
			if !cursorFailLogged.Swap(true) {
				log.Printf("cursor: probe failed %d (backoff %v): %v", streak, backoff, err)
			}
			return
		}
		noteProbeSuccess()
		if cursorFailLogged.Swap(false) {
			log.Printf("cursor: recovered")
		}
		cursorBackoffUntil.Store(0)
		var msg struct {
			C string `json:"c"`
			U string `json:"u"`
		}
		if json.Unmarshal([]byte(out), &msg) != nil {
			log.Printf("cursor: bad result: %s", out)
			return
		}
		shape := msg.C
		if !knownCursorShapes[shape] {
			shape = "default"
		}
		key := shape + "|" + msg.U
		cursorMu.Lock()
		if key == lastCursorSent {
			cursorMu.Unlock()
			return
		}
		lastCursorSent = key
		cursorMu.Unlock()
		broadcastToInputWS(map[string]any{"type": "cursor", "c": shape, "u": msg.U})
	}()
}
