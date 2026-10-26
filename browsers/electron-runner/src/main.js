// Electron offscreen runner for isola transport.
// Creates offscreen BrowserWindow "tabs", captures composited pages via the
// 'paint' event, encodes frames in ISOL binary protocol, and writes them to
// the Unix domain socket that isola-streamer listens on.
//
// Multi-tab: each tab is its own offscreen BrowserWindow. Only the active
// tab forwards paint frames to the encoder; switching pauses the previous
// window and pushes one full capture of the new one. A JSON-lines control
// socket (/run/isola/ctrl.sock) takes tab commands from isola-streamer
// (tab_new/tab_switch/tab_close/tab_navigate/tab_back/tab_reload/get_tabs) and
// broadcasts tab state back so viewers can render a tab strip.

// Electron emits its own security warnings ("Insecure Content-Security-Policy")
// as console.warn() in the renderer. The isola runner exists to render
// arbitrary, untrusted third-party pages, so a wrapper-level CSP is neither
// possible nor meaningful — and the warning is not harmless here: isola-streamer
// taps Runtime.consoleAPICalled, so it surfaces in the viewer's console panel
// looking like page output. Suppress it before any window exists. This gates
// only Electron's built-in warnings; the runner's own console.log lines (the
// [electron] output on the container's stdout) are untouched.
process.env.ELECTRON_DISABLE_SECURITY_WARNINGS = 'true';

const { app, BrowserWindow, nativeTheme, session } = require('electron');
const net = require('net');
const fs = require('fs');
const path = require('path');
const { encodeFrame, extractRect, clampRect, isBlankBitmap, shouldHoldBlank, surfaceBlankAfterFrame, frameTimestampNs, frameOutcome, FRAME_SENT, FRAME_DEDUP, FRAME_EMPTY, FRAME_IDLE } = require('./isol_protocol');
const { hardenCtrlSocket } = require('./ctrl_socket');
const { applyChromeFlags } = require('./chrome_flags');
const { isEmptyCaptureExpected, shouldStopCaptureFallback, pruneEmptyCaptureTimes } = require('./capture_fallback');
const {
  frameRateFor,
  shouldAutoRecover,
  loadCaptureDelays,
  parseDelayList,
} = require('./render_policy');
const { applyTorProxy, isTorEnabled } = require('./tor');
const { isAllowedPageNavigation, resolveAddressBar } = require('./navigation_url');
const { themeColours, fillPageCanvas } = require('./theme');

const START_URL = process.env.START_URL || 'https://example.com';
const RESOLUTION = process.env.RESOLUTION || '1280x720';
const FPS = parseInt(process.env.ISOLA_FPS || '30', 10);
// Bounds for a live viewport resize, mirroring the streamer's maxSurfaceDim and
// the backend's 64px floor. Anything outside them is refused rather than fed to
// the compositor.
const MIN_DIM = 64;
const MAX_DIM = 8192;
// Keepalive nudge rate for the compositor. A static page must not be force-
// repainted at the capture frame rate, so this defaults well below it. It is
// also the worst-case delay before a click's damage reaches the streamer on
// an idle page (the offscreen compositor does not schedule a beginFrame for
// the change by itself), so 100ms — repaints of a static page are cheap and
// the dedup layer drops them before the UDS.
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
// Last transmitted damage: the rect plus its pixels. Kept per-rect because a
// different rect means different pixels, so only a repeat of the same rect can
// be deduplicated.
let lastSentPayload = null;
let lastSentRect = null;
// The streamer seeds its surface from full-refresh frames only, so it needs
// at least one before partial damage makes sense.
let hasSentFullFrame = false;
// Whether the streamer has EVER been given a full frame. It owns one surface
// for the whole session — a navigation does not replace it — so one seed is
// enough. Deliberately NOT cleared by resetFrameState(): "this document has
// been seeded" is exactly the state in which a blank must be withheld.
let streamerSeeded = false;
// How long after a navigation or load a blank full frame is withheld. Bounded
// on purpose: a page that really is one flat colour must still appear.
const BLANK_HOLD_MS = 3000;
// A blank PARTIAL patch is withheld only when it covers this share of the
// surface. Below it the rect is ordinary content — a button fill, a panel
// background — and holding it would freeze a legitimate update; the measured
// tear was 40% of the page.
const BLANK_PATCH_MIN_SHARE = 0.2;
let blankHoldUntil = 0;
let bgColor = { r: 255, g: 255, b: 255, a: 255 };
// Whether anyone is watching. With no viewer attached, composing the surface
// and shipping it over the UDS is pure waste — measured at ~0.4 cores for an
// animating page on a CPU-only host. The streamer owns this state (it is the
// only party that knows how many viewers are attached) and pushes it over the
// ctrl socket as {"cmd":"capture","on":bool}. It defaults to true so the
// runner keeps its previous behaviour if the streamer never says anything.
let captureActive = true;

