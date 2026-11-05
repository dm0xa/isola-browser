#!/usr/bin/env node
// Soak harness: one streamer process, one continuous session, N minutes.
//
// `driver.mjs` measures a steady stream and `chaos.mjs` drives each edge in its
// own fresh process. Neither can answer the question a long-lived session
// raises: does anything accumulate? A leak, an fd that never closes, a thread
// per attach, an encoder run that is never reaped, or a change-driven
// suppression latch that never wakes up again all look identical in a 30 s
// scenario — and all of them show up as a trend over ten minutes.
//
// This file boots ONE streamer, ONE feeder and ONE long-lived viewer, then
// mixes the normal workload into it: viewer churn every 20 s, ABR steps every
// 45 s (including stepping back up), a forced keyframe every 60 s, one resize
// round trip, and one two-minute static stretch to exercise change-driven
// suppression and its wake-up. Every 5 s it samples process state (RSS, fds,
// threads, child ffmpeg), hub state (/debug/encoder: subscribers,
// encoder_runs), and viewer state (packets/s, resets). Anomalous log lines are
// indexed as they appear.
//
//   node scripts/ab/soak.mjs --bin /tmp/q-soak --minutes 10
//   node scripts/ab/soak.mjs --bin /tmp/q-soak --minutes 2   # smoke run
//
// Output: a per-minute table and a verdict on stdout, `soak-samples.csv` and
// `soak-result.json` under --out (default /tmp/isola-soak). The numbers from a
// 10-minute run live in docs/streamer-soak.md.
//
// Exit code: 0 PASS, 1 FAIL, 2 harness error.

import net from 'node:net';
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { spawn, spawnSync, execFile } from 'node:child_process';
import { promisify } from 'node:util';

const execFileP = promisify(execFile);

// ── argv ─────────────────────────────────────────────────────────────────

const argv = {};
for (let i = 2; i < process.argv.length; i++) {
  const a = process.argv[i];
  if (!a.startsWith('--')) continue;
  const key = a.slice(2);
  const next = process.argv[i + 1];
  if (next === undefined || next.startsWith('--')) argv[key] = true;
  else { argv[key] = next; i++; }
}

const ROOT = path.resolve(path.dirname(new URL(import.meta.url).pathname), '../..');
const OUT = path.resolve(argv.out ?? '/tmp/isola-soak');
const MINUTES = Number(argv.minutes ?? 10);
const SAMPLE_MS = Number(argv['sample-ms'] ?? 5000);
const PORT = Number(argv.port ?? 17400);
const W = Number(argv.w ?? 1280);
const H = Number(argv.h ?? 720);
const KBPS = Number(argv.kbps ?? 6000);
const FPS = 25; // encoder mode caps at 25 (encoder.go fpsCapFor)
const D = Math.round(MINUTES * 60000);
if (!(D > 0)) throw new Error('--minutes must be > 0');

// Phase schedule. At the canonical 10 minutes this is exactly the brief:
// resize round trip at 2:30/3:00, static 4:00–6:00, motion again after. Short
// runs scale the phase boundaries (churn/ABR/keyframe cadences stay fixed) so
// a 2-minute smoke still touches every phase.
const RATIO = D / 600000;
const T_RESIZE_OUT = Math.round(150000 * RATIO);
const T_RESIZE_BACK = Math.round(180000 * RATIO);
const T_STATIC_START = Math.round(0.40 * D);
const T_STATIC_END = Math.min(Math.round(0.60 * D), T_STATIC_START + Math.round(Math.min(120000, 0.2 * D)));

// Cadences from the brief.
const CHURN_EVERY_MS = 20000;
const ABR_EVERY_MS = 45000;
const KEYFRAME_EVERY_MS = 60000;

// ABR ladder, base 6000. Contains down-steps AND up-steps, and cycles so the
// last third runs at the base rung again where the first third did: the packet
// rate comparison at the end of the run is therefore like-for-like.
const ABR_STEPS = [4200, 2700, 1800, 6000, 3600, 6000];

// Limits the verdict enforces. Each is justified in docs/streamer-soak.md.
const LIMITS = {
  rssSlopeKbPerMin: 2048,
  rssThirdGrowthKb: 12288,
  fdSlack: 4,
  threadSlack: 6,
  attachP95Ms: 600,
  attachMaxMs: 2600,
  minMotionPps: 15,
  ppsDecayRatio: 0.85,
  staticPpsMin: 0.15,
  staticPpsMax: 2.5,
  staticWakeMs: 400,
  encoderRunsMax: 2,
  resetRecoverMaxMs: 3000,
};

const BIN = argv.bin
  ? path.resolve(String(argv.bin))
  : path.join(OUT, 'isola-streamer');
if (!argv.bin) {
  fs.mkdirSync(OUT, { recursive: true });
  const build = spawnSync('go', ['build', '-o', BIN, '.'], {
    cwd: path.join(ROOT, 'browsers/isola-streamer'),
    encoding: 'utf8',
  });
  if (build.status !== 0) {
    console.error(build.stdout, build.stderr);
    throw new Error('go build failed');
  }
}

// ── small helpers ────────────────────────────────────────────────────────

const sleep = (ms) => new Promise((r) => setTimeout(r, Math.max(0, ms)));
const nowMs = () => performance.now();
const round = (n, d = 2) => (n === null || n === undefined || !Number.isFinite(n) ? null : Math.round(n * 10 ** d) / 10 ** d);

function quantile(arr, p) {
  if (!arr.length) return null;
  const s = [...arr].sort((a, b) => a - b);
  return s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))];
}

async function poll(fn, ms, step = 25) {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (await fn()) return true;
    await sleep(step);
  }
  return false;
}

// Ordinary least squares slope in KB per minute over the given samples.
function rssSlopeKbPerMin(samples) {
  if (samples.length < 8) return null;
  const xs = samples.map((s) => s.t_s / 60);
  const ys = samples.map((s) => s.rss_kb);
  const n = xs.length;
  const mx = xs.reduce((a, b) => a + b, 0) / n;
  const my = ys.reduce((a, b) => a + b, 0) / n;
  let num = 0;
  let den = 0;
  for (let i = 0; i < n; i++) {
    num += (xs[i] - mx) * (ys[i] - my);
    den += (xs[i] - mx) ** 2;
  }
  return den === 0 ? 0 : num / den;
}

// ── corpus + ISOL feeder ─────────────────────────────────────────────────
// Same workload as chaos.mjs/driver.mjs: dense static text with one animated
// block, sent as a full-surface damage rect in the real ISOL wire format.
// `motion` off paints the identical frame every tick, which is what makes the
// static stretch actually static at the composer (changeSeq only advances on a
// real pixel change).

