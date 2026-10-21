package main

import (
	"io"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// runKey is the complete set of inputs to an encoder's argv. Two viewers may
// share one ffmpeg only when every one of these matches: anything left out
// would let a shared run encode the wrong bitrate, size or container format
// for one of the viewers attached to it.
type runKey struct {
	kbps      int
	scale     float64
	rawH264   bool
	width     int
	height    int
	fps       int
	baseKbps  int
	keepalive int
}

// muxFlushFollowUpMs: after a feed that carried a real change, this is how
// long to wait before feeding one duplicate frame. The fMP4 muxer only closes
// a fragment when the next packet arrives, so without this a single change on
// a quiet page sits unflushed until the static keepalive window (2s) — dead
// display latency for MSE viewers. The duplicate frame is the cheapest
// possible (an unchanged surface encodes to a few hundred bytes).
const muxFlushFollowUpMs = 40

// streamEvent is one item in a viewer's outbound mailbox: either a control
// message (stream_init/stream_reset) or a packed video packet.
type streamEvent struct {
	binary []byte
	text   map[string]any
}

// mailboxSize bounds a viewer's outbound queue.
//
// Fan-out must never block on one viewer: a stalled socket would otherwise
// stall the shared encoder for every other viewer of the session. At 25-50
// packets/s this holds several seconds of video, so a viewer that fills it is
// already past the point where its 10s socket-write deadline matters.
const mailboxSize = 192

// streamSub is one viewer's attachment to the shared encoder.
//
// A subscription outlives the run it points at: an ABR step re-points it at
// another run instead of creating a new mailbox, so every message a viewer
// receives — control or video — passes through one FIFO queue and keeps its
// order. Control messages therefore always follow the video of the stream they
// replace and precede the video of the stream they introduce.
//
// Every field except sentBytes is guarded by the hub mutex: the fan-out and
// the attach/release/restart paths all mutate it from different goroutines.
type streamSub struct {
	ch  chan streamEvent
	run *streamRun

	// awaitingKey drops video packets until the next keyframe. Set when a
	// subscription starts — a client must never receive a delta frame whose
	// reference it never got — and again when its mailbox overflows, since the
	// packets that bridged the gap are gone and only an IDR can resync it.
	awaitingKey bool

	// dropped counts packets shed by this viewer, either because its mailbox
	// was full or because it was waiting for a keyframe.
	dropped atomic.Int64
	// sentBytes counts video bytes actually handed to this viewer's socket.
	// The client's ABR compares its arrival rate against this rather than
	// against the bitrate target, so it must be per-viewer: two viewers of the
	// same shared run need not receive the same bytes.
	sentBytes atomic.Int64
}

func newStreamSub() *streamSub {
	return &streamSub{ch: make(chan streamEvent, mailboxSize)}
}

// push queues a video packet without blocking. A viewer that cannot keep up
// loses packets and resyncs at the next keyframe instead of stalling the
// encoder for everyone else.
func (s *streamSub) push(pkt []byte) {
	select {
	case s.ch <- streamEvent{binary: pkt}:
	default:
		s.awaitingKey = true
		s.dropped.Add(1)
	}
}

// enqueueControl queues control messages ahead of any video already waiting,
// shedding that video: it belongs to the stream these messages are replacing
// (stream_reset) or introducing (stream_init), so the client could not use it
// either way.
//
// The send cannot block — the queue is emptied first and at most two messages
// are queued — which matters because every caller holds the hub mutex.
func (s *streamSub) enqueueControl(msgs ...map[string]any) {
drain:
	for {
		select {
		case <-s.ch:
		default:
			break drain
		}
	}
	for _, m := range msgs {
		s.ch <- streamEvent{text: m}
	}
}

// streamRun is one ffmpeg process and its pumps, serving every viewer whose
// configuration matches its key.
type streamRun struct {
	key runKey
	hub *streamHub
	bc  *frameBroadcaster

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stop   chan struct{} // closed to stop this run's pumps
	exited chan struct{} // closed once both pumps have returned
	// started records that the pumps were launched; a run killed before that
	// point has nothing to wait for in stop().
	started bool
	// geometryWaitLogged is set once when the frame pump starts waiting for a
	// producer geometry that matches this run's key. Owned by that pump alone.
	geometryWaitLogged bool

	codec     string
	encW      int
	encH      int
	encodeFps int
	// startedAt is when this ffmpeg was spawned. A keyframe request that
	// arrives before the run has emitted its first IDR is ignored until
	// keyframeRestartGrace has passed: honoring it kills the encoder that
	// was about to produce that IDR, and the client's next request does
	// the same.
	startedAt time.Time

	// initSeg is the packed fMP4 init segment (ftyp+moov), the first fragment
	// ffmpeg emits. A viewer joining later has missed it, and MSE cannot parse
	// a fragment without it, so it is replayed on attach. Empty on the
	// Annex-B path, where SPS/PPS ride in-band before every IDR.
	initSeg []byte
	// lastKeyPkt is the most recent packed IDR (Annex-B AU or fMP4 fragment).
	// Replayed to a late joiner as a poster: P-frames after this IDR would
	// not decode, so awaitingKey stays set until the next live IDR.
	lastKeyPkt []byte

	// seq numbers the packets this run emits. It is shared by every viewer of
	// the run (they receive the same packets), so it identifies encoder output
	// rather than a viewer's stream.
	seq atomic.Uint32

	// skippedTicks counts encode ticks suppressed because the surface was
	// unchanged within the keepalive window.
	skippedTicks atomic.Int64
	// keyframeAt is the unix-nanosecond time of the last IDR written by this
	// run (0 = none yet). kfMu guards the reason fields: a run remembers why
	// it was started (pendingKeyframeReason) until its first IDR consumes it
	// into lastKeyframeReason ("periodic" thereafter).
	keyframeAt            atomic.Int64
	kfMu                  sync.Mutex
	lastKeyframeReason    string
	pendingKeyframeReason string

	subs     map[*streamSub]struct{}
	stopping bool
}

// recordKeyframe marks an IDR actually produced by this run and resolves its
// reason. The first IDR consumes the startup reason; later periodic GOP
// keyframes are reported as "periodic".
func (r *streamRun) recordKeyframe(now time.Time) {
	r.keyframeAt.Store(now.UnixNano())
	r.kfMu.Lock()
	if r.pendingKeyframeReason != "" {
		r.lastKeyframeReason = r.pendingKeyframeReason
		r.pendingKeyframeReason = ""
	} else {
		r.lastKeyframeReason = "periodic"
	}
	r.kfMu.Unlock()
}

// keyframeState reports the timestamp and resolved reason of this run's last
// written IDR; both are zero values when no IDR has been written yet.
func (r *streamRun) keyframeState() (at time.Time, reason string) {
	ns := r.keyframeAt.Load()
	if ns == 0 {
		return time.Time{}, ""
	}
	r.kfMu.Lock()
	reason = r.lastKeyframeReason
	r.kfMu.Unlock()
	return time.Unix(0, ns), reason
}

// initMessage describes this run to a viewer. Everything in it is a property
// of the run, so every viewer attached to the run receives the same message.
func (r *streamRun) initMessage() map[string]any {
	m := map[string]any{
		"type":           msgTypeStreamInit,
		"codec":          r.codec,
		"width":          r.encW,
		"height":         r.encH,
		"capture_width":  r.key.width,
		"capture_height": r.key.height,
		"encode_fps":     r.encodeFps,
		"bitrate_kbps":   r.key.kbps,
		"base_bitrate":   r.key.baseKbps,
	}
	if r.key.rawH264 {
		m["mode"] = "webcodecs"
	}
	return m
}

// streamHub owns the session's encoder runs. One container serves one browser
// session, so there is one hub per process; runs are created on demand and
// reaped when their last viewer leaves.
type streamHub struct {
	bc *frameBroadcaster
	// mu guards runs and every run's subs/stopping flag. The fan-out takes it
	// per packet, which is what makes "this viewer is no longer attached to
	// that run" and "this packet went to that viewer" atomic with respect to
	// each other.
	mu   sync.Mutex
	runs map[runKey]*streamRun
	// lastForcedKeyframe is when the hub last replaced a run to obtain an
	// IDR (late joiner or client request). A second restart inside
	// lateJoinRestartCooldown would kill the replacement before it flushed
	// its first IDR — that storm is what made share viewers wait tens of
	// seconds while ffmpeg was spawned and immediately killed.
	lastForcedKeyframe time.Time
}

var (
	hubRegistryMu sync.Mutex
	activeHub     *streamHub
)

// hubFor returns the hub streaming the given broadcaster, building one — and
// retiring the previous session's, encoders included — when the broadcaster
// changes. Keying it on the broadcaster keeps a freshly built broadcaster from
// inheriting another session's live encoders, which is what the tests do on
// every start.
func hubFor(bc *frameBroadcaster) *streamHub {
	hubRegistryMu.Lock()
	defer hubRegistryMu.Unlock()
	if activeHub != nil && activeHub.bc == bc {
		return activeHub
	}
	old := activeHub
	activeHub = &streamHub{bc: bc, runs: make(map[runKey]*streamRun)}
	if old != nil {
		old.shutdown()
	}
	return activeHub
}

// attach points sub at the run for key, starting one if nobody is streaming
// that configuration yet. resetReason, when non-empty, prepends a stream_reset
// so a client that is already decoding tears its pipeline down before the init
// message that follows it.
//
// It returns nil only when the encoder could not be started, in which case sub
// is left where it was.
func (h *streamHub) attach(sub *streamSub, key runKey, resetReason string) *streamRun {
	h.mu.Lock()

	run := h.runs[key]
	fresh := false
	switch {
	case run == nil:
		run = h.spawnLocked(key, keyframeReasonFor(resetReason))
		if run == nil {
			h.mu.Unlock()
			return nil
		}
		h.runs[key] = run
		fresh = true
	case sub.run == run:
		// Already streaming exactly this configuration: re-announcing it
		// would cost the viewer its decoder for nothing.
		h.mu.Unlock()
		return run
	}

	// Leaving the previous run behind is what makes an ABR step cost one
	// encoder: the rung the viewer stepped off is stopped, not left running
	// for nobody.
	orphan := h.detachLocked(sub)
	sub.run = run
	sub.awaitingKey = true
	run.subs[sub] = struct{}{}

	// stream_init must be the client's first text message, and on the fMP4
	// path it must precede any fragment, so both are queued here — under the
	// lock, before the pumps can produce anything.
	msgs := make([]map[string]any, 0, 2)
	if resetReason != "" {
		msgs = append(msgs, map[string]any{"type": msgTypeStreamReset, "reason": resetReason})
	}
	msgs = append(msgs, run.initMessage())
	sub.enqueueControl(msgs...)
	if len(run.initSeg) > 0 {
		sub.push(run.initSeg)
	}
	if len(run.lastKeyPkt) > 0 {
		sub.push(run.lastKeyPkt)
	}

	if fresh {
		run.startPumps()
	}
	h.mu.Unlock()

	// Outside the lock: the producer gate reads the hub, and stopping the
	// orphan waits for its pumps, which take the hub mutex on the way out.
	syncProducerCapture(false)
	if orphan != nil {
		h.stop(orphan)
	}
	return run
}

// detachLocked unlinks sub from the run it is attached to and returns that run
// when the caller — outside the lock — has to stop it: the viewer was its last
// one.
func (h *streamHub) detachLocked(sub *streamSub) *streamRun {
	old := sub.run
	sub.run = nil
	if old == nil {
		return nil
	}
	delete(old.subs, sub)
	if len(old.subs) > 0 || old.stopping || !old.started {
		return nil
	}
	old.stopping = true
	delete(h.runs, old.key)
	return old
}

// release detaches a viewer and stops the run it was the last viewer of.
func (h *streamHub) release(sub *streamSub) {
	h.mu.Lock()
	run := h.detachLocked(sub)
	h.mu.Unlock()

	// The last viewer leaving is what lets the producer stop capturing, so the
	// gate is evaluated before the (slower) encoder teardown.
	syncProducerCapture(false)
	if run != nil {
		h.stop(run)
	}
}

// runOf reports the run a viewer is currently streaming from, or nil when it
// has none (the encoder failed to start).
func (h *streamHub) runOf(sub *streamSub) *streamRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return sub.run
}

