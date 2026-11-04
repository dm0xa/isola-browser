package main

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

// TestSharedEncoderPerConfiguration pins the fan-out contract: two viewers
// asking for the same stream must share one encoder run, and the run must be
// reaped only when the last of them leaves. Before the hub, every viewer
// started its own ffmpeg and encoded byte-identical pixels.
func TestSharedEncoderPerConfiguration(t *testing.T) {
	const port = "15931"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	conn1, _ := dialTestViewer(t, port)
	defer conn1.Close()
	conn2, _ := dialTestViewer(t, port)
	defer conn2.Close()

	waitForFanout(t, 1, 2)

	// One viewer leaving must not disturb the other: the run stays up and
	// keeps its remaining subscriber.
	if err := conn1.Close(); err != nil {
		t.Logf("close first viewer: %v", err)
	}
	waitForFanout(t, 1, 1)

	// The last viewer leaving reaps the run.
	if err := conn2.Close(); err != nil {
		t.Logf("close second viewer: %v", err)
	}
	waitForFanout(t, 0, 0)
}

// TestProducerCaptureGateFollowsViewers pins the viewer gate the hub pushes to
// the electron runner: capture starts when the first viewer attaches and stops
// when the last one leaves. With nobody attached no run exists, so nothing
// consumes the frames — and an unwatched session composing and shipping them
// anyway is the largest cost it has.
func TestProducerCaptureGateFollowsViewers(t *testing.T) {
	const port = "15932"
	listener := startTestStreamer(t, port, 2000)
	defer listener.Close()

	// Stand in for the runner's ctrl socket. The hub only writes to it.
	runner, streamer := net.Pipe()
	defer runner.Close()
	tabMu.Lock()
	ctrlConn = streamer
	tabMu.Unlock()
	defer func() {
		tabMu.Lock()
		if ctrlConn == streamer {
			ctrlConn = nil
		}
		tabMu.Unlock()
	}()

	// Start from the state a session reaches after its last viewer left, so
	// the attach below has a transition to report.
	captureMu.Lock()
	captureOn = false
	captureMu.Unlock()

	cmds := make(chan map[string]any, 8)
	go func() {
		dec := json.NewDecoder(runner)
		for {
			var m map[string]any
			if err := dec.Decode(&m); err != nil {
				return
			}
			if m["cmd"] == "capture" {
				cmds <- m
			}
		}
	}()

	conn, _ := dialTestViewer(t, port)
	if on := awaitCaptureCmd(t, cmds); on != true {
		t.Fatalf("first viewer attach pushed capture on=%v, want true", on)
	}

	if err := conn.Close(); err != nil {
		t.Logf("close viewer: %v", err)
	}
	if on := awaitCaptureCmd(t, cmds); on != false {
		t.Fatalf("last viewer leaving pushed capture on=%v, want false", on)
	}
	t.Log("producer capture gate: PASS")
}

// awaitCaptureCmd waits for the next capture gate command and returns its
// desired state.
func awaitCaptureCmd(t *testing.T, cmds <-chan map[string]any) bool {
	t.Helper()
	select {
	case m := <-cmds:
		on, ok := m["on"].(bool)
		if !ok {
			t.Fatalf("capture command without a boolean on: %v", m)
		}
		return on
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a capture command")
		return false
	}
}

