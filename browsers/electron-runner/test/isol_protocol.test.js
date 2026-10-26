'use strict';

// Tests for the producer half of the isola damage transport. The streamer
// decodes these bytes in browsers/isola-streamer/frame_reader.go, so the
// layout asserted here is a wire contract, not an implementation detail.

const test = require('node:test');
const assert = require('node:assert');
const {
  encodeFrame,
  extractRect,
  clampRect,
  sameRect,
  isBlankBitmap,
  shouldHoldBlank,
  surfaceBlankAfterFrame,
  frameTimestampNs,
  frameOutcome,
  FRAME_MAGIC,
  FRAME_SENT,
  FRAME_DEDUP,
  FRAME_EMPTY,
} = require('../src/isol_protocol');

const W = 8;
const H = 4;

// A surface where every pixel is uniquely identifiable: BGRA = (i, i+1, i+2, 255).
function makeSurface(w, h) {
  const buf = Buffer.alloc(w * h * 4);
  for (let i = 0; i < w * h; i++) {
    buf[i * 4 + 0] = i & 0xff;
    buf[i * 4 + 1] = (i + 1) & 0xff;
    buf[i * 4 + 2] = (i + 2) & 0xff;
    buf[i * 4 + 3] = 255;
  }
  return buf;
}

function pixelAt(surface, w, x, y) {
  const off = (y * w + x) * 4;
  return surface.subarray(off, off + 4);
}

// Independent decoder written straight from the documented wire format (see
// the header comment in src/isol_protocol.js). It deliberately shares no code
// with the encoder, so a mistake common to both cannot hide.
function readFrame(buf) {
  const header = {
    magic: buf.readUInt32LE(0),
    width: buf.readUInt32LE(4),
    height: buf.readUInt32LE(8),
    stride: buf.readUInt32LE(12),
    format: buf.readUInt32LE(16),
    numDirty: buf.readUInt32LE(20),
    tsNs: buf.readBigUInt64LE(24),
  };
  let off = 32;
  const rects = [];
  for (let i = 0; i < header.numDirty; i++) {
    rects.push({
      x: buf.readUInt32LE(off),
      y: buf.readUInt32LE(off + 4),
      w: buf.readUInt32LE(off + 8),
      h: buf.readUInt32LE(off + 12),
    });
    off += 16;
  }
  return { ...header, rects, payload: buf.subarray(off) };
}

// Mirrors Frame.IsFull() in frame_reader.go: exactly one rect covering all.
function isFullFrame(f) {
  return (
    f.rects.length === 1 &&
    f.rects[0].x === 0 &&
    f.rects[0].y === 0 &&
    f.rects[0].w === f.width &&
    f.rects[0].h === f.height
  );
}

test('extractRect packs rows tightly instead of walking the surface stride', () => {
  const surface = makeSurface(W, H);
  const rect = { x: 2, y: 1, w: 3, h: 2 };
  const out = extractRect(surface, rect, W);

  assert.strictEqual(out.length, rect.w * rect.h * 4);
  const expected = Buffer.concat([
    pixelAt(surface, W, 2, 1),
    pixelAt(surface, W, 3, 1),
    pixelAt(surface, W, 4, 1),
    pixelAt(surface, W, 2, 2),
    pixelAt(surface, W, 3, 2),
    pixelAt(surface, W, 4, 2),
  ]);
  assert.ok(out.equals(expected), 'rect rows must be concatenated without padding');
});

test('extractRect of the whole surface reproduces the surface byte for byte', () => {
  const surface = makeSurface(W, H);
  const out = extractRect(surface, { x: 0, y: 0, w: W, h: H }, W);
  assert.ok(out.equals(surface));
});

test('clampRect clips damage that runs past the surface edge', () => {
  assert.deepStrictEqual(
    clampRect({ x: 6, y: 3, w: 10, h: 10 }, W, H),
    { x: 6, y: 3, w: 2, h: 1 },
  );
  assert.deepStrictEqual(
    clampRect({ x: 0, y: 0, w: W, h: H }, W, H),
    { x: 0, y: 0, w: W, h: H },
  );
});

