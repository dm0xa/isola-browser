// Electron offscreen runner: each tab is an offscreen BrowserWindow; the active
// one's paints are encoded (ISOL) and written to the frame UDS. A JSON-lines
// ctrl socket (ctrl.sock) takes tab commands from isola-streamer and broadcasts tab state.

// Runner renders arbitrary untrusted pages: wrapper CSP is meaningless, and the
// built-in warning would surface in the viewer's console via consoleAPICalled.
process.env.ELECTRON_DISABLE_SECURITY_WARNINGS = 'true';

const { app, BrowserWindow, nativeTheme, session } = require('electron');
const net = require('net');
const fs = require('fs');
const { encodeFrame, extractRect, clampRect, isBlankBitmap, shouldHoldBlank, surfaceBlankAfterFrame, frameTimestampNs, frameOutcome, FRAME_SENT, FRAME_DEDUP, FRAME_EMPTY, FRAME_IDLE } = require('./isol_protocol');
const { hardenCtrlSocket } = require('./ctrl_socket');
const { applyChromeFlags } = require('./chrome_flags');
const { isEmptyCaptureExpected, shouldStopCaptureFallback, pruneEmptyCaptureTimes } = require('./capture_fallback');
const {
  frameRateFor,
  applyCaptureCommand,
  reassertActiveRate,
  shouldAutoRecover,
  loadCaptureDelays,
  parseDelayList,
} = require('./render_policy');
const { applyTorProxy, isTorEnabled } = require('./tor');
const { isAllowedPageNavigation, resolveAddressBar } = require('./navigation_url');
const { shouldRetryLoad } = require('./load_retry');
const { themeColours, fillPageCanvas } = require('./theme');
const { applyUserAgent } = require('./user_agent');

const START_URL = process.env.START_URL || 'https://example.com';
const RESOLUTION = process.env.RESOLUTION || '1280x720';
const FPS = parseInt(process.env.ISOLA_FPS || '30', 10);
// Live resize bounds, mirroring the streamer's maxSurfaceDim and the 64px floor.
const MIN_DIM = 64;
const MAX_DIM = 8192;
// Compositor keepalive nudge: static pages must not repaint at the capture rate,
// and this is the worst-case delay before an idle page's click damage reaches the streamer.
const PRODUCER_KEEPALIVE_MS = Math.max(16, parseInt(process.env.ISOLA_PRODUCER_KEEPALIVE_MS || '100', 10) || 100);
const ISOLA_FRAME_SOCK = process.env.ISOLA_FRAME_SOCK || '/run/isola/frame.sock';
const ISOLA_CTRL_SOCK = process.env.ISOLA_CTRL_SOCK || '/run/isola/ctrl.sock';
const TOR_ENABLED = isTorEnabled(process.env.TOR_ENABLED);
const BROWSER_THEME = process.env.BROWSER_THEME || 'light';

// intEnv reads an integer switch, keeping 0 and negatives meaningful (unlike
// `parseInt(...) || default`): several knobs use 0 as "this rule is off".
function intEnv(name, fallback) {
  const raw = parseInt(process.env[name] || '', 10);
  return Number.isFinite(raw) ? raw : fallback;
}

let udsSocket = null;
let ctrlClients = [];
let frameSeq = 0;
let [w, h] = RESOLUTION.split('x').map(Number);
let frameRate = Math.max(1, Math.min(FPS, 60));
let frameBuffer = null;
// Last transmitted damage: rect plus pixels — only the same rect can dedup.
let lastSentPayload = null;
let lastSentRect = null;
// The streamer seeds its surface from full frames only; one is enough.
let hasSentFullFrame = false;
// NOT cleared by resetFrameState(): a seeded streamer must keep withholding blanks.
let streamerSeeded = false;
// Bounded on purpose: a page that really is one flat colour must still appear.
const BLANK_HOLD_MS = 3000;
// Below this share a blank rect is ordinary content; the measured tear was 40% of the page.
const BLANK_PATCH_MIN_SHARE = 0.2;
let blankHoldUntil = 0;
let bgColor = { r: 255, g: 255, b: 255, a: 255 };
// Viewer gate pushed by the streamer over ctrl: composing unwatched costs ~0.4 cores. Defaults true.
let captureActive = true;

// Renderer stall detection: compositor silence with a viewer attached is a
// reliable stall signal — the keepalive nudges guarantee frames on the active tab.
const STALL_MS = Math.max(2000, parseInt(process.env.ISOLA_STALL_MS || '8000', 10) || 8000);
// A freshly loaded document gets longer; crashing mid-load would be worse than waiting.
const STALL_LOAD_GRACE_MS = Math.max(STALL_MS, parseInt(process.env.ISOLA_STALL_LOAD_GRACE_MS || '30000', 10) || 30000);
const STALL_RECOVER_COOLDOWN_MS = Math.max(
  10000,
  parseInt(process.env.ISOLA_STALL_RECOVER_COOLDOWN_MS || '60000', 10) || 60000,
);
// Budget for automatic recovery only; manual recover never consults it. 0 disables.
const STALL_RECOVER_MAX = Math.max(0, intEnv('ISOLA_STALL_RECOVER_MAX', 3));
const STALL_RECOVER_WINDOW_MS = Math.max(0, intEnv('ISOLA_STALL_RECOVER_WINDOW_MS', 600000));
const STALL_SAME_URL_MS = Math.max(0, intEnv('ISOLA_STALL_SAME_URL_MS', 600000));
// Inactive tabs (and all tabs unwatched) composite at this rate; software-GL background pages are real CPU.
const BACKGROUND_FPS = intEnv('ISOLA_BACKGROUND_FPS', 1);
// Post-load capturePage() offsets. ISOLA_LOAD_CAPTURE_DELAYS_MS restores the
// older five-readback series ("250,600,1200,2500,5000").
const LOAD_CAPTURE_DELAYS = parseDelayList(process.env.ISOLA_LOAD_CAPTURE_DELAYS_MS);
let lastComposedAt = Date.now();
let stallGraceUntil = 0;
let emptyCaptures = 0;
let lastEmptyCaptureLogAt = 0;
let stalledSince = 0;
// Separate from stalledSince: a manual tab_recover still needs the all-clear sent afterwards.
let stallReported = false;
// The all-clear requires a frame composed after the announcement, not just grace expiry.
let stallAnnouncedAt = 0;
let recoveries = 0;
let lastRecoverAt = 0;
// Every recovery with its URL: "this page wedges every fresh renderer" vs a one-off.
let recoverHistory = [];
// While set, automatic recovery is skipped without probing capture. Manual recover ignores it.
let autoRecoverBlockedUntil = 0;
// Last "<reason>|<url>" refused, so the log says it once, not once per tick.
let recoverySuppressedKey = '';

