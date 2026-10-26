'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');

const {
  frameRateFor,
  shouldAutoRecover,
  loadCaptureDelays,
  parseDelayList,
  DEFAULT_LOAD_CAPTURE_DELAYS_MS,
} = require('../src/render_policy');

// ── frameRateFor ─────────────────────────────────────────────────────────────

test('the active tab of an attached viewer keeps the capture rate', () => {
  assert.equal(frameRateFor({ base: 25, active: true, captureActive: true }), 25);
});

test('an inactive tab is composited at the background rate', () => {
  // This is the whole point: an offscreen window keeps being composited, so a
  // background WebGL page would otherwise burn a software-GL renderer at full
  // capture rate for nobody.
  assert.equal(
    frameRateFor({ base: 25, active: false, captureActive: true, backgroundFps: 1 }),
    1,
  );
});

test('a session nobody is watching drops even the active tab', () => {
  assert.equal(
    frameRateFor({ base: 25, active: true, captureActive: false, backgroundFps: 1 }),
    1,
  );
});

test('background rate is clamped to the capture rate and never below 1', () => {
  assert.equal(
    frameRateFor({ base: 5, active: false, captureActive: true, backgroundFps: 10 }),
    5,
  );
  assert.equal(
    frameRateFor({ base: 25, active: false, captureActive: true, backgroundFps: 0.2 }),
    1,
  );
});

test('a non-positive background rate disables the throttle', () => {
  for (const backgroundFps of [0, -1]) {
    assert.equal(
      frameRateFor({ base: 25, active: false, captureActive: false, backgroundFps }),
      25,
      `backgroundFps=${backgroundFps} must keep every tab at the capture rate`,
    );
  }
});

test('an unusable base rate falls back to 30 rather than to zero', () => {
  assert.equal(frameRateFor({ base: NaN, active: true, captureActive: true }), 30);
  assert.equal(frameRateFor({ base: 0, active: true, captureActive: true }), 30);
});

// ── shouldAutoRecover ────────────────────────────────────────────────────────

const NOW = 1_800_000_000_000;
const URL = 'https://about.gitlab.com/';

function decide(overrides = {}) {
  return shouldAutoRecover({
    url: URL,
    now: NOW,
    history: [],
    cooldownMs: 0,
    lastRecoverAt: 0,
    maxPerWindow: 3,
    windowMs: 10 * 60_000,
    sameUrlWindowMs: 10 * 60_000,
    ...overrides,
  });
}

test('an empty history allows one automatic recovery', () => {
  assert.deepEqual(decide(), { allow: true, reason: 'ok' });
});

test('the cooldown still paces every recovery', () => {
  assert.deepEqual(
    decide({ cooldownMs: 60_000, lastRecoverAt: NOW - 30_000 }),
    { allow: false, reason: 'cooldown' },
  );
  assert.deepEqual(
    decide({ cooldownMs: 60_000, lastRecoverAt: NOW - 90_000 }),
    { allow: true, reason: 'ok' },
  );
});

test('a page that already wedged a fresh renderer is not reloaded again', () => {
  // The regression guard for the reload loop: three recoveries in 100s, all on
  // the same URL, each followed by the same wedge.
  const history = [
    { url: URL, at: NOW - 100_000 },
    { url: URL, at: NOW - 70_000 },
  ];
  assert.deepEqual(decide({ history }), { allow: false, reason: 'same-url' });
});

test('a different URL gets its own chance', () => {
  const history = [{ url: URL, at: NOW - 100_000 }];
  assert.deepEqual(
    decide({ url: 'https://example.com/', history }),
    { allow: true, reason: 'ok' },
  );
});

test('the per-window cap bounds recoveries across URLs', () => {
  const history = [
    { url: 'https://a.example/', at: NOW - 10_000 },
    { url: 'https://b.example/', at: NOW - 20_000 },
    { url: 'https://c.example/', at: NOW - 30_000 },
  ];
  assert.deepEqual(decide({ history }), { allow: false, reason: 'window' });
});

test('entries older than the window no longer count', () => {
  const history = [
    { url: URL, at: NOW - 11 * 60_000 },
    { url: 'https://b.example/', at: NOW - 12 * 60_000 },
    { url: 'https://c.example/', at: NOW - 13 * 60_000 },
  ];
  assert.deepEqual(decide({ history }), { allow: true, reason: 'ok' });
});

test('a zero budget disables that rule, not every recovery', () => {
  const history = [{ url: URL, at: NOW - 1000 }];
  assert.deepEqual(
    decide({ history, maxPerWindow: 0, sameUrlWindowMs: 0 }),
    { allow: true, reason: 'ok' },
  );
});

test('an empty URL only counts against the window rule', () => {
  const history = [{ url: URL, at: NOW - 1000 }];
  assert.deepEqual(decide({ url: '', history }), { allow: true, reason: 'ok' });
});

// ── loadCaptureDelays ────────────────────────────────────────────────────────

test('before this document has sent a frame, every load capture runs', () => {
  assert.deepEqual(
    loadCaptureDelays({ configured: [250, 600, 1200, 2500, 5000], fullFrameSent: false }),
    [250, 600, 1200, 2500, 5000],
  );
});

test('once a full frame is on the wire only the late capture survives', () => {
  // The late readback is what catches GPU-layer hero/background images; the
  // early ones would only re-read a surface the viewer already has.
  assert.deepEqual(
    loadCaptureDelays({ configured: [250, 600, 1200, 2500, 5000], fullFrameSent: true }),
    [5000],
  );
});

test('the late capture is kept for every configuration', () => {
  for (const configured of [[10], [10, 20], [5000, 100]]) {
    const delays = loadCaptureDelays({ configured, fullFrameSent: true });
    assert.equal(delays.length, 1);
    assert.equal(delays[0], Math.max(...configured));
  }
});

test('unusable delay entries are dropped, and a fully unusable list falls back', () => {
  assert.deepEqual(
    loadCaptureDelays({ configured: [1200, 'x', -5, null, 5000], fullFrameSent: false }),
    [1200, 5000],
  );
  assert.deepEqual(
    loadCaptureDelays({ configured: [], fullFrameSent: false }),
    DEFAULT_LOAD_CAPTURE_DELAYS_MS,
  );
});

test('parseDelayList reads the switch and keeps the default for junk', () => {
  assert.deepEqual(parseDelayList('1000, 4000'), [1000, 4000]);
  assert.deepEqual(parseDelayList(''), DEFAULT_LOAD_CAPTURE_DELAYS_MS);
  assert.deepEqual(parseDelayList(undefined), DEFAULT_LOAD_CAPTURE_DELAYS_MS);
  assert.deepEqual(parseDelayList('a,b'), DEFAULT_LOAD_CAPTURE_DELAYS_MS);
});