test('clampRect degenerates a fully off-surface rect so writeFrame drops it', () => {
  // The streamer rejects out-of-bounds rects outright (which would desync the
  // stream), so the producer must never emit one.
  const out = clampRect({ x: 20, y: 20, w: 5, h: 5 }, W, H);
  assert.strictEqual(out.w, 0);
  assert.strictEqual(out.h, 0);
});

// The paint path is driven by a 100ms keepalive nudge, so most paints on a
// static page compose an image identical to what was already sent. That must
// read as a dedup — a healthy compositor with nothing new — and never as an
// empty capture, which is the counter that switches the runner to the
// screenshot fallback.
test('frameOutcome separates a no-op repaint from an empty capture', () => {
  const rect = { x: 0, y: 0, w: W, h: H };
  const payload = makeSurface(W, H);

  const sent = frameOutcome({ composited: true, rect, payload, lastPayload: null, lastRect: null });
  assert.strictEqual(sent, FRAME_SENT, 'a first frame is sent');

  const dedup = frameOutcome({
    composited: true,
    rect,
    payload,
    lastPayload: Buffer.from(payload),
    lastRect: rect,
  });
  assert.strictEqual(dedup, FRAME_DEDUP, 'an identical repaint is a dedup, not a stall');

  const changed = Buffer.from(payload);
  changed[0] ^= 0xff;
  const resent = frameOutcome({
    composited: true,
    rect,
    payload: changed,
    lastPayload: Buffer.from(payload),
    lastRect: rect,
  });
  assert.strictEqual(resent, FRAME_SENT, 'one changed pixel is a real frame');
});

test('frameOutcome only dedups the same rect', () => {
  // The dedup compares one payload against the last one sent for that rect. A
  // different rect means different pixels, so an alternating pair of rects must
  // not be mistaken for "nothing changed".
  const a = { x: 0, y: 0, w: 1, h: 1 };
  const b = { x: 2, y: 2, w: 1, h: 1 };
  const payload = Buffer.alloc(4, 0xab);

  const out = frameOutcome({
    composited: true,
    rect: b,
    payload,
    lastPayload: Buffer.alloc(4, 0xab),
    lastRect: a,
  });
  assert.strictEqual(out, FRAME_SENT);
});

test('frameOutcome reports an uncomposable image as empty', () => {
  const rect = { x: 0, y: 0, w: W, h: H };
  for (const args of [
    { composited: false, rect, payload: makeSurface(W, H) },
    { composited: true, rect: null, payload: makeSurface(W, H) },
    { composited: true, rect: { x: 0, y: 0, w: 0, h: 0 }, payload: null },
  ]) {
    assert.strictEqual(frameOutcome(args), FRAME_EMPTY);
  }
});

test('sameRect matches only identical geometry', () => {
  assert.ok(sameRect({ x: 1, y: 2, w: 3, h: 4 }, { x: 1, y: 2, w: 3, h: 4 }));
  assert.ok(!sameRect({ x: 1, y: 2, w: 3, h: 4 }, { x: 1, y: 2, w: 3, h: 5 }));
  assert.ok(!sameRect({ x: 0, y: 0, w: 3, h: 4 }, { x: 1, y: 0, w: 3, h: 4 }));
});

// A blank full-surface frame is one the runner must withhold rather than forward:
// the unpainted document at a navigation commit, or a capturePage() that came
// back empty. Measured over a real page load, such a frame is ~100% one colour
// but not *exactly* — a few stray pixels survive — so the test is a ratio, not
// an equality.
function flat(pixels, bgra = [255, 255, 255, 255]) {
  const buf = Buffer.alloc(pixels * 4);
  for (let i = 0; i < pixels; i++) {
    buf[i * 4 + 0] = bgra[0];
    buf[i * 4 + 1] = bgra[1];
    buf[i * 4 + 2] = bgra[2];
    buf[i * 4 + 3] = bgra[3];
  }
  return buf;
}

test('isBlankBitmap accepts a flat white image', () => {
  assert.equal(isBlankBitmap(flat(64 * 64)), true);
});

test('isBlankBitmap accepts a flat dark fill', () => {
  // #1a1a2e, the runner's dark-theme background: a blank is not always white.
  assert.equal(isBlankBitmap(flat(64 * 64, [0x2e, 0x1a, 0x1a, 0xff])), true);
});