// waitForFanout polls the live hub until it reports the expected number of
// encoder runs and attached viewers.
func waitForFanout(t *testing.T, wantRuns, wantViewers int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, viewers := activeHubStats()
		if runs == wantRuns && viewers == wantViewers {
			t.Logf("encoder fan-out = %d run(s) / %d viewer(s)", runs, viewers)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("encoder fan-out = %d run(s) / %d viewer(s), want %d/%d",
				runs, viewers, wantRuns, wantViewers)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestKeyframeRestartAllowed(t *testing.T) {
	now := time.Now()
	started := now.Add(-time.Second)

	if keyframeRestartAllowed(started, 0, now) {
		t.Fatal("a run that has not emitted an IDR must not be restarted inside the grace window")
	}
	if !keyframeRestartAllowed(started, now.UnixNano(), now) {
		t.Fatal("a run that has already emitted an IDR must be restartable immediately")
	}
	if !keyframeRestartAllowed(now.Add(-keyframeRestartGrace), 0, now) {
		t.Fatal("a run that produced no IDR within the grace window must be restartable")
	}
	if !keyframeRestartAllowed(time.Time{}, 0, now) {
		t.Fatal("a run with no start time must be restartable")
	}
}

// TestKeyframeBudgetAllows pins the per-viewer budget arithmetic: one restart
// per keyframeRequestWindow, keyframeRequestsPerConn over the life of the
// connection, and refusals that do not spend anything.
func TestKeyframeBudgetAllows(t *testing.T) {
	var sub streamSub
	now := time.Now()

	if ok, _ := sub.keyframeBudgetAllows(now); !ok {
		t.Fatal("a viewer that never asked must be allowed a request")
	}

	sub.noteKeyframeRequest(now)
	if ok, reason := sub.keyframeBudgetAllows(now.Add(keyframeRequestWindow - time.Millisecond)); ok {
		t.Fatal("a second request inside the window must be refused")
	} else if reason != "per-viewer cooldown" {
		t.Fatalf("refusal reason = %q, want per-viewer cooldown", reason)
	}
	if sub.keyframeReqs != 1 {
		t.Fatalf("refused request spent budget: %d/%d", sub.keyframeReqs, keyframeRequestsPerConn)
	}
	if ok, _ := sub.keyframeBudgetAllows(now.Add(keyframeRequestWindow)); !ok {
		t.Fatal("a request at the window boundary must be allowed")
	}

	for sub.keyframeReqs < keyframeRequestsPerConn {
		at := now.Add(time.Duration(sub.keyframeReqs) * (keyframeRequestWindow + time.Second))
		sub.noteKeyframeRequest(at)
	}
	if ok, reason := sub.keyframeBudgetAllows(now.Add(24 * time.Hour)); ok {
		t.Fatal("a viewer past its connection budget must stay refused forever")
	} else if reason != "connection budget exhausted" {
		t.Fatalf("refusal reason = %q, want connection budget exhausted", reason)
	}
}

// TestStreamSubDroppedCountsOverflow pins that sub.dropped counts shed
// packets, which is what the dropped metrics field publishes.
func TestStreamSubDroppedCountsOverflow(t *testing.T) {
	sub := newStreamSub()
	for i := 0; i < mailboxSize+5; i++ {
		sub.push([]byte{0x01})
	}
	if got := sub.dropped.Load(); got != 5 {
		t.Fatalf("dropped = %d, want 5 shed packets", got)
	}
	if !sub.awaitingKey {
		t.Fatal("a mailbox overflow must leave the viewer waiting for a keyframe")
	}
}

// hubRunForTest returns the sole live run and its subscribers, which is the
// state of a hub with viewers dialed in and nothing else. It exists so tests
// can drive requestKeyframe directly instead of waiting on the clock.
func hubRunForTest(t *testing.T, hub *streamHub) (*streamRun, []*streamSub) {
	t.Helper()
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.runs) != 1 {
		t.Fatalf("hub has %d runs, want 1", len(hub.runs))
	}
	var run *streamRun
	for _, r := range hub.runs {
		run = r
	}
	subs := make([]*streamSub, 0, len(run.subs))
	for s := range run.subs {
		subs = append(subs, s)
	}
	return run, subs
}

// allowRestartForTest clears both the run's IDR grace and the hub-wide
// cooldown, leaving the per-viewer budget as the only possible refusal — which
// is exactly what these tests need to isolate.
func allowRestartForTest(hub *streamHub, run *streamRun) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	run.startedAt = time.Now().Add(-2 * keyframeRestartGrace)
	hub.lastForcedKeyframe = time.Time{}
}