// stats reports how many encoder runs are live and how many viewers they
// serve. The difference between the two is what sharing the encoders bought.
func (h *streamHub) stats() (runs, viewers int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.runs {
		viewers += len(r.subs)
	}
	return len(h.runs), viewers
}

// activeHubStats reports the current session's encoder fan-out for the debug
// endpoint, or zeros when no session is streaming.
func activeHubStats() (runs, viewers int) {
	hubRegistryMu.Lock()
	hub := activeHub
	hubRegistryMu.Unlock()
	if hub == nil {
		return 0, 0
	}
	return hub.stats()
}

// capture state pushed to the producer.
var (
	captureMu sync.Mutex
	// captureOn is the gate last pushed to the runner. It starts true to match
	// the runner's own default, so the first "nobody is watching" is sent.
	captureOn = true
)

// syncProducerCapture tells the electron runner whether anyone is watching.
//
// With no viewer attached nothing downstream of the capture exists: no run
// lives, so no ffmpeg is running and nothing consumes the frames. The producer
// composing and shipping them anyway is the largest cost of an unwatched
// session — measured at ~0.4 cores on a CPU-only host for a page that
// animates, against ~0.05 for the same session with capture paused.
//
// force re-sends the state even when it did not change, which is what a
// freshly connected ctrl socket needs: whatever was sent before it existed
// went nowhere.
func syncProducerCapture(force bool) {
	_, viewers := activeHubStats()
	// An active recording is a capture consumer in its own right: the
	// session keeps encoding even with zero viewers attached.
	on := viewers > 0 || recordingActive.Load()
	captureMu.Lock()
	changed := on != captureOn
	captureOn = on
	captureMu.Unlock()
	if !force && !changed {
		return
	}
	writeCtrlCommand(map[string]any{"cmd": "capture", "on": on})
}