class Corpus {
  constructor(w, h) {
    this.w = w; this.h = h;
    this.stride = w * 4;
    this.page = new Uint8Array(this.stride * h);
    const margin = Math.round(w * 0.02);
    const gw = 5, gh = 7, px = gw + 1, py = gh + 2;
    const lines = Math.floor((h - 2 * margin) / py);
    const perLine = Math.floor((w - 2 * margin) / px);
    let seed = 0x1234abcd;
    const rnd = () => ((seed = (seed * 1103515245 + 12345) >>> 0) / 0xffffffff);
    for (let y = 0; y < Math.round(h * 0.06); y++) {
      for (let x = 0; x < w; x++) {
        const o = y * this.stride + x * 4;
        this.page[o] = 0x1f; this.page[o + 1] = 0x33; this.page[o + 2] = 0x5f;
      }
    }
    for (let line = 0; line < lines; line++) {
      const y0 = margin + line * py;
      for (let col = 0; col < perLine; col++) {
        if (rnd() >= 0.55) continue;
        const x0 = margin + col * px;
        for (let gy = 0; gy < gh; gy++) {
          for (let gx = 0; gx < gw; gx++) {
            const edge = gy === 0 || gy === gh - 1 || gx === 0 || gx === gw - 1;
            if (!edge && gx !== 2) continue;
            const o = (y0 + gy) * this.stride + (x0 + gx) * 4;
            this.page[o] = 0x14; this.page[o + 1] = 0x14; this.page[o + 2] = 0x18;
          }
        }
      }
    }
    this.margin = margin;
  }

  // paintInto composes the page into `surf`, the pixel region of the frame
  // buffer the feeder owns. Writing straight into the output buffer is what
  // keeps the feed at a true 25 fps: a Buffer.concat per frame is a 3.6 MB
  // allocation and a second copy, and the resulting GC pauses turned the feed
  // into bursts of 40-51 fps followed by stalls.
  paintInto(surf, frame) {
    const { w, h, stride, page } = this;
    surf.set(page);
    const bw = Math.min(96, w), bh = Math.min(64, h);
    const bx = this.margin + ((frame * 7) % Math.max(1, w - 2 * this.margin - bw));
    const by = Math.max(this.margin, h - this.margin - bh);
    for (let y = 0; y < bh; y++) {
      for (let x = 0; x < bw; x++) {
        const o = (by + y) * stride + (bx + x) * 4;
        const v = ((x >> 4) + (y >> 4) + frame) & 1 ? 0xe0 : 0x30;
        surf[o] = v; surf[o + 1] = v; surf[o + 2] = v;
      }
    }
  }
}

class Feeder {
  constructor(sockPath, w, h, fps = FPS) {
    this.sockPath = sockPath;
    this.fps = fps;
    this.motion = true;
    this.out = null;
    this.setSize(w, h);
    this.sock = null;
    this.running = false;
    this.frames = 0;
    this.t0 = 0;
  }

  // One buffer per geometry, allocated once: 48-byte ISOL header followed by
  // one full-surface BGRA frame. The header's constant fields are written
  // here; only the timestamp changes per frame.
  setSize(w, h) {
    if (this.corpus && this.corpus.w === w && this.corpus.h === h) return;
    this.corpus = new Corpus(w, h);
    const stride = this.corpus.stride;
    this.out = Buffer.alloc(48 + stride * h);
    this.out.writeUInt32LE(0x4c4f5349, 0); // 'ISOL'
    this.out.writeUInt32LE(w, 4);
    this.out.writeUInt32LE(h, 8);
    this.out.writeUInt32LE(stride, 12);
    this.out.writeUInt32LE(0, 16); // BGRA
    this.out.writeUInt32LE(1, 20); // one dirty rect, the full surface
    this.out.writeUInt32LE(0, 32);
    this.out.writeUInt32LE(0, 36);
    this.out.writeUInt32LE(w, 40);
    this.out.writeUInt32LE(h, 44);
    this.pixels = new Uint8Array(this.out.buffer, this.out.byteOffset + 48, stride * h);
  }

  async connect() {
    this.sock = net.connect(this.sockPath);
    await new Promise((res, rej) => {
      this.sock.once('connect', res);
      this.sock.once('error', rej);
    });
    this.sock.on('error', () => {});
    return this;
  }

  start() {
    if (this.running) return;
    this.running = true;
    this.t0 = nowMs();
    let frame = 0;
    const loop = async () => {
      while (this.running) {
        this.corpus.paintInto(this.pixels, this.motion ? frame : 0);
        this.out.writeBigUInt64LE(BigInt(Math.round(nowMs() * 1e6)), 24);
        // Wait for the kernel to take the frame before reusing the buffer, so
        // the feeder is paced by the socket instead of by its own GC.
        const buf = this.out;
        await new Promise((r) => this.sock.write(buf, r));
        this.frames++;
        frame++;
        const wait = this.t0 + (frame * 1000) / this.fps - nowMs();
        if (wait > 0) await sleep(wait);
      }
    };
    this.loopDone = loop().catch(() => {});
  }

  async stop() {
    this.running = false;
    await Promise.race([this.loopDone?.catch(() => {}), sleep(3000)]);
    this.sock?.end();
    await sleep(50);
  }
}

// ── minimal WebSocket client over TCP ────────────────────────────────────
// Same client as chaos.mjs: exact control over the handshake, no dependency on
// the global WebSocket, and it exposes the raw packet timeline.

class RawWS {
  constructor(port, query = '?mode=webcodecs') {
    this.port = port;
    this.query = query;
    this.buf = Buffer.alloc(0);
    this.frag = null;
    this.fragOpcode = 0;
    this.closed = false;
    this.onmessage = () => {};
    this.onclose = () => {};
  }

  connect() {
    return new Promise((resolve, reject) => {
      const key = crypto.randomBytes(16).toString('base64');
      this.sock = net.connect(this.port, '127.0.0.1');
      this.sock.setNoDelay(true);
      this.sock.once('error', reject);
      this.sock.once('connect', () => {
        this.sock.write(
          `GET /ws/soak${this.query} HTTP/1.1\r\n` +
          `Host: 127.0.0.1:${this.port}\r\n` +
          'Upgrade: websocket\r\nConnection: Upgrade\r\n' +
          `Sec-WebSocket-Key: ${key}\r\nSec-WebSocket-Version: 13\r\n\r\n`,
        );
      });
      let headerDone = false;
      this.sock.on('data', (chunk) => {
        if (headerDone) return this._onData(chunk);
        this.buf = Buffer.concat([this.buf, chunk]);
        const end = this.buf.indexOf('\r\n\r\n');
        if (end < 0) return;
        const head = this.buf.subarray(0, end).toString('latin1');
        if (!/^HTTP\/1\.1 101/.test(head)) {
          this.sock.destroy();
          reject(new Error(`handshake failed: ${head.split('\r\n')[0]}`));
          return;
        }
        headerDone = true;
        const rest = this.buf.subarray(end + 4);
        this.buf = Buffer.alloc(0);
        this.sock.removeAllListeners('data');
        this.sock.on('data', (c) => this._onData(c));
        this.sock.on('error', () => {});
        this.sock.on('close', () => {
          this.closed = true;
          this.closeCause = this.selfClosed ? 'self' : 'peer-tcp';
          this.onclose();
        });
        resolve(this);
        if (rest.length) this._onData(rest);
      });
    });
  }

