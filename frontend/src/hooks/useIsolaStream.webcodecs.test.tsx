import { readFileSync } from 'node:fs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, render } from '@testing-library/react';

import { useIsolaStream } from './useIsolaStream';
import type { WCChunk, WCDecoder } from './isola/webCodecsHost';
import type { IsolaStreamMetrics } from './isola/streamMetrics';

// ── End-to-end over the real message handler ──────────────────────────────
//
// Real: the hook, its binary packet handler (10-byte wire header → access unit
// branch), the access-unit sink and its backpressure policy, the stream_reset /
// stream_init handling, the 1 Hz metrics publication. Packets are real Annex-B
// bytes from an ffmpeg-encoded .h264 fixture.
//
// Simulated: the socket (FakeWebSocket), VideoDecoder (state machine + queue,
// painting one frame per 1 ms of fake time) and EncodedVideoChunk. No pixels
// are decoded; "presentation delay" here is queue depth in stream time.

const FIXTURE = `${process.cwd()}/src/hooks/isola/__fixtures__/stream-320x180-30fps.h264`;

interface AccessUnit {
  isKey: boolean;
  data: Uint8Array;
}

function parseAccessUnits(buf: Buffer): AccessUnit[] {
  const starts: number[] = [];
  for (let i = 0; i + 2 < buf.length; i++) {
    if (buf[i] === 0 && buf[i + 1] === 0 && buf[i + 2] === 1) {
      starts.push(i);
      i += 2;
    }
  }
  const aus: AccessUnit[] = [];
  let params: Buffer[] = [];
  starts.forEach((start, idx) => {
    const body = buf.subarray(start + 3, idx + 1 < starts.length ? starts[idx + 1] : buf.length);
    const type = body[0] & 0x1f;
    if (type === 1 || type === 5) {
      aus.push({ isKey: type === 5, data: new Uint8Array(Buffer.concat([...params, body])) });
      params = [];
    } else {
      params.push(body);
    }
  });
  return aus;
}

const AUS = parseAccessUnits(readFileSync(FIXTURE));

/** 10-byte wire header + payload: byte 0 = 0x02 (raw Annex-B access unit),
 *  byte 1 bit 0 = keyframe, bytes 2-5 = LE sequence. */
function accessUnitPacket(seq: number, au: AccessUnit): ArrayBuffer {
  const buf = new ArrayBuffer(10 + au.data.byteLength);
  const bytes = new Uint8Array(buf);
  const view = new DataView(buf);
  bytes[0] = 0x02;
  bytes[1] = au.isKey ? 0x01 : 0x00;
  view.setUint32(2, seq, true);
  view.setUint32(6, au.data.byteLength, true);
  bytes.set(au.data, 10);
  return buf;
}

class FakeEncodedVideoChunk implements WCChunk {
  type: 'key' | 'delta';
  timestamp: number;
  data: Uint8Array;

  constructor(init: WCChunk) {
    this.type = init.type;
    this.timestamp = init.timestamp;
    this.data = init.data;
  }
}

class DummyEncodedAudioChunk {}

class FakeVideoDecoder implements WCDecoder {
  static constructions = 0;
  static instances: FakeVideoDecoder[] = [];
  state = 'unconfigured';
  queue: WCChunk[] = [];
  decodeCalls: WCChunk[] = [];
  private expectKey = true;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private readonly output: (frame: { close(): void; timestamp: number }) => void;

  constructor(init: { output: (frame: { close(): void; timestamp: number }) => void; error: (e: Error) => void }) {
    FakeVideoDecoder.constructions++;
    FakeVideoDecoder.instances.push(this);
    this.output = init.output;
  }

  configure(): void {
    if (this.state === 'closed') throw new Error('InvalidStateError: closed');
    this.state = 'configured';
    this.expectKey = true;
  }

  decode(chunk: WCChunk): void {
    if (this.state !== 'configured') throw new Error(`InvalidStateError: state=${this.state}`);
    if (chunk.type === 'delta' && this.expectKey) throw new Error('DataError: key frame required');
    this.expectKey = false;
    this.queue.push(chunk);
    this.decodeCalls.push(chunk);
    this.schedulePaint();
  }

