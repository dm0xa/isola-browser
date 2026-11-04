package main

import (
	"encoding/binary"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

func parseIntEnv(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultVal
}

// parseIntEnvAllowZero is parseIntEnv but accepts an explicit non-negative
// integer, including 0. A value of 0 is meaningful for
// ISOLA_STATIC_KEEPALIVE_MS, where it disables change-driven suppression and
// restores the unconditional CFR encode loop.
func parseIntEnvAllowZero(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return defaultVal
}

// handleViewerWS is the main session loop for one viewer WebSocket connection.
// It attaches the viewer to the session's shared encoder — one ffmpeg per
// distinct (bitrate, scale, mode) configuration, fanned out to every viewer
// using it — relays incoming JSON input events to CDP, and answers the client's
// ABR control messages (ping/pong RTT probes, set_bitrate, request_keyframe).
func handleViewerWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws: upgrade error: %v", err)
		return
	}
	defer conn.Close()

	// Capability negotiation via URL query (?mode=webcodecs), set by the
	// backend proxy from the client's handshake request. Negotiating on the
	// handshake (instead of a first WS message) avoids a read-deadline race:
	// a timed-out gorilla Read poisons the connection and would kill the
	// session for any client that doesn't announce itself.
	webcodecs := r.URL.Query().Get("mode") == "webcodecs"
	if webcodecs {
		log.Printf("ws: webcodecs client — raw annex-b mode")
	}

	currentMu.RLock()
	width := currentWidth
	height := currentHeight
	fps := currentFPS
	bitrate := currentBitrateKbps
	currentMu.RUnlock()

	// Fall back to env vars if config hasn't been set (e.g. in tests).
	if width == 0 {
		width = parseIntEnv("ISOLA_WIDTH", 1280)
	}
	if height == 0 {
		height = parseIntEnv("ISOLA_HEIGHT", 720)
	}
	if fps == 0 {
		fps = parseIntEnv("ISOLA_FPS", 30)
	}
	if bitrate == 0 {
		bitrate = bitrateKbpsForResolution(width, height)
	}
	// Change-driven suppression window: while the surface is unchanged the
	// encode loop skips ticks, but never for longer than this. 0 disables
	// suppression entirely (today's unconditional CFR loop).
	staticKeepaliveMS := parseIntEnvAllowZero("ISOLA_STATIC_KEEPALIVE_MS", 2000)
	// ABR ceiling: the bitrate this session actually starts with (falls back
	// to the resolution formula above), so a join-time bandwidth choice is
	// never exceeded by an adaptive step.
	baseKbps := bitrate

	// Capture the broadcaster in a local: session goroutines must never
	// dereference the package global (the resize/restart paths and tests
	// rebind it). The hub keys the session's encoder runs on it.
	bc := globalBroadcaster
	hub := hubFor(bc)

	done := make(chan struct{})
	var closeOnce sync.Once
	closeDone := func() { closeOnce.Do(func() { close(done) }) }
	defer closeDone()

	// gorilla/websocket allows ONE concurrent writer; video frames, metrics
	// JSON and audio packets all share this connection, so serialize them.
	var wmu sync.Mutex
	// 10s write deadline: a wedged client socket errors out and tears the
	// session down instead of blocking the audio/video fan-out forever.
	writeBinary := func(pkt []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(websocket.BinaryMessage, pkt)
	}
	writeText := func(v any) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(v)
	}
	// Audio packets flow only while the viewer has audio enabled server-side
	// (broadcastAudio already gates on the flag).
	unregisterAudio := registerAudioListener(func(pkt []byte) {
		_ = writeBinary(pkt)
	})
	defer unregisterAudio()

	sub := newStreamSub()
	// key is the complete description of the stream this viewer asked for.
	// Viewers whose keys match share one ffmpeg; a viewer that steps the ABR
	// ladder gets its own for the rung it moved to.
	key := runKey{
		kbps:      bitrate,
		scale:     1.0,
		rawH264:   webcodecs,
		width:     width,
		height:    height,
		fps:       fps,
		baseKbps:  baseKbps,
		keepalive: staticKeepaliveMS,
	}

	// Drain the mailbox: the hub only ever enqueues, so a viewer that stops
	// reading (or whose socket wedges) can never block the shared encoder.
	// Control messages and video share this one queue, which is what keeps
	// stream_reset/stream_init ordered against the video they replace.
	go func() {
		for {
			select {
			case <-done:
				return
			case ev := <-sub.ch:
				switch {
				case ev.binary != nil:
					if err := writeBinary(ev.binary); err != nil {
						log.Printf("ws: binary write error: %v", err)
						closeDone()
						return
					}
					sub.sentBytes.Add(int64(len(ev.binary)))
				case ev.text != nil:
					if err := writeText(ev.text); err != nil {
						log.Printf("ws: control write error: %v", err)
						closeDone()
						return
					}
				}
			}
		}
	}()

	// stream_init is the first message the client sees: attach queues it (and
	// the init segment, if the run already has one) before the pumps can
	// deliver anything.
	if hub.attach(sub, key, "") == nil {
		sendJSON(conn, map[string]any{"type": msgTypeStreamReset, "reason": "encoder_error"})
		return
	}
	defer hub.release(sub)

	// setBitrate re-points the viewer at the rung the ABR controller asked
	// for. libx264 CBR cannot re-target its bitrate live, so — exactly like
	// any other encoder restart — the client recovers from stream_reset +
	// stream_init.
	//
	// The viewer may land on a different rung than it asked for: when the
	// session is at maxConcurrentRuns, attach joins a compatible run instead
	// of starting a third encoder (stream_hub.go). The rung actually landed on
	// is whatever the returned run carries — the client reads it back from the
	// stream_init and metrics that follow, never from its own request.
	setBitrate := func(kbps int) {
		newKbps := clampAbrKbps(kbps, baseKbps)
		newScale := abrScaleForKbps(newKbps, baseKbps)
		cur := hub.runOf(sub)
		if cur != nil && newKbps == cur.key.kbps && newScale == cur.key.scale {
			return // duplicate request, or already at that rung
		}
		oldKbps := 0
		if cur != nil {
			oldKbps = cur.key.kbps
		}
		next := key
		next.kbps = newKbps
		next.scale = newScale
		landed := hub.attach(sub, next, "bitrate_change")
		if landed == nil {
			// The viewer keeps the rung it is already on.
			log.Printf("ws: encoder restart at %d kbps failed", newKbps)
			return
		}
		if landed == cur {
			// The cap mapped the request back onto the run this viewer is
			// already decoding, so nothing changed and nothing was announced.
			log.Printf("ws: bitrate %d → %d kbps asked, keeping the %d kbps run (run cap %d)",
				oldKbps, newKbps, landed.key.kbps, maxConcurrentRuns)
			return
		}
		// A shared run is logged by the hub, which knows both keys; this path
		// only has to report the step that actually happened.
		if landed.key.kbps != newKbps || landed.key.scale != newScale {
			return
		}
		log.Printf("ws: bitrate %d → %d kbps, scale %.2f (ABR)", oldKbps, newKbps, newScale)
	}

	// Goroutine C: periodic metrics for debug overlay (1 Hz). Read from the
	// run the viewer is currently attached to, so bitrate/fps/sequence stay
	// correct across ABR restarts.
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		// Egress metering state. sentBytes is the viewer's own socket total
		// and is not reset by an ABR step, so only the baseline moves.
		var (
			lastBytes int64
			lastAt    time.Time
		)

		// Damage metering: the same cumulative counters the debug endpoint
		// reports, sampled per second so a viewer can tell "the picture is
		// changing" from "the encoder is being fed the same frame again".
		var dmg damageMeter

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				run := hub.runOf(sub)
				if run == nil {
					continue
				}
				now := time.Now()
				sent := sub.sentBytes.Load()
				sentKbps := 0
				if !lastAt.IsZero() {
					if dt := now.Sub(lastAt).Seconds(); dt > 0 {
						sentKbps = int(float64(sent-lastBytes) * 8 / 1000 / dt)
					}
				}
				lastBytes, lastAt = sent, now

				rates := dmg.sample(bc.DamageCounters(), now)

				// Change-driven / ABR / keyframe observability: static time,
				// skipped ticks, encode scale and keyframe state.
				staticSecs := 0
				if _, lastChange := bc.ChangeState(); !lastChange.IsZero() {
					if d := now.Sub(lastChange); d > 0 {
						staticSecs = int(d.Seconds())
					}
				}
				kfAt, kfReason := run.keyframeState()
				kfAgeMS := -1
				if !kfAt.IsZero() {
					kfAgeMS = int(now.Sub(kfAt).Milliseconds())
				}
				// Renderer health: a wedged page renderer is the one cause of a
				// frozen stream that every other number here hides — static_secs and
				// a low fps look identical whether the page is idle or dead.
				probeStreak, probeFailAgoMS := rendererProbeHealth()
				stalled, stalledMs, stallRecoveries := rendererStallState()
				capHealth, capFresh := captureHealthSnapshot()
				_ = writeText(map[string]any{
					"type":            msgTypeMetrics,
					"input_backend":   effectiveInputBackend(),
					"input_target":    attachedTargetURL(),
					"input_target_ok": inputTargetOK(),
					"activity_mode":   getActivityMode(),
					"capture_fps":     fps,
					"encode_fps":      run.encodeFps,
					"bitrate_kbps":    run.key.kbps,
					"sent_kbps":       sentKbps,
					"width":           run.encW,
					"height":          run.encH,
					"encoder_seq":     run.seq.Load(),
					"encode_scale":    run.key.scale,
					"static_secs":     staticSecs,
					"skipped_ticks":   run.skippedTicks.Load(),
					// dropped counts this viewer's shed packets — mailbox
					// overflow and video discarded while waiting for a keyframe.
					// It is per-viewer, so a rising counter points at the one
					// socket that cannot keep up rather than at the encoder.
					"dropped": sub.dropped.Load(),
					// Did the picture actually change? skipped_ticks and a decoded
					// fps both stay high while the encoder is fed duplicate frames,
					// so changes_per_sec near zero with a healthy fps is the
					// signature of a frozen picture the other numbers cannot see.
					"changes_per_sec":       rates.ChangesPerSec,
					"full_frames_per_sec":   rates.FullPerSec,
					"damaged_pct":           rates.DamagedPct,
					"keyframe_reason":       kfReason,
					"keyframe_age_ms":       kfAgeMS,
					"renderer_stalled":      stalled,
					"renderer_stalled_ms":   stalledMs,
					"renderer_recoveries":   stallRecoveries,
					"cdp_probe_fail_streak": probeStreak,
					"cdp_probe_fail_ago_ms": probeFailAgoMS,
					// Capture path: empty paint images and the screenshot fallback.
					"capture_health_ok":     capFresh,
					"capture_empty_frames":  capHealth.EmptyTotal,
					"capture_empty_window":  capHealth.EmptyWindow,
					"capture_fallback":      capHealth.FallbackActive,
					"capture_last_paint_ms": capHealth.LastPaintMS,
					"capture_last_frame_ms": capHealth.LastFrameMS,
					// Wire accounting. capture_last_paint_ms is cleared by the 100ms
					// keepalive nudge whatever the page is doing, so on its own it
					// cannot tell a live picture from a repainted frozen one.
					// dedup_frames growing while sent_frames stalls is that state.
					"capture_last_sent_ms": capHealth.LastSentMS,
					"capture_sent_frames":  capHealth.SentTotal,
					"capture_dedup_frames": capHealth.DedupTotal,
				})
			}
		}
	}()

	// Goroutine D: WS text → control messages and input handler.
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			closeDone()
			return
		}
		if msgType != websocket.TextMessage || len(data) == 0 || data[0] != '{' {
			continue
		}
		var ctrl struct {
			Type string      `json:"type"`
			Ts   json.Number `json:"ts"`
			Kbps int         `json:"kbps"`
		}
		if json.Unmarshal(data, &ctrl) != nil {
			continue
		}
		switch ctrl.Type {
		case msgTypePing:
			// RTT probe: echo the client's timestamp verbatim so it can
			// measure video-channel latency (json.Number keeps the exact
			// literal — no float round-trip).
			if ctrl.Ts != "" {
				_ = writeText(map[string]any{"type": msgTypePong, "ts": ctrl.Ts})
			}
		case msgTypeSetBitrate:
			if ctrl.Kbps > 0 {
				setBitrate(ctrl.Kbps)
			}
		case msgTypeRequestKeyframe:
			// libx264 on a live pipe cannot be signalled for an IDR, so the
			// run behind this viewer is replaced; every viewer of it is reset
			// with it, because they all decode the same stream.
			hub.requestKeyframe(sub)
		default:
			handleInputEvent(data)
		}
	}
}