func currentRun(hub *streamHub, key runKey) *streamRun {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.runs[key]
}

// markKeyframeDelivered puts the viewers in the state deliver() leaves them in
// after an IDR — picture resumed, retry allowance refilled — so a budget test
// can model "the viewer got its picture back" without feeding frames to
// ffmpeg.
func markKeyframeDelivered(hub *streamHub, subs ...*streamSub) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, s := range subs {
		s.awaitingKey = false
		s.keyframeRetries = 0
	}
}

// TestPerViewerKeyframeBudget is the contract that keeps one stuck decoder
// from restarting the encoder for everyone else: inside a viewer's window a
// second request must not replace the run, after it must, and another viewer's
// exhausted budget must not touch a fresh viewer. Both viewers here have
// received an IDR, so the retry allowance is not in play — this is the
// windowed budget alone.
func TestPerViewerKeyframeBudget(t *testing.T) {
	const port = "15933"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	conn1, _ := dialTestViewer(t, port)
	defer conn1.Close()
	conn2, _ := dialTestViewer(t, port)
	defer conn2.Close()
	waitForFanout(t, 1, 2)

	hub := hubFor(globalBroadcaster)
	run, subs := hubRunForTest(t, hub)
	if len(subs) != 2 {
		t.Fatalf("run has %d subscribers, want 2", len(subs))
	}
	key := run.key
	asker, other := subs[0], subs[1]
	markKeyframeDelivered(hub, asker, other)

	allowRestartForTest(hub, run)
	hub.requestKeyframe(asker)
	first := currentRun(hub, key)
	if first == run {
		t.Fatal("the first request must replace the run")
	}
	markKeyframeDelivered(hub, asker, other) // the replacement emitted its IDR

	// Second request inside the window: everything global allows a restart,
	// only the viewer's own budget refuses it.
	allowRestartForTest(hub, first)
	hub.requestKeyframe(asker)
	if got := currentRun(hub, key); got != first {
		t.Fatal("a second request inside the per-viewer window replaced the run")
	}

	// Past the window the same viewer may restart again.
	hub.mu.Lock()
	asker.keyframeReqAt = time.Now().Add(-(keyframeRequestWindow + time.Second))
	hub.mu.Unlock()
	allowRestartForTest(hub, first)
	hub.requestKeyframe(asker)
	second := currentRun(hub, key)
	if second == first {
		t.Fatal("a request after the per-viewer window must replace the run")
	}
	markKeyframeDelivered(hub, asker)

	// Spend the rest of asker's connection budget, then prove it is refused
	// even with the clock and global cooldown wide open.
	for {
		hub.mu.Lock()
		spent := asker.keyframeReqs
		hub.mu.Unlock()
		if spent >= keyframeRequestsPerConn {
			break
		}
		cur := currentRun(hub, key)
		hub.mu.Lock()
		asker.keyframeReqAt = time.Now().Add(-(keyframeRequestWindow + time.Second))
		hub.mu.Unlock()
		allowRestartForTest(hub, cur)
		hub.requestKeyframe(asker)
		if currentRun(hub, key) == cur {
			t.Fatalf("request %d/%d was refused before the budget ran out",
				spent+1, keyframeRequestsPerConn)
		}
		markKeyframeDelivered(hub, asker)
	}
	exhausted := currentRun(hub, key)
	hub.mu.Lock()
	asker.keyframeReqAt = time.Now().Add(-(keyframeRequestWindow + time.Second))
	hub.mu.Unlock()
	allowRestartForTest(hub, exhausted)
	hub.requestKeyframe(asker)
	if got := currentRun(hub, key); got != exhausted {
		t.Fatal("a viewer at its connection budget still restarted the encoder")
	}

	// The other viewer never asked; its budget must be untouched.
	hub.mu.Lock()
	otherReqs := other.keyframeReqs
	hub.mu.Unlock()
	if otherReqs != 0 {
		t.Fatalf("the other viewer's budget was spent: %d requests", otherReqs)
	}
	allowRestartForTest(hub, exhausted)
	hub.requestKeyframe(other)
	if got := currentRun(hub, key); got == exhausted {
		t.Fatal("another viewer's exhausted budget blocked a fresh viewer")
	}
}