  reset(): void {
    if (this.state === 'closed') throw new Error('InvalidStateError: closed');
    // WebCodecs: reset leaves the decoder "unconfigured" — decoding again
    // requires configure(), which is why the client must not leave it bare.
    this.state = 'unconfigured';
    this.queue = [];
    this.expectKey = true;
    if (this.timer !== null) {
      clearTimeout(this.timer);
      this.timer = null;
    }
  }

  close(): void {
    this.state = 'closed';
    this.queue = [];
  }

  /** One decode per millisecond of fake time, like a decoder that is faster
   *  than the wire but not instantaneous. */
  private schedulePaint(): void {
    if (this.timer !== null) return;
    this.timer = setTimeout(() => {
      this.timer = null;
      const chunk = this.queue.shift();
      if (chunk) this.output({ close: () => {}, timestamp: chunk.timestamp });
      if (this.queue.length > 0) this.schedulePaint();
    }, 1);
  }
}

class FakeMediaSource {
  static isTypeSupported() { return true; }
  readyState: 'closed' | 'open' | 'ended' = 'closed';
  addEventListener() {}
  removeEventListener() {}
  addSourceBuffer(): never { throw new Error('MSE path not expected in WebCodecs mode'); }
  endOfStream() {}
}

class FakeWebSocket {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  static instances: FakeWebSocket[] = [];

  url: string;
  readyState = FakeWebSocket.CONNECTING;
  binaryType = 'blob';
  onopen: (() => void) | null = null;
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  readonly sent: unknown[] = [];

  constructor(url: string) {
    this.url = url;
    FakeWebSocket.instances.push(this);
  }
  send(data: unknown) { this.sent.push(data); }
  close() { this.readyState = FakeWebSocket.CLOSED; }
  addEventListener() {}
  removeEventListener() {}

  open() { this.readyState = FakeWebSocket.OPEN; this.onopen?.(); }
  deliver(data: unknown) { this.onmessage?.({ data }); }
}

/** The slice of the hook this test drives. */
interface StreamProbe {
  metrics: IsolaStreamMetrics;
  connect: (
    streamWsUrl: string,
    inputWsUrl: string,
    container: HTMLElement,
    viewOnly: boolean,
    bandwidthKbps?: number,
    sessionId?: string,
  ) => void;
}

let api: StreamProbe | null = null;

function Probe() {
  api = useIsolaStream();
  return null;
}

function streamWs(): FakeWebSocket {
  return FakeWebSocket.instances.find((w) => w.url.includes('channel=isola'))!;
}

function keyframeRequests(ws: FakeWebSocket): number {
  return ws.sent.filter((m) => typeof m === 'string' && m.includes('request_keyframe')).length;
}

function connectWebCodecs(): { ws: FakeWebSocket; container: HTMLElement } {
  const container = document.createElement('div');
  document.body.appendChild(container);
  render(<Probe />);
  act(() => {
    api!.connect(
      'ws://test/ws/session1?session_token=tok',
      'ws://test/ws/session1?channel=input&session_token=tok',
      container,
      false,
    );
  });
  const ws = streamWs();
  act(() => { ws.open(); });
  act(() => {
    ws.deliver(JSON.stringify({
      type: 'stream_init',
      mode: 'webcodecs',
      codec: 'avc1.42E01F',
      width: 320,
      height: 180,
      encode_fps: 30,
    }));
  });
  return { ws, container };
}

/** Let the decode queue drain and the 1 Hz metrics tick publish. The first
 *  tick only seeds the bitrate window, so a publication needs two. */
function settle(): void {
  act(() => { vi.advanceTimersByTime(2100); });
}