// keyframeRestartGrace is how long a run that has not yet written an IDR is
// left alone. The client's recovery timer is shorter than a cold encoder's
// first flushed IDR while capture is still coming up (the access-unit
// splitter holds that IDR until the next picture). Restarting inside the
// grace throws the in-flight IDR away and the next request repeats it.
const keyframeRestartGrace = 4 * time.Second

// lateJoinRestartCooldown is how long after a forced IDR restart another
// one is refused. It must cover the replacement encoder's first-IDR window
// (keyframeRestartGrace) so a second joiner, a host reconnect, or the
// client's recovery timer cannot thrash ffmpeg.
const lateJoinRestartCooldown = keyframeRestartGrace + time.Second

// keyframeRestartAllowed reports whether a client request_keyframe should
// replace the run. A run that has already emitted an IDR can be restarted
// immediately (the viewer is stuck mid-GOP). A run that has not is still
// producing its first one; replacing it only helps once it has had the
// grace window and still produced nothing.
func keyframeRestartAllowed(startedAt time.Time, keyframeAtUnixNano int64, now time.Time) bool {
	if keyframeAtUnixNano != 0 {
		return true
	}
	if startedAt.IsZero() {
		return true
	}
	return now.Sub(startedAt) >= keyframeRestartGrace
}

