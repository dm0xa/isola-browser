package main

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// A viewer that attaches during a cold start asks for its window size before
// the runner's ctrl socket is up. That resize used to be dropped, leaving the
// surface at the launch resolution while every encoder waited for the
// requested geometry — the stream never started. It must be delivered as soon
// as the ctrl connection exists.
func TestFlushPendingResizeDeliversDroppedResize(t *testing.T) {
	setPendingResize(1560, 1080, true)
	t.Cleanup(func() { setPendingResize(0, 0, false) })

	runner, streamer := net.Pipe()
	defer runner.Close()
	tabMu.Lock()
	prev := ctrlConn
	ctrlConn = streamer
	tabMu.Unlock()
	t.Cleanup(func() {
		tabMu.Lock()
		if ctrlConn == streamer {
			ctrlConn = prev
		}
		tabMu.Unlock()
	})

	got := make(chan map[string]any, 1)
	go func() {
		_ = runner.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := bufio.NewReader(runner).ReadString('\n')
		if err != nil {
			got <- nil
			return
		}
		var cmd map[string]any
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			got <- nil
			return
		}
		got <- cmd
	}()

	flushPendingResize()

	select {
	case cmd := <-got:
		if cmd == nil {
			t.Fatal("no resize command reached the runner")
		}
		if cmd["cmd"] != "resize" {
			t.Fatalf("cmd = %v, want resize", cmd["cmd"])
		}
		if cmd["width"] != float64(1560) || cmd["height"] != float64(1080) {
			t.Fatalf("geometry = %vx%v, want 1560x1080", cmd["width"], cmd["height"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the flushed resize")
	}

	// Delivered exactly once: a second connect must not replay a stale size
	// over a newer request.
	pendingResizeMu.Lock()
	pending := pendingResize
	pendingResizeMu.Unlock()
	if pending != nil {
		t.Fatalf("pending resize still recorded after a successful flush: %v", *pending)
	}
}

// A resize the runner did receive must not be replayed on the next reconnect.
func TestNotifiedResizeClearsPending(t *testing.T) {
	setPendingResize(1560, 1080, true)
	setPendingResize(1280, 720, false)
	pendingResizeMu.Lock()
	pending := pendingResize
	pendingResizeMu.Unlock()
	if pending != nil {
		t.Fatalf("pending resize = %v, want cleared", *pending)
	}
}
