package main

import (
	"encoding/json"
	"log"
	"sync"
	"time"
)

type inputEvent struct {
	Type   string `json:"type"`
	Button string `json:"button,omitempty"`
	// ClickCount carries the multi-click sequence (2 = double-click). Chromium
	// synthesises word/line selection and dblclick from this; every press used
	// to be 1, so double- and triple-click did nothing.
	ClickCount int    `json:"click_count,omitempty"`
	X          int    `json:"x,omitempty"`
	Y          int    `json:"y,omitempty"`
	Buttons    int    `json:"buttons,omitempty"` // bitmask while moving: L=1 R=2 M=4
	DeltaX     int    `json:"deltaX,omitempty"`
	DeltaY     int    `json:"deltaY,omitempty"`
	Key        string `json:"key,omitempty"`
	Combo      string `json:"combo,omitempty"`
	Text       string `json:"text,omitempty"`
}

var cdpClient *CDPClient

// While the CDP connection is down (tab-switch reattach, browser hiccup)
// clicks used to be dropped silently. Significant events are queued for up
// to inputQueueTTL and replayed in order once CDP is available again; stale
// events are dropped on flush so we never fire clicks into a changed UI.
const (
	inputQueueCap = 64
	inputQueueTTL = 1200 * time.Millisecond
)

type queuedInput struct {
	data []byte
	at   time.Time
}

var inputQueue struct {
	mu       sync.Mutex
	buf      []queuedInput
	flushing bool
}

func enqueuePendingInput(data []byte) {
	inputQueue.mu.Lock()
	if len(inputQueue.buf) >= inputQueueCap {
		copy(inputQueue.buf, inputQueue.buf[1:])
		inputQueue.buf[len(inputQueue.buf)-1] = queuedInput{data: data, at: time.Now()}
	} else {
		inputQueue.buf = append(inputQueue.buf, queuedInput{data: data, at: time.Now()})
	}
	needFlusher := !inputQueue.flushing
	inputQueue.flushing = true
	inputQueue.mu.Unlock()
	if needFlusher {
		go flushPendingInput()
	}
}

func flushPendingInput() {
	for {
		deadline := time.Now().Add(inputQueueTTL)
		empty := false
		for !empty && time.Now().Before(deadline) {
			if cdpClient == nil || !cdpClient.Available() {
				time.Sleep(25 * time.Millisecond)
				continue
			}
			inputQueue.mu.Lock()
			var qi queuedInput
			has := len(inputQueue.buf) > 0
			if has {
				qi = inputQueue.buf[0]
				inputQueue.buf = inputQueue.buf[1:]
			}
			empty = !has && len(inputQueue.buf) == 0
			inputQueue.mu.Unlock()
			if !has {
				continue
			}
			if time.Since(qi.at) > inputQueueTTL {
				continue
			}
			var ev inputEvent
			if json.Unmarshal(qi.data, &ev) != nil {
				continue
			}
			if !cdpClient.handleInput(ev) {
				log.Printf("input: replay failed %s: ok=false", ev.Type)
			}
		}
		inputQueue.mu.Lock()
		inputQueue.flushing = false
		remain := len(inputQueue.buf)
		if remain > 0 && time.Since(inputQueue.buf[0].at) > inputQueueTTL {
			inputQueue.buf = nil
			remain = 0
		}
		inputQueue.mu.Unlock()
		if remain == 0 {
			return
		}
	}
}

func initInput() {
	cdpClient = newCDPClient()
	// Network/console/cookie capture moved to the multi-target capture client
	// (cdp_capture.go): the input client only ever saw the active tab, and its
	// events were attributed to "whatever tab is active". Enabling the domains
	// here as well would deliver every event twice.
	go cdpClient.Start()
	if netMonitorEnabled() {
		activeCapture = newCaptureClient()
		go activeCapture.Start()
	}
	log.Printf("input: cdp backend init")
}

// handleInputEvent dispatches a received input event via CDP.
// In isola mode there is no xdotool fallback — input is CDP-only.
func handleInputEvent(data []byte) {
	var ev inputEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return
	}

	significant := ev.Type == "mousedown" || ev.Type == "mouseup" || ev.Type == "mousewheel" ||
		ev.Type == "type" || ev.Type == "chord" ||
		ev.Type == "clipboard_set" || ev.Type == "clipboard_sync"
	noteInputActivity(significant)

	// Cursor-shape feedback: the offscreen capture has no OS cursor, so the
	// viewer synthesizes one from the page's computed cursor style.
	if ev.Type == "mousemove" {
		maybeQueryCursor(ev.X, ev.Y)
	}

	if cdpClient != nil && cdpClient.Available() {
		if ok := cdpClient.handleInput(ev); ok {
			return
		}
		log.Printf("input: cdp dispatch %s: ok=false", ev.Type)
		if significant {
			// The CDP write failed (connection dying): queue for replay.
			enqueuePendingInput(data)
		}
		return
	}

	if ev.Type == "mousemove" {
		return
	}

	if significant {
		// Buffer instead of dropping: replayed once CDP reattaches.
		enqueuePendingInput(data)
		return
	}

	log.Printf("input: dropped %s (no cdp)", ev.Type)
}
