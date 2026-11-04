import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, render } from '@testing-library/react';

import { useIsolaStream } from './useIsolaStream';

// ── Test doubles ──────────────────────────────────────────────────────────
//
// Drives the real hook over a fake socket + a fake MediaSource. The WebCodecs
// path is deliberately left OFF (no VideoDecoder/EncodedVideoChunk globals), so
// the client negotiates MSE mode and the binary 0x01 segment branch — one of the
// two frame paths that used to call setMetrics per packet — is what runs.

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
  // No onclose on close(): teardown must not trigger the reconnect backoff.
  close() { this.readyState = FakeWebSocket.CLOSED; }
  addEventListener() {}
  removeEventListener() {}

  open() { this.readyState = FakeWebSocket.OPEN; this.onopen?.(); }
  deliver(data: unknown) { this.onmessage?.({ data }); }
}

class FakeMediaSource {
  static isTypeSupported() { return true; }
  readyState: 'closed' | 'open' | 'ended' = 'closed';
  addEventListener() {}
  removeEventListener() {}
  addSourceBuffer(): never { throw new Error('addSourceBuffer not expected in this test'); }
  endOfStream() {}
}

// A 10-byte header + payload fMP4 path packet: byte0 0x01, byte1 keyframe flag,
// bytes 2-5 sequence (LE), bytes 6-9 payload length (LE).
function segmentPacket(seq: number, isKey: boolean): ArrayBuffer {
  const payloadLen = 32;
  const buf = new ArrayBuffer(10 + payloadLen);
  const dv = new DataView(buf);
  const bytes = new Uint8Array(buf);
  bytes[0] = 0x01;
  bytes[1] = isKey ? 0x01 : 0x00;
  dv.setUint32(2, seq, true);
  dv.setUint32(6, payloadLen, true);
  return buf;
}

let renders = 0;
let api: ReturnType<typeof useIsolaStream> | null = null;

function Probe() {
  api = useIsolaStream();
  renders += 1;
  return null;
}

describe('useIsolaStream frame path', () => {
  beforeEach(() => {
    renders = 0;
    api = null;
    FakeWebSocket.instances = [];
    vi.useFakeTimers();
    vi.stubGlobal('WebSocket', FakeWebSocket);
    vi.stubGlobal('MediaSource', FakeMediaSource);
    URL.createObjectURL = vi.fn(() => 'blob:test') as unknown as typeof URL.createObjectURL;
    URL.revokeObjectURL = vi.fn(() => undefined) as unknown as typeof URL.revokeObjectURL;
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it('does not re-render per arriving segment, and still publishes the counters at 1 Hz', () => {
    const container = document.createElement('div');
    document.body.appendChild(container);

    render(<Probe />);

    act(() => {
      api!.connect(
        'ws://test/ws/session1?session_token=tok',
        'ws://test/ws/session1?channel=input&session_token=tok',
        container,
        false,
        undefined,
        'session1',
      );
    });

    const streamWs = FakeWebSocket.instances.find((w) => w.url.includes('channel=isola'));
    expect(streamWs).toBeDefined();

    act(() => { streamWs!.open(); });
    act(() => {
      streamWs!.deliver(JSON.stringify({
        type: 'stream_init', codec: 'avc1.42E01F', width: 1280, height: 720,
      }));
    });

    // Each deliver() runs in its own act(), mirroring one WebSocket task per
    // message — so a setMetrics in the frame handler would produce one render
    // per packet, exactly as it would in the browser.
    const N = 120;
    const before = renders;
    for (let i = 1; i <= N; i++) {
      const pkt = segmentPacket(i, i % 30 === 0);
      act(() => { streamWs!.deliver(pkt); });
    }
    const perFrameRenders = renders - before;

    // The whole point of the change: ~0 renders per frame, not ~N.
    expect(perFrameRenders).toBeLessThanOrEqual(2);

    // And the values are not lost — the 1 Hz tick surfaces the latest counters.
    act(() => { vi.advanceTimersByTime(2200); });
    expect(api!.metrics.sequenceNumber).toBe(N);
    expect(api!.metrics.keyframeCount).toBe(4); // seq 30, 60, 90, 120
  });

  // Generalised setup for the tests below.
  function connectStream(): FakeWebSocket {
    const container = document.createElement('div');
    document.body.appendChild(container);
    render(<Probe />);
    act(() => {
      api!.connect(
        'ws://test/ws/session1?session_token=tok',
        'ws://test/ws/session1?channel=input&session_token=tok',
        container,
        false,
        undefined,
        'session1',
      );
    });
    const streamWs = FakeWebSocket.instances.find((w) => w.url.includes('channel=isola'))!;
    act(() => { streamWs.open(); });
    act(() => {
      streamWs.deliver(JSON.stringify({
        type: 'stream_init', codec: 'avc1.42E01F', width: 1280, height: 720,
      }));
    });
    return streamWs;
  }

  function keyframeRequests(ws: FakeWebSocket): number {
    return ws.sent.filter((m) => typeof m === 'string' && m.includes('request_keyframe')).length;
  }

  it('consumes the server dropped counter when present and tolerates its absence', () => {
    const ws = connectStream();

    // Older streamer: no `dropped` field. Existing fields must still land and
    // serverDropped must not regress to a bogus value.
    act(() => { ws.deliver(JSON.stringify({ type: 'metrics', bitrate_kbps: 2000, sent_kbps: 1900 })); });
    expect(api!.metrics.targetBitrateKbps).toBe(2000);
    expect(api!.metrics.serverDropped).toBe(0);

    act(() => { ws.deliver(JSON.stringify({ type: 'metrics', dropped: 7 })); });
    expect(api!.metrics.serverDropped).toBe(7);

    // A later message without it keeps the last value instead of zeroing it.
    act(() => { ws.deliver(JSON.stringify({ type: 'metrics', bitrate_kbps: 2100 })); });
    expect(api!.metrics.targetBitrateKbps).toBe(2100);
    expect(api!.metrics.serverDropped).toBe(7);
  });

  it('requests one keyframe per loss burst and publishes the loss counts', () => {
    const ws = connectStream();

    for (let seq = 1; seq <= 3; seq++) {
      act(() => { ws.deliver(segmentPacket(seq, seq === 1)); });
    }
    expect(keyframeRequests(ws)).toBe(0);

    // 4..8 missing: five consecutive packets shed by the server.
    act(() => { ws.deliver(segmentPacket(9, false)); });
    expect(keyframeRequests(ws)).toBe(1);

    // The same burst keeps growing without spending the budget again.
    act(() => { ws.deliver(segmentPacket(10, false)); });
    act(() => { ws.deliver(segmentPacket(30, false)); });
    expect(keyframeRequests(ws)).toBe(1);

    act(() => { vi.advanceTimersByTime(2200); });
    expect(api!.metrics.packetsLost).toBe(24); // 4..8 (5) + 11..29 (19)
    expect(api!.metrics.packetLossRatio).toBeGreaterThan(0);
  });
});
