package main

import (
	"net/http"
	"testing"
	"time"
)

// TestResizeRekeysEncoder pins the live viewport resize: the producer's surface
// follows a new geometry on its own (composeLocked resizes it), but an ffmpeg
// run's output size is fixed in its argv — so every attached viewer has to be
// reset onto a replacement encoder at the new geometry.
func TestResizeRekeysEncoder(t *testing.T) {
	const port = "15934"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	conn, init := dialTestViewer(t, port)
	defer conn.Close()
	if got := init["capture_width"]; got != float64(e2eTestWidth) {
		t.Fatalf("initial capture_width = %v, want %d", got, e2eTestWidth)
	}
	waitForFanout(t, 1, 1)

	resp, err := http.Post("http://127.0.0.1:"+port+"/resize?width=1024&height=768", "", nil)
	if err != nil {
		t.Fatalf("resize request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resize returned %d", resp.StatusCode)
	}

	reset := readTextUntil(t, conn, 5*time.Second, msgTypeStreamReset)
	if reset["reason"] != "resize" {
		t.Fatalf("stream_reset reason = %v, want resize", reset["reason"])
	}
	next := readTextUntil(t, conn, 5*time.Second, msgTypeStreamInit)
	if next["capture_width"] != float64(1024) || next["capture_height"] != float64(768) {
		t.Fatalf("resized capture = %vx%v, want 1024x768",
			next["capture_width"], next["capture_height"])
	}
	currentMu.RLock()
	width, height := currentWidth, currentHeight
	currentMu.RUnlock()
	if width != 1024 || height != 768 {
		t.Fatalf("current geometry = %dx%d, want 1024x768", width, height)
	}
	// The replacement encoder replaced the original run rather than adding one.
	waitForFanout(t, 1, 1)
}

// TestFrameMatchesRun pins the invariant that keeps a re-keyed encoder sane: a
// run may only be fed frames composed at its own geometry. Feeding anything
// else shifts every row of ffmpeg's raw input and corrupts the rest of the run.
func TestFrameMatchesRun(t *testing.T) {
	key := runKey{width: 1024, height: 768}
	cases := []struct {
		stride, height uint32
		want           bool
	}{
		{1024 * 4, 768, true},
		{1280 * 4, 768, false}, // the pre-resize surface
		{1024 * 4, 720, false},
		{1024*4 + 64, 768, false}, // padded stride is not this run's geometry
	}
	for _, c := range cases {
		if got := frameMatchesRun(c.stride, c.height, key); got != c.want {
			t.Errorf("frameMatchesRun(stride=%d, height=%d) = %v, want %v",
				c.stride, c.height, got, c.want)
		}
	}
}

// TestResizeValidation pins the boundaries the runner and encoder rely on:
// odd sizes are rounded down (H.264 4:2:0 cannot encode them) and anything
// outside the supported range is refused instead of reaching the encoder.
func TestResizeValidation(t *testing.T) {
	const port = "15935"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	resp, err := http.Post("http://127.0.0.1:"+port+"/resize?width=1023&height=767", "", nil)
	if err != nil {
		t.Fatalf("resize request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resize returned %d, want 200", resp.StatusCode)
	}
	currentMu.RLock()
	width, height := currentWidth, currentHeight
	currentMu.RUnlock()
	if width != 1022 || height != 766 {
		t.Fatalf("odd resize = %dx%d, want 1022x766", width, height)
	}

	for _, query := range []string{"width=9000&height=800", "width=0&height=800", "width=abc&height=800"} {
		resp, err := http.Post("http://127.0.0.1:"+port+"/resize?"+query, "", nil)
		if err != nil {
			t.Fatalf("resize %q: %v", query, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("resize %q returned %d, want 400", query, resp.StatusCode)
		}
	}
	currentMu.RLock()
	defer currentMu.RUnlock()
	if currentWidth != 1022 || currentHeight != 766 {
		t.Fatalf("refused resizes changed the geometry to %dx%d", currentWidth, currentHeight)
	}
}
