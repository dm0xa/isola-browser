'use strict';

// Cost-control policy, Electron-free so tests pin it. frameRateFor: inactive
// or unwatched tabs composite at the background rate (a background WebGL tab
// kept a software-GL renderer busy for nobody). shouldAutoRecover: automatic
// renderer replacement gets a budget; manual recover never consults it.

const DEFAULT_BACKGROUND_FPS = 1;
const DEFAULT_RECOVER_MAX = 3;
const DEFAULT_RECOVER_WINDOW_MS = 10 * 60 * 1000;
const DEFAULT_SAME_URL_WINDOW_MS = 10 * 60 * 1000;
const DEFAULT_LOAD_CAPTURE_DELAYS_MS = [1200, 5000];

// frameRateFor: only the active tab of an attached viewer gets the capture
// rate; backgroundFps <= 0 disables the throttle.
function frameRateFor({ base, active, captureActive, backgroundFps = DEFAULT_BACKGROUND_FPS }) {
  const rate = Number.isFinite(base) && base > 0 ? base : 30;
  if (!(Number.isFinite(backgroundFps) && backgroundFps > 0)) return rate;
  if (active === true && captureActive === true) return rate;
  return Math.max(1, Math.min(backgroundFps, rate));
}

// shouldAutoRecover: cooldown + per-window budget + one per URL — a page that
// already wedged a fresh renderer would turn further reloads into a loop.
// history entries are { url, at } ms; a blank url only counts against the window.
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

// loadCaptureDelays: post-load capturePage() readbacks (the expensive path —
// the only place Chromium logs a GPU stall). The last offset is always kept;
// earlier ones only before the document's first full frame.
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

// captureCommandEffect: the streamer re-sends state on every reconnect, so
// equal values are a deliberate "still the truth" — and must re-assert the
// compositor rate, or a stale background rate leaves the page at ~1 fps.
function captureCommandEffect({ on, wasActive, hasActiveTab }) {
  const active = on === true;
  return {
    resume: active && wasActive !== true,
    pause: !active && wasActive === true,
    reassertRate: active && hasActiveTab === true,
  };
}

// applyCaptureCommand: the wiring (not just the decision) is tested here;
// setActive must run before hooks that read the flag.
function applyCaptureCommand({
  on,
  wasActive,
  tab,
  setActive,
  resetFrameState,
  startForwarding,
  stopForwarding,
  setTabFrameRate,
  invalidate,
  captureAndSend,
  clearPartialTimer,
  clearLoadCaptures,
}) {
  const effect = captureCommandEffect({ on, wasActive, hasActiveTab: Boolean(tab) });
  if (effect.resume) {
    setActive(true);
    resetFrameState();
    if (tab) {
      startForwarding(tab);
      // Raise the rate before the seeding capture: a tab left at the
      // background rate could hand back a stale surface as the first frame.
      setTabFrameRate(tab, true);
      invalidate(tab);
    }
    captureAndSend('viewer-attach');
    return true;
  }
  if (effect.pause) {
    setActive(false);
    if (tab) {
      stopForwarding(tab);
      // Nobody is watching: composing this surface at the capture rate is waste.
      setTabFrameRate(tab, false);
      clearPartialTimer(tab);
      clearLoadCaptures();
    }
    return true;
  }
  // Same state as before: the command is a re-assertion, not a toggle. It must
  // still repair a compositor rate left behind at the background rate, or the
  // session stays at ~1 fps with "capture active" in every log line.
  if (effect.reassertRate) setTabFrameRate(tab, true);
  return false;
}

// reassertActiveRate: repairs rates lost without a capture transition (~1 fps freeze).
function reassertActiveRate({ captureActive, tab, setTabFrameRate }) {
  if (captureActive !== true || !tab) return false;
  setTabFrameRate(tab, true);
  return true;
}

module.exports = {
  DEFAULT_BACKGROUND_FPS,
  DEFAULT_RECOVER_MAX,
  DEFAULT_RECOVER_WINDOW_MS,
  DEFAULT_SAME_URL_WINDOW_MS,
  DEFAULT_LOAD_CAPTURE_DELAYS_MS,
  frameRateFor,
  captureCommandEffect,
  applyCaptureCommand,
  reassertActiveRate,
  shouldAutoRecover,
  loadCaptureDelays,
  parseDelayList,
};
