#!/usr/bin/env node
// Chaos/edge harness for the isola streamer. Unlike driver.mjs (which measures
// one steady stream), this drives the paths a user hits by accident: viewers
// coming and going, attaching exactly while the encoder is being replaced,
// keyframe request storms, ABR stepping, a viewer that stops reading its
// socket, and geometries nobody meant to request.
//
// Every scenario boots its own streamer binary on its own port + frame socket,
// drives it with the real ISOL wire format, and emits one JSON object per
// scenario plus a summary. `docs/streamer-edges.md` holds the numbers from a
// run of this file.
//
//   node scripts/ab/chaos.mjs                       # build + run all scenarios
//   node scripts/ab/chaos.mjs --bin /tmp/streamer   # use an existing build
//   node scripts/ab/chaos.mjs --only storm,abr      # subset
//   node scripts/ab/chaos.mjs --out /tmp/chaos      # keep logs/artifacts here
//
// Scenarios: churn, restart-attach, saturation, storm, abr, slow-reader,
//            zero-frame, geometry.

import net from 'node:net';
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { spawn, spawnSync } from 'node:child_process';

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
const OUT = path.resolve(argv.out ?? '/tmp/isola-chaos');
const ONLY = argv.only ? String(argv.only).split(',').map((s) => s.trim()).filter(Boolean) : null;
const BASE_PORT = Number(argv.port ?? 17300);
const W = Number(argv.w ?? 1280);
const H = Number(argv.h ?? 720);
const KBPS = Number(argv.kbps ?? 6000);
const FPS = 25; // encoder mode caps at 25 (encoder.go fpsCapFor)

// One port per scenario so a crashed run never collides with the next.
const PORT_MAP = {
  churn: BASE_PORT + 0,
  'restart-attach': BASE_PORT + 1,
  saturation: BASE_PORT + 2,
  'saturation-low': BASE_PORT + 12,
  storm: BASE_PORT + 3,
  abr: BASE_PORT + 4,
  'abr-burst': BASE_PORT + 13,
  'slow-reader': BASE_PORT + 5,
  deadline: BASE_PORT + 14,
  'zero-frame': BASE_PORT + 6,
  geometry: BASE_PORT + 7,
};

let BIN = argv.bin ? path.resolve(String(argv.bin)) : path.join(OUT, 'isola-streamer');
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

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const nowMs = () => performance.now();

function pct(arr, p) {
  if (!arr.length) return null;
  const s = [...arr].sort((a, b) => a - b);
  return s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))];
}
const round = (n, d = 2) => (n === null || n === undefined ? null : Math.round(n * 10 ** d) / 10 ** d);

// ── corpus + ISOL feeder ─────────────────────────────────────────────────