test('isBlankBitmap accepts a blank with a few stray pixels', () => {
  // The measured case: the white frames carried 3-22 distinct values but were
  // otherwise uniform, which an equality test let through.
  const buf = flat(100 * 100);
  for (const px of [7, 1234, 5678, 9999]) buf[px * 4 + 1] = 0xfe;
  assert.equal(isBlankBitmap(buf), true);
});

test('isBlankBitmap rejects a frame with real content', () => {
  // Every pixel unique — the opposite extreme from a blank.
  assert.equal(isBlankBitmap(makeSurface(64, 64)), false);
});

test('isBlankBitmap accepts a blank that keeps the site chrome bar', () => {
  // Measured on unibet.com's country gateway: the navigation-commit blank is
  // white with the site's responsible-play bar across the top — 1.6-3.4% of a
  // 3448x1797 surface. At the old 2% allowance that real case read as content
  // and the white page went out, which is the flash this rule exists to stop.
  const w = 100;
  const h = 100;
  const buf = flat(w * h);
  for (let y = 0; y < 4; y++) {
    for (let x = 0; x < w; x++) {
      const off = (y * w + x) * 4;
      buf[off] = 0x14;
      buf[off + 1] = 0x3a;
      buf[off + 2] = 0x14;
      buf[off + 3] = 0xff;
    }
  }
  assert.equal(isBlankBitmap(buf), true);
});

test('the blank allowance stays under a sparse but real page', () => {
  // 8% differing pixels is still content: 5% must not swallow it.
  const buf = flat(100 * 100);
  for (let px = 0; px < 800; px++) buf[px * 4 + 1] = 0x10;
  assert.equal(isBlankBitmap(buf), false);
});

test('isBlankBitmap rejects a page that is mostly one colour', () => {
  // The least-uniform real frame measured was 84.8% one colour, so 85% must
  // still count as content: the 5% allowance cannot swallow a real page.
  const buf = flat(100 * 100);
  for (let px = 1500; px < 10000; px++) buf[px * 4 + 1] = 0x00;
  assert.equal(isBlankBitmap(buf), false);
});

test('isBlankBitmap tolerates a degenerate buffer', () => {
  assert.equal(isBlankBitmap(Buffer.alloc(0)), true);
  assert.equal(isBlankBitmap(Buffer.alloc(4)), true);
});

test('encodeFrame writes the header, rect table and damage payload the streamer decodes', () => {
  const surface = makeSurface(W, H);
  const rect = { x: 2, y: 1, w: 3, h: 2 };
  const payload = extractRect(surface, rect, W);
  const wire = encodeFrame(W, H, W * 4, [rect], payload, 123456789);

  const f = readFrame(wire);
  assert.strictEqual(f.magic, FRAME_MAGIC);
  assert.strictEqual(f.width, W);
  assert.strictEqual(f.height, H);
  assert.strictEqual(f.stride, W * 4);
  assert.strictEqual(f.format, 0, '0 = BGRA8888');
  assert.strictEqual(f.numDirty, 1);
  assert.strictEqual(f.tsNs, 123456789n);
  assert.deepStrictEqual(f.rects, [rect]);

  // The payload is the rects' pixels — NOT stride * height.
  assert.strictEqual(f.payload.length, rect.w * rect.h * 4);
  assert.ok(Buffer.from(f.payload).equals(payload));
  assert.strictEqual(wire.length, 32 + 16 + rect.w * rect.h * 4);
  assert.ok(!isFullFrame(f), 'a partial rect must not look like a full refresh');
});

test('a full-surface frame is what the receiver treats as a full refresh', () => {
  const surface = makeSurface(W, H);
  const rect = { x: 0, y: 0, w: W, h: H };
  const f = readFrame(encodeFrame(W, H, W * 4, [rect], surface, 1));

  assert.ok(isFullFrame(f));
  assert.ok(Buffer.from(f.payload).equals(surface));
});

test('a small damage update costs a fraction of a full frame', () => {
  const bigW = 1280;
  const bigH = 720;
  const surface = makeSurface(bigW, bigH);
  const caret = { x: 640, y: 360, w: 8, h: 8 };

  const damage = encodeFrame(
    bigW,
    bigH,
    bigW * 4,
    [caret],
    extractRect(surface, caret, bigW),
    1,
  );
  const full = encodeFrame(bigW, bigH, bigW * 4, [{ x: 0, y: 0, w: bigW, h: bigH }], surface, 1);

  assert.strictEqual(damage.length, 32 + 16 + caret.w * caret.h * 4);
  assert.ok(
    damage.length < full.length / 1000,
    `damage frame ${damage.length}B should be far smaller than full ${full.length}B`,
  );
});