// requestKeyframe restarts the run behind sub so that the next frame is an IDR.
//
// libx264 on a live pipe cannot be signalled for a keyframe, so — exactly like
// the ABR path — the cheapest reliable way is a fresh encoder. Every viewer of
// that run is reset with it, because they are all decoding the same stream.
func (h *streamHub) requestKeyframe(sub *streamSub) {
	h.mu.Lock()
	run := sub.run
	if run == nil || run.stopping {
		h.mu.Unlock()
		return
	}
	if !keyframeRestartAllowed(run.startedAt, run.keyframeAt.Load(), time.Now()) {
		h.mu.Unlock()
		log.Printf("ws: keyframe request ignored, encoder has not emitted an IDR yet")
		return
	}
	if !h.lastForcedKeyframe.IsZero() && time.Since(h.lastForcedKeyframe) < lateJoinRestartCooldown {
		h.mu.Unlock()
		log.Printf("ws: keyframe request ignored, restart cooldown")
		return
	}
	// Start the replacement first: if ffmpeg will not come up, the viewers keep
	// the run they already have instead of being left with nothing.
	fresh := h.spawnLocked(run.key, "requested")
	if fresh == nil {
		h.mu.Unlock()
		log.Printf("ws: keyframe restart at %d kbps failed", run.key.kbps)
		return
	}
	h.lastForcedKeyframe = time.Now()
	key := run.key
	subs := make([]*streamSub, 0, len(run.subs))
	for s := range run.subs {
		subs = append(subs, s)
	}
	run.stopping = true
	h.runs[key] = fresh
	for _, s := range subs {
		if s.run != run {
			continue // the viewer left while the encoder was being replaced
		}
		delete(run.subs, s)
		s.run = fresh
		s.awaitingKey = true
		fresh.subs[s] = struct{}{}
		s.enqueueControl(
			map[string]any{"type": msgTypeStreamReset, "reason": "keyframe_request"},
			fresh.initMessage(),
		)
	}
	orphan := fresh
	if len(fresh.subs) > 0 {
		orphan = nil
		fresh.startPumps()
	} else {
		delete(h.runs, key)
	}
	h.mu.Unlock()

	// The old run's pumps may still be draining into the emptied subscriber
	// set; stopping it here (outside the lock) keeps that off the critical
	// path for the viewers already streaming from the new one.
	h.stop(run)
	if orphan != nil {
		h.stop(orphan)
	}
	log.Printf("ws: forced keyframe at %d kbps (client request)", key.kbps)
}

