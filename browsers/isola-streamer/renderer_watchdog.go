package main

import (
	"errors"
	"log"
	"time"
)

// Renderer watchdog (streamer side).
//
// Why this is a CDP probe and not "frames stopped": a renderer whose main
// thread is blocked still lets the compositor hand out the last frame it
// produced — the process sits at ~90% CPU while the stream keeps re-composing
// that same picture, which the dedup layer drops, so the viewer sees a frozen
// page at ~1 fps. Measured on a deliberately wedged tab: paints never stopped,
// so a capture-based detector saw a perfectly healthy session. What a blocked
// renderer genuinely cannot do is answer CDP, because its main thread is the
// thing that is stuck.
//
// On a diagnosed stall this asks the runner (over the control socket) to
// recreate the tab's window, which is what actually replaces the renderer
// process. The runner announces the stall and the recovery to viewers; this
// watchdog decides *when* to ask, and feeds the probe-failure metric the
// toolbar reads.
const (
	rendererProbeTimeoutMS   = 2500
	rendererStallThreshold   = 3 // consecutive unanswered probes
	rendererStalledProbeSecs = 15
	rendererRecoverCooldown  = 60 * time.Second
	// A renderer is allowed to be busy: a long task or a GC pause blocks its
	// main thread for a second or two and recovers by itself. Recreating the
	// window (which reloads the page and loses its state) must be reserved for
	// a renderer that is gone, not one that is thinking — three unanswered
	// probes at ~3s apart is ~10s of continuous silence.
	rendererSettleAfterNavSecs     = 10
	rendererSettleAfterRecoverSecs = 20
	// Circuit breaker: if replacing the renderer did not stop the stalls, the
	// page's own work is the problem and another reload may be exactly what keeps
	// it busy (a heavy page re-runs everything on every load). After this many
	// recoveries inside the window, stop replacing — keep reporting the state so
	// the toolbar can offer the manual recover — until the window passes.
	rendererRecoverLoopWindow  = 180 * time.Second
	rendererRecoverMaxInWindow = 2
)

func startRendererWatchdog() {
	go func() {
		interval := time.Duration(parseIntEnv("ISOLA_RENDERER_PROBE_MS", 3000)) * time.Millisecond
		cooldown := rendererRecoverCooldown
		if v := parseIntEnv("ISOLA_RENDERER_RECOVER_COOLDOWN_MS", 0); v > 0 {
			cooldown = time.Duration(v) * time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		failStreak := 0
		var stalledAt time.Time
		var lastRequestAt time.Time
		var nextProbeAt time.Time
		var lastURL string
		requests := 0
		var recoverTimes []time.Time
		suppressedFor := time.Time{}
		var lastShapeLogAt time.Time

		for range ticker.C {
			// With no viewers nothing is displayed, so a stall cannot be seen
			// — and recreating a window would throw away the page's state for
			// nobody.
			if viewers, _ := activeHubStats(); viewers == 0 {
				failStreak = 0
				stalledAt = time.Time{}
				nextProbeAt = time.Time{}
				continue
			}
			// A navigation replaces the document: the old renderer's silence says
			// nothing about the new one, and a loading page legitimately blocks.
			if url := activeTabURL(); url != lastURL {
				lastURL = url
				failStreak = 0
				stalledAt = time.Time{}
				nextProbeAt = time.Now().Add(rendererSettleAfterNavSecs * time.Second)
				continue
			}
			if !nextProbeAt.IsZero() && time.Now().Before(nextProbeAt) {
				continue
			}
			// Not attached (startup, tab switch, reconnect) is not evidence of
			// a stall: skip without counting it as a failure.
			if cdpClient == nil || !cdpClient.Available() {
				nextProbeAt = time.Now().Add(interval)
				continue
			}
			nextProbeAt = time.Now().Add(interval)

			// The expression must be a STRING: evaluateStringTimeout parses
			// `{"result":{"value":<string>}}` strictly, and a bare "1" comes back
			// as a JSON number, which the parser (rightly) rejects — that bug made
			// every healthy renderer look blocked, and the watchdog dutifully
			// reloaded the page twice on a page that was fine.
			_, err := cdpClient.evaluateStringTimeout("'1'", rendererProbeTimeoutMS*time.Millisecond)
			if err != nil && errors.Is(err, errBadResultShape) {
				// The renderer ANSWERED, it just did not answer with a string.
				// That is a bug in this probe, not evidence about the page — so it
				// clears the streak and never triggers a recovery. (It did once:
				// probing with a bare "1" parses as a JSON number, and every
				// healthy renderer looked blocked.)
				noteProbeSuccess()
				failStreak = 0
				if time.Since(lastShapeLogAt) > time.Minute {
					lastShapeLogAt = time.Now()
					log.Printf("renderer: probe answered with a non-string result (%v) — fix the probe expression", err)
				}
				continue
			}
			if err == nil {
				noteProbeSuccess()
				if !stalledAt.IsZero() {
					log.Printf("renderer: responsive again after %v", time.Since(stalledAt).Round(time.Second))
					stalledAt = time.Time{}
				}
				failStreak = 0
				continue
			}

			noteProbeFailure()
			failStreak++
			if failStreak < rendererStallThreshold {
				continue
			}
			if stalledAt.IsZero() {
				stalledAt = time.Now()
				log.Printf("renderer: %d probes unanswered in a row (%v) — treating the renderer as blocked", failStreak, err)
			}
			// While the renderer stays blocked, probe on the slower cadence:
			// every probe is a Runtime.evaluate queued behind the stuck work.
			nextProbeAt = time.Now().Add(rendererStalledProbeSecs * time.Second)

			if !lastRequestAt.IsZero() && time.Since(lastRequestAt) < cooldown {
				continue
			}
			// Circuit breaker (see rendererRecoverLoopWindow): keep the state
			// visible, stop throwing the page away.
			recent := 0
			for _, at := range recoverTimes {
				if time.Since(at) < rendererRecoverLoopWindow {
					recent++
				}
			}
			if recent >= rendererRecoverMaxInWindow {
				if suppressedFor.IsZero() {
					suppressedFor = time.Now()
					log.Printf("renderer: %d recoveries in %v did not stop the stalls — leaving the page alone (manual recover still available)",
						recent, rendererRecoverLoopWindow)
				}
				continue
			}
			if rendererRecoverBlockedByCapture() {
				continue
			}

			lastRequestAt = time.Now()
			recoverTimes = append(recoverTimes, lastRequestAt)
			requests++
			// "auto" marks this as the watchdog's own request, so the runner applies
			// its automatic-recovery budget (per URL and per window). A viewer's
			// manual recover arrives without the flag and always recreates the
			// window. With the two budgets independent, a page that wedges every
			// fresh renderer got reloaded about once a minute instead of being left
			// alone after the first fresh renderer failed too.
			if !writeCtrlCommand(map[string]any{"cmd": "tab_recover", "auto": true}) {
				log.Printf("renderer: blocked, but the runner control socket is down — cannot request recovery")
				continue
			}
			log.Printf("renderer: asked the runner to recreate the tab window (attempt %d)", requests)
			// Give the reloaded page time to settle before judging it again.
			failStreak = 0
			stalledAt = time.Time{}
			nextProbeAt = time.Now().Add(rendererSettleAfterRecoverSecs * time.Second)
		}
	}()
}
