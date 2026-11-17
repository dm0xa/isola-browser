package main

import (
	"io"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// runKey is the encoder's full argv input; sharing a run requires every field to match.
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

// muxFlushFollowUpMs: duplicate frame after a real change so the fMP4 muxer closes its fragment now, not at the 2s keepalive.
const muxFlushFollowUpMs = 40

// streamEvent is one item in a viewer's outbound mailbox: either a control
// message (stream_init/stream_reset) or a packed video packet.
type streamEvent struct {
	binary []byte
	text   map[string]any
}

// mailboxSize bounds a viewer's outbound queue; a full mailbox means that viewer resyncs, the encoder never blocks.
const mailboxSize = 192

// streamSub is one viewer's FIFO mailbox. ABR re-points it at another run, so control
// messages keep order with the video they replace. Hub mutex guards all fields but sentBytes.
type streamSub struct {
	ch  chan streamEvent
	run *streamRun

	// awaitingKey drops video packets until the next keyframe. Set when a
	// subscription starts — a client must never receive a delta frame whose
	// reference it never got — and again when its mailbox overflows, since the
	// packets that bridged the gap are gone and only an IDR can resync it.
	awaitingKey bool

	// dropped counts packets shed by this viewer for either of the two
	// reasons below. It stays the published total, so an operator reading one
	// number keeps the old meaning; the split says which of the two it is.
	dropped atomic.Int64
	// dropOverflow counts packets shed because the mailbox was full: this
	// viewer's socket is not draining at the rate the encoder produces, and
	// everything until the next IDR is undecodable for it.
	dropOverflow atomic.Int64
	// dropAwaitingKey counts packets shed because the viewer had no picture
	// yet — it joined (or resynced) and these were deltas whose references it
	// never received. This is expected for the first GOP of a late join and
	// says nothing about the socket.
	dropAwaitingKey atomic.Int64
	// sentBytes counts video bytes actually handed to this viewer's socket.
	// The client's ABR compares its arrival rate against this rather than
	// against the bitrate target, so it must be per-viewer: two viewers of the
	// same shared run need not receive the same bytes.
	sentBytes atomic.Int64

	// keyframeReqAt is when this viewer last forced an encoder restart, and
	// keyframeReqs how many it has forced over the life of the connection.
	// Together they enforce the per-viewer keyframe budget; both are guarded
	// by the hub mutex.
	keyframeReqAt time.Time
	keyframeReqs  int
	// keyframeRetries counts the restarts this viewer forced while it was
	// still waiting for an IDR. A viewer with no picture is recovering, not
	// farming, so those come out of a small separate allowance that resets
	// when an IDR finally arrives.
	keyframeRetries int
}

func newStreamSub() *streamSub {
	return &streamSub{ch: make(chan streamEvent, mailboxSize)}
}

// noteDrop records a shed packet by cause; only overflow is a transport problem.
func (s *streamSub) noteDrop(overflow bool) {
	if overflow {
		s.dropOverflow.Add(1)
	} else {
		s.dropAwaitingKey.Add(1)
	}
	s.dropped.Add(1)
}

// push queues a packet; a slow viewer drops and resyncs at the next keyframe.
func (s *streamSub) push(pkt []byte) {
	select {
	case s.ch <- streamEvent{binary: pkt}:
	default:
		s.awaitingKey = true
		s.noteDrop(true)
	}
}

// enqueueControl queues control messages ahead of the replaced/introduced stream's video.
// Non-blocking: every caller holds the hub mutex.
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

// recordKeyframe marks an IDR produced by this run; the first consumes the startup reason.
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

// keyframeState reports the time and resolved reason of the last written IDR.
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

// initMessage describes the run; every attached viewer gets the same message.
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

// streamHub owns the session's encoder runs: one per process, created on demand, reaped with its last viewer.
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

// hubFor keys the hub on the broadcaster, retiring the previous session's encoders.
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

// maxConcurrentRuns: measured 720p/25fps three rungs = 0.77 core-s/s (3 encoders) vs
// 0.57 with this cap; a third encoder starves the frame pump in a 3-CPU container.
const maxConcurrentRuns = 2

// reusableRunLocked picks a live run want can decode when the session is at the cap.
// Container format and geometry must match; bitrate/scale may differ — attach follows
// with a stream_init carrying the run's real values. Most expensive run at or below
// want's bitrate, else the cheapest; sharing must never cost more bytes than the step reacted to.
func (h *streamHub) reusableRunLocked(want runKey) *streamRun {
	var atOrBelow, cheapest *streamRun
	for k, r := range h.runs {
		if r.stopping || !rungCompatible(k, want) {
			continue
		}
		if cheapest == nil || k.kbps < cheapest.key.kbps {
			cheapest = r
		}
		if k.kbps <= want.kbps && (atOrBelow == nil || k.kbps > atOrBelow.key.kbps) {
			atOrBelow = r
		}
	}
	if atOrBelow != nil {
		return atOrBelow
	}
	return cheapest
}

// rungCompatible: fps/keepalive/base bitrate are session properties; only bitrate and scale differ across rungs.
func rungCompatible(have, want runKey) bool {
	return have.rawH264 == want.rawH264 &&
		have.width == want.width && have.height == want.height &&
		have.fps == want.fps &&
		have.keepalive == want.keepalive &&
		have.baseKbps == want.baseKbps
}

// liveRunsAfterLocked excludes sub's own run: its last viewer's encoder is stopped by the very step being decided.
func (h *streamHub) liveRunsAfterLocked(sub *streamSub) int {
	n := len(h.runs)
	if old := sub.run; old != nil && old.started && !old.stopping && len(old.subs) == 1 {
		n--
	}
	return n
}

// attach points sub at key's run, starting one or joining a compatible run at the cap;
// the returned run's key may differ from key. nil only when the encoder failed to start.
func (h *streamHub) attach(sub *streamSub, key runKey, resetReason string) *streamRun {
	h.mu.Lock()

	run := h.runs[key]
	if run == nil && h.liveRunsAfterLocked(sub) >= maxConcurrentRuns {
		run = h.reusableRunLocked(key)
	}
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
		// would cost the viewer its decoder for nothing. This is also how a
		// capped step that maps back onto the viewer's own run is absorbed —
		// the viewer keeps decoding, and the client's next step re-evaluates.
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
	shared := run.key
	h.mu.Unlock()

	if !fresh && (shared.kbps != key.kbps || shared.scale != key.scale) {
		log.Printf("ws: run cap %d, sharing %dkbps/%.2f → %dkbps/%.2f",
			maxConcurrentRuns, key.kbps, key.scale, shared.kbps, shared.scale)
	}
	// Outside the lock: the producer gate reads the hub, and stopping the
	// orphan waits for its pumps, which take the hub mutex on the way out.
	syncProducerCapture(false)
	if orphan != nil {
		h.stop(orphan)
	}
	return run
}

// detachLocked unlinks sub; the caller stops the returned run when it was the last viewer.
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

// stats reports live runs and their viewers; the difference is what sharing bought.
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

// syncProducerCapture pauses capture when nobody watches: ~0.4 cores unwatched vs ~0.05 paused.
// force re-sends the state for a freshly connected ctrl socket.
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

// keyframeRestartGrace: restarting before the first IDR lands discards the in-flight one.
const keyframeRestartGrace = 4 * time.Second

// lateJoinRestartCooldown must cover the replacement encoder's first-IDR window.
const lateJoinRestartCooldown = keyframeRestartGrace + time.Second

// Per-viewer keyframe budget: the cooldown bounds a storm, not the total; a viewer gets
// keyframeRequestsPerConn per window per connection. Still-awaiting viewers spend the
// per-stall allowance instead; the budget resets once an IDR lands.
const (
	keyframeRequestWindow   = 30 * time.Second
	keyframeRequestsPerConn = 4
	keyframeRetriesPerStall = 3
)

// keyframeBudgetAllows reports whether this viewer may force another IDR; only a real restart spends budget.
func (s *streamSub) keyframeBudgetAllows(now time.Time) (bool, string) {
	if s.awaitingKey && s.keyframeRetries < keyframeRetriesPerStall {
		return true, ""
	}
	if s.keyframeReqs >= keyframeRequestsPerConn {
		return false, "connection budget exhausted"
	}
	if !s.keyframeReqAt.IsZero() && now.Sub(s.keyframeReqAt) < keyframeRequestWindow {
		return false, "per-viewer cooldown"
	}
	return true, ""
}

// noteKeyframeRequest records a forced restart; a still-pictureless retry uses the stall allowance.
func (s *streamSub) noteKeyframeRequest(now time.Time) {
	s.keyframeReqAt = now
	if s.awaitingKey {
		s.keyframeRetries++
		return
	}
	s.keyframeReqs++
}

// keyframeRestartAllowed: an IDR-emitting run restarts immediately; an IDR-less one only past the grace.
func keyframeRestartAllowed(startedAt time.Time, keyframeAtUnixNano int64, now time.Time) bool {
	if keyframeAtUnixNano != 0 {
		return true
	}
	if startedAt.IsZero() {
		return true
	}
	return now.Sub(startedAt) >= keyframeRestartGrace
}

// requestKeyframe restarts the run: libx264 on a live pipe cannot be signalled, so the
// next keyframe means a fresh encoder, resetting every viewer of the run.
func (h *streamHub) requestKeyframe(sub *streamSub) {
	h.mu.Lock()
	run := sub.run
	if run == nil || run.stopping {
		h.mu.Unlock()
		return
	}
	now := time.Now()
	if ok, reason := sub.keyframeBudgetAllows(now); !ok {
		h.mu.Unlock()
		log.Printf("ws: keyframe refused (%s) %d/%d", reason, sub.keyframeReqs, keyframeRequestsPerConn)
		return
	}
	if !keyframeRestartAllowed(run.startedAt, run.keyframeAt.Load(), now) {
		h.mu.Unlock()
		log.Printf("ws: keyframe refused (no idr) %d/%d", sub.keyframeReqs, keyframeRequestsPerConn)
		return
	}
	if !h.lastForcedKeyframe.IsZero() && now.Sub(h.lastForcedKeyframe) < lateJoinRestartCooldown {
		h.mu.Unlock()
		log.Printf("ws: keyframe refused (cooldown) viewer %d/%d", sub.keyframeReqs, keyframeRequestsPerConn)
		return
	}
	// Start the replacement first: if ffmpeg will not come up, the viewers keep
	// the run they already have instead of being left with nothing.
	fresh := h.spawnLocked(run.key, "requested")
	if fresh == nil {
		h.mu.Unlock()
		log.Printf("ws: keyframe restart %dkbps: spawn failed", run.key.kbps)
		return
	}
	h.lastForcedKeyframe = now
	sub.noteKeyframeRequest(now)
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
}

// resizeAll re-keys every run: output size is fixed in argv, so a resize is a replacement
// run per configuration. A failed start keeps the old geometry — a resize must never kill the session.
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
				log.Printf("ws: resize %dx%d failed (kbps=%d scale=%.2f), keeping old encoder",
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
		log.Printf("ws: re-keyed %dx%d (%d runs)", width, height, len(plan))
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

// spawnLocked starts ffmpeg without pumps so subscribers and their control messages are registered first.
func (h *streamHub) spawnLocked(key runKey, reason string) *streamRun {
	cmd, stdin, stdout, codec, encW, encH, encodeFps, err :=
		startEncoder(key.width, key.height, key.fps, key.kbps, key.scale, key.rawH264)
	if err != nil {
		log.Printf("ws: encoder: %v", err)
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

// stop tears the run down and waits for its pumps outside the hub mutex — a reset viewer
// must never receive a packet from the stream it was reset away from.
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
			log.Printf("ws: pump exit timeout")
		}
	}
	// cmd.Wait is called here and only here — a Cmd must be waited on exactly
	// once, and this is after the stdout pump has drained (Wait closes the
	// pipe the pump reads, which would otherwise race it).
	if err := run.cmd.Wait(); err != nil {
		log.Printf("encoder: ffmpeg exit: %v", err)
	}
}

// startPumps runs under the hub mutex: no packet broadcasts before registered subscribers get their control messages.
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

// frameMatchesRun: ffmpeg raw input has a fixed row length; any other shape corrupts the encode for the run's life.
func frameMatchesRun(stride, height uint32, key runKey) bool {
	return int(stride) == key.width*4 && int(height) == key.height
}

// pumpFrames copies the composed surface to ffmpeg at the encode rate; snapshot buffers are recycled, so steady state allocates nothing.
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
		// CFR ceiling: never feed two frames closer together than the encode
		// interval, whichever path called us. The ticker keeps the cadence on
		// its own, but the damage path can feed a few milliseconds before a
		// tick and the tick then feeds again, so the wall-clock rate follows
		// the producer instead of the encode rate (a 51 fps producer measured
		// 48 packets/s against a 25 fps ceiling). The mux-flush follow-up is
		// the one allowed to run closer, because the fMP4 muxer only closes a
		// fragment when the next frame arrives.
		if fedOnce {
			// A tenth of the interval of slack: a producer paced at exactly the
			// encode rate jitters below it, and a strict comparison threw those
			// frames away (1080p dropped from 25 to 17 fps against a realtime
			// feeder). The slack still forbids the double feed this guards.
			minGap := interval - interval/10
			if lastFedChange && muxFlushFollowUpMs*time.Millisecond < minGap {
				minGap = muxFlushFollowUpMs * time.Millisecond
			}
			if time.Since(lastFeedAt) < minGap {
				return
			}
		}
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
					log.Printf("ws: waiting geometry %dx%d (surface %dx%d)",
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

	// Both reassemblers read ffmpeg's stdout straight into their own buffer,
	// so each byte is copied once — into the packet the viewers receive —
	// instead of once on the read and again on the way out. The segments they
	// return alias those buffers and are packed here before the next read.
	reader := newFMP4Reader()
	splitter := &auSplitter{}
	first := true
	for {
		select {
		case <-r.stop:
			return
		default:
		}
		if r.key.rawH264 {
			recordRawRun() // no-op when not recording
			aus, rerr := splitter.readFrom(r.stdout)
			for _, au := range aus {
				if au.keyframe {
					r.recordKeyframe(time.Now())
				}
				r.deliver(packAccessUnit(au.data, r.seq.Add(1), au.keyframe), au.keyframe, false)
			}
			if rerr != nil {
				r.reportOutputError(rerr)
				return
			}
			continue
		}
		segs, rerr := reader.readSegments(r.stdout)
		for _, seg := range segs {
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
		if rerr != nil {
			r.reportOutputError(rerr)
			return
		}
	}
}

// reportOutputError logs a stdout read failure. EOF after a stop/kill is
// normal; anything else is ffmpeg dying mid-session.
func (r *streamRun) reportOutputError(err error) {
	select {
	case <-r.stop:
	default:
		log.Printf("ws: ffmpeg stdout: %v", err)
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
			// The stall is over: a later one gets its own retry allowance.
			s.keyframeRetries = 0
		} else if s.awaitingKey {
			s.noteDrop(false)
			continue
		}
		s.push(pkt)
	}
}

// anyAwaitingKey suspends change suppression so an idle page still feeds at full rate until the late joiner's keyframe lands.
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