// Capture-path degradation: a paint event can hand back an EMPTY image while the
// renderer is healthy. A high empty rate switches capture to capturePage(), which
// skips the copy-output hand-off. Measured ~10/s when Chromium shm was off /dev/shm.
const EMPTY_WINDOW_MS = 10000;
const EMPTY_FALLBACK_THRESHOLD = 5; // empties within the window to enter fallback
const FALLBACK_INTERVAL_MS = 200; // 5 fps of screenshot-driven frames
const FALLBACK_RECOVER_MS = 1500; // paint silence that ends the fallback
let emptyCaptureTimes = [];
let lastGoodPaintAt = 0;
let lastEmptyCaptureAt = 0;
let fallbackTimer = null;

// Wire accounting: dedup repaints are the steady state on an unchanged page, and
// only sent frames prove the picture can move.
let sentTotal = 0;
let dedupTotal = 0;
let lastSentAt = 0;

// Opt-in (ISOLA_PAINT_STATS=1) damage statistics.
const PAINT_STATS = process.env.ISOLA_PAINT_STATS === '1';
const paintStats = { paints: 0, full: 0, partial: 0, dirtyPx: 0, surfPx: 0 };
let paintStatsAt = Date.now();

// notePaint folds one paint's damage into paintStats, summarized every 10s.
function notePaint(dirty) {
  if (!PAINT_STATS) return;
  const surfPx = w * h;
  const px = dirty ? dirty.width * dirty.height : surfPx;
  paintStats.paints++;
  if (px >= surfPx * 0.85) paintStats.full++;
  else paintStats.partial++;
  paintStats.dirtyPx += px;
  paintStats.surfPx += surfPx;
  const now = Date.now();
  if (now - paintStatsAt < 10000) return;
  paintStatsAt = now;
  const dirtyPct = paintStats.surfPx
    ? ((100 * paintStats.dirtyPx) / paintStats.surfPx).toFixed(2)
    : '0.00';
  console.log(
    `[electron] paint-stats paints=${paintStats.paints} full=${paintStats.full} ` +
      `partial=${paintStats.partial} dirty_pct=${dirtyPct}`,
  );
}

// ── Tabs ────────────────────────────────────────────────────────────────
let nextTabId = 1;
let tabs = []; // { id, win, url, title, invalidateTimer, partialTimer }
let activeTabId = 0;

function ensureFrameBuffer() {
  const bytes = w * h * 4;
  if (!frameBuffer || frameBuffer.length !== bytes) {
    frameBuffer = Buffer.alloc(bytes);
    // The surface is the page canvas, not the app theme — see theme.js.
    fillPageCanvas(frameBuffer);
  }
  return frameBuffer;
}

function blitBitmap(full, bitmap, bmpW, bmpH, ox, oy) {
  const rowBytes = bmpW * 4;
  if (bitmap.length !== bmpW * bmpH * 4) return false;

  for (let row = 0; row < bmpH; row++) {
    const dstRow = oy + row;
    if (dstRow < 0 || dstRow >= h) continue;
    const copyW = Math.min(bmpW, w - ox);
    if (copyW <= 0) continue;
    const srcOff = row * rowBytes;
    const dstOff = dstRow * w * 4 + ox * 4;
    bitmap.copy(full, dstOff, srcOff, srcOff + copyW * 4);
  }
  return true;
}

// Electron often delivers 1919×1079 bitmaps for a 1920×1080 window — extrapolate
// the last valid column/row so ffmpeg never sees stale edge pixels.
function extrapolateEdges(full, bmpW, bmpH) {
  const rows = Math.min(bmpH, h);
  if (bmpW > 0 && bmpW < w) {
    for (let row = 0; row < rows; row++) {
      const rowOff = row * w * 4;
      const srcOff = rowOff + (bmpW - 1) * 4;
      const pixel = full.slice(srcOff, srcOff + 4);
      for (let col = bmpW; col < w; col++) {
        pixel.copy(full, rowOff + col * 4);
      }
    }
  }
  if (bmpH > 0 && bmpH < h) {
    const lastRowOff = (bmpH - 1) * w * 4;
    for (let row = bmpH; row < h; row++) {
      full.copy(full, row * w * 4, lastRowOff, lastRowOff + w * 4);
    }
  }
}

// Blocked-navigation/address events can fire many times per second (a page
// hammering redirects); one line per kind per second is enough to see it.
const blockedLogAt = {};
function logBlocked(kind, url) {
  const now = Date.now();
  if (now - (blockedLogAt[kind] || 0) < 1000) return;
  blockedLogAt[kind] = now;
  console.log(`[electron] blocked ${kind}: ${String(url).slice(0, 200)}`);
}