// resizeAll re-keys every encoder run to a new producer geometry.
//
// The composed surface follows the producer by itself — composeLocked resizes
// it when frames arrive at the new size — but an ffmpeg run's output size is
// fixed in its argv and the run key carries width/height. A viewport resize is
// therefore a replacement run per configuration, with every viewer reset onto
// it, exactly like a forced keyframe.
//
// A run that cannot be started leaves its viewers on the old geometry: a
// resize must never take a session down.
func (h *streamHub) resizeAll(width, height int) {
	if width <= 0 || height <= 0 {
		return
	}
	h.mu.Lock()
	type replacement struct {
		old   *streamRun
		fresh *streamRun
	}
	var plan []replacement
	for key, run := range h.runs {
		if run.stopping || (key.width == width && key.height == height) {
			continue
		}
		newKey := key
		newKey.width, newKey.height = width, height

		// Two configurations that differed only in geometry collapse onto one
		// new key here; share the replacement instead of spawning twice.
		fresh := h.runs[newKey]
		startPumps := false
		if fresh == nil || fresh.stopping {
			fresh = h.spawnLocked(newKey, "resize")
			if fresh == nil {
				log.Printf("ws: resize to %dx%d failed for kbps=%d scale=%.2f; keeping the old encoder",
					width, height, key.kbps, key.scale)
				continue
			}
			h.runs[newKey] = fresh
			startPumps = true
		}
		run.stopping = true
		delete(h.runs, key)
		for s := range run.subs {
			delete(run.subs, s)
			s.run = fresh
			s.awaitingKey = true
			fresh.subs[s] = struct{}{}
			s.enqueueControl(
				map[string]any{"type": msgTypeStreamReset, "reason": "resize"},
				fresh.initMessage(),
			)
		}
		if startPumps {
			fresh.startPumps()
		}
		plan = append(plan, replacement{old: run, fresh: fresh})
	}
	h.mu.Unlock()

	// The old runs' pumps may still be draining; stop them outside the lock so
	// viewers already reading the replacement are not held up.
	for _, p := range plan {
		h.stop(p.old)
	}
	if len(plan) > 0 {
		log.Printf("ws: encoder re-keyed to %dx%d (%d run(s))", width, height, len(plan))
	}
}