  _onData(chunk) {
    this.buf = Buffer.concat([this.buf, chunk]);
    for (;;) {
      if (this.buf.length < 2) return;
      const b0 = this.buf[0], b1 = this.buf[1];
      const fin = (b0 & 0x80) !== 0;
      const opcode = b0 & 0x0f;
      const masked = (b1 & 0x80) !== 0;
      let len = b1 & 0x7f;
      let off = 2;
      if (len === 126) {
        if (this.buf.length < 4) return;
        len = this.buf.readUInt16BE(2);
        off = 4;
      } else if (len === 127) {
        if (this.buf.length < 10) return;
        const big = this.buf.readBigUInt64BE(2);
        if (big > BigInt(1 << 30)) { this.sock.destroy(); return; }
        len = Number(big);
        off = 10;
      }
      let mask = null;
      if (masked) {
        if (this.buf.length < off + 4) return;
        mask = this.buf.subarray(off, off + 4);
        off += 4;
      }
      if (this.buf.length < off + len) return;
      let payload = this.buf.subarray(off, off + len);
      this.buf = this.buf.subarray(off + len);
      if (mask) {
        payload = Buffer.from(payload);
        for (let i = 0; i < payload.length; i++) payload[i] ^= mask[i & 3];
      }
      if (opcode === 8) {
        this.closed = true;
        this.closeCause = 'ws-close-frame';
        this.sock.destroy();
        this.onclose();
        return;
      }
      if (opcode === 9) { this._send(10, payload); continue; }
      if (opcode === 10) continue;
      if (opcode === 0) {
        if (!this.frag) continue;
        this.frag = Buffer.concat([this.frag, payload]);
        if (fin) {
          const full = this.frag; const op = this.fragOpcode;
          this.frag = null;
          this.onmessage({ binary: op === 2, data: full });
        }
        continue;
      }
      if (!fin) {
        this.frag = Buffer.from(payload);
        this.fragOpcode = opcode;
        continue;
      }
      this.onmessage({ binary: opcode === 2, data: payload });
    }
  }

  _send(opcode, payload) {
    if (!this.sock || this.closed) return;
    const len = payload.length;
    let head;
    const mask = crypto.randomBytes(4);
    if (len < 126) {
      head = Buffer.from([0x80 | opcode, 0x80 | len]);
    } else if (len < 65536) {
      head = Buffer.alloc(4);
      head[0] = 0x80 | opcode; head[1] = 0x80 | 126; head.writeUInt16BE(len, 2);
    } else {
      head = Buffer.alloc(10);
      head[0] = 0x80 | opcode; head[1] = 0x80 | 127; head.writeBigUInt64BE(BigInt(len), 2);
    }
    const masked = Buffer.alloc(len);
    for (let i = 0; i < len; i++) masked[i] = payload[i] ^ mask[i & 3];
    this.sock.write(Buffer.concat([head, mask, masked]));
  }

  sendJSON(obj) { this._send(1, Buffer.from(JSON.stringify(obj))); }
  close() {
    this.selfClosed = true;
    try { this.sock.destroy(); } catch { /* already gone */ }
  }
}

// ── instrumented viewer ──────────────────────────────────────────────────

class Viewer {
  constructor(name, port) {
    this.name = name;
    this.port = port;
    this.packets = 0;
    this.bytes = 0;
    this.idr = 0;
    this.firstBinMs = null;
    this.firstKeyMs = null;
    this.firstBinWasKey = null;
    this.secondKeyMs = null;
    this.deltasBeforeKey = 0;
    this.resets = [];
    this.inits = [];
    this.resetRecoveries = []; // ms from stream_reset message to next video packet
    this.metrics = null;
    this.metricsHistory = [];
    this.seqLast = null;
    this.seqFirst = null;
    this.seqGaps = 0;
    this.seqBack = 0;
    this.lastAt = null;
    this.maxGapMs = 0;
    this.closedByServer = false;
    this.ws = null;
    this.pendingResetAt = null;
    this.t0 = nowMs();
  }

  async attach() {
    this.t0 = nowMs();
    const ws = new RawWS(this.port);
    this.ws = ws;
    ws.onclose = () => {
      this.closeCause = ws.closeCause ?? 'unknown';
      this.closedByServer = this.closeCause !== 'self';
    };
    ws.onmessage = (m) => this._onMessage(m);
    await ws.connect();
    return this;
  }

  _onMessage({ binary, data }) {
    if (!binary) {
      let msg;
      try { msg = JSON.parse(data.toString()); } catch { return; }
      if (msg.type === 'stream_init') this.inits.push({ ...msg, at: nowMs() - this.t0 });
      else if (msg.type === 'stream_reset') {
        this.resets.push({ ...msg, at: nowMs() - this.t0 });
        this.pendingResetAt = nowMs();
      } else if (msg.type === 'metrics') {
        this.metrics = msg;
        this.metricsHistory.push({ at: Math.round(nowMs() - this.t0), dropped: msg.dropped, seq: msg.encoder_seq });
        if (this.metricsHistory.length > 40) this.metricsHistory.shift();
      }
      return;
    }
    if (data.length <= 10) return;
    const marker = data[0];
    if (marker !== 0x02 && marker !== 0x01) return;
    const isKey = (data[1] & 1) === 1;
    const seq = data.readUInt32LE(2);
    const at = nowMs() - this.t0;
    this.packets++;
    this.bytes += data.readUInt32LE(6);
    if (this.pendingResetAt !== null) {
      this.resetRecoveries.push(at - (this.pendingResetAt - this.t0));
      this.pendingResetAt = null;
    }
    if (this.lastAt !== null) this.maxGapMs = Math.max(this.maxGapMs, at - this.lastAt);
    this.lastAt = at;
    if (this.seqFirst === null) this.seqFirst = seq;
    if (this.seqLast !== null && seq > this.seqLast + 1) this.seqGaps += seq - this.seqLast - 1;
    if (this.seqLast !== null && seq <= this.seqLast) this.seqBack++;
    this.seqLast = seq;
    if (isKey) {
      this.idr++;
      if (this.firstKeyMs === null) this.firstKeyMs = at;
      else if (this.secondKeyMs === null) this.secondKeyMs = at;
    }
    if (this.firstBinMs === null) {
      this.firstBinMs = at;
      this.firstBinWasKey = isKey;
      if (!isKey) this.deltasBeforeKey++;
    } else if (this.firstKeyMs === null && !isKey) {
      this.deltasBeforeKey++;
    }
  }

  send(obj) { this.ws?.sendJSON(obj); }
  close() { this.ws?.close(); }

  snapshot() {
    return {
      packets: this.packets,
      bytes: this.bytes,
      idr: this.idr,
      max_gap_ms: round(this.maxGapMs, 1),
      resets: this.resets.length,
      resets_by_reason: [...new Set(this.resets.map((r) => r.reason))],
      inits: this.inits.length,
      reset_recoveries_ms: this.resetRecoveries.map((v) => round(v, 1)),
      dropped_metric: this.metrics?.dropped ?? null,
      seq_gaps: this.seqGaps,
      seq_back_steps: this.seqBack,
      closed_by_server: this.closedByServer === true,
      close_cause: this.closeCause ?? null,
    };
  }
}

// ── streamer process ─────────────────────────────────────────────────────

const LIVE = new Set();
process.on('exit', () => {
  for (const st of LIVE) {
    try { st.proc?.kill('SIGKILL'); } catch { /* already gone */ }
  }
});