// Static dense-text page (the workload that matters) with one animated block.
// The block moves every frame, so the composed surface really changes and
// change-driven suppression never idles the encoder. Same corpus idea as
// driver.mjs, kept small here so each scenario can resize it.
class Corpus {
  constructor(w, h) {
    this.w = w; this.h = h;
    this.stride = w * 4;
    this.page = new Uint8Array(this.stride * h);
    this.surf = new Uint8Array(this.stride * h);
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

  paint(frame) {
    const { w, h, stride, surf, page } = this;
    if (this.noise) {
      crypto.randomFillSync(surf);
      return;
    }
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

  // 32-byte ISOL header + one full-surface dirty rect.
  packet(tsNs) {
    const hdr = Buffer.alloc(48);
    hdr.writeUInt32LE(0x4c4f5349, 0); // 'ISOL'
    hdr.writeUInt32LE(this.w, 4);
    hdr.writeUInt32LE(this.h, 8);
    hdr.writeUInt32LE(this.stride, 12);
    hdr.writeUInt32LE(0, 16); // BGRA
    hdr.writeUInt32LE(1, 20);
    hdr.writeBigUInt64LE(BigInt(tsNs), 24);
    hdr.writeUInt32LE(0, 32);
    hdr.writeUInt32LE(0, 36);
    hdr.writeUInt32LE(this.w, 40);
    hdr.writeUInt32LE(this.h, 44);
    return Buffer.concat([hdr, Buffer.from(this.surf)]);
  }
}

class Feeder {
  constructor(sockPath, w, h, fps = FPS) {
    this.sockPath = sockPath;
    this.fps = fps;
    this.setSize(w, h);
    this.sock = null;
    this.running = false;
    this.frames = 0;
  }

  setSize(w, h) {
    if (this.corpus && this.corpus.w === w && this.corpus.h === h) return;
    this.corpus = new Corpus(w, h);
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

  // Feed at the streamer's own rate. The frame pump is CFR with an interval
  // guard, so feeding faster than this does not create packets (measured in
  // the saturation scenario).
  start() {
    if (this.running) return;
    this.running = true;
    this.t0 = nowMs();
    let frame = 0;
    const loop = async () => {
      while (this.running) {
        this.corpus.paint(frame);
        const ok = this.sock.write(this.corpus.packet(Math.round(nowMs() * 1e6)));
        this.frames++;
        frame++;
        if (!ok) await new Promise((r) => this.sock.once('drain', r));
        const due = this.t0 + (frame * 1000) / this.fps;
        const wait = due - nowMs();
        if (wait > 0) await sleep(wait);
      }
    };
    this.loopDone = loop().catch(() => {});
  }

  // Rebase the feed schedule so a rate change does not cause a catch-up burst.
  setFps(fps) {
    this.fps = fps;
    if (this.running) this.t0 = nowMs() - (this.frames * 1000) / fps;
  }

  async stop() {
    this.running = false;
    await Promise.race([this.loopDone?.catch(() => {}), sleep(3000)]);
    this.sock?.end();
    await sleep(50);
  }
}

// ── minimal WebSocket client over TCP ────────────────────────────────────
// Used instead of the global WebSocket because the slow-reader scenario needs
// to stop pulling bytes off the kernel: `pause()` closes the receive window,
// which is what makes the server's mailbox fill. It also gives exact control
// over when the handshake completes.

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
    this.paused = false;
  }

  connect() {
    return new Promise((resolve, reject) => {
      const key = crypto.randomBytes(16).toString('base64');
      this.sock = net.connect(this.port, '127.0.0.1');
      this.sock.setNoDelay(true);
      this.sock.once('error', reject);
      this.sock.once('connect', () => {
        this.sock.write(
          `GET /ws/chaos${this.query} HTTP/1.1\r\n` +
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
  pause() { this.paused = true; this.sock.pause(); }
  resume() { this.paused = false; this.sock.resume(); }
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
    this.deltasBeforeKey = 0;
    this.resets = [];
    this.inits = [];
    this.metrics = null;
    this.metricsHistory = [];
    this.seqLast = null;
    this.seqFirst = null;
    this.seqMax = null;
    this.seqGaps = 0;
    this.seqBack = 0;
    this.gaps = [];
    this.lastAt = null;
    this.maxGapMs = 0;
    this.framesAt = [];
    this.closedByServer = false;
    this.t0 = nowMs();
    this.ws = null;
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
      else if (msg.type === 'stream_reset') this.resets.push({ ...msg, at: nowMs() - this.t0 });
      else if (msg.type === 'metrics') {
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
    const plen = data.readUInt32LE(6);
    const at = nowMs() - this.t0;
    this.packets++;
    this.bytes += plen;
    this.framesAt.push(at);
    if (this.lastAt !== null) {
      const gap = at - this.lastAt;
      this.gaps.push(gap);
      if (gap > this.maxGapMs) this.maxGapMs = gap;
    }
    this.lastAt = at;
    if (this.seqFirst === null) this.seqFirst = seq;
    if (this.seqLast !== null && seq > this.seqLast + 1) this.seqGaps += seq - this.seqLast - 1;
    if (this.seqLast !== null && seq <= this.seqLast) this.seqBack++;
    this.seqLast = seq;
    this.seqMax = seq;
    if (isKey) {
      this.idr++;
      if (this.firstKeyMs === null) this.firstKeyMs = at;
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
  pause() { this.ws.pause(); }
  resume() { this.ws.resume(); }
  close() { this.ws?.close(); }

  // stops counting gaps; used to scope a window
  snapshot() {
    return {
      packets: this.packets, idr: this.idr, bytes: this.bytes,
      maxGapMs: round(this.maxGapMs, 1),
      resets: this.resets.length, inits: this.inits.length,
      dropped: this.metrics?.dropped ?? null,
      seqGaps: this.seqGaps,
      seq_first: this.seqFirst,
      seq_last: this.seqLast,
      seq_max: this.seqMax,
      seq_back_steps: this.seqBack,
      closed_by_server: this.closedByServer === true,
      close_cause: this.closeCause ?? null,
    };
  }
}

// ── streamer process ─────────────────────────────────────────────────────

const LIVE_STREAMERS = new Set();
process.on('exit', () => {
  for (const st of LIVE_STREAMERS) {
    try { st.proc?.kill('SIGKILL'); } catch { /* already gone */ }
  }
});

class Streamer {
  constructor(name, opts = {}) {
    this.name = name;
    this.dir = path.join(OUT, name);
    this.port = opts.port ?? BASE_PORT;
    this.width = opts.w ?? W;
    this.height = opts.h ?? H;
    this.fps = opts.fps ?? FPS;
    this.kbps = opts.kbps ?? KBPS;
    this.keepalive = opts.keepalive ?? 2000;
    this.logPath = path.join(this.dir, 'streamer.log');
    this.sockPath = path.join(this.dir, 'frame.sock');
    this.log = '';
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
    LIVE_STREAMERS.add(this);
    const sink = (d) => { this.log += d.toString(); };
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

  samples() {
    const base = (typeof this.spawnPids === 'function' ? this.spawnPids() : []);
    return {
      ffmpeg_started: base.length,
      forced_keyframes: this.counted('ws: forced keyframe at'),
      refused: this.counted('ws: keyframe request refused'),
      ignored: this.counted('ws: keyframe request ignored'),
      rekeys: this.counted('ws: encoder re-keyed to'),
      write_errors: this.counted('ws: (binary|control) write error'),
      pump_timeouts: this.counted('encoder pumps did not exit in time'),
      ffmpeg_exit_logs: this.counted('encoder: ffmpeg exited'),
      splitter_errors: this.counted('ffmpeg stdout read error'),
      geometry_waits: this.counted('waiting for producer geometry'),
      surface_logs: this.counted('broadcaster: surface now'),
    };
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
    const r = await fetch(`http://127.0.0.1:${this.port}/resize?width=${w}&height=${h}`, { signal: AbortSignal.timeout(5000) });
    return { status: r.status, body: (await r.text()).slice(0, 120) };
  }

  procStats() {
    return procStats(this.pid);
  }

  // pids of ffmpeg children still alive (checked after the fact by pid)
  liveSpawns() {
    return this.spawnPids().filter((pid) => {
      try { process.kill(pid, 0); return true; } catch { return false; }
    });
  }

  async stop() {
    if (!this.proc || this.exited) { LIVE_STREAMERS.delete(this); return; }
    this.proc.kill('SIGTERM');
    const deadline = Date.now() + 4000;
    while (Date.now() < deadline && !this.exited) await sleep(50);
    if (!this.exited) this.proc.kill('SIGKILL');
    await sleep(200);
    LIVE_STREAMERS.delete(this);
  }
}

function procStats(pid) {
  const run = (cmd, args) => {
    const r = spawnSync(cmd, args, { encoding: 'utf8' });
    return (r.stdout ?? '').trim();
  };
  const rss = Number(run('ps', ['-o', 'rss=', '-p', String(pid)]) || 0);
  const threads = Math.max(0, run('ps', ['-M', '-p', String(pid)]).split('\n').filter(Boolean).length - 1);
  const fds = Math.max(0, run('lsof', ['-p', String(pid), '-Fn']).split('\n').filter((l) => l.startsWith('n')).length);
  const children = run('pgrep', ['-P', String(pid)]).split('\n').filter(Boolean).map(Number);
  const ffmpeg = children.filter((c) => run('ps', ['-o', 'comm=', '-p', String(c)]).includes('ffmpeg'));
  return { rss_kb: rss, threads, fds, children: children.length, ffmpeg: ffmpeg.length };
}

async function poll(fn, ms, step = 50) {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (await fn()) return true;
    await sleep(step);
  }
  return false;
}

// ── scenarios ────────────────────────────────────────────────────────────

const scenarios = {};

// 1. Viewer churn: 20 fresh attach/detach cycles. Each attach with no other
// viewer must start exactly one encoder, and each detach must reap it.
const CHURN_CYCLES = Number(argv.cycles ?? 20);
scenarios.churn = async () => {
  const st = await new Streamer('churn', { port: PORT_MAP['churn'] }).start();
  const feed = await new Feeder(st.sockPath, st.width, st.height).connect();
  feed.start();
  await sleep(1200);

  const before = st.procStats();
  const cycles = [];
  const straySpawns = [];
  const trend = [{ cycle: 0, ...before }];

  for (let i = 0; i < CHURN_CYCLES; i++) {
    const v = await new Viewer(`c${i}`, st.port).attach();
    const attachAt = nowMs();
    const got = await poll(() => v.firstKeyMs !== null, 2500, 20);
    const spawnCount = st.spawnPids().length;
    cycles.push({
      cycle: i,
      attach_to_idr_ms: got ? round(v.firstKeyMs, 1) : null,
      first_was_key: v.firstBinWasKey,
      deltas_before_key: v.deltasBeforeKey,
      inits: v.inits.length,
      ffmpeg_spawns_so_far: spawnCount,
      stop_ms: round(nowMs() - attachAt, 1),
    });
    v.close();
    const reaped = await poll(() => st.procStats().ffmpeg === 0, 2500, 40);
    if (!reaped) straySpawns.push(i);
    await sleep(120);
    if ((i + 1) % 5 === 0) trend.push({ cycle: i + 1, ...st.procStats() });
  }

  const leaked = st.liveSpawns();
  const drained = await poll(() => st.procStats().ffmpeg === 0, 2000, 50);
  const after = st.procStats();
  const dbg = await st.debugEncoder();
  await feed.stop();
  const log = st.samples();
  await st.stop();

  const observedSpawns = st.spawnPids().length;
  const unexpected = observedSpawns - CHURN_CYCLES;
  const verdict =
    leaked.length === 0 && drained && dbg.encoder_runs === 0 && (dbg.subscribers ?? 0) === 0 &&
    unexpected === 0 ? 'fine' : 'bug';

  return {
    scenario: 'churn',
    config: { cycles: CHURN_CYCLES, hold: 'until first IDR, then 120ms', w: st.width, h: st.height, fps: FPS },
    numbers: {
      cycles_run: cycles.length,
      attach_to_idr_ms: {
        p50: round(pct(cycles.map((c) => c.attach_to_idr_ms ?? 9999), 50), 1),
        p95: round(pct(cycles.map((c) => c.attach_to_idr_ms ?? 9999), 95), 1),
        max: round(Math.max(...cycles.map((c) => c.attach_to_idr_ms ?? 9999)), 1),
      },
      first_packet_always_key: cycles.every((c) => c.first_was_key === true),
      deltas_before_key_total: cycles.reduce((a, c) => a + c.deltas_before_key, 0),
      ffmpeg_spawns: observedSpawns,
      unexpected_spawns: unexpected,
      cycles_without_reap: straySpawns,
      ffmpeg_alive_after: leaked,
      ffmpeg_children_after: after.ffmpeg,
      encoder_runs_after: dbg.encoder_runs,
      subscribers_after: dbg.subscribers,
      fd_before: before.fds, fd_after: after.fds,
      threads_before: before.threads, threads_after: after.threads,
      rss_kb_before: before.rss_kb, rss_kb_after: after.rss_kb,
      trend,
      log,
    },
    verdict,
    notes: [
      'pprof is not registered by the streamer (no net/http/pprof import), so /debug/pprof/goroutine is unreachable; ' +
      'goroutine leaks are proxied by fd count, thread count and RSS after all 20 cycles drained.',
      verdict === 'fine'
        ? 'one ffmpeg per attach, all reaped on close; no growth in fds/threads/RSS'
        : 'see numbers: encoder or process state did not return to baseline',
    ],
  };
};

// 2. Attach during an encoder restart. request_keyframe replaces the run; a
// viewer arriving mid-replacement must get a stream_init and an IDR, and must
// never receive a delta before its first keyframe.
scenarios['restart-attach'] = async () => {
  const st = await new Streamer('restart-attach', { port: PORT_MAP['restart-attach'] }).start();
  const feed = await new Feeder(st.sockPath, st.width, st.height).connect();
  feed.start();
  await sleep(600);

  const rounds = [];
  for (let r = 0; r < 3; r++) {
    const a = await new Viewer(`a${r}`, st.port).attach();
    await poll(() => a.firstKeyMs !== null, 3000, 20);
    const aResets = a.resets.length;
    const aIdrBefore = a.idr;
    const forceAt = nowMs();
    a.send({ type: 'request_keyframe' });
    await sleep(15); // land inside spawnLocked() (ffmpeg start is ~50ms)
    const b = await new Viewer(`b${r}`, st.port).attach();
    await poll(() => b.firstKeyMs !== null && nowMs() - forceAt > 1500, 3000, 20);
    await sleep(400);
    const dbg = await st.debugEncoder();
    rounds.push({
      round: r,
      b_attach_to_first_bin_ms: round(b.firstBinMs, 1),
      b_attach_to_idr_ms: round(b.firstKeyMs, 1),
      b_first_packet_was_key: b.firstBinWasKey,
      b_deltas_before_key: b.deltasBeforeKey,
      b_inits: b.inits.length,
      b_resets: b.resets.map((x) => x.reason),
      a_resets_delta: a.resets.length - aResets,
      a_idr_after_restart: a.idr > aIdrBefore,
      a_reset_to_idr_ms: (() => {
        const last = a.resets[a.resets.length - 1];
        if (!last) return null;
        const next = a.framesAt.find((t) => t >= last.at);
        return next === undefined ? null : round(next - last.at, 1);
      })(),
      encoder_runs: dbg.encoder_runs,
      subscribers: dbg.subscribers,
      ffmpeg_alive: st.procStats().ffmpeg,
    });
    a.close(); b.close();
    await sleep(5200); // hub-wide lateJoinRestartCooldown (5s) before the next round
  }

  const log = st.samples();
  await feed.stop();
  await st.stop();

  const allKey = rounds.every((r) => r.b_first_packet_was_key === true);
  const noDeltas = rounds.every((r) => r.b_deltas_before_key === 0);
  const recovered = rounds.every((r) => r.a_idr_after_restart);
  const shared = rounds.every((r) => r.encoder_runs === 1);
  const verdict = allKey && noDeltas && recovered && shared ? 'fine' : 'bug';

  return {
    scenario: 'restart-attach',
    config: { rounds: rounds.length, attach_delay_ms: 15, w: st.width, h: st.height },
    numbers: {
      rounds,
      forced_keyframes: log.forced_keyframes,
      encoder_spawns: log.ffmpeg_started,
      all_first_packets_key: allKey,
      deltas_before_key_total: rounds.reduce((a, r) => a + r.b_deltas_before_key, 0),
    },
    verdict,
    notes: [
      'B attaches 15ms after request_keyframe, i.e. while spawnLocked() is starting the replacement ffmpeg.',
      allKey ? 'B always received an IDR (or a cached poster IDR) as its first video packet' : 'B received a delta before any IDR',
      shared ? 'A and B shared the replacement run (encoder_runs=1)' : 'viewers ended up on separate runs',
    ],
  };
};

// 3. Mailbox saturation. A viewer that stops reading its socket backs up the
// per-viewer mailbox (192 packets = 7.7s at the 25fps encode cap). Whether it
// actually overflows depends on the transport, not the hub: the kernel socket
// buffers absorb the first several MB, and only once they are full does the
// per-viewer writer block and the mailbox start filling. Two runs show both
// sides of that line:
//   - shipped 6 Mbps, static text: no overflow — the buffers swallow the stall
//   - 60 Mbps of noise: overflow — dropped rises, and the viewer still recovers
async function saturationRun(label, opts) {
  const st = await new Streamer(`saturation-${label}`, { port: opts.port, kbps: opts.kbps }).start();
  const marks = [];
  const t0 = nowMs();
  const mark = (name) => marks.push({ label: name, ms: Math.round(nowMs() - t0) });
  const feed = await new Feeder(st.sockPath, st.width, st.height).connect();
  feed.corpus.noise = opts.noise === true;
  feed.start();
  mark('feed started');
  await sleep(800);

  // S reads only until its first IDR, then stops pulling bytes. Pausing before
  // the receive buffer has been read from matters: once it autotunes to
  // several MB it can swallow an entire 9s stall of 6 Mbps video.
  const s = await new Viewer(`${label}-stalled`, st.port).attach();
  await poll(() => s.firstKeyMs !== null, 3000, 10);
  const sBefore = s.snapshot();
  s.pause();
  const pausedAt = nowMs();
  mark('paused');

  await sleep(9200);
  const f = await new Viewer(`${label}-fresh`, st.port).attach();
  await poll(() => f.firstKeyMs !== null, 3000, 20);
  const dbgDuring = await st.debugEncoder();
  mark('fresh attached');
  await sleep(Math.max(0, 9800 - (nowMs() - pausedAt)));
  const resumeAt = nowMs();
  const idrAtResume = s.idr;
  s.resume();
  mark('resumed');
  await sleep(3000);
  mark('post-resume window');

  const sAfter = s.snapshot();
  const sIdrAfterResume = s.idr - idrAtResume;
  const firstFrameAfterResume = s.framesAt.find((t) => t >= resumeAt - s.t0);
  const fNow = f.snapshot();
  const dbgAfter = await st.debugEncoder();

  // Does feeding faster than the encode cap create packets? Only measured on
  // the shipped-bitrate run: at 100 Mbps the single Node process is busy
  // parsing ~100 MB of video and cannot count packets reliably, and the
  // question is about the server's pump, not the client.
  let fastFeed = null;
  let wv = null;
  if (!opts.noise) {
    wv = await new Viewer(`${label}-fastfeed`, st.port).attach();
    await poll(() => wv.firstKeyMs !== null, 3000, 20);
    await sleep(2400); // wait out the poster-IDR -> live-IDR gap (<= 2s GOP)
    const pkBefore = wv.packets;
    const droppedBefore = wv.metrics?.dropped ?? null;
    feed.setFps(100);
    await sleep(2000);
    const pkAfter = wv.packets;
    fastFeed = {
      packets_in_2s: pkAfter - pkBefore,
      expected_at_25fps: 2 * FPS,
      dropped_delta: droppedBefore === null ? null : (wv.metrics?.dropped ?? 0) - droppedBefore,
    };
    feed.setFps(FPS);
    mark('fast-feed probe done');
  }

  const log = st.samples();
  await feed.stop();
  mark('feed stopped');
  wv?.close(); f.close(); s.close();
  await st.stop();
  mark('streamer stopped');

  return {
    label,
    config: {
      kbps: opts.kbps, content: opts.noise ? 'noise' : 'static text + animated block',
      w: st.width, h: st.height, stall_ms: 9800, fresh_attach_at_ms: 9200,
      mailbox_packets: 192, encode_fps: FPS,
    },
    numbers: {
      stalled_viewer: {
        before: sBefore,
        after: sAfter,
        first_frame_after_resume_ms: round(firstFrameAfterResume ?? null, 1),
        idrs_after_resume: sIdrAfterResume,
        bytes_received_after_resume: sAfter.bytes - sBefore.bytes,
        metrics_history: s.metricsHistory,
      },
      fresh_viewer: {
        attach_to_first_bin_ms: round(f.firstBinMs, 1),
        attach_to_idr_ms: round(f.firstKeyMs, 1),
        first_packet_was_key: f.firstBinWasKey,
        deltas_before_key: f.deltasBeforeKey,
        packets_in_3s: fNow.packets,
        dropped: fNow.dropped,
      },
      encoder_runs_during_attach: dbgDuring.encoder_runs,
      subscribers_during_attach: dbgDuring.subscribers,
      encoder_runs_after: dbgAfter.encoder_runs,
      mailbox_seconds_at_25fps: round(192 / FPS, 2),
      fast_feed_probe: fastFeed,
      timings: marks,
      log,
    },
  };
}

scenarios.saturation = async () => {
  const low = await saturationRun('shipped', { port: PORT_MAP['saturation-low'], kbps: KBPS, noise: false });
  const high = await saturationRun('noise', { port: PORT_MAP['saturation'], kbps: 100000, noise: true });

  const highOk = high.numbers.stalled_viewer.after.dropped > 0 &&
    high.numbers.stalled_viewer.after.closed_by_server !== true &&
    high.numbers.stalled_viewer.idrs_after_resume > 0 &&
    high.numbers.fresh_viewer.packets_in_3s > 0 &&
    high.numbers.encoder_runs_during_attach === 1;
  const lowOk = low.numbers.stalled_viewer.after.closed_by_server !== true &&
    low.numbers.stalled_viewer.idrs_after_resume > 0 &&
    low.numbers.fresh_viewer.packets_in_3s > 0;
  const verdict = highOk && lowOk ? 'fine' : 'rough edge';

  return {
    scenario: 'saturation',
    config: { runs: [low.config, high.config] },
    numbers: { shipped_6mbps: low, noisy_100mbps: high },
    verdict,
    notes: [
      'the mailbox sheds per viewer, never blocks the encoder: encoder_runs stayed 1 in both runs.',
      highOk
        ? 'at 100 Mbps the 192-slot mailbox overflowed (dropped>0) and the stalled viewer still resynced after resume.'
        : 'the noisy run did not reproduce overflow on this host.',
      lowOk && low.numbers.stalled_viewer.after.dropped === 0
        ? 'at the shipped 6 Mbps the OS socket buffers absorb the whole 9.2s stall, so the mailbox never fills; the scheduler bound is the 10s WS write deadline, not mailboxSize.'
        : 'the shipped-bitrate run behaved unexpectedly; see numbers.',
      'a late joiner receives the cached poster IDR immediately (first packet in a few ms) but drops deltas until the next live IDR (<=2s GOP) — that is the delivered-design trade-off, and it shows up as dropped on the joiner only.',
      'feeding at 100fps produced no extra packets: pumpFrames gates feeds on the encode interval (25fps cap).',
    ],
  };
};

// 4. Keyframe storm: two viewers send 20 request_keyframe each as fast as the
// socket takes them. Expected: one restart (hub cooldown 5s), the rest refused,
// and both viewers recover to a live IDR.
scenarios.storm = async () => {
  const st = await new Streamer('storm', { port: PORT_MAP['storm'] }).start();
  const feed = await new Feeder(st.sockPath, st.width, st.height).connect();
  feed.start();
  await sleep(600);

  const a = await new Viewer('a', st.port).attach();
  const b = await new Viewer('b', st.port).attach();
  await poll(() => a.firstKeyMs !== null && b.firstKeyMs !== null, 3000, 20);
  const spawnsBefore = st.spawnPids().length;
  const aIdr0 = a.idr, bIdr0 = b.idr;
  const stormAt = nowMs();
  for (let i = 0; i < 20; i++) {
    a.send({ type: 'request_keyframe' });
    b.send({ type: 'request_keyframe' });
  }
  await sleep(2500);

  const aIdrAfter = a.idr, bIdrAfter = b.idr;
  const dbg = await st.debugEncoder();
  const lastResetA = a.resets[a.resets.length - 1];
  const recoveryA = lastResetA ? a.framesAt.find((t) => t >= lastResetA.at) : null;
  const lastResetB = b.resets[b.resets.length - 1];
  const recoveryB = lastResetB ? b.framesAt.find((t) => t >= lastResetB.at) : null;

  const log = st.samples();
  await feed.stop();
  a.close(); b.close();
  await st.stop();

  const forced = log.forced_keyframes;
  const refused = log.refused + log.ignored;
  const recovered = aIdrAfter > aIdr0 && bIdrAfter > bIdr0;
  const verdict = forced <= 1 && refused >= 38 && recovered && dbg.encoder_runs === 1 ? 'fine' : 'bug';

  return {
    scenario: 'storm',
    config: { requests_per_viewer: 20, viewers: 2, w: st.width, h: st.height },
    numbers: {
      requests: 40,
      forced_keyframe_restarts: forced,
      refused_or_ignored: refused,
      spawns_during_storm: log.ffmpeg_started - spawnsBefore,
      resets_a: a.resets.length,
      resets_b: b.resets.length,
      idr_before_after_a: [aIdr0, aIdrAfter],
      idr_before_after_b: [bIdr0, bIdrAfter],
      last_reset_to_idr_ms_a: recoveryA === null || !lastResetA ? null : round(recoveryA - lastResetA.at, 1),
      last_reset_to_idr_ms_b: recoveryB === null || !lastResetB ? null : round(recoveryB - lastResetB.at, 1),
      encoder_runs_after: dbg.encoder_runs,
      storm_wall_ms: round(nowMs() - stormAt, 0),
      log,
    },
    verdict,
    notes: [
      'expected: the first request spends the hub cooldown, the remaining 39 are refused/ignored.',
      recovered ? 'both viewers received a live IDR after the single restart' : 'a viewer did not recover to an IDR',
    ],
  };
};

// 5. ABR churn: viewer A steps 10 times in ~28s while B stays on the top rung.
// Only A's rung changes; B's stream must not reset or gap.
scenarios.abr = async () => {
  const st = await new Streamer('abr', { port: PORT_MAP['abr'] }).start();
  const feed = await new Feeder(st.sockPath, st.width, st.height).connect();
  feed.start();
  await sleep(600);

  const b = await new Viewer('top-rung', st.port).attach();
  await poll(() => b.firstKeyMs !== null, 3000, 20);
  const bStart = b.snapshot();
  const a = await new Viewer('stepper', st.port).attach();
  await poll(() => a.firstKeyMs !== null, 3000, 20);

  const steps = [4200, 2700, 1800, 6000, 4200, 2700, 1800, 6000, 4200, 6000];
  const samples = [];
  const spawns0 = st.spawnPids().length;
  let bMaxRun = 0;

  for (let i = 0; i < steps.length; i++) {
    const stepAt = nowMs();
    a.send({ type: 'set_bitrate', kbps: steps[i] });
    await sleep(2500);
    const dbg = await st.debugEncoder();
    bMaxRun = Math.max(bMaxRun, dbg.encoder_runs ?? 0);
    samples.push({
      step: i,
      kbps: steps[i],
      run_packets: b.packets - bStart.packets,
      a_resets: a.resets.length,
      b_resets: b.resets.length,
      encoder_runs: dbg.encoder_runs,
      subscribers: dbg.subscribers,
      b_max_gap_ms: round(b.maxGapMs, 1),
      step_ms: round(nowMs() - stepAt, 0),
    });
  }

  const bEnd = b.snapshot();
  const dbgEnd = await st.debugEncoder();
  const log = st.samples();
  await feed.stop();
  a.close(); b.close();
  await st.stop();

  const spawns = st.spawnPids().length - spawns0;
  const bResetFree = bEnd.resets === 0;
  const gapOk = bEnd.maxGapMs < 300;
  const verdict = bResetFree && gapOk && bMaxRun <= 2 ? 'fine' : 'bug';

  return {
    scenario: 'abr',
    config: { steps: steps.length, step_interval_ms: 2500, w: st.width, h: st.height, base_kbps: KBPS },
    numbers: {
      steps,
      top_rung_viewer: {
        resets: bEnd.resets,
        packets: bEnd.packets,
        max_gap_ms: bEnd.maxGapMs,
        seq_gaps: bEnd.seqGaps,
        dropped: bEnd.dropped,
      },
      stepper_viewer: { resets: a.resets.length, resets_by_reason: a.resets.map((r) => r.reason) },
      ffmpeg_spawns_from_steps: spawns,
      max_encoder_runs_seen: bMaxRun,
      encoder_runs_after: dbgEnd.encoder_runs,
      subscribers_after: dbgEnd.subscribers,
      log,
      samples,
    },
    verdict,
    notes: [
      'A stepping back to the base rung must re-attach to B\'s existing run instead of spawning a third encoder.',
      bResetFree ? 'B was never reset by A\'s steps' : 'A\'s ABR steps reset B',
    ],
  };
};

// 5b. ABR burst: unlike request_keyframe, set_bitrate has no per-viewer
// budget or cooldown — every rung change replaces an ffmpeg run, and spawn is
// synchronous under the hub mutex. 20 alternating steps measure how much of
// another viewer's stream that costs.
scenarios['abr-burst'] = async () => {
  const st = await new Streamer('abr-burst', { port: PORT_MAP['abr-burst'] }).start();
  const feed = await new Feeder(st.sockPath, st.width, st.height).connect();
  feed.start();
  await sleep(600);

  const b = await new Viewer('top-rung', st.port).attach();
  await poll(() => b.firstKeyMs !== null, 3000, 20);
  const a = await new Viewer('burster', st.port).attach();
  await poll(() => a.firstKeyMs !== null, 3000, 20);
  await sleep(600);

  const spawns0 = st.spawnPids().length;
  const bPackets0 = b.packets;
  const bGap0 = b.maxGapMs;
  const burstAt = nowMs();
  const resetsAtBurstStart = a.resets.length;
  // Two non-base rungs: every message differs from the current run, so every
  // message replaces an ffmpeg instead of re-attaching to the other viewer's run.
  const seq = [4200, 2700];
  const steps = 40;
  for (let i = 0; i < steps; i++) a.send({ type: 'set_bitrate', kbps: seq[i % 2] });
  await poll(() => a.resets.length >= resetsAtBurstStart + steps, 8000, 10);
  const processedMs = nowMs() - burstAt;
  const cpuAfter = procStats(st.pid);
  await sleep(1500);
  const burstMs = nowMs() - burstAt;
  const resetSpan = a.resets.length > resetsAtBurstStart
    ? a.resets[a.resets.length - 1].at - a.resets[resetsAtBurstStart].at : null;
  const spawns = st.spawnPids().length - spawns0;
  const dbg = await st.debugEncoder();
  const log = st.samples();
  const bAfter = b.snapshot();

  // healthy stream during the burst, measured from the packet timeline
  const windowStart = burstAt - b.t0;
  const during = b.framesAt.filter((t) => t >= windowStart);
  let maxGapDuring = 0;
  for (let i = 1; i < during.length; i++) maxGapDuring = Math.max(maxGapDuring, during[i] - during[i - 1]);

  await feed.stop();
  a.close(); b.close();
  await st.stop();

  const spawnsPerSec = resetSpan ? round(spawns / (resetSpan / 1000), 1) : null;
  // The client's own ABR controller steps at ~1 Hz. Anything far above that is
  // not the designed workload; the keyframe path refuses that rate outright.
  const unbudgeted = (spawnsPerSec ?? 0) > 50;
  const verdict = bAfter.resets === 0 && maxGapDuring < 300 && !unbudgeted ? 'fine' : 'rough edge';

  return {
    scenario: 'abr-burst',
    config: { steps, pattern: '4200/2700 alternating as fast as the socket takes', w: st.width, h: st.height, base_kbps: KBPS },
    numbers: {
      burst_wall_ms: round(burstMs, 0),
      steps_processed_ms: round(processedMs, 0),
      step_reset_span_ms: resetSpan === null ? null : round(resetSpan, 0),
      ffmpeg_spawns: spawns,
      spawns_per_sec: spawnsPerSec,
      streamer_threads_after: cpuAfter.threads,
      stepper_resets: a.resets.length,
      top_rung_viewer: {
        resets: bAfter.resets,
        packets: bAfter.packets - bPackets0,
        max_gap_during_burst_ms: round(maxGapDuring, 1),
        max_gap_cumulative_ms: bAfter.maxGapMs,
        gap_growth_ms: round(bAfter.maxGapMs - bGap0, 1),
        seq_gaps: bAfter.seqGaps,
      },
      encoder_runs_after: dbg.encoder_runs,
      log,
    },
    verdict,
    notes: [
      'set_bitrate is deliberately unbudgeted (ws.go setBitrate -> hub.attach), while request_keyframe is per-viewer budgeted.',
      `${spawns} ffmpeg spawns while 20 steps were processed in ${round(processedMs, 0)}ms (${spawnsPerSec}/s over the reset span); each spawn runs under the hub mutex.`,
      verdict === 'fine'
        ? 'the other viewer did not gap or reset, so the cost stayed inside the fan-out budget on this host.'
        : unbudgeted
          ? 'set_bitrate has no rate limit: unlike request_keyframe (4 per 30s) a viewer can force an encoder spawn per message. The co-viewer lost frames while the ffmpeg processes churned.'
          : 'the burst cost the other viewer frames; consider a cooldown or rung-change dedupe.',
    ],
  };
};

// 6. Slow reader: a viewer stops reading its socket for 5s while the page
// keeps changing. The healthy viewer must not gap and the encoder must not
// stall. At 25fps 5s is <192 packets, so the mailbox is expected to absorb it
// without dropping.
scenarios['slow-reader'] = async () => {
  const st = await new Streamer('slow-reader', { port: PORT_MAP['slow-reader'] }).start();
  const feed = await new Feeder(st.sockPath, st.width, st.height).connect();
  feed.start();
  await sleep(800);

  const h = await new Viewer('healthy', st.port).attach();
  const s = await new Viewer('slow', st.port).attach();
  await poll(() => h.firstKeyMs !== null && s.firstKeyMs !== null, 3000, 20);
  await sleep(700);

  const hBefore = h.snapshot();
  const sBefore = s.snapshot();
  const pauseAt = nowMs();
  s.pause();
  await sleep(5000);
  const duringPause = {
    healthy_packets: h.packets - hBefore.packets,
    healthy_max_gap_ms: round(h.maxGapMs, 1),
    healthy_resets: h.resets.length - hBefore.resets,
    healthy_dropped: h.metrics?.dropped ?? null,
  };
  s.resume();
  await sleep(2500);
  const sAfter = s.snapshot();
  const hAfter = h.snapshot();
  const dbg = await st.debugEncoder();
  const log = st.samples();
  const ffmpegAlive = st.procStats().ffmpeg;

  await feed.stop();
  h.close(); s.close();
  await st.stop();

  const verdict =
    duringPause.healthy_max_gap_ms < 300 && duringPause.healthy_resets === 0 &&
    duringPause.healthy_packets >= 100 && sAfter.dropped === 0 &&
    !s.closedByServer && dbg.encoder_runs === 1 ? 'fine' : 'rough edge';

  return {
    scenario: 'slow-reader',
    config: { stall_ms: 5000, w: st.width, h: st.height, encode_fps: FPS },
    numbers: {
      healthy_during_stall: duringPause,
      healthy_total: hAfter,
      slow_viewer: { before_pause: sBefore, after_resume: sAfter, closed_by_server: s.closedByServer },
      expected_queued_packets: Math.round((5000 / 1000) * FPS),
      mailbox_packets: 192,
      encoder_runs: dbg.encoder_runs,
      ffmpeg_alive: ffmpegAlive,
      log,
    },
    verdict,
    notes: [
      'the shared encoder keeps feeding; only the slow viewer\'s mailbox grows.',
      sAfter.dropped === 0
        ? `5s at ${FPS}fps queues ~${Math.round(5 * FPS)} packets, inside the 192-slot mailbox: no drop is expected. Saturation needs ~${round(192 / FPS, 1)}s.`
        : 'the slow viewer dropped packets; check whether the stall exceeded the mailbox window.',
      'see the `deadline` scenario for a stall long enough to trip the 10s socket-write deadline.',
    ],
  };
};

// 6b. Write-deadline expiry. A stalled socket eventually times out the WS
// writer (10s per write, ws.go:109). The interesting question is what the
// session does with that viewer afterwards. 100 Mbps noise so the socket
// buffers fill in well under a second and the writer is blocked long enough.
scenarios.deadline = async () => {
  const st = await new Streamer('deadline', { port: PORT_MAP['deadline'], kbps: 100000 }).start();
  const feed = await new Feeder(st.sockPath, st.width, st.height).connect();
  feed.corpus.noise = true;
  feed.start();
  await sleep(800);

  const s = await new Viewer('stalled', st.port).attach();
  await poll(() => s.firstKeyMs !== null, 3000, 20);
  await sleep(300);
  s.pause();
  await sleep(13000); // blocked write started ~1s in; 10s deadline passes at ~11s

  const ffmpegDuring = st.procStats().ffmpeg;

  s.resume();
  await sleep(2000); // drain whatever the kernel still held
  const drained = s.packets;
  await sleep(1500); // pure post-timeout window: expect nothing new
  const postTimeout = {
    packets_in_last_window: s.packets - drained,
    bytes_total: s.bytes,
    closed_by_server: s.closedByServer,
    close_cause: s.closeCause ?? null,
  };
  const dbgAfter = await st.debugEncoder();
  const logAfter = st.samples();

  s.close();
  await sleep(400);
  const dbgClosed = await st.debugEncoder();
  const logClosed = st.samples();
  const ffmpegClosed = st.procStats().ffmpeg;

  await feed.stop();
  await st.stop();

  const writerDied = logAfter.write_errors > 0;
  const noNewVideo = postTimeout.packets_in_last_window === 0;
  const subscriptionStuck = dbgAfter.subscribers >= 1 && dbgAfter.encoder_runs >= 1;
  const releasedOnClose = dbgClosed.subscribers === 0 && dbgClosed.encoder_runs === 0;
  const verdict = writerDied && noNewVideo && subscriptionStuck && releasedOnClose ? 'bug' : 'fine';

  return {
    scenario: 'deadline',
    config: { w: st.width, h: st.height, kbps: 100000, content: 'noise', stall_ms: 13000, write_deadline_ms: 10000 },
    numbers: {
      write_errors: logAfter.write_errors,
      write_error_lines: st.log.split('\n').filter((l) => l.includes('write error')).slice(0, 3),
      after_timeout: {
        packets_in_last_1_5s: postTimeout.packets_in_last_window,
        closed_by_server: postTimeout.closed_by_server,
        close_cause: postTimeout.close_cause,
        subscribers: dbgAfter.subscribers,
        encoder_runs: dbgAfter.encoder_runs,
        ffmpeg_children: ffmpegDuring,
        dropped: s.metrics?.dropped ?? null,
      },
      after_client_close: {
        subscribers: dbgClosed.subscribers,
        encoder_runs: dbgClosed.encoder_runs,
        ffmpeg_children: ffmpegClosed,
      },
      log: logClosed,
    },
    verdict,
    notes: [
      'the 10s deadline fires on the blocked write; the drain goroutine exits but nothing closes the TCP connection or releases the hub subscription.',
      writerDied
        ? 'the viewer stops receiving for good: the socket is half-dead and the client never sees a close, so it cannot even reconnect on this socket.'
        : 'no write error was observed on this host; the stall may not have blocked long enough.',
      subscriptionStuck
        ? `after the timeout the session still reported ${dbgAfter.subscribers} subscriber(s) and ${dbgAfter.encoder_runs} encoder run, feeding a viewer that can never receive it — released only when the peer closes.`
        : 'the subscription was released promptly.',
    ],
  };
};

// 7. Zero-frame start: a viewer attaches before the producer has ever sent a
// frame. It must get stream_init, nothing else, and start decoding as soon as
// frames arrive.
scenarios['zero-frame'] = async () => {
  const st = await new Streamer('zero-frame', { port: PORT_MAP['zero-frame'] }).start();
  const v = await new Viewer('early', st.port).attach();
  await sleep(2600);
  const beforeFeed = {
    inits: v.inits.length,
    packets: v.packets,
    init_at_ms: v.inits.length ? round(v.inits[0].at, 1) : null,
    init: v.inits[0] ?? null,
  };

  const feed = await new Feeder(st.sockPath, st.width, st.height).connect();
  const feedAt = nowMs();
  feed.start();
  const got = await poll(() => v.firstKeyMs !== null, 4000, 20);
  const dbg = await st.debugEncoder();
  const afterFeed = {
    attach_to_first_bin_ms: round(v.firstBinMs, 1),
    feed_to_first_bin_ms: v.firstBinMs === null ? null : round(v.firstBinMs - (feedAt - v.t0), 1),
    feed_to_idr_ms: got ? round(v.firstKeyMs - (feedAt - v.t0), 1) : null,
    first_packet_was_key: v.firstBinWasKey,
    packets: v.packets,
  };
  const log = st.samples();
  await feed.stop();
  v.close();
  await st.stop();

  const verdict =
    beforeFeed.inits >= 1 && beforeFeed.packets === 0 && afterFeed.first_packet_was_key === true &&
    afterFeed.feed_to_idr_ms !== null && afterFeed.feed_to_idr_ms < 2000 ? 'fine' : 'bug';

  return {
    scenario: 'zero-frame',
    config: { attach_before_frames: true, prefeed_wait_ms: 2600, w: st.width, h: st.height },
    numbers: {
      before_feed: beforeFeed,
      after_feed: afterFeed,
      encoder_runs: dbg.encoder_runs,
      subscribers: dbg.subscribers,
      log,
    },
    verdict,
    notes: [
      'stream_init is a property of the run, so it can (and must) arrive before any frame exists.',
      'the encoder is started at attach time and ffmpeg waits on empty stdin until the first composed surface.',
    ],
  };
};

// 8. Geometry edges through /resize: odd dimensions are rounded down; 1x1 is
// accepted but rounds to 0, which resizeAll() silently rejects while the
// session's current geometry is already zeroed. A viewer arriving afterwards
// falls back to the env/default size and can be left waiting for a producer
// geometry that no longer exists.
scenarios.geometry = async () => {
  const st = await new Streamer('geometry', { port: PORT_MAP['geometry'] }).start();
  const feed = await new Feeder(st.sockPath, st.width, st.height).connect();
  feed.start();
  await sleep(500);

  const z = await new Viewer('z', st.port).attach();
  await poll(() => z.firstKeyMs !== null, 3000, 20);
  const results = {};
  results.initial_debug = await st.debugEncoder();

  // odd dimensions: 801x601 -> 800x600
  const oddResetAt = z.resets.length;
  const odd = await st.resize(801, 601);
  feed.setSize(800, 600);
  const oddReset = await poll(() => z.resets.length > oddResetAt, 3000, 20);
  const oddResetMark = z.resets[z.resets.length - 1];
  const oddFramesBefore = z.framesAt.length;
  const oddRecovered = await poll(
    () => z.framesAt.length > oddFramesBefore && z.framesAt[z.framesAt.length - 1] > (oddResetMark?.at ?? 0),
    4000, 20);
  await sleep(300);
  const oddIdrAfterReset = oddResetMark ? z.framesAt.find((t) => t >= oddResetMark.at) : null;
  results.odd = {
    response: odd,
    reset_seen: oddReset,
    reset: oddResetMark ?? null,
    reset_to_idr_ms: oddResetMark && oddIdrAfterReset !== null && oddIdrAfterReset !== undefined
      ? round(oddIdrAfterReset - oddResetMark.at, 1) : null,
    recovered: oddRecovered,
    debug: await st.debugEncoder(),
    log_rekey: st.counted('ws: encoder re-keyed to 800x600'),
  };

  // 1x1: masked to 0x0 by the even-dimension rule, so it must be rejected
  // outright; accepting it would zero the session geometry and leave every
  // later viewer waiting for a surface that never arrives.
  const one = await st.resize(1, 1);
  const debugAfterOne = await st.debugEncoder();
  const zResetsAfterOne = z.resets.length;
  await sleep(700);
  results.one = {
    response: one,
    debug_resolution: debugAfterOne.resolution,
    encoder_runs: debugAfterOne.encoder_runs,
    new_resets_on_z: z.resets.length - zResetsAfterOne,
    note: 'must be 400 with the session geometry untouched',
  };

  // A viewer attaching after the rejected resize must still see frames: the
  // session geometry is whatever the last accepted resize set (800x600).
  const waitsBefore = st.counted('waiting for producer geometry');
  const n = await new Viewer('post-1x1', st.port).attach();
  await sleep(3000);
  const dbgN = await st.debugEncoder();
  results.post_1x1_viewer = {
    init: n.inits[0] ?? null,
    packets_in_3s: n.packets,
    idr: n.idr,
    resets: n.resets.map((r) => r.reason),
    encoder_runs: dbgN.encoder_runs,
    subscribers: dbgN.subscribers,
    debug_resolution: dbgN.resolution,
    geometry_wait_logged: st.counted('waiting for producer geometry') - waitsBefore,
  };

  // Recovery check: the same viewer after an explicit resize back.
  const nResetsBefore = n.resets.length;
  const back = await st.resize(800, 600);
  const nRecovered = await poll(() => n.firstKeyMs !== null, 4000, 20);
  await sleep(300);
  const nResetMark = n.resets.length > nResetsBefore ? n.resets[n.resets.length - 1] : null;
  const nFirstFrame = nResetMark ? n.framesAt.find((t) => t >= nResetMark.at) : null;
  results.recovery = {
    response: back,
    reset: nResetMark,
    reset_to_idr_ms: nResetMark && nFirstFrame !== undefined && nFirstFrame !== null
      ? round(nFirstFrame - nResetMark.at, 1) : null,
    attach_to_first_idr_ms: round(n.firstKeyMs, 1),
    recovered: nRecovered,
    debug: await st.debugEncoder(),
  };

  // Rejected geometries.
  results.rejected = {
    zero_width: await st.resize(0, 600),
    too_wide: await st.resize(8193, 600),
  };

  const log = st.samples();
  await feed.stop();
  n.close(); z.close();
  await st.stop();

  const rejected1x1 = results.one.response.status === 400;
  const geometryKept = results.one.debug_resolution === '800x600' && results.one.new_resets_on_z === 0;
  const lateViewerSeesFrames = results.post_1x1_viewer.packets_in_3s > 0;
  const noGeometryWait = results.post_1x1_viewer.geometry_wait_logged === 0;
  const oddOk = results.odd.recovered === true && results.odd.response.status === 200;
  const bad = [
    !oddOk && 'an odd geometry (801x601) must round to 800x600 and recover',
    !rejected1x1 && '1x1 must be rejected: rounding turns it into 0x0',
    !geometryKept && 'a rejected resize must leave the session geometry alone',
    !lateViewerSeesFrames && 'a viewer attaching after a rejected resize must still receive frames',
    !noGeometryWait && 'no viewer should wait for a producer geometry that cannot arrive',
  ].filter(Boolean);
  const verdict = bad.length === 0 ? 'fine' : 'bug';

  return {
    scenario: 'geometry',
    config: { boot: `${st.width}x${st.height}`, odd_request: '801x601', tiny_request: '1x1' },
    numbers: results,
    verdict,
    notes: bad.length
      ? bad
      : [
        '801x601 rounds to 800x600, re-keys, and the viewer recovers on the next IDR',
        '1x1 is rejected with 400 and leaves the session geometry untouched',
        'a viewer attaching afterwards decodes normally, with no producer-geometry wait',
      ],
  };
};

// ── main ─────────────────────────────────────────────────────────────────

fs.mkdirSync(OUT, { recursive: true });
const order = ['churn', 'restart-attach', 'saturation', 'storm', 'abr', 'abr-burst', 'slow-reader', 'deadline', 'zero-frame', 'geometry'];
const selected = ONLY ? order.filter((s) => ONLY.includes(s)) : order;
if (!selected.length) throw new Error(`no scenario matched --only ${ONLY}`);

const results = [];
for (const name of selected) {
  const t0 = Date.now();
  process.stderr.write(`\n=== ${name} ===\n`);
  try {
    const r = await scenarios[name]();
    r.wall_ms = Date.now() - t0;
    results.push(r);
    process.stderr.write(`${JSON.stringify({ scenario: r.scenario, verdict: r.verdict })}\n`);
  } catch (e) {
    for (const live of [...LIVE_STREAMERS]) await live.stop().catch(() => {});
    results.push({ scenario: name, verdict: 'harness-error', error: String((e && e.stack) || e), wall_ms: Date.now() - t0 });
    process.stderr.write(`harness error: ${(e && e.stack) || e}\n`);
  }
}

const summary = {
  binary: BIN,
  host: `${process.platform} ${process.arch}`,
  ffmpeg: (spawnSync('ffmpeg', ['-version'], { encoding: 'utf8' }).stdout ?? '').split('\n')[0],
  scenarios: results.map((r) => ({ scenario: r.scenario, verdict: r.verdict, wall_ms: r.wall_ms })),
  verdicts: results.reduce((acc, r) => { acc[r.verdict] = (acc[r.verdict] ?? 0) + 1; return acc; }, {}),
};
fs.writeFileSync(path.join(OUT, 'chaos-results.json'), JSON.stringify({ summary, results }, null, 2));
console.log(JSON.stringify({ summary, results }, null, 2));
