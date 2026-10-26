'use strict';

// Cost-control policy for the offscreen runner, kept free of Electron imports so
// the rules are pinned by test/render_policy.test.js — the same reason
// capture_fallback.js exists as its own module.
//
// Two rules live here:
//
//  1. frameRateFor — an offscreen BrowserWindow is composited at whatever
//     webContents.setFrameRate() was last given, and backgroundThrottling is off
//     for streamed windows. An inactive tab therefore keeps painting at the
//     capture rate unless someone lowers it. Measured on about.gitlab.com: a
//     background tab running a full-viewport WebGL hero kept a software-GL
//     renderer busy for a viewer that could not see it.
//
//  2. shouldAutoRecover — replacing a wedged renderer reloads the page. When a
//     page wedges every fresh renderer (heavy software-GL content does), that
//     becomes a reload loop that costs the page's state and never converges.
//     Automatic recovery therefore gets a budget; the manual recover action is
//     never limited by it.

const DEFAULT_BACKGROUND_FPS = 1;
const DEFAULT_RECOVER_MAX = 3;
const DEFAULT_RECOVER_WINDOW_MS = 10 * 60 * 1000;
const DEFAULT_SAME_URL_WINDOW_MS = 10 * 60 * 1000;
const DEFAULT_LOAD_CAPTURE_DELAYS_MS = [1200, 5000];

// frameRateFor returns the compositor frame rate a tab should run at now.
//
// Only the active tab of an attached viewer is worth the capture rate. A tab
// that is inactive, or a session nobody is watching (capture paused), is
// composited at the background rate instead: the picture cannot reach anyone,
// and on a software-GL page every frame is real CPU.
//
// backgroundFps <= 0 disables the throttle (escape hatch to the previous
// behaviour: every tab at the capture rate).
function frameRateFor({ base, active, captureActive, backgroundFps = DEFAULT_BACKGROUND_FPS }) {
  const rate = Number.isFinite(base) && base > 0 ? base : 30;
  if (!(Number.isFinite(backgroundFps) && backgroundFps > 0)) return rate;
  if (active === true && captureActive === true) return rate;
  return Math.max(1, Math.min(backgroundFps, rate));
}

// shouldAutoRecover decides whether one more automatic renderer replacement is
// worth a reload. Manual recover does not consult this.
//
//   cooldown  — at least this long since ANY recovery (the runner's pacing rule)
//   window    — at most maxPerWindow automatic recoveries in windowMs
//   same-url  — at most one per URL in sameUrlWindowMs: a page that has already
//               wedged a brand-new renderer is the page's work, and another
//               reload is what turns it into a loop
//
// history entries are { url, at } in milliseconds; a missing/blank url only
// counts against the window rule.
function shouldAutoRecover({
  url,
  now,
  history = [],
  cooldownMs = 0,
  lastRecoverAt = 0,
  maxPerWindow = DEFAULT_RECOVER_MAX,
  windowMs = DEFAULT_RECOVER_WINDOW_MS,
  sameUrlWindowMs = DEFAULT_SAME_URL_WINDOW_MS,
} = {}) {
  const at = Number.isFinite(now) ? now : 0;
  if (cooldownMs > 0 && lastRecoverAt > 0 && at - lastRecoverAt < cooldownMs) {
    return { allow: false, reason: 'cooldown' };
  }
  const recent = (Array.isArray(history) ? history : []).filter(
    (entry) => entry && Number.isFinite(entry.at) && at - entry.at < windowMs,
  );
  if (maxPerWindow > 0 && recent.length >= maxPerWindow) {
    return { allow: false, reason: 'window' };
  }
  if (
    typeof url === 'string' &&
    url !== '' &&
    sameUrlWindowMs > 0 &&
    recent.some((entry) => entry.url === url && at - entry.at < sameUrlWindowMs)
  ) {
    return { allow: false, reason: 'same-url' };
  }
  return { allow: true, reason: 'ok' };
}

// loadCaptureDelays returns the post-load capturePage() offsets to schedule.
//
// Each one is a full-surface ReadPixels — the expensive path on a software-GL
// page, and the only place Chromium itself complains ("GPU stall due to
// ReadPixels"). The series exists because offscreen paint misses GPU-layer
// hero/background images that arrive after did-finish-load, so the LAST offset
// is always kept; the earlier ones are only worth a readback while this
// document has not put a full frame on the wire yet.
function loadCaptureDelays({
  configured = DEFAULT_LOAD_CAPTURE_DELAYS_MS,
  fullFrameSent = false,
} = {}) {
  const delays = (Array.isArray(configured) ? configured : [])
    // null/undefined/booleans must not become 0 via Number(): a zero delay is a
    // valid "capture immediately", a missing one is not a delay at all.
    .map((ms) => (typeof ms === 'number' ? ms : typeof ms === 'string' ? Number(ms.trim()) : NaN))
    .filter((ms) => Number.isFinite(ms) && ms >= 0)
    .sort((a, b) => a - b);
  if (delays.length === 0) return DEFAULT_LOAD_CAPTURE_DELAYS_MS.slice();
  if (fullFrameSent !== true) return delays;
  return [delays[delays.length - 1]];
}

// parseDelayList reads ISOLA_LOAD_CAPTURE_DELAYS_MS ("1200,5000"). An empty or
// unusable value keeps the default rather than silently disabling load captures.
function parseDelayList(raw, fallback = DEFAULT_LOAD_CAPTURE_DELAYS_MS) {
  if (raw === undefined || raw === null || String(raw).trim() === '') {
    return fallback.slice();
  }
  const parsed = String(raw)
    .split(',')
    .map((part) => part.trim())
    .filter((part) => part !== '')
    .map((part) => Number(part))
    .filter((ms) => Number.isFinite(ms) && ms >= 0);
  return parsed.length > 0 ? parsed : fallback.slice();
}

module.exports = {
  DEFAULT_BACKGROUND_FPS,
  DEFAULT_RECOVER_MAX,
  DEFAULT_RECOVER_WINDOW_MS,
  DEFAULT_SAME_URL_WINDOW_MS,
  DEFAULT_LOAD_CAPTURE_DELAYS_MS,
  frameRateFor,
  shouldAutoRecover,
  loadCaptureDelays,
  parseDelayList,
};