// shutdown stops every run. It is called when the session's broadcaster is
// replaced.
func (h *streamHub) shutdown() {
	h.mu.Lock()
	runs := make([]*streamRun, 0, len(h.runs))
	for _, r := range h.runs {
		r.stopping = true
		runs = append(runs, r)
	}
	h.runs = make(map[runKey]*streamRun)
	h.mu.Unlock()

	for _, r := range runs {
		h.stop(r)
	}
}

// spawnLocked starts ffmpeg for key without launching its pumps, so the caller
// can register subscribers and queue their control messages before anything is
// broadcast. reason becomes the run's first keyframe reason in metrics.
func (h *streamHub) spawnLocked(key runKey, reason string) *streamRun {
	cmd, stdin, stdout, codec, encW, encH, encodeFps, err :=
		startEncoder(key.width, key.height, key.fps, key.kbps, key.scale, key.rawH264)
	if err != nil {
		log.Printf("ws: encoder error: %v", err)
		return nil
	}
	return &streamRun{
		key: key, hub: h, bc: h.bc,
		cmd: cmd, stdin: stdin, stdout: stdout,
		stop:   make(chan struct{}),
		exited: make(chan struct{}),
		codec:  codec, encW: encW, encH: encH, encodeFps: encodeFps,
		startedAt:             time.Now(),
		pendingKeyframeReason: reason,
		subs:                  make(map[*streamSub]struct{}),
	}
}

// stop tears a run's ffmpeg down and waits for its pumps to finish.
//
// The wait is what makes a restart safe: a subscriber must never receive a
// packet from the stream it is being reset away from. It must not happen under
// the hub mutex, or the pumps would deadlock trying to take it on their way
// out.
func (h *streamHub) stop(run *streamRun) {
	close(run.stop)
	_ = run.stdin.Close()
	if run.cmd.Process != nil {
		_ = run.cmd.Process.Kill()
	}
	if run.started {
		select {
		case <-run.exited:
		case <-time.After(2 * time.Second):
			log.Printf("ws: encoder pumps did not exit in time")
		}
	}
	// cmd.Wait is called here and only here — a Cmd must be waited on exactly
	// once, and this is after the stdout pump has drained (Wait closes the
	// pipe the pump reads, which would otherwise race it).
	if err := run.cmd.Wait(); err != nil {
		log.Printf("encoder: ffmpeg exited: %v", err)
	}
}

// startPumps launches the run's two pumps. It must be called under the hub
// mutex so that no packet can be broadcast before the subscribers registered
// by the caller have received their control messages.
func (r *streamRun) startPumps() {
	r.started = true
	var wg sync.WaitGroup
	wg.Add(2)
	go r.pumpFrames(&wg)
	go r.pumpOutput(&wg)
	go func() {
		wg.Wait()
		close(r.exited)
	}()
}

// frameMatchesRun reports whether a composed surface of (stride, height) is the
// geometry this run's encoder was started with. The pump must never feed a
// frame of any other shape: ffmpeg's raw input has a fixed row length, so a
// wider or taller frame shifts every row after the first and corrupts the
// encode for the rest of the run's life.
func frameMatchesRun(stride, height uint32, key runKey) bool {
	return int(stride) == key.width*4 && int(height) == key.height
}