// TestKeyframeRetryWhileNoIDR is the recovery contract: a viewer that asked
// for a keyframe and still has no picture may retry the restart a few times
// (each after the hub-wide cooldown) instead of waiting out the 30s window
// that exists to stop a viewer with a working picture from farming restarts.
func TestKeyframeRetryWhileNoIDR(t *testing.T) {
	const port = "15935"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	conn, _ := dialTestViewer(t, port)
	defer conn.Close()
	waitForFanout(t, 1, 1)

	hub := hubFor(globalBroadcaster)
	run, subs := hubRunForTest(t, hub)
	sub := subs[0]
	key := run.key
	if !sub.awaitingKey {
		t.Fatal("a viewer that never received an IDR must be awaiting one")
	}

	cur := run
	for i := 0; i < keyframeRetriesPerStall; i++ {
		allowRestartForTest(hub, cur)
		hub.requestKeyframe(sub)
		next := currentRun(hub, key)
		if next == cur {
			t.Fatalf("retry %d of %d was refused while no IDR had arrived",
				i+1, keyframeRetriesPerStall)
		}
		cur = next
	}

	hub.mu.Lock()
	retries, reqs := sub.keyframeRetries, sub.keyframeReqs
	hub.mu.Unlock()
	if retries != keyframeRetriesPerStall || reqs != 0 {
		t.Fatalf("after %d retries: retries = %d, connection budget = %d; want %d/0",
			keyframeRetriesPerStall, retries, reqs, keyframeRetriesPerStall)
	}

	// Past the allowance the window applies again — a hostile viewer is
	// bounded at keyframeRetriesPerStall fast restarts per stall.
	allowRestartForTest(hub, cur)
	hub.requestKeyframe(sub)
	if got := currentRun(hub, key); got != cur {
		t.Fatalf("retry %d replaced the run; the per-stall allowance must cap at %d",
			keyframeRetriesPerStall+1, keyframeRetriesPerStall)
	}

	// An IDR ends the stall and refills the per-stall allowance, but the
	// windowed budget still applies: this viewer has a picture again, so it
	// waits out keyframeRequestWindow like any other viewer instead of being
	// treated as recovering.
	markKeyframeDelivered(hub, sub)
	hub.mu.Lock()
	if sub.keyframeRetries != 0 {
		retries := sub.keyframeRetries
		hub.mu.Unlock()
		t.Fatalf("an IDR left %d retries on the viewer, want 0", retries)
	}
	sub.keyframeReqAt = time.Now().Add(-(keyframeRequestWindow + time.Second))
	hub.mu.Unlock()
	allowRestartForTest(hub, cur)
	hub.requestKeyframe(sub)
	if got := currentRun(hub, key); got == cur {
		t.Fatal("after the window elapsed the viewer's budget must be usable again")
	}
}

// TestKeyframeRetriesResetOnIDR pins the deliver() side of the retry
// allowance: an IDR clears awaitingKey and zeroes the per-stall count, so the
// next stall gets its own retries.
func TestKeyframeRetriesResetOnIDR(t *testing.T) {
	hub := &streamHub{runs: map[runKey]*streamRun{}}
	sub := newStreamSub()
	sub.awaitingKey = true
	sub.keyframeRetries = keyframeRetriesPerStall
	sub.keyframeReqAt = time.Now()
	run := &streamRun{hub: hub, subs: map[*streamSub]struct{}{sub: {}}}

	// A delta frame while the viewer waits is shed and must not touch the
	// retry allowance.
	run.deliver([]byte{0x02}, false, false)
	if got := sub.dropped.Load(); got != 1 {
		t.Fatalf("delta while awaiting a keyframe: dropped = %d, want 1", got)
	}
	if sub.keyframeRetries != keyframeRetriesPerStall {
		t.Fatalf("a shed delta frame reset the retry allowance to %d", sub.keyframeRetries)
	}

	run.deliver([]byte{0x02}, true, false)
	if sub.awaitingKey {
		t.Fatal("an IDR must clear awaitingKey")
	}
	if sub.keyframeRetries != 0 {
		t.Fatalf("keyframeRetries = %d after an IDR, want 0", sub.keyframeRetries)
	}

	// The next stall gets its retries again.
	hub.mu.Lock()
	sub.awaitingKey = true
	hub.mu.Unlock()
	if ok, reason := sub.keyframeBudgetAllows(time.Now()); !ok {
		t.Fatalf("the next stall was refused a retry: %s", reason)
	}
}