function connectToUds(sockPath) {
  let udsAttempts = 0;
  return new Promise((resolve) => {
    const attempt = () => {
      const sock = net.createConnection(sockPath);
      sock.on('connect', () => {
        console.log(`[electron] uds connected ${sockPath}`);
        resolve(sock);
      });
      sock.on('error', (err) => {
        udsAttempts++;
        if (udsAttempts === 1 || udsAttempts % 20 === 0) {
          console.log(`[electron] uds retry ${udsAttempts}: ${err.message}`);
        }
        sock.destroy();
        setTimeout(attempt, 500);
      });
    };
    attempt();
  });
}

// Transmits only the region that actually changed. The composited surface is
// still built locally (that is what ffmpeg needs), but the wire carries one
// dirty rect plus just that rect's pixels — a blinking cursor or a caret
// costs a few KB instead of a full surface (14.7 MB at 1440p).
// nothingToPaint reports that the active tab's surface is blank and its document
// has finished loading, so an empty capture means "there is nothing to show"
// rather than "the compositor stopped". See writeFrame and startStallWatchdog.
function nothingToPaint() {
  const tab = activeTab();
  if (!tab || !tab.win || tab.win.isDestroyed()) return Boolean(tab && tab.surfaceBlank);
  return isEmptyCaptureExpected({
    surfaceBlank: !!tab.surfaceBlank,
    // While a load is in flight an empty image is still the old signal — the tab
    // is about to paint — and the load grace window covers that.
    loading: tab.win.webContents.isLoading(),
  });
}

// isEmptyDocument reports that the active tab has no document of its own — a
// fresh tab (about:blank). Only such a tab's blank is normalised below.
function isEmptyDocument(tab) {
  if (!tab || !tab.win || tab.win.isDestroyed()) return false;
  const u = tab.win.webContents.getURL();
  return u === '' || u === 'about:blank';
}

// A new tab's blank must be one stable theme colour, or it flickers between
// dark and white. about:blank only; real pages are untouched.
function normaliseEmptyDocument(payload) {
  if (payload[0] === bgColor.r && payload[1] === bgColor.g
    && payload[2] === bgColor.b && payload[3] === bgColor.a) {
    return; // already the theme background — steady state, no work to do
  }
  for (let i = 0; i < payload.length; i += 4) {
    payload[i] = bgColor.r;
    payload[i + 1] = bgColor.g;
    payload[i + 2] = bgColor.b;
    payload[i + 3] = bgColor.a;
  }
}

function writeFrame(dirtyRects, nativeImage) {
  if (!captureActive) return FRAME_IDLE;
  if (!udsSocket || udsSocket.destroyed) return FRAME_IDLE;

  const bitmap = nativeImage.toBitmap();
  const dirty = dirtyRects[0] || { x: 0, y: 0, w, h };
  const full = ensureFrameBuffer();
  const fullBytes = w * h * 4;
  const size = nativeImage.getSize();
  const bmpW = size.width;
  const bmpH = size.height;
  const bmpBytes = bmpW * bmpH * 4;
  const activeWin = activeTab();
  let composited = false;
  // A full (or extrapolated near-full) bitmap updates the whole surface.
  let rect = null;

  if (bitmap.length === fullBytes) {
    bitmap.copy(full, 0);
    composited = true;
    rect = { x: 0, y: 0, w, h };
  } else if (bitmap.length === bmpBytes) {
    const nearFull = bmpW >= w - 2 && bmpH >= h - 2;
    if (nearFull) {
      composited = blitBitmap(full, bitmap, bmpW, bmpH, 0, 0);
      if (composited) extrapolateEdges(full, bmpW, bmpH);
      if (composited) rect = { x: 0, y: 0, w, h };
    } else if (bitmap.length === dirty.w * dirty.h * 4) {
      composited = blitBitmap(full, bitmap, dirty.w, dirty.h, dirty.x, dirty.y);
      if (composited) rect = clampRect(dirty, w, h);
    } else {
      composited = blitBitmap(full, bitmap, bmpW, bmpH, dirty.x, dirty.y);
      if (composited) {
        rect = clampRect({ x: dirty.x, y: dirty.y, w: bmpW, h: bmpH }, w, h);
      }
    }
  } else if (bitmap.length === dirty.w * dirty.h * 4) {
    composited = blitBitmap(full, bitmap, dirty.w, dirty.h, dirty.x, dirty.y);
    if (composited) rect = clampRect(dirty, w, h);
  }

  if (!composited || !rect || rect.w <= 0 || rect.h <= 0) {
    // An empty image on a blank, finished document is "nothing to paint", not a
    // stall — counting it recreated fresh tabs' renderers in a loop.
    if (nothingToPaint()) {
      dedupTotal++;
      return FRAME_DEDUP;
    }
    // Empty bitmap: rate-limited; the count feeds the stall watchdog.
    emptyCaptures++;
    const now = Date.now();
    if (now - lastEmptyCaptureLogAt > 5000) {
      lastEmptyCaptureLogAt = now;
      console.warn(
        `[electron] skip frame: bitmap=${bitmap.length} dirty=${dirty.w}x${dirty.h} config=${w}x${h} (empty captures so far: ${emptyCaptures})`,
      );
    }
    return FRAME_EMPTY;
  }
  lastComposedAt = Date.now();

  const isFull = rect.x === 0 && rect.y === 0 && rect.w === w && rect.h === h;
  const payload = isFull ? full : extractRect(full, rect, w);

  // A blank full frame (navigation commit, empty capturePage) must not replace
  // the previous surface — hold it within the window and while loading. The
  // seeding frame and a tab's first frame always go out.
  const loading = !!(activeWin && activeWin.win && !activeWin.win.isDestroyed()
    && activeWin.win.webContents.isLoading());
  // A blank partial patch tears: the rest of the frame still shows the previous
  // document. Only patches ≥ BLANK_PATCH_MIN_SHARE while a hold is possible —
  // the scan costs ~22 ms on a full 1760p surface.
  const blank = isFull && isBlankBitmap(payload);
  const patchLargeEnough = !isFull
    && rect.w * rect.h >= w * h * BLANK_PATCH_MIN_SHARE
    && (loading || Date.now() < blankHoldUntil);
  const blankPatch = patchLargeEnough && isBlankBitmap(payload);
  // Both decisions read the surface's state BEFORE this frame: whether a blank is
  // worth hiding depends on what is being replaced, not on what this frame is.
  const established = !!(activeWin && activeWin.surfaceEstablished);
  if (shouldHoldBlank({
    blank: blank || blankPatch,
    streamerSeeded,
    surfaceEstablished: established,
    loading,
    withinHoldWindow: Date.now() < blankHoldUntil,
  })) {
    dedupTotal++;
    return FRAME_DEDUP;
  }
  // This frame goes out, so record how the surface changed.
  if (activeWin) {
    activeWin.surfaceBlank = surfaceBlankAfterFrame({ isFull, blank });
    activeWin.surfaceEstablished = true;
  }
  // An empty document's blank must be one stable colour, or the new tab flickers
  // between the theme background and about:blank's white.
  if (isFull && blank && isEmptyDocument(activeWin)) normaliseEmptyDocument(payload);

  // Dedup only against the same rect; a dedup is a no-op repaint, not a stall.
  const outcome = frameOutcome({
    composited,
    rect,
    payload,
    lastPayload: lastSentPayload,
    lastRect: lastSentRect,
  });
  if (outcome === FRAME_DEDUP) {
    dedupTotal++;
    return outcome;
  }

  const tsNs = frameTimestampNs();
  const buf = encodeFrame(w, h, w * 4, [rect], payload, tsNs);
  udsSocket.write(buf);
  sentTotal++;
  lastSentAt = Date.now();
  if (!lastSentPayload || lastSentPayload.length !== payload.length) {
    lastSentPayload = Buffer.alloc(payload.length);
  }
  payload.copy(lastSentPayload);
  lastSentRect = rect;
  if (isFull) {
    hasSentFullFrame = true;
    streamerSeeded = true;
  }
  frameSeq++;
  return FRAME_SENT;
}