// pumpFrames feeds the run's ffmpeg at the encode rate, from the broadcaster's
// composed surface. Damage updates are merged centrally, so this side only has
// to copy the finished image and hand it over. Snapshot buffers are recycled,
// so the steady state allocates nothing per frame.
func (r *streamRun) pumpFrames(wg *sync.WaitGroup) {
	defer wg.Done()
	defer r.stdin.Close()

	frames := r.bc.Subscribe()
	defer r.bc.Unsubscribe(frames)

	interval := time.Second / time.Duration(r.encodeFps)
	if interval <= 0 {
		interval = time.Second / 10
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	toWrite := make(chan []byte, 1)
	// Two buffers are enough: one is being written to ffmpeg while the other
	// is filled from the surface.
	spare := make(chan []byte, 2)

	go func() {
		for {
			select {
			case <-r.stop:
				return
			case frame := <-toWrite:
				if _, werr := r.stdin.Write(frame); werr != nil {
					return
				}
				select {
				case spare <- frame:
				default:
				}
			}
		}
	}()

	var surfaceSize int
	var fedOnce bool
	var lastChangeSeq uint64
	var lastFeedAt time.Time
	// Set when the last feed carried a real change. The fMP4 muxer only closes
	// a fragment when the NEXT packet arrives, so on a quiet page a single
	// change would sit unflushed for the whole keepalive window (2s of dead
	// display latency for MSE viewers). A short follow-up window feeds one
	// duplicate frame right after a change, pushing the fragment out.
	lastFedChange := false
	enqueue := func() {
		// Change-driven suppression: skip this tick entirely while the surface
		// has not changed since the last feed and the keepalive window has not
		// elapsed. Never while a viewer is waiting for an IDR — see
		// anyAwaitingKey.
		if r.key.keepalive > 0 && fedOnce && !r.anyAwaitingKey() {
			seq, _ := r.bc.ChangeState()
			window := time.Duration(r.key.keepalive) * time.Millisecond
			if lastFedChange {
				window = muxFlushFollowUpMs * time.Millisecond
			}
			if seq == lastChangeSeq && time.Since(lastFeedAt) < window {
				r.skippedTicks.Add(1)
				return
			}
		}
		// Re-arm the buffers when the surface geometry changes (first frame,
		// resize, producer restart).
		if stride, height, ok := r.bc.SurfaceDims(); ok {
			// The encoder's input geometry is fixed in its argv. Writing a frame
			// of any other shape shifts every row after the first in the raw
			// BGRA stream, which corrupts the encode for the rest of the run's
			// life — so wait for the producer to reach this run's geometry. A
			// viewport resize re-keys the run, and the runner's first frame at
			// the new size is a full repaint, so the wait is a frame or two.
			if !frameMatchesRun(stride, height, r.key) {
				if !r.geometryWaitLogged {
					r.geometryWaitLogged = true
					log.Printf("ws: waiting for producer geometry %dx%d (surface is %dx%d)",
						r.key.width, r.key.height, int(stride)/4, height)
				}
				r.skippedTicks.Add(1)
				return
			}
			if want := int(stride) * int(height); want != surfaceSize {
				surfaceSize = want
				// Buffers sized for the old geometry are unusable.
			drainSpare:
				for {
					select {
					case <-spare:
					default:
						break drainSpare
					}
				}
				spare <- make([]byte, want)
				spare <- make([]byte, want)
				log.Printf("ws: surface %dx%d (bytes=%d)", stride/4, height, want)
			}
		} else {
			return
		}

		var buf []byte
		select {
		case buf = <-spare:
		case <-r.stop:
			return
		default:
			// ffmpeg is behind on both buffers; skipping this tick keeps
			// latency down and the next snapshot is fresher.
			return
		}
		// Read the change sequence before snapshotting: a change that lands
		// mid-snapshot then advances it past this value and is re-fed on the
		// next tick rather than swallowed.
		seqNow, _ := r.bc.ChangeState()
		if !r.bc.Snapshot(buf) {
			select {
			case spare <- buf:
			default:
			}
			return
		}
		// Frame-level watermark: applied to the full fed frame so it cannot
		// be lost to change-driven suppression between ticks.
		if watermarkText != "" {
			if wStride, wHeight, ok := r.bc.SurfaceDims(); ok {
				stampWatermark(buf, wStride, wHeight)
			}
		}
		select {
		case toWrite <- buf:
			lastFedChange = seqNow != lastChangeSeq
			lastChangeSeq = seqNow
			lastFeedAt = time.Now()
			fedOnce = true
		case <-r.stop:
		}
	}

	for {
		select {
		case <-r.stop:
			return
		case _, ok := <-frames:
			if !ok {
				// The broadcaster is gone (container shutdown): stop feeding
				// ffmpeg; the session is being torn down with it.
				return
			}
			// Edge-triggered feed: fresh damage goes to the encoder NOW when
			// the encode rate allows, instead of waiting for the next tick —
			// the first change after a quiet period otherwise waits up to a
			// full tick (40ms at 25fps) before it is even fed. The suppression
			// logic inside enqueue() still applies, so static pages stay
			// cheap, and the interval guard keeps the CFR rate honest.
			if !fedOnce || time.Since(lastFeedAt) >= interval {
				enqueue()
			}
		case <-ticker.C:
			enqueue()
		}
	}
}

// pumpOutput turns ffmpeg's stdout into packed packets and fans them out to
// the run's subscribers: fMP4 fragments for MSE clients, Annex-B access units
// (marker 0x02) for WebCodecs.
func (r *streamRun) pumpOutput(wg *sync.WaitGroup) {
	defer wg.Done()

	reader := newFMP4Reader()
	splitter := &auSplitter{}
	first := true
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-r.stop:
			return
		default:
		}
		n, rerr := r.stdout.Read(buf)
		if n > 0 {
			if r.key.rawH264 {
				recordRawRun() // no-op when not recording
				for _, au := range splitter.feed(buf[:n]) {
					if au.keyframe {
						r.recordKeyframe(time.Now())
					}
					r.deliver(packAccessUnit(au.data, r.seq.Add(1), au.keyframe), au.keyframe, false)
				}
			} else {
				for _, seg := range reader.Feed(buf[:n]) {
					isKey := isKeyframeFragment(seg)
					if isKey {
						r.recordKeyframe(time.Now())
					}
					if rec.active || recordingActive.Load() {
						recordSegment(seg, first) // raw fMP4 segment tee
					}
					r.deliver(packVideoSegment(seg, r.seq.Add(1), isKey), isKey, first)
					first = false
				}
			}
		}
		if rerr != nil {
			// EOF after a stop/kill is normal; anything else is ffmpeg dying
			// mid-session.
			select {
			case <-r.stop:
			default:
				log.Printf("ws: ffmpeg stdout read error: %v", rerr)
			}
			return
		}
	}
}