// TestMetricsCarriesDropped pins the per-viewer drop counter in the 1 Hz
// metrics snapshot. A value set on the subscription must arrive verbatim in
// the JSON, which is what a client watching a stalling socket needs.
func TestMetricsCarriesDropped(t *testing.T) {
	const port = "15934"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	conn, _ := dialTestViewer(t, port)
	defer conn.Close()
	waitForFanout(t, 1, 1)

	hub := hubFor(globalBroadcaster)
	_, subs := hubRunForTest(t, hub)
	if len(subs) != 1 {
		t.Fatalf("run has %d subscribers, want 1", len(subs))
	}
	subs[0].dropped.Store(7)

	metrics := readTextUntil(t, conn, 5*time.Second, "metrics")
	got, ok := metrics["dropped"].(float64)
	if !ok {
		t.Fatalf("metrics has no numeric dropped field: %v", metrics["dropped"])
	}
	if got != 7 {
		t.Fatalf("metrics dropped = %v, want 7", got)
	}
}

// ── run cap: one ffmpeg per distinct rung is not free ─────────────────────

// capTestKey is the run key a viewer of this test session asks for at the
// given rung. baseKbps is the session ceiling (the bitrate the streamer boots
// with), which is what the client's ABR ladder is computed from.
func capTestKey(kbps int, scale float64) runKey {
	return runKey{
		kbps:      kbps,
		scale:     scale,
		width:     e2eTestWidth,
		height:    e2eTestHeight,
		fps:       15,
		baseKbps:  4000,
		keepalive: 2000,
	}
}

// drainControl returns the control messages queued on a subscription, in order,
// without blocking. Video packets are skipped: these tests feed no frames.
func drainControl(sub *streamSub) []map[string]any {
	var out []map[string]any
	for {
		select {
		case ev := <-sub.ch:
			if ev.text != nil {
				out = append(out, ev.text)
			}
		default:
			return out
		}
	}
}

