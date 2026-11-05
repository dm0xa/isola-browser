// A/B harness driver for the isola streamer. Throwaway tooling, not part of the
// shipped binary: it feeds a deterministic corpus over the ISOL unix socket,
// attaches a WebCodecs viewer, captures what the viewer receives, and prints
// JSON metrics. `scripts/ab/README.md` explains the workflow.
//
//   node driver.mjs --sock <uds> --port <p> --w 1280 --h 720 --frames 100 --fps 25 \
//                   --out <file.h264> --refs <dir> [--attackers N --attack-ms MS] \
//                   [--scroll 1]
//
// The corpus is a static dense-text page (5x7 glyph rows — the workload that
// matters) with one small 96x64 block animating at 25fps. Page text stays
// pixel-identical in every frame, so the quality score does not depend on
// matching decoded frame N to a specific fed frame: the encoder is CFR and the
// frame pump is free to repeat or skip a feed. The animated block is ~1% of the
// surface and is identical for both arms.

import net from 'node:net';
import fs from 'node:fs';

const argv = Object.fromEntries(
  process.argv.slice(2).reduce((acc, a, i, arr) => {
    if (a.startsWith('--')) acc.push([a.slice(2), arr[i + 1]]);
    return acc;
  }, []),
);
const SOCK = argv.sock;
const PORT = Number(argv.port ?? 5900);
const W = Number(argv.w ?? 1280);
const H = Number(argv.h ?? 720);
const N = Number(argv.frames ?? 100);
const FPS = Number(argv.fps ?? 25);
const OUT = argv.out;
const REFS = argv.refs;
const ATTACKERS = Number(argv.attackers ?? 0);
const ATTACK_MS = Number(argv['attack-ms'] ?? 0);
const SCROLL = (argv.scroll ?? '0') !== '0';

// ── corpus ────────────────────────────────────────────────────────────────

function lcg(seed) {
  let s = seed >>> 0;
  return () => ((s = (s * 1664525 + 1013904223) >>> 0) / 0x100000000);
}

const MARGIN = Math.round(W * 0.02);
const GLYPH_W = 5;
const GLYPH_H = 7;
const PITCH_X = GLYPH_W + 1;
const PITCH_Y = GLYPH_H + 2;
const lineCount = Math.floor((H - 2 * MARGIN) / PITCH_Y);
const perLine = Math.floor((W - 2 * MARGIN) / PITCH_X);
const stride = W * 4;
const surf = new Uint8Array(stride * H);
const PAGE = new Uint8Array(stride * H);

{
  PAGE.fill(0xf2);
  const rnd = lcg(0x15f01a);
  for (let y = 0; y < Math.round(H * 0.06); y++) {
    for (let x = 0; x < W; x++) {
      const o = y * stride + x * 4;
      PAGE[o] = 0x1f; PAGE[o + 1] = 0x33; PAGE[o + 2] = 0x5f;
    }
  }
  for (let line = 0; line < lineCount; line++) {
    const y0 = MARGIN + line * PITCH_Y;
    if (y0 + GLYPH_H >= H - MARGIN) continue;
    for (let col = 0; col < perLine; col++) {
      if (rnd() >= 0.55) continue;
      const x0 = MARGIN + col * PITCH_X;
      for (let gy = 0; gy < GLYPH_H; gy++) {
        for (let gx = 0; gx < GLYPH_W; gx++) {
          const edge = gy === 0 || gy === GLYPH_H - 1 || gx === 0 || gx === GLYPH_W - 1;
          if (!edge && gx !== 2) continue;
          const o = (y0 + gy) * stride + (x0 + gx) * 4;
          PAGE[o] = 0x14; PAGE[o + 1] = 0x14; PAGE[o + 2] = 0x18;
        }
      }
    }
  }
}

const BLOCK_W = 96;
const BLOCK_H = 64;

function paint(frame) {
  surf.set(PAGE);
  if (SCROLL) {
    // Vertical scroll between frames. The frame pump may shed a feed, so this
    // corpus cannot be scored frame-to-frame; it is only useful for inspecting
    // what the encoder does under real motion.
    const rows = (frame * 2) % H;
    if (rows > 0) {
      surf.copyWithin(0, rows * stride, H * stride);
      surf.set(PAGE.subarray((H - rows) * stride, H * stride), (H - rows) * stride);
    }
  }
  const bx = MARGIN + ((frame * 7) % Math.max(1, W - 2 * MARGIN - BLOCK_W));
  const by = H - MARGIN - BLOCK_H;
  for (let y = 0; y < BLOCK_H; y++) {
    for (let x = 0; x < BLOCK_W; x++) {
      const o = (by + y) * stride + (bx + x) * 4;
      const v = ((x >> 4) + (y >> 4) + frame) & 1 ? 0xe0 : 0x30;
      surf[o] = v; surf[o + 1] = v; surf[o + 2] = v;
    }
  }
}