// ── Renderer stall detection ────────────────────────────────────────────
// A wedged renderer is the failure mode that looks like "isola is broken"
// while every other metric looks healthy: the compositor stops emitting
// paints, capturePage() hands back an empty image, and the damage stream just
// goes quiet (the viewer keeps showing the same frame at ~1 fps while the page
// burns CPU). It is reachable with heavy pages on a CPU-only host — a WebGL
// map, for instance, where the renderer blocks in the software-GL readback.
//
// The keepalive nudges (startForwarding) guarantee that the ACTIVE tab keeps
// producing composited frames while a viewer is attached, whatever the page is
// doing, so prolonged silence is a reliable stall signal rather than "the page
// is static".
const STALL_MS = Math.max(2000, parseInt(process.env.ISOLA_STALL_MS || '8000', 10) || 8000);
// A freshly loaded document gets longer: a big page can take a while to produce
// its first composited frame, and crashing the renderer mid-load would be worse
// than waiting.
const STALL_LOAD_GRACE_MS = Math.max(STALL_MS, parseInt(process.env.ISOLA_STALL_LOAD_GRACE_MS || '30000', 10) || 30000);
const STALL_RECOVER_COOLDOWN_MS = Math.max(
  10000,
  parseInt(process.env.ISOLA_STALL_RECOVER_COOLDOWN_MS || '60000', 10) || 60000,
);
// Automatic-recovery budget (see src/render_policy.js shouldAutoRecover). The
// manual recover action never consults it: a person asking for a fresh renderer
// is not the loop this protects against. A 0 disables that rule.
const STALL_RECOVER_MAX = Math.max(0, intEnv('ISOLA_STALL_RECOVER_MAX', 3));
const STALL_RECOVER_WINDOW_MS = Math.max(0, intEnv('ISOLA_STALL_RECOVER_WINDOW_MS', 600000));
const STALL_SAME_URL_MS = Math.max(0, intEnv('ISOLA_STALL_SAME_URL_MS', 600000));
// Inactive tabs — and every tab while no viewer is attached — are composited at
// this rate instead of the capture rate (<= 0 keeps them all at the capture
// rate). An offscreen window is composited whether or not anyone can see it, so
// a background software-GL page is real CPU for no picture. See frameRateFor.
const BACKGROUND_FPS = intEnv('ISOLA_BACKGROUND_FPS', 1);
// Post-load capturePage() offsets. ISOLA_LOAD_CAPTURE_DELAYS_MS restores the
// older five-readback series ("250,600,1200,2500,5000").
const LOAD_CAPTURE_DELAYS = parseDelayList(process.env.ISOLA_LOAD_CAPTURE_DELAYS_MS);
let lastComposedAt = Date.now();
let stallGraceUntil = 0;
let emptyCaptures = 0;
let lastEmptyCaptureLogAt = 0;
let stalledSince = 0;
// Whether viewers currently have a stall reported. Kept separately from
// stalledSince: a recovery can also be requested by hand (tab_recover), which
// never goes through the watchdog, and the all-clear still has to be sent
// afterwards or the toolbar would keep saying "renderer stuck" forever.
let stallReported = false;
// When the stall was announced, so the all-clear can require a frame composed
// after it rather than merely "no complaint within the grace window".
let stallAnnouncedAt = 0;
let recoveries = 0;
let lastRecoverAt = 0;
// Every recovery with the URL it replaced, so the budget can tell "this page
// wedges every fresh renderer" from a one-off bad load.
let recoverHistory = [];
// While set, automatic recovery is skipped without probing capture: the budget
// just said no, and a full-surface capturePage() every 3s on a wedged renderer
// is its own cost. Manual recover ignores it.
let autoRecoverBlockedUntil = 0;
// Last "<reason>|<url>" refused, so the log says it once, not once per tick.
let recoverySuppressedKey = '';