// TestRunCapReuseSelection pins the choice made when the cap is reached, with
// no encoder involved: never a run the viewer cannot decode, never a stopping
// one, the most expensive rung at or below the request when there is one, and
// the cheapest live run when the request is below all of them.
func TestRunCapReuseSelection(t *testing.T) {
	mk := func(kbps int, scale float64) *streamRun {
		return &streamRun{key: capTestKey(kbps, scale), subs: map[*streamSub]struct{}{}}
	}
	r4000, r2800, r1200 := mk(4000, 1.0), mk(2800, 1.0), mk(1200, 0.5)
	h := &streamHub{runs: map[runKey]*streamRun{
		capTestKey(4000, 1.0): r4000,
		capTestKey(2800, 1.0): r2800,
		capTestKey(1200, 0.5): r1200,
	}}

	// 1800 is between rungs: the most expensive run that does not exceed the
	// request wins, so sharing never spends more bytes than the client's own
	// evidence allowed.
	if got := h.reusableRunLocked(capTestKey(1800, 0.8)); got != r1200 {
		t.Fatalf("1800 kbps shared the wrong run: %v", got.key)
	}
	// 3000 is between 2800 and 4000: 2800, for the same reason.
	if got := h.reusableRunLocked(capTestKey(3000, 1.0)); got != r2800 {
		t.Fatalf("3000 kbps shared the wrong run: %v", got.key)
	}
	// Above every live run the most expensive one is still at or below the
	// request, so it wins — sharing a cheaper run is always allowed.
	if got := h.reusableRunLocked(capTestKey(5000, 1.0)); got != r4000 {
		t.Fatalf("5000 kbps shared the wrong run: %v", got.key)
	}
	// Below every live run there is nothing at or below, so the cheapest run
	// is the closest thing to the request.
	if got := h.reusableRunLocked(capTestKey(1100, 0.5)); got != r1200 {
		t.Fatalf("1100 kbps shared the wrong run: %v", got.key)
	}

	// A different container format is undecodable, whatever the bitrate.
	raw := capTestKey(3000, 1.0)
	raw.rawH264 = true
	if got := h.reusableRunLocked(raw); got != nil {
		t.Fatalf("a WebCodecs viewer was offered the fMP4 run %v", got.key)
	}
	// A different producer geometry is undecodable too: the encoder's raw
	// input stride is fixed in its argv.
	geom := capTestKey(3000, 1.0)
	geom.width, geom.height = 1024, 768
	if got := h.reusableRunLocked(geom); got != nil {
		t.Fatalf("a resized viewer was offered a run at another geometry: %v", got.key)
	}

	// A run that is being torn down is never a candidate.
	r4000.stopping, r2800.stopping = true, true
	if got := h.reusableRunLocked(capTestKey(3000, 1.0)); got != r1200 {
		t.Fatalf("a stopping run was offered: %v", got.key)
	}
	r4000.stopping, r2800.stopping = false, false

	// Exactly the requested rung is handled by the map lookup, not here, but
	// the helper must agree: 2800 for a 2800 request.
	if got := h.reusableRunLocked(capTestKey(2800, 1.0)); got != r2800 {
		t.Fatalf("2800 kbps shared the wrong run: %v", got.key)
	}
}

// TestRunCapSharesThirdRung is the cap itself, end to end with real encoders:
// two viewers on two rungs get an encoder each, and a third rung joins one of
// them instead of starting a third ffmpeg. The joining viewer must be told
// where it landed — stream_init carries the shared run's real bitrate and
// encoded geometry, never the rung it asked for.
func TestRunCapSharesThirdRung(t *testing.T) {
	const port = "15940"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	hub := hubFor(globalBroadcaster)
	subA, subB, subC := newStreamSub(), newStreamSub(), newStreamSub()
	defer hub.release(subA)
	defer hub.release(subB)
	defer hub.release(subC)

	runA := hub.attach(subA, capTestKey(4000, 1.0), "")
	runB := hub.attach(subB, capTestKey(2800, 1.0), "bitrate_change")
	if runA == nil || runB == nil || runA == runB {
		t.Fatalf("two rungs must get two runs: %v / %v", runA, runB)
	}
	if runs, viewers := hub.stats(); runs != 2 || viewers != 2 {
		t.Fatalf("fan-out = %d run(s) / %d viewer(s), want 2/2", runs, viewers)
	}

	// The third configuration would be a third encoder. It shares the most
	// expensive run that does not exceed it — 1800 is below both, so the
	// cheapest live run (2800).
	runC := hub.attach(subC, capTestKey(1800, 0.8), "bitrate_change")
	if runC == nil {
		t.Fatal("the third viewer got no run at all")
	}
	if runC != runB {
		t.Fatalf("third rung landed on %v, want the 2800 run %v", runC.key, runB.key)
	}
	if runs, viewers := hub.stats(); runs != 2 || viewers != 3 {
		t.Fatalf("fan-out = %d run(s) / %d viewer(s), want 2/3", runs, viewers)
	}
	// No encoder was started for the requested rung.
	if got := currentRun(hub, capTestKey(1800, 0.8)); got != nil {
		t.Fatalf("a run was started at the capped rung: %v", got.key)
	}

	// What the client is told: a reset into a stream whose bitrate and encoded
	// size are the shared run's. The requested rung's geometry (0.8 scale) must
	// not appear anywhere — landing elsewhere is allowed, landing elsewhere
	// silently is not.
	msgs := drainControl(subC)
	if len(msgs) != 2 {
		t.Fatalf("capped attach queued %d control messages, want stream_reset+stream_init: %v", len(msgs), msgs)
	}
	if msgs[0]["type"] != msgTypeStreamReset || msgs[0]["reason"] != "bitrate_change" {
		t.Fatalf("first control message = %v, want stream_reset/bitrate_change", msgs[0])
	}
	init := msgs[1]
	if init["type"] != msgTypeStreamInit {
		t.Fatalf("second control message = %v, want stream_init", init)
	}
	if init["bitrate_kbps"] != runB.key.kbps {
		t.Fatalf("stream_init bitrate_kbps = %v, want the shared run's %d", init["bitrate_kbps"], runB.key.kbps)
	}
	if init["width"] != runB.encW || init["height"] != runB.encH {
		t.Fatalf("stream_init encoded size = %vx%v, want the shared run's %dx%d",
			init["width"], init["height"], runB.encW, runB.encH)
	}
	if init["capture_width"] != e2eTestWidth || init["capture_height"] != e2eTestHeight {
		t.Fatalf("stream_init capture size = %vx%v, want %dx%d",
			init["capture_width"], init["capture_height"], e2eTestWidth, e2eTestHeight)
	}
	wantW, wantH := encodeDimensions(e2eTestWidth, e2eTestHeight, 0.8)
	if runB.encW == wantW && runB.encH == wantH {
		t.Fatalf("test setup is not discriminating: the shared run already has the requested geometry %dx%d", wantW, wantH)
	}
}