function frameHeader(tsNs) {
  const hdr = Buffer.alloc(32);
  hdr.writeUInt32LE(0x4c4f5349, 0); // 'ISOL'
  hdr.writeUInt32LE(W, 4);
  hdr.writeUInt32LE(H, 8);
  hdr.writeUInt32LE(stride, 12);
  hdr.writeUInt32LE(0, 16); // BGRA
  hdr.writeUInt32LE(1, 20); // one dirty rect = full surface
  hdr.writeBigUInt64LE(BigInt(Math.round(tsNs)), 24);
  const rect = Buffer.alloc(16);
  rect.writeUInt32LE(0, 0);
  rect.writeUInt32LE(0, 4);
  rect.writeUInt32LE(W, 8);
  rect.writeUInt32LE(H, 12);
  return Buffer.concat([hdr, rect]);
}

function writePPM(path, frame) {
  paint(frame);
  const px = Buffer.alloc(W * H * 3);
  for (let i = 0, o = 0; i < W * H; i++, o += 4) {
    px[i * 3] = surf[o + 2];
    px[i * 3 + 1] = surf[o + 1];
    px[i * 3 + 2] = surf[o];
  }
  fs.writeFileSync(path, Buffer.concat([Buffer.from(`P6\n${W} ${H}\n255\n`), px]));
}

// ── viewer ────────────────────────────────────────────────────────────────

const outStream = fs.createWriteStream(OUT);
const metrics = { packets: 0, bytes: 0, idr: 0, firstPacketMs: null, init: null };
const startedAt = Date.now();

function attachViewer() {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(`ws://127.0.0.1:${PORT}/ws/ab?mode=webcodecs`);
    ws.binaryType = 'arraybuffer';
    ws.onopen = () => resolve(ws);
    ws.onerror = (e) => reject(new Error(`viewer ws: ${e.message ?? e}`));
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        try { metrics.init = JSON.parse(ev.data); } catch { /* metrics tick */ }
        return;
      }
      const buf = Buffer.from(ev.data);
      if (buf.length <= 10) return;
      if (buf[0] !== 0x02) return;
      if (metrics.firstPacketMs === null) metrics.firstPacketMs = Date.now() - startedAt;
      metrics.packets++;
      metrics.bytes += buf.length - 10;
      if (buf[1] & 1) metrics.idr++;
      outStream.write(buf.subarray(10));
    };
  });
}

function attachAttacker() {
  const ws = new WebSocket(`ws://127.0.0.1:${PORT}/ws/ab-attack?mode=webcodecs`);
  ws.binaryType = 'arraybuffer';
  ws.onmessage = () => {};
  ws.onopen = () => {
    const t = setInterval(() => {
      try { ws.send(JSON.stringify({ type: 'request_keyframe' })); } catch { /* closed */ }
    }, 1000);
    setTimeout(() => { clearInterval(t); ws.close(); }, ATTACK_MS);
  };
  return ws;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ── main ──────────────────────────────────────────────────────────────────

const viewer = await attachViewer();
for (let i = 0; i < ATTACKERS; i++) attachAttacker();

if (REFS) {
  fs.mkdirSync(REFS, { recursive: true });
  if (SCROLL) {
    for (let i = 0; i < Math.min(N, 30); i++) {
      writePPM(`${REFS}/ref_${String(i).padStart(4, '0')}.ppm`, i);
    }
  } else {
    writePPM(`${REFS}/ref_0000.ppm`, 0);
  }
}

const sock = net.connect(SOCK);
await new Promise((res, rej) => { sock.on('connect', res); sock.on('error', rej); });

const feedStart = Date.now();
for (let i = 0; i < N; i++) {
  paint(i);
  const ok = sock.write(Buffer.concat([frameHeader(Date.now() * 1e6), Buffer.from(surf)]));
  if (!ok) await new Promise((r) => sock.once('drain', r));
  const due = feedStart + ((i + 1) * 1000) / FPS;
  const wait = due - Date.now();
  if (wait > 0) await sleep(wait);
}
await sleep(600);
sock.end();
viewer.close();
await new Promise((r) => outStream.end(r));

console.log(JSON.stringify({
  ...metrics,
  w: W, h: H, frames: N, fps: FPS,
  feedMs: Date.now() - feedStart,
  codec: metrics.init?.codec, bitrate_kbps: metrics.init?.bitrate_kbps,
  encode_fps: metrics.init?.encode_fps,
}));