// noteEmptyCapture folds one empty paint image into the rolling window and
// starts the fallback path once the rate says this is a pattern, not a blip.
function noteEmptyCapture() {
  const now = Date.now();
  lastEmptyCaptureAt = now;
  emptyCaptureTimes.push(now);
  pruneEmptyCaptureTimes(emptyCaptureTimes, now, EMPTY_WINDOW_MS);
  maybeStartCaptureFallback();
}

function stopCaptureFallback(reason) {
  if (!fallbackTimer) return;
  clearInterval(fallbackTimer);
  fallbackTimer = null;
  console.warn(`[electron] fallback stopped (${reason})`);
}

function maybeStartCaptureFallback() {
  if (fallbackTimer || !captureActive) return;
  if (emptyCaptureTimes.length < EMPTY_FALLBACK_THRESHOLD) return;
  console.warn(
    `[electron] ${emptyCaptureTimes.length} empty captures in ${EMPTY_WINDOW_MS}ms — driving frames from capturePage() until paint recovers`,
  );
  const tickFallback = () => {
    if (!captureActive) {
      stopCaptureFallback('capture paused');
      return;
    }
    const now = Date.now();
    if (shouldStopCaptureFallback({
      now,
      lastEmptyCaptureAt,
      recoverMs: FALLBACK_RECOVER_MS,
    })) {
      stopCaptureFallback('paint recovered');
      return;
    }
    void captureAndSend('fallback');
  };
  void captureAndSend('fallback');
  fallbackTimer = setInterval(tickFallback, FALLBACK_INTERVAL_MS);
}

// captureStatsPayload reaches viewers, so a capture stall is identifiable from a user report.
function captureStatsPayload() {
  const now = Date.now();
  // Age the window even when recovered, or "capture stalled" never clears.
  pruneEmptyCaptureTimes(emptyCaptureTimes, now, EMPTY_WINDOW_MS);
  return {
    ev: 'capture_stats',
    empty_total: emptyCaptures,
    empty_window: emptyCaptureTimes.length,
    fallback: fallbackTimer !== null,
    last_paint_ms: lastGoodPaintAt ? now - lastGoodPaintAt : -1,
    last_frame_ms: lastComposedAt ? now - lastComposedAt : -1,
    // last_paint_ms stays near zero via keepalive; last_sent_ms only moves on real frames.
    last_sent_ms: lastSentAt ? now - lastSentAt : -1,
    sent_total: sentTotal,
    dedup_total: dedupTotal,
  };
}

function startCaptureStatsReport() {
  setInterval(() => broadcastRunnerEvent(captureStatsPayload()), 2000);
}

async function captureAndSend(reason) {
  if (!captureActive) return;
  const tab = activeTab();
  if (!tab || !tab.win || tab.win.isDestroyed()) return;
  try {
    const image = await tab.win.webContents.capturePage();
    writeFrame([{ x: 0, y: 0, w, h }], image);
    console.log(`[electron] capturePage (${reason})`);
  } catch (err) {
    console.warn(`[electron] capturePage ${reason}: ${err.message}`);
  }
}

// broadcastRunnerEvent reaches every attached streamer over the ctrl socket.
function broadcastRunnerEvent(ev) {
  const payload = JSON.stringify(ev);
  for (const c of ctrlClients) {
    try { c.write(payload + '\n'); } catch { /* ignore */ }
  }
}