// TestRunCapNeverCrossesDecodeMode pins the exception: the cap bounds encoder
// count, it does not hand a viewer a stream it cannot decode. A WebCodecs
// viewer arriving while the cap is spent on fMP4 runs gets its own encoder,
// because the alternative is a picture that never decodes.
func TestRunCapNeverCrossesDecodeMode(t *testing.T) {
	const port = "15941"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	hub := hubFor(globalBroadcaster)
	mseA, mseB, raw := newStreamSub(), newStreamSub(), newStreamSub()
	defer hub.release(mseA)
	defer hub.release(mseB)
	defer hub.release(raw)

	if hub.attach(mseA, capTestKey(4000, 1.0), "") == nil ||
		hub.attach(mseB, capTestKey(2800, 1.0), "bitrate_change") == nil {
		t.Fatal("fMP4 runs failed to start")
	}
	rawKey := capTestKey(1800, 0.8)
	rawKey.rawH264 = true
	got := hub.attach(raw, rawKey, "bitrate_change")
	if got == nil {
		t.Fatal("the WebCodecs viewer got no run")
	}
	if !got.key.rawH264 {
		t.Fatalf("a WebCodecs viewer was attached to an fMP4 run: %v", got.key)
	}
	if got.key != rawKey {
		t.Fatalf("WebCodecs viewer landed on %v, want its own rung %v", got.key, rawKey)
	}
	if runs, viewers := hub.stats(); runs != 3 || viewers != 3 {
		t.Fatalf("fan-out = %d run(s) / %d viewer(s), want 3/3 (cap yields to decode mode)", runs, viewers)
	}
}

