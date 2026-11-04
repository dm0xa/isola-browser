package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestAbrLadder checks the rung math and set_bitrate clamping (no encoder
// needed).
func TestAbrLadder(t *testing.T) {
	ladder := abrLadderKbps(2000)
	want := [abrLevels]int{2000, 1400, 900, 600}
	if ladder != want {
		t.Fatalf("abrLadderKbps(2000) = %v, want %v", ladder, want)
	}

	// The 30% floor: small bases never drop below abrFloorKbps.
	ladder = abrLadderKbps(1000)
	want = [abrLevels]int{1000, 700, 500, 500}
	if ladder != want {
		t.Fatalf("abrLadderKbps(1000) = %v, want %v", ladder, want)
	}

	if got := clampAbrKbps(999999, 2000); got != 2000 {
		t.Fatalf("clampAbrKbps(999999, 2000) = %d, want 2000", got)
	}
	if got := clampAbrKbps(10, 2000); got != abrMinKbps {
		t.Fatalf("clampAbrKbps(10, 2000) = %d, want %d", got, abrMinKbps)
	}
	if got := clampAbrKbps(1200, 2000); got != 1200 {
		t.Fatalf("clampAbrKbps(1200, 2000) = %d, want 1200", got)
	}
	t.Log("abr ladder + clamp: PASS")
}

// TestAbrScaleForKbps checks the bitrate→scale mapping a rung change uses.
func TestAbrScaleForKbps(t *testing.T) {
	// Base 2000 has four distinct rung bitrates, so each maps exactly. The
	// top two rungs share scale 1.0: the first step down is bitrate-only.
	ladder := abrLadderKbps(2000)
	want := [abrLevels]float64{1.0, 1.0, 0.8, 0.5}
	for i, kbps := range ladder {
		if got := abrScaleForKbps(kbps, 2000); got != want[i] {
			t.Fatalf("abrScaleForKbps(%d, 2000) = %v, want %v", kbps, got, want[i])
		}
	}

	// Floor collision: base 1000's two lowest rungs both floor at 500; the
	// first (higher-scale) rung wins — still a defined scale.
	if got := abrScaleForKbps(500, 1000); got != 0.8 {
		t.Fatalf("abrScaleForKbps(500, 1000) = %v, want 0.8", got)
	}
	t.Log("abr scale mapping: PASS")
}

// TestEncodeDimensionsAtScale checks the encoder resize and its yuv420p
// even-dimension constraint.
func TestEncodeDimensionsAtScale(t *testing.T) {
	if w, h := encodeDimensions(1280, 720, 1.0); w != 1280 || h != 720 {
		t.Fatalf("encodeDimensions(1280,720,1.0) = %dx%d, want 1280x720", w, h)
	}

	// An odd capture size must still yield positive, even dimensions strictly
	// below the capture size.
	w, h := encodeDimensions(1281, 721, 0.6)
	if w <= 0 || h <= 0 || w%2 != 0 || h%2 != 0 {
		t.Fatalf("encodeDimensions(1281,721,0.6) = %dx%d, want positive even", w, h)
	}
	if w >= 1281 || h >= 721 {
		t.Fatalf("encodeDimensions(1281,721,0.6) = %dx%d, want smaller than capture", w, h)
	}
	t.Log("encode dimensions at scale: PASS")
}

// startTestStreamer wires up the frame broadcaster and mux on the given
// port, mirroring the in-process e2e setup (requires ffmpeg on PATH for
// tests that connect a viewer WS).
func startTestStreamer(t *testing.T, port string, bitrateKbps int) net.Listener {
	t.Helper()
	os.Remove(e2eTestSockPath)

	currentMu.Lock()
	currentWidth = e2eTestWidth
	currentHeight = e2eTestHeight
	currentFPS = 15
	currentBitrateKbps = bitrateKbps
	currentMu.Unlock()
	os.Setenv("ISOLA_FRAME_SOCK", e2eTestSockPath)
	os.Setenv("ISOLA_WIDTH", fmt.Sprint(e2eTestWidth))
	os.Setenv("ISOLA_HEIGHT", fmt.Sprint(e2eTestHeight))
	os.Setenv("ISOLA_FPS", "15")
	os.Setenv("ISOLA_BITRATE_KBPS", fmt.Sprint(bitrateKbps))

	incoming, err := listenFrames(e2eTestSockPath)
	if err != nil {
		t.Fatalf("listenFrames: %v", err)
	}
	globalBroadcaster = newFrameBroadcaster(incoming)

	listener, err := setupMux(port, handleRoot)
	if err != nil {
		t.Fatalf("setupMux: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	return listener
}

// dialTestViewer connects a viewer WS and consumes the stream_init,
// returning the connection and the init message.
func dialTestViewer(t *testing.T, port string) (*websocket.Conn, map[string]any) {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+port+"/", nil)
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read stream_init: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("expected text stream_init, got binary")
	}
	var init map[string]any
	if err := json.Unmarshal(data, &init); err != nil {
		t.Fatalf("parse stream_init: %v", err)
	}
	if init["type"] != "stream_init" {
		t.Fatalf("expected stream_init, got %v", init["type"])
	}
	return conn, init
}

// readTextUntil reads text messages until one matches want, skipping others.
// Binary frames (none expected without a frame source) fail the test.
func readTextUntil(t *testing.T, conn *websocket.Conn, timeout time.Duration, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		conn.SetReadDeadline(time.Now().Add(time.Until(deadline)))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read %s: %v", want, err)
		}
		if mt != websocket.TextMessage {
			t.Fatalf("unexpected binary frame waiting for %s", want)
		}
		var msg map[string]any
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if msg["type"] == want {
			return msg
		}
	}
}