beforeEach(() => {
  FakeWebSocket.instances = [];
  FakeVideoDecoder.constructions = 0;
  FakeVideoDecoder.instances = [];
  api = null;
  vi.useFakeTimers();
  vi.stubGlobal('WebSocket', FakeWebSocket);
  vi.stubGlobal('VideoDecoder', FakeVideoDecoder);
  vi.stubGlobal('EncodedVideoChunk', FakeEncodedVideoChunk);
  vi.stubGlobal('EncodedAudioChunk', DummyEncodedAudioChunk);
  vi.stubGlobal('MediaSource', FakeMediaSource);
  URL.createObjectURL = vi.fn(() => 'blob:test') as unknown as typeof URL.createObjectURL;
  URL.revokeObjectURL = vi.fn(() => undefined) as unknown as typeof URL.revokeObjectURL;
  HTMLCanvasElement.prototype.getContext = (() => ({
    drawImage: () => {},
  })) as unknown as typeof HTMLCanvasElement.prototype.getContext;
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('useIsolaStream WebCodecs backpressure', () => {
  it('decodes a steady stream unchanged and publishes a near-zero backlog', () => {
    const { ws } = connectWebCodecs();

    // 30 frames, one per frame interval, each drained before the next arrives.
    for (let i = 0; i < 30; i++) {
      act(() => { ws.deliver(accessUnitPacket(i + 1, AUS[i])); });
      act(() => { vi.advanceTimersByTime(34); });
    }
    settle();

    expect(api!.metrics.decodeDroppedFrames).toBe(0);
    expect(api!.metrics.decodeCatchUps).toBe(0);
    expect(api!.metrics.decodeBacklogMs).toBe(0);
    expect(keyframeRequests(ws)).toBe(0);
  });

  it('drops a 500 ms burst and asks for a keyframe instead of replaying it', () => {
    const { ws } = connectWebCodecs();
    act(() => { ws.deliver(accessUnitPacket(1, AUS[0])); });
    act(() => { vi.advanceTimersByTime(5); });

    // 15 frames (500 ms at 30 fps) in a single task: nothing can paint
    // between arrivals, which is exactly what a burst looks like.
    const burst = AUS.slice(1, 16);
    act(() => {
      burst.forEach((au, i) => ws.deliver(accessUnitPacket(i + 2, au)));
    });
    settle();

    expect(api!.metrics.decodeCatchUps).toBe(1);
    expect(api!.metrics.decodeDroppedFrames).toBeGreaterThan(0);
    // The skip is not silent: the client asks the streamer for a fresh IDR so
    // the wait is an encoder restart, not the rest of the GOP.
    expect(keyframeRequests(ws)).toBe(1);
  });

  it('reuses the decoder and canvas across a stream_reset with identical codec and dimensions', () => {
    const { ws, container } = connectWebCodecs();
    const firstDecoder = FakeVideoDecoder.constructions;
    expect(firstDecoder).toBe(1);
    expect(container.querySelectorAll('canvas')).toHaveLength(1);

    act(() => { ws.deliver(accessUnitPacket(1, AUS[0])); });
    act(() => { vi.advanceTimersByTime(20); });

    // ABR step: the streamer restarts ffmpeg, announces reset + init with the
    // same codec and geometry.
    act(() => { ws.deliver(JSON.stringify({ type: 'stream_reset', reason: 'bitrate_change' })); });
    act(() => {
      ws.deliver(JSON.stringify({
        type: 'stream_init',
        mode: 'webcodecs',
        codec: 'avc1.42E01F',
        width: 320,
        height: 180,
        encode_fps: 30,
      }));
    });

    // Nothing was re-allocated: same VideoDecoder object, same canvas element.
    expect(FakeVideoDecoder.constructions).toBe(1);
    expect(container.querySelectorAll('canvas')).toHaveLength(1);

    // And the stream is alive: the replacement run's IDR decodes, where the
    // bare-reset path left the decoder "unconfigured" and silently refused
    // every access unit for the rest of the connection.
    const decoder = FakeVideoDecoder.instances[0];
    const decodedBefore = decoder.decodeCalls.length;
    act(() => { ws.deliver(accessUnitPacket(1, AUS[0])); });
    act(() => { ws.deliver(accessUnitPacket(2, AUS[1])); });
    expect(decoder.decodeCalls.length).toBe(decodedBefore + 2);
    expect(decoder.state).toBe('configured');
    settle();
    expect(api!.metrics.recentFps).toBeGreaterThan(0);
    expect(api!.metrics.decodeDroppedFrames).toBe(0);
  });
});