// shouldHoldBlank is the anti-flash rule for blanks. It lives here so the
// branch that used to delay a brand-new tab is covered: the runner withholds a
// blank only when the surface it would replace holds a picture.
test('a blank is withheld once the tab has established the surface', () => {
  const base = {
    blank: true,
    streamerSeeded: true,
    surfaceEstablished: true,
    loading: false,
    withinHoldWindow: false,
  };
  // The commit-time blank of a link click: still loading the new document.
  assert.strictEqual(shouldHoldBlank({ ...base, loading: true }), true);
  // Past did-finish-load, the scheduled load captures can still return an
  // empty frame, so the time window holds the blank too.
  assert.strictEqual(shouldHoldBlank({ ...base, withinHoldWindow: true }), true);
});

test('the first frame of an active tab is never held (new tab opens at once)', () => {
  // The regression: a tab that has not put anything on the surface yet must show
  // its first frame. Holding it left the previous tab's picture up for the whole
  // hold window — "new tab opens too slow".
  assert.strictEqual(
    shouldHoldBlank({
      blank: true,
      streamerSeeded: true,
      surfaceEstablished: false,
      loading: true,
      withinHoldWindow: true,
    }),
    false,
  );
});

test('a blank is only withheld when it would replace a picture', () => {
  const pictured = {
    blank: true,
    streamerSeeded: true,
    surfaceEstablished: true,
    loading: true,
    withinHoldWindow: true,
  };
  // Real content is never withheld — it is the picture.
  assert.strictEqual(shouldHoldBlank({ ...pictured, blank: false }), false);
  // Nothing is withheld before the streamer has a surface at all (the seed).
  assert.strictEqual(shouldHoldBlank({ ...pictured, streamerSeeded: false }), false);
  // The hold is bounded: once the window closes, a page that really is one
  // flat colour still appears.
  assert.strictEqual(
    shouldHoldBlank({ ...pictured, loading: false, withinHoldWindow: false }),
    false,
  );
});

test('a blank partial patch is withheld like a blank full frame', () => {
  // The recorded tear: Chromium repaints part of the viewport white at a
  // navigation commit and the runner blitted it onto the composed surface,
  // leaving the untouched remainder on screen as the previous document. The
  // rule must not care whether the blank arrived as a whole surface or a rect.
  assert.strictEqual(
    shouldHoldBlank({
      blank: true,
      streamerSeeded: true,
      surfaceEstablished: true,
      loading: false,
      withinHoldWindow: true,
    }),
    true,
  );
});

// frameTimestampNs feeds the header's ts_ns, which frame_reader.go decodes as
// time.Unix(0, ts). The regression it guards is process.hrtime.bigint(): a
// nanosecond-looking value counting from boot, which decodes as 1970.
test('a partial damage frame leaves a picture on the surface', () => {
  // Content that arrived as partial damage still means there is a picture to
  // protect. Treating such a page as "never painted" forwarded its next commit
  // blank — the white flash that came back after the new-tab fix.
  assert.strictEqual(surfaceBlankAfterFrame({ isFull: false, blank: false }), false);
  // A blank full frame is the whole surface.
  assert.strictEqual(surfaceBlankAfterFrame({ isFull: true, blank: true }), true);
  assert.strictEqual(surfaceBlankAfterFrame({ isFull: true, blank: false }), false);
});

test('frame ts_ns is wall-clock UTC nanoseconds, not the monotonic clock', () => {
  const ns = frameTimestampNs();
  assert.strictEqual(typeof ns, 'bigint');
  const wall = BigInt(Date.now()) * 1_000_000n;
  const drift = ns > wall ? ns - wall : wall - ns;
  assert.ok(
    drift < 60n * 1_000_000_000n,
    `ts_ns ${ns} drifted ${drift}ns from the wall clock ${wall}`,
  );
  // Sub-millisecond and monotonic within a session.
  assert.ok(frameTimestampNs() >= ns);
});