// clearStallReport sends the all-clear to viewers exactly once per stall.
function clearStallReport() {
  if (!stallReported) return;
  stallReported = false;
  broadcastRunnerEvent({ ev: 'renderer_stall', stalled: false, recoveries });
}

// All-clear only on a frame composed after the announcement, not grace expiry.
function maybeClearStallReport() {
  if (!stallReported) return;
  if (stallAnnouncedAt !== 0 && lastComposedAt > stallAnnouncedAt) clearStallReport();
}

// confirmStalled: keepalive nudges make a healthy renderer produce a bitmap; empty = stalled.
async function confirmStalled() {
  const tab = activeTab();
  if (!tab || !tab.win || tab.win.isDestroyed()) return false;
  try {
    const image = await tab.win.webContents.capturePage();
    const bitmap = image.toBitmap();
    if (bitmap.length > 0) {
      writeFrame([{ x: 0, y: 0, w, h }], image);
      return false;
    }
    return true;
  } catch (err) {
    console.warn(`[electron] stall probe: ${err.message}`);
    return true;
  }
}

// recoverStalledRenderer: a new window (fresh renderer) — reload() cannot service a
// wedged renderer and forcefullyCrashRenderer left tabs with no renderer at all.
// auto=false (viewer request) skips the budget; returns whether the window was replaced.
function recoverStalledRenderer(reason, { auto = false } = {}) {
  const tab = activeTab();
  if (!tab) return false;
  const now = Date.now();
  const url =
    tab.win && !tab.win.isDestroyed() ? tab.win.webContents.getURL() : tab.url;

  if (auto) {
    if (now < autoRecoverBlockedUntil) return false;
    const decision = shouldAutoRecover({
      url,
      now,
      history: recoverHistory,
      cooldownMs: STALL_RECOVER_COOLDOWN_MS,
      lastRecoverAt,
      maxPerWindow: STALL_RECOVER_MAX,
      windowMs: STALL_RECOVER_WINDOW_MS,
      sameUrlWindowMs: STALL_SAME_URL_MS,
    });
    if (!decision.allow) {
      // Back off for a full cooldown: retrying the decision (and the capture
      // readback that leads to it) every watchdog tick would be pure cost.
      autoRecoverBlockedUntil = now + STALL_RECOVER_COOLDOWN_MS;
      announceRecoverySuppressed(decision.reason, url);
      return false;
    }
  }

  lastRecoverAt = now;
  recoveries++;
  recoverHistory.push({ url, at: now });
  // Bound the history: only entries inside a budget window can ever matter.
  const pruneMs = Math.max(STALL_RECOVER_WINDOW_MS, STALL_SAME_URL_MS, STALL_RECOVER_COOLDOWN_MS);
  recoverHistory = recoverHistory.filter((entry) => now - entry.at < pruneMs);
  const stalledMs = stalledSince ? now - stalledSince : 0;
  console.warn(
    `[electron] renderer stalled (${reason}) — recreating the tab window with a fresh renderer`,
  );
  stallReported = true;
  stallAnnouncedAt = now;
  broadcastRunnerEvent({ ev: 'renderer_stall', stalled: true, ms: stalledMs, recoveries });
  recreateTabWindow(tab, reason);
  return true;
}

// announceRecoverySuppressed explains once per reason|url why nothing was reloaded.
function announceRecoverySuppressed(reason, url) {
  const key = `${reason}|${url}`;
  if (recoverySuppressedKey === key) return;
  recoverySuppressedKey = key;
  console.warn(
    `[electron] renderer still stalled (${reason}) — not replacing it again for this page; manual recover is still available`,
  );
  if (!stallReported) {
    stallReported = true;
    stallAnnouncedAt = Date.now();
    broadcastRunnerEvent({
      ev: 'renderer_stall',
      stalled: true,
      ms: stalledSince ? Date.now() - stalledSince : 0,
      recoveries,
    });
  }
}

// RATE_REASSERT_MS repairs rates stuck at the background value (late resize, lost resume).
const RATE_REASSERT_MS = 5000;

function startRateWatchdog() {
  const timer = setInterval(() => {
    reassertActiveRate({ captureActive, tab: activeTab(), setTabFrameRate });
  }, RATE_REASSERT_MS);
  if (timer.unref) timer.unref();
}

function startStallWatchdog() {
  let probing = false;
  setInterval(() => {
    // Evidence-based all-clear first: it must be evaluated every tick, also
    // while the load grace window is open.
    maybeClearStallReport();
    if (!captureActive) {
      stalledSince = 0;
      return;
    }
    const tab = activeTab();
    if (!tab || !tab.win || tab.win.isDestroyed()) {
      stalledSince = 0;
      return;
    }
    // A blank surface has nothing to compose; silence is not a stall.
    if (nothingToPaint()) {
      stalledSince = 0;
      clearStallReport();
      return;
    }
    const silentMs = Date.now() - lastComposedAt;
    const allowMs = Date.now() < stallGraceUntil ? STALL_LOAD_GRACE_MS : STALL_MS;
    if (silentMs < allowMs) {
      stalledSince = 0;
      return;
    }
    if (!stalledSince) stalledSince = Date.now() - silentMs;
    // One recovery per cooldown window, none while the budget just refused one.
    if (probing || Date.now() < autoRecoverBlockedUntil) return;
    if (Date.now() - lastRecoverAt < STALL_RECOVER_COOLDOWN_MS) return;
    probing = true;
    void (async () => {
      try {
        if (await confirmStalled()) {
          await recoverStalledRenderer(
            `${Math.round(silentMs / 1000)}s without a composited frame`,
            { auto: true },
          );
        } else {
          // Capture still works, so the paint path (not the renderer) was quiet.
          lastComposedAt = Date.now();
          stalledSince = 0;
        }
      } finally {
        probing = false;
      }
    })();
  }, 3000);
}