// ── Capture-path degradation ────────────────────────────────────────────
// A paint event can hand back an EMPTY image — the compositor's copy-output
// failing — while the renderer is perfectly healthy and the page is animating.
// The renderer watchdog cannot see that (CDP answers fine), and the viewer just
// stops updating: this is the second shape of "frozen picture", measured at ~10
// empty captures per second while /tmp held Chromium's shared memory instead of
// /dev/shm. Empties are counted in a rolling window, and a high rate switches
// the capture to the capturePage() path, which does not go through the same
// copy-output hand-off, so the picture survives whatever breaks paint.
const EMPTY_WINDOW_MS = 10000;
const EMPTY_FALLBACK_THRESHOLD = 5; // empties within the window to enter fallback
const FALLBACK_INTERVAL_MS = 200; // 5 fps of screenshot-driven frames
const FALLBACK_RECOVER_MS = 1500; // paint silence that ends the fallback
let emptyCaptureTimes = [];
let lastGoodPaintAt = 0;
let lastEmptyCaptureAt = 0;
let fallbackTimer = null;

// Wire accounting. A paint that composes but carries no new pixels is not a
// stall — the compositor is working — but it is also not a frame, and the
// 100ms keepalive nudge makes those repaints the steady state on an unchanged
// page. Counting them apart from what actually went out is the only way this
// side can show a frozen picture: every paint-based age below is refreshed by
// the nudge whether or not the picture moved.
let sentTotal = 0;
let dedupTotal = 0;
let lastSentAt = 0;

// Opt-in capture-geometry stats (ISOLA_PAINT_STATS=1). Records what the
// compositor's paint event actually offers as damage — the input a
// region-based encoder would need. Off by default so normal logs stay quiet.
const PAINT_STATS = process.env.ISOLA_PAINT_STATS === '1';
const paintStats = { paints: 0, full: 0, partial: 0, dirtyPx: 0, surfPx: 0 };
let paintStatsAt = Date.now();

// notePaint folds one paint event's reported damage into paintStats and emits
// a summary every 10s. `dirty` is the compositor's damage rect, or null when
// it repainted everything.
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