// deliver fans one packed packet out to every subscriber of the run, caching
// the run's init segment (the first fragment ffmpeg emits) so that a viewer
// arriving later can be handed it.
func (r *streamRun) deliver(pkt []byte, isKey, isInit bool) {
	r.hub.mu.Lock()
	defer r.hub.mu.Unlock()
	if isInit {
		r.initSeg = pkt
	}
	if isKey && !isInit {
		r.lastKeyPkt = pkt
	}
	if r.stopping {
		return
	}
	for s := range r.subs {
		if isInit {
			// The init segment is a precondition of decoding, not a frame:
			// a viewer waiting for its first IDR still needs it.
			s.push(pkt)
			continue
		}
		if isKey {
			s.awaitingKey = false
		} else if s.awaitingKey {
			s.dropped.Add(1)
			continue
		}
		s.push(pkt)
	}
}

// anyAwaitingKey reports whether any subscriber is still waiting for an IDR.
// The frame pump uses it to suspend change-driven suppression: libx264 counts
// keyint in encoded frames, so an idle page whose frames are suppressed would
// otherwise stretch a late joiner's wait for a keyframe far past the GOP
// interval. Feeding at the full rate until the keyframe lands bounds that wait
// at the GOP, and costs nothing on a page that is actually changing.
func (r *streamRun) anyAwaitingKey() bool {
	r.hub.mu.Lock()
	defer r.hub.mu.Unlock()
	for s := range r.subs {
		if s.awaitingKey {
			return true
		}
	}
	return false
}

// keyframeReasonFor names the reason a run's first IDR will be attributed to.
func keyframeReasonFor(resetReason string) string {
	if resetReason == "" {
		return "initial"
	}
	return resetReason
}