let loadCaptureTimers = [];

function clearLoadCaptures() {
  for (const t of loadCaptureTimers) clearTimeout(t);
  loadCaptureTimers = [];
}

// Offscreen paint misses GPU-layer backgrounds; capturePage composites all layers.
// Full-surface readbacks are expensive, so early ones are skipped once a full frame went out.
function scheduleLoadCaptures() {
  clearLoadCaptures();
  const delays = loadCaptureDelays({
    configured: LOAD_CAPTURE_DELAYS,
    fullFrameSent: hasSentFullFrame,
  });
  for (const ms of delays) {
    loadCaptureTimers.push(setTimeout(() => {
      void captureAndSend(`load+${ms}ms`);
    }, ms));
  }
}

function resetFrameState() {
  frameBuffer = null;
  lastSentPayload = null;
  lastSentRect = null;
  // The streamer still holds the previous surface (old tab, old size), so a
  // full refresh is required before damage-only updates are meaningful.
  hasSentFullFrame = false;
  // A new document starts the stall clock over and earns a longer grace period.
  lastComposedAt = Date.now();
  stallGraceUntil = Date.now() + STALL_LOAD_GRACE_MS;
  // A new document also opens the window in which a blank full frame is
  // withheld instead of forwarded — see writeFrame.
  blankHoldUntil = Date.now() + BLANK_HOLD_MS;
}

// Applies the theme and returns the WINDOW backdrop; see theme.js.
function themeBackgroundColor() {
  const colours = themeColours(BROWSER_THEME);
  bgColor = colours.emptyDocument;
  nativeTheme.themeSource = colours.themeSource;
  return colours.window;
}

function loadResolved(win, input) {
  const url = resolveAddressBar(input);
  if (!url) {
    logBlocked('address', input);
    return false;
  }
  win.loadURL(url);
  return true;
}

function activeTab() {
  return tabs.find((t) => t.id === activeTabId) || null;
}

function broadcastTabs() {
  const payload = JSON.stringify({
    ev: 'tabs',
    tabs: tabs.map((t) => ({
      id: t.id,
      url: t.win && !t.win.isDestroyed() ? t.win.webContents.getURL() : t.url,
      title: t.win && !t.win.isDestroyed() ? t.win.webContents.getTitle() : '',
      active: t.id === activeTabId,
    })),
  });
  for (const c of ctrlClients) {
    try { c.write(payload + '\n'); } catch { /* ignore */ }
  }
}

function stopForwarding(tab) {
  if (tab.invalidateTimer) {
    clearInterval(tab.invalidateTimer);
    tab.invalidateTimer = null;
  }
}

// startForwarding keeps the compositor awake for a watched tab.
function startForwarding(tab) {
  stopForwarding(tab);
  if (!captureActive) return;
  const keepaliveMs = PRODUCER_KEEPALIVE_MS;
  tab.invalidateTimer = setInterval(() => {
    if (tab.win && !tab.win.isDestroyed()) {
      tab.win.webContents.invalidate();
    }
  }, keepaliveMs);
}

// tabFrameRate is the compositor rate a tab should run at now: the capture rate
// only while it is active and a viewer is attached.
function tabFrameRate(active) {
  return frameRateFor({
    base: frameRate,
    active: active === true,
    captureActive,
    backgroundFps: BACKGROUND_FPS,
  });
}

// setTabFrameRate applies that rate to an attached tab.
function setTabFrameRate(tab, active) {
  if (!tab || !tab.win || tab.win.isDestroyed()) return;
  const next = tabFrameRate(active);
  try {
    tab.win.webContents.setFrameRate(next);
  } catch (err) {
    console.warn(`[electron] fps ${next}: ${err.message}`);
    return;
  }
  // Only log real changes: a stuck-at-background rate is invisible elsewhere.
  if (tab.appliedFrameRate !== next) {
    tab.appliedFrameRate = next;
    console.log(`[electron] tab ${tab.id} ${next}fps`);
  }
}

// setCaptureActive: pause stops the nudges and captures but leaves the page running; resume re-seeds with one full frame.
function setCaptureActive(on) {
  const changed = applyCaptureCommand({
    on,
    wasActive: captureActive,
    tab: activeTab(),
    setActive: (value) => { captureActive = value; },
    resetFrameState,
    startForwarding,
    stopForwarding,
    setTabFrameRate,
    invalidate: (tab) => {
      if (tab.win && !tab.win.isDestroyed()) tab.win.webContents.invalidate();
    },
    captureAndSend: (reason) => { void captureAndSend(reason); },
    clearPartialTimer: (tab) => {
      if (tab.partialTimer) {
        clearTimeout(tab.partialTimer);
        tab.partialTimer = null;
      }
    },
    clearLoadCaptures,
  });
  if (!changed) return;
  if (!on) stopCaptureFallback('capture paused');
}

function switchTab(id) {
  const target = tabs.find((t) => t.id === id);
  if (!target) return;
  const prev = activeTab();
  if (prev && prev.id !== id) {
    stopForwarding(prev);
    // The tab is leaving the screen: keep it live, but stop it competing with
    // the one that is about to be streamed.
    setTabFrameRate(prev, false);
  }
  activeTabId = id;
  // The surface still holds the previous tab: this tab's first frame is the replacement.
  target.surfaceEstablished = false;
  target.surfaceBlank = true;
  resetFrameState();
  clearLoadCaptures();
  // A fresh tab starts with a clean renderer-health slate; restore full rate
  // BEFORE the seeding capture.
  stalledSince = 0;
  clearStallReport();
  setTabFrameRate(target, true);
  startForwarding(target);
  try {
    target.win.webContents.focus();
    target.win.webContents.invalidate();
    void captureAndSend('tab-switch');
  } catch { /* ignore */ }
  broadcastTabs();
}

