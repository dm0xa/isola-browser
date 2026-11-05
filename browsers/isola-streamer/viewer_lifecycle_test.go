package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestAttachFailureIsExplicit pins what a client gets when no encoder can be
// started for it.
//
// The failure used to be a bare {"type":"stream_reset","reason":"encoder_error"}
// and a return. Every client treats stream_reset as "tear down, wait for the
// stream_init that follows" — and none ever came, so the only real signal was
// the socket dying, which is indistinguishable from a network drop. Now the
// socket says what happened (stream_error, no follow-up stream promised) and
// closes with an application code, so a client can stop waiting instead of
// sitting on a poster forever.
func TestAttachFailureIsExplicit(t *testing.T) {
	const port = "15945"
	// ffmpeg is resolved on PATH at spawn time. An empty PATH makes every
	// encoder start fail exactly as it does in a container without the binary
	// or one that is out of process slots.
	t.Setenv("PATH", t.TempDir())

	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+port+"/", nil)
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read stream_error: %v", err)
	}
	if mt != websocket.TextMessage {
		t.Fatalf("first message is binary (type %d), want a text stream_error", mt)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("parse first message: %v", err)
	}
	if msg["type"] != msgTypeStreamError {
		t.Fatalf("failure reported as %v, want %s (a stream_reset promises a stream_init that will never come)",
			msg["type"], msgTypeStreamError)
	}
	if msg["reason"] != "encoder_error" {
		t.Fatalf("stream_error reason = %v, want encoder_error", msg["reason"])
	}
	if msg["retryable"] != true {
		t.Fatalf("stream_error retryable = %v, want true: a reconnect may find a working encoder", msg["retryable"])
	}

	// Bounded: the socket ends on its own rather than leaving the viewer
	// waiting, and it ends with the code that names the cause.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("socket stayed open after stream_error")
	} else if !websocket.IsCloseError(err, closeCodeEncoderError) {
		t.Fatalf("close after stream_error = %v, want close code %d", err, closeCodeEncoderError)
	}

	// Nothing was left behind: the failed attach registers no run and no
	// subscriber, so the session is exactly where it started.
	runs, viewers := activeHubStats()
	if runs != 0 || viewers != 0 {
		t.Fatalf("after a failed attach: %d run(s) / %d viewer(s), want 0/0", runs, viewers)
	}
}

// TestViewerCloseReleasesEverything pins the teardown contract for a socket
// that dies without a close handshake (a dropped network, a killed tab, a
// sleeping phone): the subscription is released exactly once, and the last
// viewer leaving stops the encoder run.
//
// Two viewers of the same run closing at once is the case worth pinning: the
// second release must stop the run, and must not stop it twice — a second
// close(run.stop) panics the handler goroutine and takes the process with it.
func TestViewerCloseReleasesEverything(t *testing.T) {
	const port = "15946"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	conn1, _ := dialTestViewer(t, port)
	defer conn1.Close()
	conn2, _ := dialTestViewer(t, port)
	defer conn2.Close()
	waitForFanout(t, 1, 2)

	// Kill the transport, not the WebSocket: the read loop is parked in
	// ReadMessage and only its error path can release the subscription.
	if err := conn1.UnderlyingConn().Close(); err != nil {
		t.Logf("close first viewer transport: %v", err)
	}
	waitForFanout(t, 1, 1)

	if err := conn2.UnderlyingConn().Close(); err != nil {
		t.Logf("close second viewer transport: %v", err)
	}
	// waitForFanout gives up after 5s, so "0 runs / 0 viewers" here is the
	// bounded teardown: ffmpeg is gone, the mailbox is dropped, the hub is
	// empty.
	waitForFanout(t, 0, 0)
}
