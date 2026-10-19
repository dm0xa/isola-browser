// Pure helpers for capturePage fallback — no Electron dependency (unit-tested).

/**
 * Whether an empty paint image is EXPECTED rather than a capture stall: the tab
 * has a blank surface and its document has finished loading, so there is
 * genuinely nothing to paint.
 *
 * Chromium's offscreen compositor hands back a zero-length copy-output for a
 * blank, unchanging document (an about:blank tab after a keepalive nudge is the
 * clearest case). Counting those as empties made a fresh tab raise "capture
 * stalled — recovering", start the screenshot fallback and — because
 * capturePage() comes back empty for the same reason — recreate its renderer in
 * a loop.
 *
 * The blank surface is what makes an empty image safe to ignore: there is no
 * picture to lose. Once content is on screen an empty image is the real stall
 * signal again, so this is not simply "the page is quiet".
 *
 * @param {{ surfaceBlank: boolean, loading: boolean }} opts
 * @returns {boolean}
 */
function isEmptyCaptureExpected({ surfaceBlank, loading }) {
  return Boolean(surfaceBlank && !loading);
}

/**
 * Whether to leave capturePage fallback because the paint copy-output path has
 * been quiet long enough (no empty captures in recoverMs).
 *
 * lastGoodPaintAt must NOT be used here: keepalive dedups refresh it every
 * 100ms even when copy-output is broken and the surface is frozen.
 *
 * @param {{ now: number, lastEmptyCaptureAt: number, recoverMs: number }} opts
 * @returns {boolean}
 */
function shouldStopCaptureFallback({ now, lastEmptyCaptureAt, recoverMs }) {
  if (!lastEmptyCaptureAt) return false;
  return now - lastEmptyCaptureAt >= recoverMs;
}

// Drop timestamps older than the rolling window. Must run on the stats
// interval too, not only when a new empty arrives: after paint recovers,
// nothing else calls this, and a frozen count keeps the viewer badge up.
function pruneEmptyCaptureTimes(times, now, windowMs) {
  const cutoff = now - windowMs;
  let drop = 0;
  while (drop < times.length && times[drop] < cutoff) drop += 1;
  if (drop > 0) times.splice(0, drop);
  return times.length;
}

module.exports = { isEmptyCaptureExpected, shouldStopCaptureFallback, pruneEmptyCaptureTimes };