function createTab(url, activate = true) {
  const id = nextTabId++;
  const tab = {
    id,
    win: null,
    url,
    title: '',
    invalidateTimer: null,
    partialTimer: null,
    // What the viewer's single surface holds; starts blank ("nothing to paint").
    surfaceBlank: true,
    // Whether the surface holds something from THIS tab — decides if a blank would replace a picture.
    surfaceEstablished: false,
  };
  tabs.push(tab);
  attachTabWindow(tab, url);
  if (activate || tabs.length === 1) {
    activeTabId = id;
    startForwarding(tab);
  }
  // attachTabWindow cannot know yet whether this tab ends up active.
  setTabFrameRate(tab, tab.id === activeTabId);
  broadcastTabs();
  return tab;
}

function recreateTabWindow(tab, reason) {
  const current =
    tab.win && !tab.win.isDestroyed() ? tab.win.webContents.getURL() : tab.url;
  stopForwarding(tab);
  stopCaptureFallback('window recreated');
  emptyCaptureTimes = [];
  if (tab.partialTimer) {
    clearTimeout(tab.partialTimer);
    tab.partialTimer = null;
  }
  try {
    if (tab.win && !tab.win.isDestroyed()) tab.win.destroy();
  } catch (err) {
    console.warn(`[electron] old window destroy: ${err.message}`);
  }
  resetFrameState();
  attachTabWindow(tab, current || 'about:blank');
  if (tab.id === activeTabId) startForwarding(tab);
  broadcastTabs();
  console.log(`[electron] tab #${tab.id} recreated (${reason})`);
}

function attachTabWindow(tab, url) {
  const id = tab.id;
  const win = new BrowserWindow({
    width: w,
    height: h,
    show: false,
    useContentSize: true,
    backgroundColor: themeBackgroundColor(),
    webPreferences: {
      offscreen: true,
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: false,
      backgroundThrottling: false,
    },
  });

  win.webContents.on('paint', (event, dirty, image) => {
    if (tab.id !== activeTabId) return; // background tab: don't stream
    notePaint(dirty);
    const rects = dirty ? [{
      x: dirty.x, y: dirty.y, w: dirty.width, h: dirty.height,
    }] : [{ x: 0, y: 0, w, h }];
    const outcome = writeFrame(rects, image);
    // A dedup is a good paint; only FRAME_EMPTY is a capture stall.
    if (outcome === FRAME_SENT || outcome === FRAME_DEDUP) lastGoodPaintAt = Date.now();
    else if (outcome === FRAME_EMPTY && captureActive) noteEmptyCapture();

    const dirtyArea = dirty ? dirty.width * dirty.height : w * h;
    const isPartial = dirty && dirtyArea < w * h * 0.85;
    if (isPartial && !hasSentFullFrame && !tab.partialTimer) {
      tab.partialTimer = setTimeout(() => {
        tab.partialTimer = null;
        void captureAndSend('partial-paint');
      }, 150);
    }
  });

  const onNavigated = (painted = false) => {
    tab.url = win.webContents.getURL();
    tab.title = win.webContents.getTitle();
    resetFrameState();
    win.webContents.focus();
    win.webContents.invalidate();
    if (painted && tab.id === activeTabId) scheduleLoadCaptures();
    broadcastTabs();
  };
  win.webContents.on('did-finish-load', () => {
    console.log(`[electron] tab #${id} loaded: ${win.webContents.getURL()}`);
    onNavigated(true);
  });
  // `painted` separates rendered loads from bare commits, where capturePage returns a blank.
  win.webContents.on('did-navigate-in-page', () => onNavigated(true));
  win.webContents.on('did-navigate', () => onNavigated());
  win.webContents.on('page-title-updated', (e, title) => {
    tab.title = title || '';
    broadcastTabs();
  });
  // Retry a main-frame load once when the first address was unreachable: the
  // sandbox has no IPv6 route, and an AAAA-first host fails its first
  // navigation while the same URL loads over IPv4 on the retry. Every other
  // failure is reported unchanged. See load_retry.js.
  let lastLoadRetry = null;
  win.webContents.on('did-fail-load', (event, code, desc, url, isMainFrame) => {
    console.log(`[electron] load fail ${url}: ${code}`);
    if (!shouldRetryLoad({ code, isMainFrame, url, lastRetry: lastLoadRetry })) return;
    if (!isAllowedPageNavigation(url)) return;
    lastLoadRetry = { url, at: Date.now() };
    console.log(`[electron] retrying ${url} once after address unreachable`);
    setTimeout(() => {
      if (!win.isDestroyed()) void win.webContents.loadURL(url).catch(() => {});
    }, 150);
  });

  const blockDisallowed = (event, navUrl) => {
    if (isAllowedPageNavigation(navUrl)) return;
    event.preventDefault();
    logBlocked('navigation', navUrl);
  };
  win.webContents.on('will-navigate', blockDisallowed);
  win.webContents.on('will-redirect', blockDisallowed);
  win.webContents.on('will-frame-navigate', blockDisallowed);

  // target=_blank / window.open / middle-click → a new sandbox tab.
  // Denying the native window lets the tab strip own all browsing surfaces.
  win.webContents.setWindowOpenHandler(({ url, disposition }) => {
    if (!isAllowedPageNavigation(url)) {
      logBlocked('window.open', url);
      return { action: 'deny' };
    }
    const activate = disposition !== 'background-tab';
    createTab(url, activate);
    console.log(`[electron] window.open ${disposition}: ${url}`);
    return { action: 'deny' };
  });

  // Tor is applied once to the default session at startup, not per window.
  if (!loadResolved(win, url)) win.loadURL('about:blank');
  tab.win = win;
  // Now that the tab owns the window, give it the rate its visibility deserves.
  setTabFrameRate(tab, tab.id === activeTabId);
  return win;
}

