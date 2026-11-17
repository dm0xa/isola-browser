// ISOL frame protocol encoder.
// Writes binary frames over a Unix domain socket in the format expected by
// browsers/isola-streamer/frame_reader.go:
//
//   32-byte header: magic(4) width(4) height(4) stride(4) format(4) num_dirty(4) ts_ns(8)
//   num_dirty × 16-byte dirty rects (x, y, w, h × uint32 LE)
//   the dirty rects' BGRA pixels, row-major, concatenated in rect order
//
// Only damaged regions travel; a whole-surface rect is a full refresh.

const FRAME_MAGIC = 0x4c4f5349; // 'ISOL'

// ts_ns must be CLOCK_REALTIME (frame_reader.go decodes it as time.Unix(0, ts));
// hrtime.bigint() counts from boot and would stamp frames in 1970. Date.now()
// anchored to a monotonic reference keeps sub-ms deltas immune to NTP steps.
const WALL_REF_MS = Date.now();
const MONO_REF_NS = process.hrtime.bigint();

function frameTimestampNs() {
  return BigInt(WALL_REF_MS) * 1_000_000n + (process.hrtime.bigint() - MONO_REF_NS);
}

function encodeFrame(width, height, stride, dirtyRects, pixels, timestampNs) {
  const numDirty = dirtyRects.length;
  const headerSize = 32;
  const dirtySize = numDirty * 16;
  // `pixels` is the concatenation of the dirty rects' BGRA rows (NOT a full
  // frame) — the receiver derives its length from the rects it was sent.
  const pixelSize = pixels.length;

  const buf = Buffer.alloc(headerSize + dirtySize + pixelSize);
  let off = 0;

  // Header
  buf.writeUInt32LE(FRAME_MAGIC, off);         off += 4;  // magic
  buf.writeUInt32LE(width, off);               off += 4;  // width
  buf.writeUInt32LE(height, off);              off += 4;  // height
  buf.writeUInt32LE(stride, off);              off += 4;  // stride
  buf.writeUInt32LE(0, off);                   off += 4;  // format (0 = BGRA8888)
  buf.writeUInt32LE(numDirty, off);            off += 4;  // num_dirty_rects
  buf.writeBigUInt64LE(BigInt(timestampNs), off); off += 8;  // timestamp ns

  // Dirty rects
  for (const r of dirtyRects) {
    buf.writeUInt32LE(r.x, off); off += 4;
    buf.writeUInt32LE(r.y, off); off += 4;
    buf.writeUInt32LE(r.w, off); off += 4;
    buf.writeUInt32LE(r.h, off); off += 4;
  }

  // Pixel payload
  pixels.copy(buf, off);

  return buf;
}

// Copies a sub-rectangle out of a full BGRA surface into a tightly packed
// buffer (no row padding). This is the wire layout for one dirty rect: rows
// are concatenated with no stride, matching payloadBytes() in frame_reader.go.
// `surfaceW` is the full surface width, which is also its stride in pixels.
function extractRect(full, r, surfaceW) {
  const rowBytes = r.w * 4;
  const out = Buffer.allocUnsafe(rowBytes * r.h);
  for (let row = 0; row < r.h; row++) {
    const srcOff = (r.y + row) * surfaceW * 4 + r.x * 4;
    full.copy(out, row * rowBytes, srcOff, srcOff + rowBytes);
  }
  return out;
}

// Clips a rect to the surface so the streamer never receives out-of-bounds
// damage (its decoder rejects such frames outright, which would desync the
// stream). Degenerate rects clamp to zero width/height and are dropped.
function clampRect(r, surfaceW, surfaceH) {
  const x = Math.max(0, Math.min(r.x, surfaceW));
  const y = Math.max(0, Math.min(r.y, surfaceH));
  return {
    x,
    y,
    w: Math.max(0, Math.min(r.w, surfaceW - x)),
    h: Math.max(0, Math.min(r.h, surfaceH - y)),
  };
}

function sameRect(a, b) {
  return a.x === b.x && a.y === b.y && a.w === b.w && a.h === b.h;
}

// dominantColour: mode of a pixel-aligned sample; the page's colour beats a chrome bar by area.
function dominantColour(bitmap, sampleMax = 1024) {
  const pixels = bitmap.length >>> 2;
  const step = Math.max(1, Math.floor(pixels / sampleMax)) * 4;
  const counts = new Map();
  let best = bitmap.readUInt32LE(0);
  let bestCount = 0;
  for (let i = 0; i + 4 <= bitmap.length; i += step) {
    const key = bitmap.readUInt32LE(i);
    const n = (counts.get(key) || 0) + 1;
    counts.set(key, n);
    if (n > bestCount) {
      bestCount = n;
      best = key;
    }
  }
  return best;
}

// isBlankBitmap: a navigation-commit blank is ~100% one colour with a few stray
// pixels (3-22 values measured), so exact uniformity misses it; the least-uniform
// real page measured 84.8%. Reference = the sample mode (bitmap[0] can be a top
// chrome bar, 1.6-3.4% of the surface). 5% allowance covers the chrome with 3x
// margin. The count exits early on real content; only a blank pays the full scan.
function isBlankBitmap(bitmap, maxDifferingRatio = 0.05) {
  if (bitmap.length < 8) return true;
  const allowed = Math.floor((bitmap.length / 4) * maxDifferingRatio);
  const ref = dominantColour(bitmap);
  let differing = 0;
  for (let i = 0; i + 4 <= bitmap.length; i += 4) {
    if (bitmap.readUInt32LE(i) !== ref) {
      differing++;
      if (differing > allowed) return false;
    }
  }
  return true;
}

// shouldHoldBlank — the anti-flash rule: a committed blank document must not
// replace the picture (a full blank, or a large blank patch that would tear).
// Gate on surfaceEstablished (the CURRENT tab owns the surface; a tab's first
// frame always goes out), not on "has painted real content" — partial damage
// also establishes a picture. The seed frame is exempt.
function shouldHoldBlank({ blank, streamerSeeded, surfaceEstablished, loading, withinHoldWindow }) {
  return Boolean(blank && streamerSeeded && surfaceEstablished && (loading || withinHoldWindow));
}

// surfaceBlankAfterFrame: only a blank FULL frame blanks the surface — a patch
// sits on top of a picture. This is what the stall gate reads back.
function surfaceBlankAfterFrame({ isFull, blank }) {
  return Boolean(isFull && blank);
}

// What a composition attempt produced. The three non-idle outcomes all mean
// "the compositor handed us an image", which is why the age metrics the runner
// reports cannot distinguish them on their own:
const FRAME_SENT = 'sent';   // written to the streamer
const FRAME_DEDUP = 'dedup'; // composed, but identical to what was already sent
const FRAME_EMPTY = 'empty'; // nothing usable came back — a capture stall
const FRAME_IDLE = 'idle';   // capture paused or the socket is down

// frameOutcome: empty (capture stall) vs dedup (no-op repaint) — dedup climbing
// while sent stalls is the frozen-picture signature the compositor can still produce.
function frameOutcome({ composited, rect, payload, lastPayload, lastRect }) {
  if (!composited || !rect || rect.w <= 0 || rect.h <= 0) return FRAME_EMPTY;
  if (
    lastPayload &&
    lastRect &&
    sameRect(lastRect, rect) &&
    lastPayload.length === payload.length &&
    payload.equals(lastPayload)
  ) {
    return FRAME_DEDUP;
  }
  return FRAME_SENT;
}

module.exports = {
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
  FRAME_IDLE,
};
