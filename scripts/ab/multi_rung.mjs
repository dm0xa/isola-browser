// Multi-viewer rung harness for the isola streamer. Throwaway tooling, not part
// of the shipped binary.
//
// It feeds the same deterministic corpus driver.mjs uses over the ISOL unix
// socket and attaches N real WebCodecs viewers, each asking for its own ABR
// rung via {type:'set_bitrate'}. That is the case the per-session encoder fan-out
// has to bound: every distinct (kbps, scale) a viewer asks for used to be a
// separate ffmpeg, so three viewers on three rungs meant three encoders in a
// three-CPU container.
//
//   node multi_rung.mjs --sock <uds> --port <p> --w 1280 --h 720 --base 4000 \
//                       --frames 200 --fps 25 [--rungs 4000,2800,1800] \
//                       [--switch-ms 800]
//
// Prints one JSON line: per-viewer stream_init history (so a viewer that landed
// on a rung other than the one it asked for is visible), packets, resets, and
// the total fed frames. CPU comes from wrapping the streamer in
// /usr/bin/time -l, exactly like ab-run.sh.

import net from 'node:net';

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
const N = Number(argv.frames ?? 200);
const FPS = Number(argv.fps ?? 25);
const BASE = Number(argv.base ?? 4000);
const SWITCH_MS = Number(argv['switch-ms'] ?? 800);
const SCROLL = (argv.scroll ?? '0') !== '0';
const RUNGS = (argv.rungs ?? '')
  .split(',')
  .map((v) => Number(v))
  .filter((v) => v > 0);

// ── corpus (identical to driver.mjs: static text + one animating block) ────

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
    // Every row moves every frame: the whole surface is dirty, so the encoder
    // does maximal work. This is the CPU worst case, not a quality corpus.
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

// ── viewers ───────────────────────────────────────────────────────────────

function attachViewer(idx, wantKbps) {
  return new Promise((resolve, reject) => {
    const m = { idx, wantKbps, inits: [], resets: 0, packets: 0, bytes: 0 };
    const ws = new WebSocket(`ws://127.0.0.1:${PORT}/ws/ab?mode=webcodecs`);
    ws.binaryType = 'arraybuffer';
    ws.onopen = () => resolve({ ws, m });
    ws.onerror = (e) => reject(new Error(`viewer ${idx} ws: ${e.message ?? e}`));
    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        try {
          const msg = JSON.parse(ev.data);
          if (msg.type === 'stream_init') {
            m.inits.push({
              bitrate_kbps: msg.bitrate_kbps,
              width: msg.width,
              height: msg.height,
              base_bitrate: msg.base_bitrate,
            });
          } else if (msg.type === 'stream_reset') {
            m.resets++;
          }
        } catch { /* metrics tick */ }
        return;
      }
      const buf = Buffer.from(ev.data);
      if (buf.length <= 10) return;
      m.packets++;
      m.bytes += buf.length - 10;
    };
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ── main ──────────────────────────────────────────────────────────────────

const viewers = [];
for (let i = 0; i < Math.max(1, RUNGS.length); i++) {
  viewers.push(await attachViewer(i, RUNGS[i] ?? BASE));
}
await sleep(500);

const sock = net.connect(SOCK);
await new Promise((res, rej) => { sock.on('connect', res); sock.on('error', rej); });

const feedStart = Date.now();
let switched = false;
for (let i = 0; i < N; i++) {
  paint(i);
  const ok = sock.write(Buffer.concat([frameHeader(Date.now() * 1e6), Buffer.from(surf)]));
  if (!ok) await new Promise((r) => sock.once('drain', r));
  if (!switched && Date.now() - feedStart >= SWITCH_MS) {
    switched = true;
    for (const { ws, m } of viewers) {
      if (m.wantKbps !== BASE) ws.send(JSON.stringify({ type: 'set_bitrate', kbps: m.wantKbps }));
    }
  }
  const due = feedStart + ((i + 1) * 1000) / FPS;
  const wait = due - Date.now();
  if (wait > 0) await sleep(wait);
}
await sleep(800);
sock.end();
for (const { ws } of viewers) ws.close();

console.log(JSON.stringify({
  w: W, h: H, frames: N, fps: FPS, base: BASE,
  feedMs: Date.now() - feedStart,
  viewers: viewers.map(({ m }) => ({
    idx: m.idx,
    want_kbps: m.wantKbps,
    // Every stream_init this viewer received, in order: the last one is where
    // it ended up, and anything before it is an ABR step.
    inits: m.inits,
    resets: m.resets,
    packets: m.packets,
    bytes: m.bytes,
  })),
}));