function connectToUds(sockPath) {
  return new Promise((resolve) => {
    const attempt = () => {
      const sock = net.createConnection(sockPath);
      sock.on('connect', () => {
        console.log(`[electron] connected to ${sockPath}`);
        resolve(sock);
      });
      sock.on('error', (err) => {
        console.log(`[electron] UDS connect error: ${err.message}, retrying in 500ms`);
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

// A blank surface of an EMPTY document must be one stable colour.
//
// A new tab is the one surface whose colour is ours to choose: Chromium paints
// an empty document on the browser canvas (white), while the dark theme must
// open a dark tab — and if the compositor ever hands back a different blank, the
// two variants alternate and the new tab FLICKERS between dark and white. That
// alternation is the "blink" on every new tab.
//
// Normalising to the theme background gives it one stable colour. Real pages are
// untouched: this only runs for about:blank, never for a document that paints
// its own colour.
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
  // Must cover everything written into `full`: a full-size bitmap (or a
  // near-full one, whose edges get extrapolated) updates the whole surface
  // regardless of what the compositor reported as damaged.
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
    // An empty paint image on a page that has nothing to paint is not a capture
    // stall. Chromium's offscreen compositor hands back a zero-length
    // copy-output for a blank, unchanging document — an about:blank tab after a
    // keepalive nudge is the clearest case — and counting those made a fresh tab
    // raise "capture stalled — recovering", start the screenshot fallback and
    // (because capturePage() comes back empty for the same reason) recreate its
    // renderer in a loop.
    //
    // The gate is deliberately narrow: only while the surface is blank AND the
    // document has finished loading, a state where an empty image can only mean
    // "nothing to show" and there is no picture to lose. Once the tab has
    // content on screen an empty image is the real stall signal again.
    // (capture_fallback.js warns that lastGoodPaintAt must not be used to leave
    // the fallback because keepalive dedups refresh it; this is not that — it is
    // a statement about what is on the surface.)
    if (nothingToPaint()) {
      dedupTotal++;
      return FRAME_DEDUP;
    }
    // Empty bitmap = the compositor had nothing to hand over. Rate-limited:
    // a wedged renderer produced ~14 of these before, one per capture, each
    // one a line of log. The count is kept for the stall watchdog.
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

  // A full-surface frame that is one colour (modulo a few stray pixels) is a
  // blank: the unpainted new document at a navigation commit, or a capturePage()
  // that came back empty. Forwarding it is the white page the viewer shows before
  // the new document paints, so hold the previous surface instead.
  //
  // Two independent signals, because each covers a case the other misses:
  //   * isLoading() is polled off the webContents, and catches the commit-time
  //     blank paint that reaches this handler *before* did-navigate delivers its
  //     callback (measured: a uniform white full frame 86 ms after the commit,
  //     while the hold window still held the previous load's expired deadline).
  //   * blankHoldUntil carries the window past did-finish-load, where the
  //     scheduled load captures can hand back an empty capturePage().
  //
  // Gated on surfaceEstablished (see shouldHoldBlank): a blank only needs hiding
  // when it would replace a picture. The surface belongs to a tab, so the first
  // frame after a tab becomes active always goes out — holding it would leave the
  // previous tab on screen for the whole BLANK_HOLD_MS window, which is exactly
  // the "new tab opens too slow" the hold caused.
  //
  // Never withheld: the frame that seeds the streamer's surface, so a blank first
  // frame still goes out and the viewer is never left without one. Returned as a
  // dedup because the compositor handed over a real image — it is simply empty —
  // so this must not read as a capture stall.
  const loading = !!(activeWin && activeWin.win && !activeWin.win.isDestroyed()
    && activeWin.win.webContents.isLoading());
  // A blank arrives as a partial patch at least as often as it arrives as a
  // whole surface: Chromium repaints part of the viewport at a navigation
  // commit, and blitting that patch onto the composed surface leaves the rest
  // of the frame showing the PREVIOUS document — a tear no browser presents.
  // Measured on unibet.com's country gateway: 40% of the page white on the
  // right, the old page intact on the left, twice in one country switch.
  //
  // Only a patch big enough to read as "the picture" is tested, and only while a
  // hold is possible at all: the scan costs ~22 ms on a full 1760p surface, which
  // an ordinary large partial (a scroll, an animation) must not pay on every
  // paint. A small uniform rect is content either way.
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

  // Dedup only against the same rect — a different rect means different
  // pixels and must always be sent. A dedup is not a capture stall: the
  // compositor handed over a usable image, it simply had nothing new in it,
  // which is the steady state on an unchanged page. It is counted on its own
  // so this side can be told apart from a stream that is actually moving.
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
  console.warn(`[electron] capture fallback stopped (${reason})`);
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

// captureStatsPayload is what the streamer forwards to viewers, so a capture
// stall is identifiable from a user's report instead of only from the sandbox
// log (the metric did not exist when the freeze was first reported).
function captureStatsPayload() {
  const now = Date.now();
  // Age the window even when paint has recovered. Otherwise the last burst
  // of empties stays in empty_window forever and the viewer never clears
  // "capture stalled".
  pruneEmptyCaptureTimes(emptyCaptureTimes, now, EMPTY_WINDOW_MS);
  return {
    ev: 'capture_stats',
    empty_total: emptyCaptures,
    empty_window: emptyCaptureTimes.length,
    fallback: fallbackTimer !== null,
    last_paint_ms: lastGoodPaintAt ? now - lastGoodPaintAt : -1,
    last_frame_ms: lastComposedAt ? now - lastComposedAt : -1,
    // last_paint_ms above cannot be trusted on its own: the keepalive nudge
    // repaints an unchanged page every 100ms, so it stays near zero whatever
    // the page is doing. last_sent_ms only moves when a frame with new pixels
    // goes out, and the two totals say how the paint traffic split between
    // real frames and no-op repaints.
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
    console.warn(`[electron] capturePage failed (${reason}): ${err.message}`);
  }
}

// ── Stall watchdog ──────────────────────────────────────────────────────
// Sends one runner event to every attached streamer (they multiplex tabs,
// renderer state and captures over the same ctrl socket as JSON lines).
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

// maybeClearStallReport clears the viewer's warning only on evidence that the
// picture is moving again: a frame composed after the stall was announced.
// Clearing on "we are inside the load grace window" instead meant a recovery
// that never produced a renderer still looked recovered.
function maybeClearStallReport() {
  if (!stallReported) return;
  if (stallAnnouncedAt !== 0 && lastComposedAt > stallAnnouncedAt) clearStallReport();
}

// confirmStalled asks the active tab's capture path directly. A healthy
// renderer answers with a real bitmap even for a static page (the keepalive
// nudges see to that), so an empty image here means the renderer stopped
// producing frames — not that nothing changed.
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
    console.warn(`[electron] stall probe failed: ${err.message}`);
    return true;
  }
}

// recoverStalledRenderer gives the active tab a fresh window (and therefore a
// fresh renderer process) at the same URL.
//
// Earlier attempts are worth recording: reload() alone does nothing for a
// renderer blocked in its main thread (the request queues behind the work that
// is stuck), and forcefullyCrashRenderer() + reload() left the tab with NO
// renderer process at all — no CDP answers, no capturePage, forever. A new
// BrowserWindow always comes with a new renderer.
// recoverStalledRenderer recreates the window only when the automatic budget
// allows it (opts.auto), or always for a manual/viewer request. It reports
// whether the window was actually replaced, so callers can keep the stall
// visible instead of assuming a recovery happened.
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

// announceRecoverySuppressed keeps the viewer's warning up — the picture really
// is stuck — while explaining once, per URL and reason, why nothing was
// reloaded. The manual recover action stays available.
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
    // A blank surface has nothing to compose, so silence here is not a stall.
    // Without this the watchdog recreates the renderer of a fresh tab (or any
    // all-white page) in a loop and the viewer keeps "recovering" up.
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
    // One recovery per cooldown window, and nothing at all while the automatic
    // budget has just refused one: a page that wedges its renderer on every load
    // must not turn into a reload loop, and a full-surface capturePage() every
    // 3s on a wedged renderer is its own cost.
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

// Bing and similar sites load hero/background images after did-finish-load.
// Offscreen paint often misses GPU-layer backgrounds — capturePage composites all
// layers. Every one of these is a full-surface ReadPixels, the expensive path on
// a software-GL page, so the early ones are skipped once this document has
// already put a full frame on the wire; the late one always runs.
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

// Applies the theme and returns the WINDOW backdrop. The two colours are
// deliberately different — see theme.js for why the backdrop is never the
// theme colour.
function themeBackgroundColor() {
  const colours = themeColours(BROWSER_THEME);
  bgColor = colours.emptyDocument;
  nativeTheme.themeSource = colours.themeSource;
  return colours.window;
}

function loadResolved(win, input) {
  const url = resolveAddressBar(input);
  if (!url) {
    console.log(`[electron] blocked address: ${String(input).slice(0, 200)}`);
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
    console.warn(`[electron] setFrameRate(${next}) failed: ${err.message}`);
  }
}

// setCaptureActive is driven by the streamer's viewer count. Pausing stops the
// keepalive nudges and makes every capture path a no-op, but leaves the page
// itself untouched (it keeps loading and rendering). Resuming re-seeds the
// streamer's surface with one full frame: the damage stream has a gap of
// unknown length behind it, so partial updates could not be applied to it.
function setCaptureActive(on) {
  if (on === captureActive) return;
  captureActive = on;
  const tab = activeTab();
  if (on) {
    resetFrameState();
    if (tab) startForwarding(tab);
    // Raise the rate before the seeding capture: a tab left at the background
    // rate could hand back a stale surface as the first frame.
    if (tab) setTabFrameRate(tab, true);
    if (tab && tab.win && !tab.win.isDestroyed()) tab.win.webContents.invalidate();
    void captureAndSend('viewer-attach');
  } else if (tab) {
    stopForwarding(tab);
    // Nobody is watching: composing this surface at the capture rate is waste.
    setTabFrameRate(tab, false);
    if (tab.partialTimer) {
      clearTimeout(tab.partialTimer);
      tab.partialTimer = null;
    }
    clearLoadCaptures();
  }
  console.log(`[electron] capture ${on ? 'resumed' : 'paused'}`);
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
  // The surface still holds the tab we are leaving, so the first frame from this
  // one must not be held back as a blank replacing a picture — it is the
  // replacement.
  target.surfaceEstablished = false;
  target.surfaceBlank = true;
  resetFrameState();
  clearLoadCaptures();
  // A fresh tab starts with a clean renderer-health slate: the previous tab's
  // stall (if any) said nothing about this one.
  stalledSince = 0;
  clearStallReport();
  // Restore full rate BEFORE the seeding capture, or the switch could hand the
  // viewer a surface the tab produced at the background rate.
  setTabFrameRate(target, true);
  startForwarding(target);
  try {
    target.win.webContents.focus();
    target.win.webContents.invalidate();
    void captureAndSend('tab-switch');
  } catch { /* ignore */ }
  broadcastTabs();
  console.log(`[electron] tab switch → #${id} (${target.url})`);
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
    // What the viewer's single surface currently holds. Used by the stall logic:
    // an empty paint image on a loaded blank surface is "nothing to paint".
    // Starts true: a new tab has no content yet.
    surfaceBlank: true,
    // Whether the surface holds something from THIS tab. Reset when the tab
    // becomes active (the surface still shows the tab we came from), set by the
    // first frame that goes out. This is what decides whether a blank would
    // replace a picture — see shouldHoldBlank.
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

// recreateTabWindow gives a tab a brand-new BrowserWindow at the same URL.
//
// This is the renderer-stall recovery, and it deliberately does not go through
// webContents.reload(): a renderer wedged in its main thread cannot service a
// reload, and after forcefullyCrashRenderer() the reload did not reliably
// respawn one either (measured: the tab was left with no renderer process at
// all, answering neither CDP nor capturePage forever). A new window always
// gets a new renderer process.
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
    console.warn(`[electron] old window destroy failed: ${err.message}`);
  }
  resetFrameState();
  attachTabWindow(tab, current || 'about:blank');
  if (tab.id === activeTabId) startForwarding(tab);
  broadcastTabs();
  console.log(`[electron] tab #${tab.id} window recreated (${reason}) at ${current}`);
}

// attachTabWindow builds the offscreen window for a tab and wires every event
// handler. Split out of createTab so a stall recovery can hand a tab a fresh
// window without touching the rest of its state.
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
      preload: path.join(__dirname, 'preload.js'),
      backgroundThrottling: false,
    },
  });
  // `tab.win` is assigned at the end of this function, so the rate is applied
  // there — one applier only: a window attached for a background tab starts at
  // the background rate, and whoever activates it raises it back (see
  // createTab/switchTab).

  win.webContents.on('paint', (event, dirty, image) => {
    if (tab.id !== activeTabId) return; // background tab: don't stream
    notePaint(dirty);
    const rects = dirty ? [{
      x: dirty.x, y: dirty.y, w: dirty.width, h: dirty.height,
    }] : [{ x: 0, y: 0, w, h }];
    const outcome = writeFrame(rects, image);
    // A dedup is a good paint: the compositor handed over a real image, the
    // page just had nothing new in it. Only an unusable image is a capture
    // stall — and only the outcome tells the two apart.
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

  // `painted` distinguishes a moment when the document has actually rendered
  // (a finished load, or an in-page navigation — both leave pixels on screen)
  // from a bare navigation *commit*, where the new document has usually not
  // painted yet. capturePage() at a commit returns a blank, background-coloured
  // image, and scheduleLoadCaptures() pushes that as a FULL-viewport frame —
  // one white flash per link click.
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
  // Wrapped rather than passed bare: a listener's first argument is the Event
  // object, which is truthy, so binding onNavigated directly would read as
  // `painted = true` and re-arm the pre-paint burst this guards against.
  win.webContents.on('did-navigate-in-page', () => onNavigated(true));
  win.webContents.on('did-navigate', () => onNavigated());
  win.webContents.on('page-title-updated', (e, title) => {
    tab.title = title || '';
    broadcastTabs();
  });
  win.webContents.on('did-fail-load', (event, code, desc, url) => {
    console.log(`[electron] tab #${id} load failed: ${url} (${code}: ${desc})`);
  });

  const blockDisallowed = (event, navUrl) => {
    if (isAllowedPageNavigation(navUrl)) return;
    event.preventDefault();
    console.log(`[electron] blocked navigation: ${String(navUrl).slice(0, 200)}`);
  };
  win.webContents.on('will-navigate', blockDisallowed);
  win.webContents.on('will-redirect', blockDisallowed);
  win.webContents.on('will-frame-navigate', blockDisallowed);

  // target=_blank / window.open / middle-click → a new sandbox tab.
  // Denying the native window lets the tab strip own all browsing surfaces.
  win.webContents.setWindowOpenHandler(({ url, disposition }) => {
    if (!isAllowedPageNavigation(url)) {
      console.log(`[electron] blocked window.open: ${String(url).slice(0, 200)}`);
      return { action: 'deny' };
    }
    const activate = disposition !== 'background-tab';
    createTab(url, activate);
    console.log(`[electron] window.open → new tab (${disposition}): ${url}`);
    return { action: 'deny' };
  });

  // Tor, when enabled, is applied to the shared default session at startup
  // (see app.whenReady) — before any window exists — so it is deliberately not
  // re-applied per window here.
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
      // A viewer-initiated "the picture is frozen" action recreates the window
      // with a fresh renderer regardless of the watchdog's timers and of the
      // automatic-recovery budget. `auto` is set only by the streamer's own
      // watchdog, which IS budgeted: a page that wedges every fresh renderer
      // must not become a reload loop.
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
      // Live viewport resize (viewer fullscreen / window drag). Bound and even
      // the request here as well as in the streamer: an odd or absurd size
      // would corrupt the H.264 encode the whole session shares.
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
      // Viewer-count gate from the streamer: with nobody attached there is
      // nothing to encode, so stop composing and shipping frames.
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
    console.log(`[electron] ctrl socket listening on ${ISOLA_CTRL_SOCK}`);
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
  console.log(`[electron] resized to ${w}x${h}`);
}

// Chromium switches live in ./chrome_flags.js: the list is load-bearing (see the
// --disable-dev-shm-usage note there) and is asserted by test/chrome_flags.test.js.
applyChromeFlags(app.commandLine);

app.whenReady().then(async () => {
  console.log(`[electron] starting (${w}x${h}, ${frameRate}fps, url=${START_URL})`);
  // Apply Tor to the shared default session BEFORE any tab exists. setProxy is
  // async, so a navigation issued without awaiting it would race the proxy and
  // could leave the first requests unproxied. On failure we exit instead of
  // serving: an unproxied session under a Tor request is the leak this feature
  // must never produce (the entrypoint's bootstrap gate fails closed too).
  if (TOR_ENABLED) {
    try {
      await applyTorProxy(session.defaultSession);
      console.log('[electron] Tor proxy applied (socks5, no bypass)');
    } catch (err) {
      console.error(`[electron] FATAL: could not apply Tor proxy: ${err && err.message ? err.message : err}`);
      app.exit(1);
      return;
    }
  }
  udsSocket = await connectToUds(ISOLA_FRAME_SOCK);
  createTab(START_URL, true);
  setupCtrlSocket();
  startStallWatchdog();
  startCaptureStatsReport();
  console.log('[electron] ready');
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