async function procStats(pid) {
  const out = async (cmd, args) => {
    try {
      const r = await execFileP(cmd, args, { encoding: 'utf8', timeout: 4000 });
      return (r.stdout ?? '').trim();
    } catch (e) {
      // pgrep/lsof exit non-zero when there is nothing to report.
      return ((e && e.stdout) ?? '').trim();
    }
  };
  const rss = Number((await out('ps', ['-o', 'rss=', '-p', String(pid)])) || 0);
  const threads = Math.max(0, (await out('ps', ['-M', '-p', String(pid)])).split('\n').filter(Boolean).length - 1);
  // lsof exits 1 and prints nothing for a dead pid.
  let fds = 0;
  try {
    const r = await execFileP('lsof', ['-p', String(pid), '-Fn'], { encoding: 'utf8', timeout: 5000 });
    fds = r.stdout.split('\n').filter((l) => l.startsWith('n')).length;
  } catch { /* dead */ }
  const children = (await out('pgrep', ['-P', String(pid)])).split('\n').filter(Boolean).map(Number);
  let ffmpeg = 0;
  for (const c of children) {
    if ((await out('ps', ['-o', 'comm=', '-p', String(c)])).includes('ffmpeg')) ffmpeg++;
  }
  return { rss_kb: rss, threads, fds, children: children.length, ffmpeg };
}

class Streamer {
  constructor(name, opts = {}) {
    this.name = name;
    this.dir = path.join(OUT, name);
    this.port = opts.port ?? PORT;
    this.width = opts.w ?? W;
    this.height = opts.h ?? H;
    this.fps = opts.fps ?? FPS;
    this.kbps = opts.kbps ?? KBPS;
    this.keepalive = opts.keepalive ?? 2000;
    this.logPath = path.join(this.dir, 'streamer.log');
    this.sockPath = path.join(this.dir, 'frame.sock');
    this.log = '';
    this.logScanned = 0;
    this.pid = null;
  }

  async start() {
    fs.rmSync(this.dir, { recursive: true, force: true });
    fs.mkdirSync(this.dir, { recursive: true });
    const env = {
      ...process.env,
      PORT: String(this.port),
      ISOLA_FRAME_SOCK: this.sockPath,
      ISOLA_WIDTH: String(this.width),
      ISOLA_HEIGHT: String(this.height),
      ISOLA_FPS: String(this.fps),
      ISOLA_BITRATE_KBPS: String(this.kbps),
      ISOLA_STATIC_KEEPALIVE_MS: String(this.keepalive),
    };
    this.proc = spawn(BIN, [], { env, stdio: ['ignore', 'pipe', 'pipe'] });
    this.pid = this.proc.pid;
    LIVE.add(this);
    const sink = (d) => {
      this.log += d.toString();
      // Cap the in-memory log: 10 minutes of noise is fine, an hour is not.
      if (this.log.length > 4 << 20) this.log = this.log.slice(-(2 << 20));
    };
    this.proc.stdout.on('data', sink);
    this.proc.stderr.on('data', sink);
    this.proc.on('exit', () => { this.exited = true; });
    const deadline = Date.now() + 8000;
    while (Date.now() < deadline) {
      if (fs.existsSync(this.sockPath) && this.log.includes('listening on')) return this;
      await sleep(50);
    }
    throw new Error(`streamer did not start:\n${this.log}`);
  }

  counted(pattern) {
    const m = this.log.match(new RegExp(pattern, 'g'));
    return m ? m.length : 0;
  }

  spawnPids() {
    const pids = [];
    for (const line of this.log.split('\n')) {
      const m = line.match(/encoder: ffmpeg started .*\(pid=(\d+)\)/);
      if (m) pids.push(Number(m[1]));
    }
    return pids;
  }

  // Live ffmpeg children by pid, independent of the process tree snapshot.
  liveSpawns() {
    return this.spawnPids().filter((pid) => {
      try { process.kill(pid, 0); return true; } catch { return false; }
    });
  }

  // New log lines containing any of the anomaly tokens, since the last call.
  scanAnomalies(re) {
    const fresh = this.log.slice(this.logScanned);
    this.logScanned = this.log.length;
    const hits = [];
    for (const line of fresh.split('\n')) {
      if (line && re.test(line)) hits.push(line.replace(/^\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2} /, ''));
    }
    return hits;
  }

  async debugEncoder() {
    try {
      const r = await fetch(`http://127.0.0.1:${this.port}/debug/encoder`, { signal: AbortSignal.timeout(2000) });
      return await r.json();
    } catch (e) {
      return { error: String(e) };
    }
  }

  async resize(w, h) {
    // POST: /resize mutates session geometry, so the streamer accepts it the
    // same way it accepts /navigate.
    const r = await fetch(`http://127.0.0.1:${this.port}/resize?width=${w}&height=${h}`, {
      method: 'POST',
      signal: AbortSignal.timeout(5000),
    });
    return { status: r.status, body: (await r.text()).slice(0, 160) };
  }

  async stop() {
    if (!this.proc || this.exited) { LIVE.delete(this); return; }
    this.proc.kill('SIGTERM');
    const deadline = Date.now() + 4000;
    while (Date.now() < deadline && !this.exited) await sleep(50);
    if (!this.exited) this.proc.kill('SIGKILL');
    await sleep(200);
    LIVE.delete(this);
  }
}

// ── the soak ─────────────────────────────────────────────────────────────

const ANOMALY_RE = /error|panic|timeout|leak|refused/i;

// Lines the regex matches that are NOT session-health signals. Everything else
// that matches is an anomaly and fails R4.
const ALLOWED_ANOMALIES = [
  {
    id: 'harness-no-renderer',
    re: /^cdp: connect failed: .*\(retry in 2s\)$/,
    why: 'the harness runs the streamer standalone with a synthetic ISOL producer, so there is deliberately no ' +
      'renderer at :9222 for the capture client to attach to; the retry loop is expected and cannot be reached ' +
      'by a real session',
  },
  {
    id: 'x264-params-parse',
    re: /^ffmpeg: \[libx264 @ 0x[0-9a-f]+\] Error parsing option 'me:dia:force-cfr = 1'\.$/,
    why: 'pre-existing encoder argv defect: encoder.go libx264Quality.params writes "me:dia" instead of "me=dia", ' +
      'so libx264 rejects the rest of the -x264-params string. ffmpeg logs and continues (verified: keyint=50 still ' +
      'applies). Reported as a finding; not a session-health signal and out of this slice\'s scope to change',
  },
  {
    id: 'keyframe-budget-exhausted',
    re: /^ws: keyframe request refused \(connection budget exhausted\): viewer used 4\/4/,
    why: 'designed: stream_hub.go keyframeRequestsPerConn = 4 is a per-connection lifetime cap (spaced by a 30 s ' +
      'window), so a session that asks once a minute loses request_keyframe after the fourth accepted restart. The ' +
      'refusal is a no-op and the stream keeps flowing (R8 asserts that). Reported because a long-lived client can ' +
      'no longer force an IDR, and set_bitrate is the only remaining lever',
  },
  {
    id: 'torn-frame-at-teardown',
    re: /rawvideo.*(Packet corrupt|corrupt input packet|Invalid buffer size|Error submitting packet to decoder)/,
    why: 'a run whose last viewer leaves is killed with a frame mid-write on its stdin pipe, so ffmpeg reports a ' +
      'truncated rawvideo packet as it dies (packet size 983040 = 192 of 720 rows fits a 64 KB pipe at EOF). It ' +
      'belongs to the run being torn down, never to a survivor, and cannot be avoided without draining the pipe ' +
      'first. Reported, not failed',
  },
];


