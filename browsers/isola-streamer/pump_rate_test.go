package main

import (
	"net"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestPumpRespectsEncodeFpsCeiling pins the CFR ceiling: a producer that pushes
// frames faster than the encode rate must not make the encoder run faster.
//
// The pump has two feed paths — the damage event (edge-triggered, so the first
// change after a quiet period does not wait for a tick) and the ticker. Only
// the event path checked the interval, so a bursty producer was fed on both,
// and the wall-clock rate followed the producer instead of the encode rate
// (measured 48 packets/s while feeding 51 fps, against a 25 fps ceiling in a
// real session and 15 here).
func TestPumpRespectsEncodeFpsCeiling(t *testing.T) {
	const port = "15940"
	listener := startTestStreamer(t, port, 2000) // currentFPS = 15 in this harness
	defer listener.Close()

	conn, init := dialWebcodecsViewer(t, port)
	defer conn.Close()
	fps, _ := init["encode_fps"].(float64)
	if fps == 0 {
		t.Fatalf("stream_init encode_fps = %v", init["encode_fps"])
	}

	w, h := e2eTestWidth, e2eTestHeight
	uds, err := net.Dial("unix", e2eTestSockPath)
	if err != nil {
		t.Fatalf("uds dial: %v", err)
	}
	defer uds.Close()

	// Feed in bursts: ten frames every 100 ms, i.e. ~10x the encode rate with
	// the same phase relationship a real compositor produces (a burst of
	// damage, then a gap). A steady feed would hide the defect, because the
	// ticker keeps lastFeedAt fresh and the event path then stays closed.
	plane := make([]byte, w*h*4)
	frameNum := 0
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			for i := 0; i < 10; i++ {
				fillAnimatedFrame(plane, w, h, frameNum)
				frameNum++
				if err := writeFrame(uds, w, h, plane); err != nil {
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	defer close(stop)

	// Count what the viewer actually receives. One deadline for the whole
	// window: gorilla poisons the connection after a read deadline expires, so
	// a per-read timeout would end the count at the first quiet moment.
	packets := 0
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage && len(data) > 10 && data[0] == 0x02 {
			packets++
		}
	}

	ceiling := int(fps*2 + 2)
	if packets > ceiling {
		t.Fatalf("%d packets in 2s from a producer feeding 4x the encode rate; ceiling is %d at %.0f fps",
			packets, ceiling, fps)
	}
	if packets < int(fps) {
		t.Fatalf("only %d packets in 2s; the pump stalled below %.0f fps", packets, fps)
	}
	t.Logf("%d packets in 2s at %.0f fps ceiling (fed in 10-frame bursts every 100ms)", packets, fps)
}