function closeTab(id) {
  const idx = tabs.findIndex((t) => t.id === id);
  if (idx === -1) return;
  const tab = tabs[idx];
  const wasActive = tab.id === activeTabId;
  stopForwarding(tab);
  if (tab.partialTimer) clearTimeout(tab.partialTimer);
  tabs.splice(idx, 1);
  try { if (!tab.win.isDestroyed()) tab.win.destroy(); } catch { /* ignore */ }
  if (tabs.length === 0) {
    createTab('about:blank', true);
    return;
  }
  if (wasActive) {
    switchTab(tabs[Math.max(0, idx - 1)].id);
    return;
  }
  broadcastTabs();
}

function handleCtrlCommand(line) {
  let cmd;
  try { cmd = JSON.parse(line); } catch { return; }
  switch (cmd.cmd) {
    case 'tab_new':
      createTab(cmd.url || 'about:blank', true);
      break;
    case 'tab_switch':
      switchTab(parseInt(cmd.id, 10));
      break;
    case 'tab_close':
      closeTab(parseInt(cmd.id, 10));
      break;
    case 'tab_navigate': {
      const t = activeTab();
      if (t && t.win && !t.win.isDestroyed()) loadResolved(t.win, cmd.url);
      break;
    }
    case 'tab_back': {
      const t = activeTab();
      if (t && t.win && !t.win.isDestroyed()) {
        const nav = t.win.webContents.navigationHistory;
        if (nav && nav.canGoBack()) nav.goBack();
      }
      break;
    }
    case 'tab_reload': {
      const t = activeTab();
      if (t && t.win && !t.win.isDestroyed()) {
        resetFrameState();
        t.win.webContents.reload();
      }
      break;
    }
    case 'tab_recover': {
      // A viewer-initiated recover ignores the budget; only the streamer's watchdog is `auto`.
      const auto = cmd.auto === true;
      if (!auto) lastRecoverAt = 0;
      void recoverStalledRenderer(
        auto ? 'requested by the renderer watchdog' : 'requested by viewer',
        { auto },
      );
      break;
    }
    case 'get_tabs':
      broadcastTabs();
      break;
    case 'resize': {
    // Bound and even the request: an odd or absurd size corrupts the shared encode.
      const nw = parseInt(cmd.width, 10) & ~1;
      const nh = parseInt(cmd.height, 10) & ~1;
      if (Number.isFinite(nw) && Number.isFinite(nh) &&
          nw >= MIN_DIM && nh >= MIN_DIM && nw <= MAX_DIM && nh <= MAX_DIM) {
        handleResize(nw, nh);
      } else {
        console.warn(`[electron] resize refused: ${cmd.width}x${cmd.height}`);
      }
      break;
    }
    case 'capture':
      // Viewer-count gate from the streamer: nobody attached → nothing to encode.
      setCaptureActive(cmd.on !== false);
      break;
    default:
      break;
  }
}

function setupCtrlSocket() {
  try { fs.unlinkSync(ISOLA_CTRL_SOCK); } catch { /* not there */ }
  const server = net.createServer((client) => {
    ctrlClients.push(client);
    client.on('data', (data) => {
      for (const line of data.toString().split('\n')) {
        if (line.trim()) handleCtrlCommand(line);
      }
    });
    client.on('error', () => {});
    client.on('close', () => {
      ctrlClients = ctrlClients.filter((c) => c !== client);
    });
  });
  server.listen(ISOLA_CTRL_SOCK, () => {
    hardenCtrlSocket(ISOLA_CTRL_SOCK);
  });
}

function handleResize(newWidth, newHeight) {
  w = newWidth;
  h = newHeight;
  clearLoadCaptures();
  resetFrameState();
  for (const t of tabs) {
    if (t.win && !t.win.isDestroyed()) {
      t.win.setContentSize(w, h);
      t.win.webContents.invalidate();
    }
  }
  // The surface just changed shape, so the streamer needs one complete frame
  // before it can apply damage again.
  void captureAndSend('resize');
}

// Chromium switches live in ./chrome_flags.js: the list is load-bearing (see the
// --disable-dev-shm-usage note there) and is asserted by test/chrome_flags.test.js.
applyChromeFlags(app.commandLine);

// Advertise plain Chrome, not Electron: UA-sniffing gates reject "Electron". Set before whenReady().
applyUserAgent(app);

app.whenReady().then(async () => {
  console.log(`[electron] ready ${w}x${h}@${frameRate}`);
  // Await Tor before any navigation; on failure exit — an unproxied Tor session is the leak this must never produce.
  if (TOR_ENABLED) {
    try {
      await applyTorProxy(session.defaultSession);
      console.log('[electron] tor proxy applied');
    } catch (err) {
      console.error(`[electron] FATAL tor proxy: ${err && err.message ? err.message : err}`);
      app.exit(1);
      return;
    }
  }
  udsSocket = await connectToUds(ISOLA_FRAME_SOCK);
  createTab(START_URL, true);
  setupCtrlSocket();
  startStallWatchdog();
  startRateWatchdog();
  startCaptureStatsReport();
});

app.on('window-all-closed', () => {});

app.on('before-quit', () => {
  if (udsSocket) udsSocket.destroy();
});

function setupResizeListener() {
  process.stdin.resume();
  process.stdin.setEncoding('utf8');
  process.stdin.on('data', (data) => {
    const m = data.match(/RESIZE\s+(\d+)x(\d+)/);
    if (m) handleResize(parseInt(m[1], 10), parseInt(m[2], 10));
  });
}

setupResizeListener();