async function soak() {
  fs.mkdirSync(OUT, { recursive: true });
  const st = await new Streamer('soak').start();
  const feed = await new Feeder(st.sockPath, W, H).connect();
  feed.start();
  await sleep(600);

  // The long-lived viewer. It is what "a session stays healthy" means: one
  // connection open for the whole run, receiving the normal stream while
  // everything else churns around it.
  const steady = await new Viewer('steady', st.port).attach();
  const steadyUp = await poll(() => steady.firstBinMs !== null, 4000, 10);
  if (!steadyUp) throw new Error('steady viewer received no video');

  const T0 = nowMs();
  const t = () => nowMs() - T0;
  let running = true;
  let wake = null;
  const events = [];
  const attaches = [];
  const anomalies = [];
  const samples = [];

  const inStatic = () => t() >= T_STATIC_START && t() < T_STATIC_END;
  // Keep the resize round trip clean: an ABR step or forced keyframe inside it
  // would replace the run the resize is re-keying.
  const inResizeWindow = () => t() >= T_RESIZE_OUT && t() < T_RESIZE_BACK + 2000;

  const track = (kind, extra = {}) => events.push({ t_s: round(t() / 1000, 1), kind, ...extra });

  // 1. Viewer churn every 20 s: attach, wait for the first video packet, hold
  //    briefly, detach. The subscription is added to the EXISTING run, so this
  //    measures attach latency and subscriber bookkeeping, not encoder spawn.
  const churnLoop = (async () => {
    let i = 0;
    for (;;) {
      const due = 12000 + i * CHURN_EVERY_MS;
      i++;
      if (due >= D - 5000) return;
      await sleep(due - t());
      if (!running) return;
      if (inStatic() || inResizeWindow()) continue;
      const name = `churn-${i}`;
      const v = new Viewer(name, st.port);
      const attachedAt = nowMs();
      try {
        await v.attach();
        const got = await poll(() => v.firstBinMs !== null, 3000, 10);
        await sleep(800);
        const rec = {
          sample: samples.length,
          t_s: round(t() / 1000, 1),
          name,
          first_packet_ms: got ? round(v.firstBinMs, 1) : null,
          first_packet_was_key: v.firstBinWasKey,
          poster_to_live_idr_ms: v.secondKeyMs === null ? null : round(v.secondKeyMs - (v.firstKeyMs ?? 0), 1),
          deltas_before_key: v.deltasBeforeKey,
          inits: v.inits.length,
          packets: v.packets,
        };
        attaches.push(rec);
        track('attach', rec);
      } catch (e) {
        attaches.push({ t_s: round(t() / 1000, 1), name, error: String(e) });
        track('attach-error', { name, error: String(e) });
      } finally {
        v.close();
        await sleep(Math.max(0, 600 - (nowMs() - attachedAt)));
      }
    }
  })();

  // 2. ABR steps every 45 s from the steady viewer, cycling down AND back up.
  const abrLoop = (async () => {
    let i = 0;
    for (;;) {
      const due = 30000 + i * ABR_EVERY_MS;
      i++;
      if (due >= D - 5000) return;
      await sleep(due - t());
      if (!running) return;
      if (inStatic() || inResizeWindow()) continue;
      const kbps = ABR_STEPS[(i - 1) % ABR_STEPS.length];
      const resetsBefore = steady.resets.length;
      const at = nowMs();
      steady.send({ type: 'set_bitrate', kbps });
      const resetSeen = await poll(() => steady.resets.length > resetsBefore, 2500, 10);
      const p0 = steady.packets;
      const recovered = await poll(() => steady.packets > p0, 3000, 10);
      track('abr', {
        t_s: round(t() / 1000, 1),
        kbps,
        reset_seen: resetSeen,
        recover_ms: recovered ? round(nowMs() - at, 1) : null,
        resets: steady.resets.length,
        runs: (await st.debugEncoder()).encoder_runs,
      });
    }
  })();

  // 3. One forced keyframe per minute from the steady viewer.
  //
  // The hub budgets forced restarts per connection (stream_hub.go:
  // keyframeRequestsPerConn = 4 over the life of the connection, spaced by a
  // 30 s window), so past the fourth request the answer is a refusal, not a
  // restart. That is the designed behaviour: a refusal must cost nothing, so
  // the check there is that the picture kept flowing instead of resetting.
  const keyframeLoop = (async () => {
    let i = 0;
    for (;;) {
      const due = 55000 + i * KEYFRAME_EVERY_MS;
      i++;
      if (due >= D - 5000) return;
      await sleep(due - t());
      if (!running) return;
      if (inStatic() || inResizeWindow()) continue;
      const resetsBefore = steady.resets.length;
      const at = nowMs();
      steady.send({ type: 'request_keyframe' });
      const resetSeen = await poll(() => steady.resets.length > resetsBefore, 2500, 10);
      if (resetSeen) {
        const p0 = steady.packets;
        const recovered = await poll(() => steady.packets > p0, 3000, 10);
        track('keyframe', {
          t_s: round(t() / 1000, 1),
          accepted: true,
          reset_seen: true,
          recover_ms: recovered ? round(nowMs() - at, 1) : null,
        });
      } else {
        // Refused or ignored: measure the wait for the next frame from now,
        // not from the request, so a no-op cannot look like a stall.
        const p0 = steady.packets;
        const at2 = nowMs();
        const flowing = await poll(() => steady.packets > p0, 1000, 5);
        track('keyframe', {
          t_s: round(t() / 1000, 1),
          accepted: false,
          reset_seen: false,
          recover_ms: flowing ? round(nowMs() - at2, 1) : null,
        });
      }
    }
  })();

  // 4. Resize round trip, then the static stretch and its wake-up.
  const controlLoop = (async () => {
    const doResize = async (w, h, label) => {
      const resetsBefore = steady.resets.length;
      const at = nowMs();
      const res = await st.resize(w, h);
      feed.setSize(w, h);
      const resetSeen = await poll(() => steady.resets.length > resetsBefore, 4000, 10);
      const p0 = steady.packets;
      const recovered = await poll(() => steady.packets > p0, 4000, 10);
      const dbg = await st.debugEncoder();
      track('resize', {
        t_s: round(t() / 1000, 1),
        label,
        request: `${w}x${h}`,
        status: res.status,
        reset_seen: resetSeen,
        recover_ms: recovered ? round(nowMs() - at, 1) : null,
        resolution: dbg.resolution,
      });
    };

    await sleep(T_RESIZE_OUT - t());
    if (!running) return;
    await doResize(Math.min(1024, W), Math.min(576, H), 'scale-down');

    await sleep(T_RESIZE_BACK - t());
    if (!running) return;
    await doResize(W, H, 'back-to-boot');

    await sleep(T_STATIC_START - t());
    if (!running) return;
    const p0 = steady.packets;
    const d0 = (await st.debugEncoder()).damage_updates ?? null;
    feed.motion = false;
    track('static-start', { packets_at: p0, damage_updates_at: d0, t_s: round(t() / 1000, 1) });

    await sleep(T_STATIC_END - t());
    if (!running) return;
    const p1 = steady.packets;
    const d1 = (await st.debugEncoder()).damage_updates ?? null;
    feed.motion = true;
    const wakeAt = nowMs();
    const woke = await poll(() => steady.packets > p1, 3000, 5);
    wake = {
      quiet_packets: p1 - p0,
      quiet_seconds: round((T_STATIC_END - T_STATIC_START) / 1000, 1),
      damage_updates_during_static: d0 === null || d1 === null ? null : d1 - d0,
      wake_ms: woke ? round(nowMs() - wakeAt, 1) : null,
    };
    track('wake-up', wake);
  })();

  // 5. The sampler: process + hub + viewer state every SAMPLE_MS.
  const findings = new Map();
  let lastPackets = steady.packets;
  let lastFeedFrames = feed.frames;
  let lastAt = nowMs();
  let idx = 0;
  while (t() < D) {
    const due = idx * SAMPLE_MS;
    await sleep(due - t());
    if (nowMs() - T0 >= D) break;
    const at = nowMs();
    const ps = await procStats(st.pid);
    const dbg = await st.debugEncoder();
    const newAnoms = st.scanAnomalies(ANOMALY_RE);
    for (const line of newAnoms) {
      const allowed = ALLOWED_ANOMALIES.find((a) => a.re.test(line));
      if (allowed) {
        const f = findings.get(allowed.id) ?? {
          id: allowed.id, count: 0, first_sample: idx,
          first_t_s: round(t() / 1000, 1), example: line, why: allowed.why,
        };
        f.count++;
        findings.set(allowed.id, f);
      } else {
        anomalies.push({ sample: idx, t_s: round(t() / 1000, 1), line });
      }
    }
    const packets = steady.packets;
    const dt = (at - lastAt) / 1000;
    // The first sample lands within a few hundred ms of the viewer attaching,
    // so a delta over that interval is startup-burst noise, not a rate.
    const s = {
      idx,
      t_s: round((at - T0) / 1000, 1),
      phase: inStatic() ? 'static' : (t() >= T_STATIC_END && t() < T_STATIC_END + 5000 ? 'wake' : (inResizeWindow() ? 'resize' : 'motion')),
      rss_kb: ps.rss_kb,
      fds: ps.fds,
      threads: ps.threads,
      ffmpeg: ps.ffmpeg,
      children: ps.children,
      subscribers: dbg.subscribers ?? null,
      encoder_runs: dbg.encoder_runs ?? null,
      resolution: dbg.resolution ?? null,
      bitrate_kbps: dbg.bitrate_kbps ?? null,
      damage_updates: dbg.damage_updates ?? null,
      packets_total: packets,
      pps: dt >= 1 ? round((packets - lastPackets) / dt, 2) : null,
      feed_fps: dt >= 1 ? round((feed.frames - lastFeedFrames) / dt, 1) : null,
      drops: steady.metrics?.dropped ?? null,
      anomalies_total: anomalies.length,
      allowlisted_total: [...findings.values()].reduce((a, f) => a + f.count, 0),
    };
    samples.push(s);
    lastPackets = packets;
    lastFeedFrames = feed.frames;
    lastAt = at;
    idx++;
  }

  running = false;
  await Promise.race([Promise.allSettled([churnLoop, abrLoop, keyframeLoop, controlLoop]), sleep(6000)]);

  // Final state. The steady viewer is still attached here, so fds/threads are
  // directly comparable to the baseline sample: same viewer, same one-run hub.
  await sleep(1500);
  const finalStats = await procStats(st.pid);
  const finalDebug = await st.debugEncoder();
  const steadySnap = steady.snapshot();

  // ... and now with every viewer gone: ffmpeg children and runs must reach 0.
  steady.close();
  await sleep(500);
  const drained = await poll(async () => {
    const s = await procStats(st.pid);
    const d = await st.debugEncoder();
    return s.ffmpeg === 0 && (d.encoder_runs ?? 0) === 0 && (d.subscribers ?? 0) === 0;
  }, 6000, 200);
  const afterAll = await procStats(st.pid);
  const afterDebug = await st.debugEncoder();
  const leakedPids = st.liveSpawns();
  const logTail = st.log.split('\n').slice(-40).join('\n');
  fs.writeFileSync(st.logPath, st.log);
  await feed.stop();
  await st.stop();

  // A binary that never started a session is a harness error, not a verdict.
  const spawns = st.spawnPids().length;
  if (!samples.length) throw new Error('no samples collected');

  // ── verdict ────────────────────────────────────────────────────────────
  const rules = [];
  const push = (id, ok, detail, measured) => rules.push({ id, ok, detail, measured });

  // R1: RSS plateaus. Slope over the second half of the run, plus the growth
  // between the middle and last thirds of the run.
  const half = samples.filter((s) => s.t_s >= samples[samples.length - 1].t_s / 2);
  const slope = rssSlopeKbPerMin(half);
  const third = samples.length / 3;
  const midMax = Math.max(...samples.slice(Math.floor(third), Math.floor(2 * third)).map((s) => s.rss_kb));
  const lastMax = Math.max(...samples.slice(Math.floor(2 * third)).map((s) => s.rss_kb));
  const rssOk = slope !== null && slope <= LIMITS.rssSlopeKbPerMin && lastMax - midMax <= LIMITS.rssThirdGrowthKb;
  push('R1-rss-plateau', rssOk,
    `second-half RSS slope ${round(slope, 1)} KB/min (limit ${LIMITS.rssSlopeKbPerMin}); ` +
    `max RSS middle third ${midMax} KB -> last third ${lastMax} KB (limit +${LIMITS.rssThirdGrowthKb})` +
    (MINUTES < 5 ? ' [run shorter than 5 min: heap warm-up dominates, see docs/streamer-soak.md]' : ''),
    { slope_kb_per_min: round(slope, 1), mid_third_max_kb: midMax, last_third_max_kb: lastMax });

  // R2: fds and threads return to the settled baseline after every churn cycle.
  const baseline = samples[0];
  const fdDelta = finalStats.fds - baseline.fds;
  const threadDelta = finalStats.threads - baseline.threads;
  const resourceOk = fdDelta <= LIMITS.fdSlack && threadDelta <= LIMITS.threadSlack;
  push('R2-no-fd-thread-growth', resourceOk,
    `${attaches.length} attach/detach cycles; fds ${baseline.fds} -> ${finalStats.fds} (+${fdDelta}, limit ${LIMITS.fdSlack}), ` +
    `threads ${baseline.threads} -> ${finalStats.threads} (+${threadDelta}, limit ${LIMITS.threadSlack})`,
    { fds_baseline: baseline.fds, fds_final: finalStats.fds, threads_baseline: baseline.threads, threads_final: finalStats.threads });

  // R3: every encoder run and ffmpeg child is reaped once the last viewer leaves.
  const reapOk = drained && afterAll.ffmpeg === 0 && (afterDebug.encoder_runs ?? 0) === 0 &&
    (afterDebug.subscribers ?? 0) === 0 && leakedPids.length === 0;
  push('R3-encoders-reaped', reapOk,
    `after the last detach: ffmpeg children ${afterAll.ffmpeg}, encoder_runs ${afterDebug.encoder_runs}, ` +
    `subscribers ${afterDebug.subscribers}, live spawned pids ${leakedPids.length} (all must be 0)`,
    { drained, ffmpeg_children: afterAll.ffmpeg, encoder_runs: afterDebug.encoder_runs, subscribers: afterDebug.subscribers, leaked_pids: leakedPids.length });

  // R4: no log line carries an anomaly token, apart from the documented
  // harness-only and pre-existing-defect lines (which are still reported).
  const allowedList = [...findings.values()];
  const allowedSummary = allowedList.length
    ? allowedList.map((f) => `${f.id} x${f.count} (first at sample ${f.first_sample}, t=${f.first_t_s}s)`).join(', ')
    : 'none';
  push('R4-no-anomaly-log-lines', anomalies.length === 0,
    `${anomalies.length} unexplained line(s) matched /${ANOMALY_RE.source}/i` +
    (anomalies.length ? `: ${anomalies.slice(0, 3).map((a) => `sample ${a.sample}: ${a.line}`).join(' | ')}` : '') +
    `; allowlisted: ${allowedSummary}`,
    { lines: anomalies.slice(0, 20), allowlisted: allowedList });

  // R5: the steady-state packet rate does not decay, comparing like-for-like
  // minutes (the ladder cycles back to the base rung by the end).
  const minutes = [];
  for (let m = 0; m * 60 < D / 1000; m++) {
    const inMin = samples.filter((s) => s.t_s >= m * 60 && s.t_s < (m + 1) * 60);
    const motionSamples = inMin.filter((s) => s.phase === 'motion' || s.phase === 'resize');
    // A static minute has no motion samples; report its own (keepalive) rate.
    const rateSamples = motionSamples.length ? motionSamples : inMin.filter((s) => typeof s.pps === 'number');
    minutes.push({
      m,
      n: inMin.length,
      phase: inMin.some((s) => s.phase === 'static') ? (motionSamples.length ? 'mixed' : 'static') : 'motion',
      pps: round(rateSamples.reduce((a, s) => a + s.pps, 0) / Math.max(1, rateSamples.length), 2),
      rss_mb: round(inMin.reduce((a, s) => a + s.rss_kb, 0) / Math.max(1, inMin.length) / 1024, 1),
      fds: Math.max(...inMin.map((s) => s.fds), 0),
      threads: Math.max(...inMin.map((s) => s.threads), 0),
      ffmpeg: Math.max(...inMin.map((s) => s.ffmpeg), 0),
      runs: Math.max(...inMin.map((s) => s.encoder_runs ?? 0), 0),
      subs: round(inMin.reduce((a, s) => a + (s.subscribers ?? 0), 0) / Math.max(1, inMin.length), 1),
      attach_p50: round(quantile(attaches.filter((a) => a.t_s >= m * 60 && a.t_s < (m + 1) * 60 && a.first_packet_ms !== null).map((a) => a.first_packet_ms), 50), 1),
      anom: inMin.reduce((a, s) => Math.max(a, s.anomalies_total), 0),
      static: motionSamples.length === 0,
    });
  }
  const motionMinutes = minutes.filter((m) => !m.static);
  const firstRate = motionMinutes[0]?.pps ?? null;
  const lastRate = motionMinutes[motionMinutes.length - 1]?.pps ?? null;
  const minRate = Math.min(...motionMinutes.map((m) => m.pps));
  const rateOk = firstRate !== null && lastRate !== null &&
    lastRate >= LIMITS.ppsDecayRatio * firstRate && minRate >= LIMITS.minMotionPps;
  push('R5-packet-rate-stable', rateOk,
    `motion-minute mean pps: first ${firstRate}, last ${lastRate} (must be >= ${LIMITS.ppsDecayRatio}x first), ` +
    `minimum motion minute ${minRate} (floor ${LIMITS.minMotionPps})`,
    { first_minute_pps: firstRate, last_minute_pps: lastRate, min_motion_minute_pps: minRate });

  // R6: the static stretch really went quiet, and motion woke it up at once.
  const staticSamples = samples.filter((s) => s.phase === 'static' && s.t_s >= (T_STATIC_START + 8000) / 1000);
  const staticPps = staticSamples.length ? staticSamples.reduce((a, s) => a + s.pps, 0) / staticSamples.length : null;
  const staticOk = staticPps !== null && wake !== null &&
    staticPps >= LIMITS.staticPpsMin && staticPps <= LIMITS.staticPpsMax &&
    wake.wake_ms !== null && wake.wake_ms <= LIMITS.staticWakeMs &&
    (wake.damage_updates_during_static ?? 99) <= 10;
  push('R6-static-suppression-and-wake', staticOk,
    `static window packets/s ${round(staticPps, 2)} (allowed ${LIMITS.staticPpsMin}-${LIMITS.staticPpsMax}, keepalive is 0.5/s), ` +
    `damage updates during it ${wake?.damage_updates_during_static}, wake-up ${wake?.wake_ms} ms (limit ${LIMITS.staticWakeMs})`,
    { static_pps: round(staticPps, 2), damage_updates: wake?.damage_updates_during_static ?? null, wake_ms: wake?.wake_ms ?? null });

  // R7: attach latency stays flat and the first packet is always a keyframe.
  const lat = attaches.filter((a) => typeof a.first_packet_ms === 'number').map((a) => a.first_packet_ms);
  const p95 = quantile(lat, 95);
  const worst = lat.length ? Math.max(...lat) : null;
  const allKey = attaches.length > 0 && attaches.every((a) => a.first_packet_was_key === true);
  const attachOk = lat.length === attaches.length && allKey &&
    p95 !== null && p95 <= LIMITS.attachP95Ms && worst <= LIMITS.attachMaxMs;
  push('R7-attach-latency', attachOk,
    `${lat.length}/${attaches.length} attaches measured; p50 ${round(quantile(lat, 50), 1)} ms, p95 ${round(p95, 1)} ms ` +
    `(limit ${LIMITS.attachP95Ms}), max ${round(worst, 1)} ms (limit ${LIMITS.attachMaxMs}), first packet always key: ${allKey}`,
    { count: lat.length, p50_ms: round(quantile(lat, 50), 1), p95_ms: round(p95, 1), max_ms: round(worst, 1), all_key: allKey });

  // R8: the hub never needed more than two encoder runs, and every restart
  // the harness asked for recovered the picture. A keyframe request the hub
  // refused (its per-connection budget) must be a true no-op: no reset, and
  // the next frame arrives straight away.
  const maxRuns = Math.max(...samples.map((s) => s.encoder_runs ?? 0));
  const abrEvents = events.filter((e) => e.kind === 'abr');
  const kfEvents = events.filter((e) => e.kind === 'keyframe');
  const accepted = kfEvents.filter((e) => e.accepted);
  const refused = kfEvents.filter((e) => !e.accepted);
  const restartsOk = abrEvents.every((e) => e.reset_seen && e.recover_ms !== null) &&
    accepted.every((e) => e.reset_seen && e.recover_ms !== null) &&
    refused.every((e) => e.reset_seen === false && e.recover_ms !== null) &&
    abrEvents.length + accepted.length > 0 && maxRuns <= LIMITS.encoderRunsMax;
  const worstRecover = [...abrEvents, ...accepted, ...refused]
    .reduce((a, e) => Math.max(a, e.recover_ms ?? Infinity), 0);
  push('R8-restart-recovery', restartsOk,
    `${abrEvents.length} ABR + ${accepted.length} accepted keyframe restarts, all reset+recovered: ` +
    `${restartsOk ? 'yes' : 'no'}; ${refused.length} budget-refused request(s) were no-ops ` +
    `(no reset, stream continued); worst recovery ${round(worstRecover, 1)} ms (limit ${LIMITS.resetRecoverMaxMs}); ` +
    `max encoder_runs seen ${maxRuns}`,
    { abr: abrEvents.length, keyframe_accepted: accepted.length, keyframe_refused: refused.length, worst_recover_ms: round(worstRecover, 1), max_encoder_runs: maxRuns });

  // R9: no viewer ever had to be resynchronised because the hub shed a packet.
  const seqGapOk = steady.seqGaps === 0 && (steady.metrics?.dropped ?? 0) === 0 && !steady.closedByServer;
  push('R9-stream-integrity', seqGapOk,
    `steady viewer: seq gaps ${steady.seqGaps}, dropped ${steady.metrics?.dropped ?? 'n/a'}, ` +
    `closed by server ${steady.closedByServer} (close cause ${steady.closeCause ?? 'n/a'})`,
    { seq_gaps: steady.seqGaps, dropped: steady.metrics?.dropped ?? null, resets: steady.resets.length });

  // R10: the resize round trip came back to the boot geometry with video.
  const resizeEvents = events.filter((e) => e.kind === 'resize');
  const resizeOk = resizeEvents.length === 2 &&
    resizeEvents[0].status === 200 && resizeEvents[1].status === 200 &&
    resizeEvents[1].resolution === `${W}x${H}` &&
    resizeEvents.every((e) => e.reset_seen && e.recover_ms !== null);
  push('R10-resize-round-trip', resizeOk,
    resizeEvents.map((e) => `${e.label}: ${e.request} -> ${e.status} ${e.resolution} recover ${e.recover_ms} ms`).join('; ') ||
    'resize events missing',
    { events: resizeEvents });

  const failed = rules.filter((r) => !r.ok);
  const verdict = failed.length === 0 ? 'PASS' : 'FAIL';

  // ── output ─────────────────────────────────────────────────────────────
  const header = ['idx', 't_s', 'phase', 'rss_kb', 'fds', 'threads', 'ffmpeg', 'subscribers', 'encoder_runs', 'packets_total', 'pps', 'feed_fps', 'damage_updates', 'resolution', 'anomalies_total'];
  const csv = [header.join(','), ...samples.map((s) => header.map((k) => s[k] ?? '').join(','))].join('\n') + '\n';
  const csvPath = path.join(OUT, 'soak-samples.csv');
  fs.writeFileSync(csvPath, csv);

  const result = {
    harness: 'scripts/ab/soak.mjs',
    verdict,
    binary: BIN,
    host: `${process.platform} ${process.arch}`,
    ffmpeg: (spawnSync('ffmpeg', ['-version'], { encoding: 'utf8' }).stdout ?? '').split('\n')[0],
    config: {
      minutes: MINUTES, sample_ms: SAMPLE_MS, w: W, h: H, base_kbps: KBPS, fps_cap: FPS,
      churn_every_ms: CHURN_EVERY_MS, abr_every_ms: ABR_EVERY_MS, keyframe_every_ms: KEYFRAME_EVERY_MS,
      abr_steps: ABR_STEPS,
      schedule_s: { resize_out: T_RESIZE_OUT / 1000, resize_back: T_RESIZE_BACK / 1000, static_start: T_STATIC_START / 1000, static_end: T_STATIC_END / 1000 },
    },
    limits: LIMITS,
    rules,
    samples: samples.length,
    attaches,
    events,
    anomalies,
    known_findings: [...findings.values()],
    wake,
    steady_viewer: steadySnap,
    final: {
      fds: finalStats.fds, threads: finalStats.threads, rss_kb: finalStats.rss_kb,
      ffmpeg: finalStats.ffmpeg,
      encoder_runs: finalDebug.encoder_runs, subscribers: finalDebug.subscribers,
      after_all: { fds: afterAll.fds, threads: afterAll.threads, rss_kb: afterAll.rss_kb, ffmpeg: afterAll.ffmpeg },
      ffmpeg_spawns_total: spawns,
    },
    minutes,
    log_tail: logTail,
    samples_csv: csvPath,
  };
  fs.writeFileSync(path.join(OUT, 'soak-result.json'), JSON.stringify(result, null, 2));

  // Human table.
  const pad = (v, n, left = false) => {
    const s = String(v ?? '-');
    return left ? s.padEnd(n) : s.padStart(n);
  };
  const lines = [];
  lines.push(`soak: ${MINUTES} min, ${samples.length} samples every ${SAMPLE_MS} ms, ${attaches.length} attach/detach cycles, ${spawns} ffmpeg spawns`);
  lines.push('');
  lines.push(`${pad('min', 4)} ${pad('phase', 7)} ${pad('rssMB', 7)} ${pad('fds', 4)} ${pad('thr', 4)} ${pad('ff', 3)} ${pad('runs', 4)} ${pad('subs', 4)} ${pad('pps', 6)} ${pad('attachP50', 9)} ${pad('anom', 4)}`);
  for (const m of minutes) {
    lines.push(`${pad(m.m, 4)} ${pad(m.phase, 7)} ${pad(m.rss_mb, 7)} ${pad(m.fds, 4)} ${pad(m.threads, 4)} ${pad(m.ffmpeg, 3)} ${pad(m.runs, 4)} ${pad(m.subs, 4)} ${pad(m.pps, 6)} ${pad(m.attach_p50, 9)} ${pad(m.anom, 4)}`);
  }
  lines.push('');
  for (const r of rules) lines.push(`${r.ok ? 'ok  ' : 'FAIL'} ${r.id}: ${r.detail}`);
  if (allowedList.length) {
    lines.push('');
    lines.push('known findings (allowlisted for R4, still reported):');
    for (const f of allowedList) {
      lines.push(`  ${f.id} x${f.count}, first at sample ${f.first_sample} (t=${f.first_t_s}s)`);
      lines.push(`    ${f.why}`);
    }
  }
  lines.push('');
  lines.push(`VERDICT: ${verdict} (${rules.length - failed.length}/${rules.length} rules)`);
  lines.push(`samples: ${csvPath}`);
  lines.push(`result:  ${path.join(OUT, 'soak-result.json')}`);
  console.log(lines.join('\n'));

  return verdict === 'PASS' ? 0 : 1;
}

soak()
  .then((code) => process.exit(code))
  .catch((e) => {
    console.error(`soak harness error: ${(e && e.stack) || e}`);
    for (const st of [...LIVE]) st.stop().catch(() => {});
    process.exitCode = 2;
    setTimeout(() => process.exit(2), 500);
  });