// TestViewerPingPong verifies the app-level RTT probe: ping in, pong out
// with the same ts (Phase 1 of the ABR plan).
func TestViewerPingPong(t *testing.T) {
	const port = "15911"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	conn, _ := dialTestViewer(t, port)
	defer conn.Close()

	const ts = "1712345678.901"
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"ping","ts":`+ts+`}`)); err != nil {
		t.Fatalf("send ping: %v", err)
	}
	// Read until the pong, decoding with UseNumber so the echoed ts is
	// compared as the exact literal the server sent (a plain unmarshal
	// would float-normalize it).
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn.SetReadDeadline(time.Now().Add(time.Until(deadline)))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read pong: %v", err)
		}
		if mt != websocket.TextMessage {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var msg map[string]any
		if dec.Decode(&msg) != nil || msg["type"] != "pong" {
			continue
		}
		num, ok := msg["ts"].(json.Number)
		if !ok {
			t.Fatalf("pong ts missing: %v", msg)
		}
		if num.String() != ts {
			t.Fatalf("pong ts = %s, want %s (must echo verbatim)", num.String(), ts)
		}
		break
	}
	t.Log("ping/pong echo: PASS")
}

// TestViewerSetBitrate verifies the ABR control channel (Phase 2): a
// set_bitrate restarts the encoder, emits stream_reset + stream_init, and
// the new target shows up in the 1Hz metrics. A duplicate request must NOT
// restart again.
func TestViewerSetBitrate(t *testing.T) {
	const port = "15912"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	conn, init := dialTestViewer(t, port)
	defer conn.Close()

	base, ok := init["base_bitrate"].(float64)
	if !ok || base != 4000 {
		t.Fatalf("stream_init base_bitrate = %v, want 4000", init["base_bitrate"])
	}

	// Step down to 1500 kbps.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"set_bitrate","kbps":1500}`)); err != nil {
		t.Fatalf("send set_bitrate: %v", err)
	}
	reset := readTextUntil(t, conn, 5*time.Second, "stream_reset")
	if reset["reason"] != "bitrate_change" {
		t.Fatalf("stream_reset reason = %v, want bitrate_change", reset["reason"])
	}
	init2 := readTextUntil(t, conn, 5*time.Second, "stream_init")
	if got, ok := init2["bitrate_kbps"].(float64); !ok || got != 1500 {
		t.Fatalf("post-restart bitrate_kbps = %v, want 1500", init2["bitrate_kbps"])
	}
	metrics := readTextUntil(t, conn, 5*time.Second, "metrics")
	if got, ok := metrics["bitrate_kbps"].(float64); !ok || got != 1500 {
		t.Fatalf("metrics bitrate_kbps = %v, want 1500", metrics["bitrate_kbps"])
	}
	t.Log("set_bitrate restart + metrics: PASS")

	// Duplicate request: same rung must be a no-op (no new stream_reset).
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"set_bitrate","kbps":1500}`)); err != nil {
		t.Fatalf("send duplicate set_bitrate: %v", err)
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		conn.SetReadDeadline(time.Now().Add(time.Until(deadline)))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				break // window elapsed with no stream_reset — duplicate ignored
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				break
			}
			t.Fatalf("read after duplicate set_bitrate: %v", err)
		}
		if mt == websocket.TextMessage && len(data) > 0 && data[0] == '{' {
			var msg map[string]any
			if json.Unmarshal(data, &msg) == nil && msg["type"] == "stream_reset" {
				t.Fatal("duplicate set_bitrate triggered a second restart")
			}
		}
	}
	t.Log("duplicate set_bitrate ignored: PASS")
}

// TestViewerSetBitrateFloodBudget pins the per-viewer set_bitrate budget: an
// accepted step replaces the encoder run, so a client that ignores the ABR
// controller's pacing (8s between steps) must not be able to spawn ffmpeg per
// message. Twenty distinct rungs in one burst used to mean twenty restarts.
func TestViewerSetBitrateFloodBudget(t *testing.T) {
	const port = "15916"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	conn, init := dialTestViewer(t, port)
	defer conn.Close()
	base, _ := init["base_bitrate"].(float64)
	if base == 0 {
		t.Fatalf("stream_init base_bitrate = %v, want 4000", init["base_bitrate"])
	}

	// Twenty rungs, all distinct, all inside one second.
	for i := 1; i <= 20; i++ {
		kbps := 500 + i*150
		msg := fmt.Sprintf(`{"type":"set_bitrate","kbps":%d}`, kbps)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			t.Fatalf("send set_bitrate %d: %v", kbps, err)
		}
	}

	// Exactly one step may be accepted: the rest are refused inside the
	// interval. A refill after the interval is allowed but must not arrive
	// inside this window, so the reset count is a hard bound here.
	resets := 0
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(time.Until(deadline)))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break // window elapsed
		}
		if mt != websocket.TextMessage || len(data) == 0 || data[0] != '{' {
			continue
		}
		var msg map[string]any
		if json.Unmarshal(data, &msg) == nil && msg["type"] == "stream_reset" {
			if msg["reason"] == "bitrate_change" {
				resets++
			}
		}
	}
	if resets > 1 {
		t.Fatalf("20 set_bitrate messages in one burst caused %d restarts, want 1", resets)
	}
	if resets == 0 {
		t.Fatal("the first set_bitrate was refused; the budget must allow one step")
	}
	t.Logf("set_bitrate flood: %d restart(s) for 20 messages", resets)
}