func sendJSON(conn *websocket.Conn, v any) {
	if err := conn.WriteJSON(v); err != nil {
		log.Printf("ws: writeJSON error: %v", err)
	}
}

// isKeyframeFragment checks if the fMP4 segment contains a keyframe by
// scanning the trun box sample flags recursively (moof / traf / trun may
// be nested as child boxes). Returns true if parsing fails (conservative).
func isKeyframeFragment(data []byte) bool {
	moof := findBoxDeep(data, "moof")
	if moof < 0 {
		return true // can't find moof, assume keyframe
	}
	traf := findBoxDeep(data[moof:], "traf")
	if traf < 0 {
		return true
	}
	traf += moof
	trun := findBoxDeep(data[traf:], "trun")
	if trun < 0 {
		return true
	}
	trun += traf

	if trun+16 > len(data) {
		return true
	}

	flags := binary.BigEndian.Uint32(data[trun+8 : trun+12])
	sampleCount := binary.BigEndian.Uint32(data[trun+12 : trun+16])

	hasFirstSampleFlags := flags&0x000004 != 0
	hasSampleFlags := flags&0x000400 != 0

	pos := trun + 16
	if hasFirstSampleFlags {
		if pos+4 > len(data) {
			return true
		}
		f := binary.BigEndian.Uint32(data[pos : pos+4])
		return isSyncSampleFlag(f)
	}

	if !hasSampleFlags {
		return false // fragment has delta frames; don't mark as IDR on wire
	}

	hasDuration := flags&0x000100 != 0
	hasSize := flags&0x000200 != 0
	hasCts := flags&0x000800 != 0

	entrySize := 0
	if hasDuration {
		entrySize += 4
	}
	if hasSize {
		entrySize += 4
	}
	if hasSampleFlags {
		entrySize += 4
	}
	if hasCts {
		entrySize += 4
	}
	if entrySize == 0 || int(sampleCount)*entrySize+pos > len(data) {
		return true
	}

	// Read first sample's sample-flags
	sampleFlagsOff := pos + entrySize - 4
	if sampleFlagsOff+4 > len(data) {
		return true
	}
	f := binary.BigEndian.Uint32(data[sampleFlagsOff : sampleFlagsOff+4])
	return isSyncSampleFlag(f)
}

// findBoxDeep searches for a box type recursively — it enters nested boxes.
// Returns the absolute offset in data, or -1 if not found.
func findBoxDeep(data []byte, boxType string) int {
	off := 0
	for off+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		if size < 8 || off+size > len(data) {
			return -1
		}
		if string(data[off+4:off+8]) == boxType {
			return off
		}
		// Recurse into the box content for child boxes
		if innerOff := findBoxDeep(data[off+8:off+size], boxType); innerOff >= 0 {
			return off + 8 + innerOff
		}
		off += size
	}
	return -1
}

// isSyncSampleFlag returns true if the sample-flags indicate a sync sample
// (bit 16 set, bit 12 clear). Per ISO 14496-12 §8.8.3.1.
func isSyncSampleFlag(f uint32) bool {
	const (
		sampleDependsOn = 0x03000000 // bits 24-25
		sampleIsNonSync = 0x00010000 // bit 16: 1 = non-sync
	)
	// If sample_depends_on == 2 (reserved), assume keyframe.
	if f&sampleDependsOn == 2<<24 {
		return true
	}
	// sample_is_non_sync_sample == 0 means keyframe
	return f&sampleIsNonSync == 0
}