// TestSingleViewerKeepsRequestedRung pins that the cap never touches the case
// it is not meant for: a viewer alone in the session walks the whole ladder and
// gets exactly the rung it asks for every time, because the run it steps off is
// its own and is stopped as part of the step.
func TestSingleViewerKeepsRequestedRung(t *testing.T) {
	const port = "15942"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	hub := hubFor(globalBroadcaster)
	sub := newStreamSub()
	defer hub.release(sub)

	rungs := []struct {
		kbps  int
		scale float64
	}{{4000, 1.0}, {2800, 1.0}, {1800, 0.8}, {1200, 0.5}, {2800, 1.0}}
	for i, r := range rungs {
		key := capTestKey(r.kbps, r.scale)
		run := hub.attach(sub, key, "bitrate_change")
		if run == nil {
			t.Fatalf("step %d to %d kbps failed", i, r.kbps)
		}
		if run.key != key {
			t.Fatalf("step %d landed on %v, want %v", i, run.key, key)
		}
		if runs, viewers := hub.stats(); runs != 1 || viewers != 1 {
			t.Fatalf("step %d: fan-out = %d run(s) / %d viewer(s), want 1/1", i, runs, viewers)
		}
		init := run.initMessage()
		if init["bitrate_kbps"] != r.kbps {
			t.Fatalf("step %d: stream_init bitrate_kbps = %v, want %d", i, init["bitrate_kbps"], r.kbps)
		}
		wantW, wantH := encodeDimensions(e2eTestWidth, e2eTestHeight, r.scale)
		if init["width"] != wantW || init["height"] != wantH {
			t.Fatalf("step %d: stream_init encoded size = %vx%v, want %dx%d",
				i, init["width"], init["height"], wantW, wantH)
		}
	}
}

// TestRungChangeDuringKeyframeRestart pins the interleaving a second viewer can
// hit: one viewer replaces the run for an IDR while another steps to a rung.
// Both paths mutate the hub under the same lock, so whatever the order, every
// viewer must end up attached to a run that is live and in the hub's map — not
// to the encoder being torn down, and not to nothing.
func TestRungChangeDuringKeyframeRestart(t *testing.T) {
	const port = "15943"
	listener := startTestStreamer(t, port, 4000)
	defer listener.Close()

	hub := hubFor(globalBroadcaster)
	for iter := 0; iter < 3; iter++ {
		subA, subB := newStreamSub(), newStreamSub()
		run := hub.attach(subA, capTestKey(4000, 1.0), "")
		if run == nil {
			t.Fatal("first viewer got no run")
		}
		if got := hub.attach(subB, capTestKey(4000, 1.0), ""); got != run {
			t.Fatal("second viewer on the same rung must share the run")
		}
		// Give the run an IDR (so a keyframe request is not held back by the
		// startup grace) and clear the hub-wide cooldown.
		hub.mu.Lock()
		run.keyframeAt.Store(time.Now().UnixNano())
		run.startedAt = time.Now().Add(-2 * keyframeRestartGrace)
		hub.lastForcedKeyframe = time.Time{}
		subA.awaitingKey, subB.awaitingKey = false, false
		hub.mu.Unlock()

		done := make(chan struct{}, 2)
		go func() {
			hub.requestKeyframe(subA)
			done <- struct{}{}
		}()
		go func() {
			hub.attach(subB, capTestKey(2800, 1.0), "bitrate_change")
			done <- struct{}{}
		}()
		<-done
		<-done

		hub.mu.Lock()
		live := make(map[*streamRun]bool, len(hub.runs))
		for _, r := range hub.runs {
			if r.stopping {
				hub.mu.Unlock()
				t.Fatalf("iteration %d left a stopping run in the map", iter)
			}
			live[r] = true
		}
		for i, s := range []*streamSub{subA, subB} {
			if s.run == nil {
				hub.mu.Unlock()
				t.Fatalf("iteration %d: viewer %d is attached to no run", iter, i)
			}
			if !live[s.run] {
				hub.mu.Unlock()
				t.Fatalf("iteration %d: viewer %d is attached to a run that is not live", iter, i)
			}
			if _, ok := s.run.subs[s]; !ok {
				hub.mu.Unlock()
				t.Fatalf("iteration %d: viewer %d is missing from its run's subscriber set", iter, i)
			}
		}
		runs := len(hub.runs)
		hub.mu.Unlock()
		if runs > maxConcurrentRuns {
			t.Fatalf("iteration %d: %d runs live, cap is %d", iter, runs, maxConcurrentRuns)
		}

		hub.release(subA)
		hub.release(subB)
	}
}
