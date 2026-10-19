const { test } = require('node:test');
const assert = require('node:assert/strict');
const { isEmptyCaptureExpected, shouldStopCaptureFallback, pruneEmptyCaptureTimes } = require('../src/capture_fallback');

const RECOVER_MS = 1500;

// A blank surface that has finished loading has nothing to paint, so Chromium's
// zero-length copy-output for it is expected — not the compositor stopping. This
// is the regression guard for a fresh about:blank tab reporting "capture stalled
// — recovering" and recreating its renderer in a loop.
test('an empty capture on a loaded blank surface is expected, not a stall', () => {
  assert.equal(isEmptyCaptureExpected({ surfaceBlank: true, loading: false }), true);
});

test('an empty capture is a stall signal once there is content on the surface', () => {
  assert.equal(isEmptyCaptureExpected({ surfaceBlank: false, loading: false }), false);
});

test('an empty capture during a load is still the real signal', () => {
  // The tab is about to paint; the load grace window covers this, so the empty
  // is not waved away just because the surface is blank.
  assert.equal(isEmptyCaptureExpected({ surfaceBlank: true, loading: true }), false);
  assert.equal(isEmptyCaptureExpected({ surfaceBlank: false, loading: true }), false);
});

test('stays on fallback while empty captures keep arriving', () => {
  const now = 10_000;
  const lastEmptyCaptureAt = now - 200;
  assert.equal(
    shouldStopCaptureFallback({ now, lastEmptyCaptureAt, recoverMs: RECOVER_MS }),
    false,
  );
});

test('stops fallback after recover window with no new empties', () => {
  const now = 10_000;
  const lastEmptyCaptureAt = now - RECOVER_MS;
  assert.equal(
    shouldStopCaptureFallback({ now, lastEmptyCaptureAt, recoverMs: RECOVER_MS }),
    true,
  );
});

test('does not stop when no empty capture was recorded', () => {
  assert.equal(
    shouldStopCaptureFallback({ now: 10_000, lastEmptyCaptureAt: 0, recoverMs: RECOVER_MS }),
    false,
  );
});

test('ages empty captures out of the window without a new empty', () => {
  const times = [0, 1000, 9000];
  const left = pruneEmptyCaptureTimes(times, 12_000, 10_000);
  assert.equal(left, 1);
  assert.deepEqual(times, [9000]);
  assert.equal(pruneEmptyCaptureTimes(times, 20_000, 10_000), 0);
  assert.deepEqual(times, []);
});

test('recent dedup paints do not imply recovery (no lastGoodPaintAt input)', () => {
  // Simulates keepalive dedup refreshing lastGoodPaintAt every 100ms while
  // copy-output still fails — only lastEmptyCaptureAt matters.
  const now = 10_000;
  const lastEmptyCaptureAt = now - 100;
  assert.equal(
    shouldStopCaptureFallback({ now, lastEmptyCaptureAt, recoverMs: RECOVER_MS }),
    false,
  );
});
