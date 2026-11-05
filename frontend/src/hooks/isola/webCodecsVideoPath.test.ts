import { readFileSync } from 'node:fs';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { MAX_DECODE_BACKLOG_MS } from './decodeConstants';
import type { WCChunk, WCDecoder } from './webCodecsHost';
import {
  applyWcStreamInit,
  createWcAccessUnitSink,
  resetWcDecoder,
  type WcAccessUnitSink,
  type WcPushOutcome,
  type WcSinkStats,
  type WebCodecsVideoPathCallbacks,
  type WebCodecsVideoPathRefs,
} from './webCodecsVideoPath';

// ── What is simulated, and what is not ───────────────────────────────────
//
// Real: the Annex-B payload (ffmpeg x264, one slice per frame, IDR every 30
// frames), the access-unit splitter below, the admission/backpressure policy
// (WcAccessUnitSink), resetWcDecoder and applyWcStreamInit/ensureWcCanvas.
//
// Simulated: VideoDecoder itself. FakeVideoDecoder below implements the
// WebCodecs state machine (configure → configured, reset → "unconfigured"
// per spec, decode on an unconfigured decoder throws) plus a decode queue that
// drains at one frame per virtual millisecond, driven by the harness clock.
// Arrival times are scripted by the test. No real pixels are decoded — these
// tests measure queue depth in stream time, not SPS/PSNR.

const FIXTURE = `${process.cwd()}/src/hooks/isola/__fixtures__/stream-320x180-30fps.h264`;
const FPS = 30;
const FRAME_US = Math.round(1_000_000 / FPS);
const DECODE_MS = 1;

interface AccessUnit {
  isKey: boolean;
  data: Uint8Array;
}

/** Split an Annex-B stream into access units: parameter sets attach to the
 *  next VCL NAL (type 1 = non-IDR slice, 5 = IDR slice). */
function parseAccessUnits(buf: Buffer): AccessUnit[] {
  const starts: number[] = [];
  for (let i = 0; i + 2 < buf.length; i++) {
    if (buf[i] === 0 && buf[i + 1] === 0 && buf[i + 2] === 1) {
      starts.push(i);
      i += 2;
    }
  }
  const aus: AccessUnit[] = [];
  let pendingParams: Buffer[] = [];
  starts.forEach((start, idx) => {
    const nalStart = start + 3;
    const nalEnd = idx + 1 < starts.length ? starts[idx + 1] : buf.length;
    // Trim the 4-byte start code of the next NAL down to its 3-byte form.
    const body = buf.subarray(nalStart, nalEnd);
    const type = body[0] & 0x1f;
    if (type === 1 || type === 5) {
      aus.push({
        isKey: type === 5,
        data: new Uint8Array(Buffer.concat([...pendingParams, body])),
      });
      pendingParams = [];
    } else {
      pendingParams.push(body);
    }
  });
  return aus;
}

const AUS = parseAccessUnits(readFileSync(FIXTURE));

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

/** WebCodecs VideoDecoder state machine + queue, no real decoding. */
class FakeVideoDecoder implements WCDecoder {
  static constructions = 0;
  static instances: FakeVideoDecoder[] = [];

  state = 'unconfigured';
  queue: WCChunk[] = [];
  /** Every chunk ever handed to decode() — survives reset(), unlike the
   *  queue, so a test can count what was submitted. */
  decodeCalls: WCChunk[] = [];
  configureCalls: string[] = [];
  private expectKey = true;
  private readonly output: (frame: { close(): void; timestamp: number }) => void;

  constructor(init: { output: (frame: { close(): void; timestamp: number }) => void; error: (e: Error) => void }) {
    FakeVideoDecoder.constructions++;
    FakeVideoDecoder.instances.push(this);
    this.output = init.output;
  }

  configure(config: { codec: string; optimizeForLatency?: boolean }): void {
    if (this.state === 'closed') throw new Error('InvalidStateError: closed');
    this.state = 'configured';
    this.configureCalls.push(config.codec);
    this.expectKey = true;
  }

  decode(chunk: WCChunk): void {
    if (this.state !== 'configured') throw new Error(`InvalidStateError: state=${this.state}`);
    if (chunk.type === 'delta' && this.expectKey) {
      throw new Error('DataError: key frame required after configure');
    }
    this.expectKey = false;
    this.queue.push(chunk);
    this.decodeCalls.push(chunk);
  }

  /** Paint one queued frame (the harness clock decides when this happens). */
  paintOne(): void {
    const chunk = this.queue.shift();
    if (chunk) this.output({ close: () => {}, timestamp: chunk.timestamp });
  }

  reset(): void {
    if (this.state === 'closed') throw new Error('InvalidStateError: closed');
    // WebCodecs: "Set state to unconfigured" — a bare reset is NOT decodable.
    this.state = 'unconfigured';
    this.queue = [];
    this.expectKey = true;
  }

  close(): void {
    this.state = 'closed';
    this.queue = [];
  }
}

interface Harness {
  decoder: FakeVideoDecoder;
  sink: WcAccessUnitSink;
  push: (au: AccessUnit) => WcPushOutcome;
  advance: (ms: number) => void;
  stats: () => WcSinkStats;
  nowMs: () => number;
  skipAhead: number;
  keyframeSeen: () => boolean;
}

function makeHarness(): Harness {
  let now = 0;
  const keyframeSeen = { current: false };
  const tsUs = { current: 0 };
  FakeVideoDecoder.constructions = 0;
  FakeVideoDecoder.instances = [];
  const decoder = new FakeVideoDecoder({ output: () => {}, error: () => {} });
  decoder.state = 'configured';
  const cfg = { codec: 'avc1.42E01F', w: 320, h: 180 };
  const harness: Harness = {
    decoder,
    sink: null as unknown as WcAccessUnitSink,
    push: () => 'unavailable',
    advance: () => {},
    stats: () => harness.sink.stats(),
    nowMs: () => now,
    skipAhead: 0,
    keyframeSeen: () => keyframeSeen.current,
  };
  harness.sink = createWcAccessUnitSink({
    getDecoder: () => decoder,
    getChunkCtor: () => FakeEncodedVideoChunk,
    frameStepUs: () => FRAME_US,
    tsUs,
    isKeyframeSeen: () => keyframeSeen.current,
    setKeyframeSeen: (v) => { keyframeSeen.current = v; },
    onSkipAhead: () => {
      harness.skipAhead++;
      // The hook's implementation: abandon the queue, stay decodable.
      resetWcDecoder(decoder, cfg);
    },
    onDecodeError: (e) => { throw e; },
    now: () => now,
  });
  harness.push = (au) => harness.sink.push(au.isKey, au.data);
  harness.advance = (ms) => {
    let left = ms;
    while (left >= DECODE_MS && decoder.queue.length > 0) {
      decoder.paintOne();
      harness.sink.notePainted();
      left -= DECODE_MS;
      now += DECODE_MS;
    }
    now += left;
  };
  return harness;
}

describe('annex-b fixture', () => {
  it('is 60 real access units with an IDR every 30 frames', () => {
    expect(AUS).toHaveLength(60);
    expect(AUS.filter((a) => a.isKey)).toHaveLength(2);
    expect(AUS[0].isKey).toBe(true);
    expect(AUS[30].isKey).toBe(true);
    // Every AU starts with a NAL unit (parameter sets are attached up front).
    expect(AUS[0].data[0] & 0x1f).toBe(7); // SPS
    expect(AUS[1].data[0] & 0x1f).toBe(1); // non-IDR slice
  });
});

describe('WebCodecs decode backpressure', () => {
  let h: Harness;

  beforeEach(() => {
    h = makeHarness();
  });

  it('drops a 500 ms burst instead of replaying it', () => {
    // Joining picture: the first IDR, painted.
    expect(h.push(AUS[0])).toBe('resynced');
    h.advance(DECODE_MS);
    expect(h.stats().backlogMs).toBe(0);

    // 500 ms of stream (15 frames at 30 fps) delivered in one go: nothing can
    // paint between arrivals, so the decoder queue is the presentation delay.
    const burst = AUS.slice(1, 16);
    let peakBacklogMs = 0;
    const outcomes = burst.map((au) => {
      const out = h.push(au);
      peakBacklogMs = Math.max(peakBacklogMs, h.stats().backlogMs);
      return out;
    });

    // The queue never grew to the burst's 500 ms: the policy skipped ahead
    // once the backlog crossed the budget and refused the rest of the burst.
    expect(peakBacklogMs).toBeLessThan(MAX_DECODE_BACKLOG_MS + 2 * (FRAME_US / 1000));
    expect(h.stats().catchUps).toBe(1);
    expect(h.skipAhead).toBe(1);
    expect(outcomes.filter((o) => o === 'dropped' || o === 'waiting-key').length).toBeGreaterThan(0);

    // "Dropped, not replayed": most of those 15 frames never reached decode().
    const decodedFromBurst = h.decoder.decodeCalls.length - 1; // minus the IDR
    expect(decodedFromBurst).toBeLessThan(burst.length);
    expect(h.decoder.queue).toHaveLength(0); // skip-ahead abandoned the queue
    expect(h.decoder.state).toBe('configured'); // still usable
    expect(h.stats().droppedFrames).toBeGreaterThanOrEqual(burst.length - decodedFromBurst);
  });

  it('replays the whole burst with a straight pipe (the pre-fix behaviour)', () => {
    // Control measurement: the same 15 access units handed to the decoder with
    // no admission policy, which is what the client did before this change.
    const decoder = new FakeVideoDecoder({ output: () => {}, error: () => {} });
    decoder.configure({ codec: 'avc1.42E01F' });
    for (const au of AUS.slice(0, 16)) {
      decoder.decode(new FakeEncodedVideoChunk({
        type: au.isKey ? 'key' : 'delta',
        timestamp: 0,
        data: au.data,
      }));
    }
    // 16 frames queued = 533 ms of stream time waiting to be painted in order;
    // the viewer is watching the oldest of them.
    expect(decoder.queue).toHaveLength(16);
    expect(decoder.queue.length * (FRAME_US / 1000)).toBeGreaterThanOrEqual(500);
  });

  it('leaves a steady 30 fps stream untouched', () => {
    const outcomes: WcPushOutcome[] = [];
    let peakBacklogMs = 0;
    for (const au of AUS) {
      outcomes.push(h.push(au));
      peakBacklogMs = Math.max(peakBacklogMs, h.stats().backlogMs);
      h.advance(FRAME_US / 1000); // next arrival one frame interval later
    }

    expect(outcomes.filter((o) => o === 'decoded' || o === 'resynced')).toHaveLength(AUS.length);
    expect(h.stats().catchUps).toBe(0);
    expect(h.stats().droppedFrames).toBe(0);
    // One frame in flight at most — well inside the budget.
    expect(peakBacklogMs).toBeLessThanOrEqual(FRAME_US / 1000 + 1);
    expect(h.decoder.decodeCalls).toHaveLength(AUS.length);
  });

  it('jumps straight to an IDR that arrives while backed up', () => {
    expect(h.push(AUS[0])).toBe('resynced');
    h.advance(DECODE_MS);
    // Backlog first, then the IDR: the newest decodable frame instead of the
    // 30 deltas ahead of it.
    for (const au of AUS.slice(1, 16)) h.push(au);
    const before = h.decoder.decodeCalls.length;
    expect(h.push(AUS[30])).toBe('resynced');
    // Exactly the IDR was submitted on the skip-ahead, nothing queued before it.
    expect(h.decoder.decodeCalls.length).toBe(before + 1);
    expect(h.decoder.decodeCalls[h.decoder.decodeCalls.length - 1].type).toBe('key');
    expect(h.decoder.queue).toHaveLength(1);
    h.advance(DECODE_MS);
    expect(h.stats().backlogMs).toBe(0);
  });

  it('waits for the next IDR after a skip and resumes decoding there', () => {
    expect(h.push(AUS[0])).toBe('resynced');
    h.advance(DECODE_MS);
    for (const au of AUS.slice(1, 16)) h.push(au);
    expect(h.stats().catchUps).toBe(1);

    // Deltas between the skip and the next IDR are refused — decoding them
    // would paint garbage from a broken reference chain.
    const midGop = AUS.slice(16, 30).map((au) => h.push(au));
    expect(new Set(midGop)).toEqual(new Set(['waiting-key']));

    expect(h.push(AUS[30])).toBe('resynced');
    expect(h.push(AUS[31])).toBe('decoded');
    expect(h.keyframeSeen()).toBe(true);
  });
});

describe('VideoDecoder reuse across an ABR step', () => {
  function makeRefs(): WebCodecsVideoPathRefs {
    return {
      lastWcConfigRef: { current: null },
      canvasRef: { current: null },
      wcFramesRef: { current: 0 },
      wcDecodedThisSecondRef: { current: 0 },
      firstFrameRecordedRef: { current: false },
      connectStartedAtRef: { current: 0 },
      decodeErrorCountRef: { current: 0 },
      keyframeSeenRef: { current: false },
      decoderRef: { current: null },
      tsUsRef: { current: 0 },
      wcChunkCtorRef: { current: null },
      frameStepUsRef: { current: 33333 },
      mediaSourceRef: { current: null },
      sourceBufferRef: { current: null },
      pendingSegmentsRef: { current: [] },
      videoRef: { current: null },
    };
  }

  function makeCallbacks(): WebCodecsVideoPathCallbacks {
    return {
      setFirstFrame: vi.fn(),
      setMetrics: vi.fn(),
      beginRecovery: vi.fn(),
      requestKeyframeNow: vi.fn(),
      applyInsets: vi.fn(),
      letterboxBg: '#000',
      onVideoRemoved: vi.fn(),
      onCanvasMounted: vi.fn(),
    };
  }

  beforeEach(() => {
    FakeVideoDecoder.constructions = 0;
    FakeVideoDecoder.instances = [];
    vi.stubGlobal('VideoDecoder', FakeVideoDecoder);
    vi.stubGlobal('EncodedVideoChunk', FakeEncodedVideoChunk);
    // jsdom has no 2d context; the output callback would log a not-implemented
    // error on every painted frame.
    HTMLCanvasElement.prototype.getContext = (() => ({
      drawImage: () => {},
    })) as unknown as typeof HTMLCanvasElement.prototype.getContext;
  });

  it('keeps the same decoder and canvas when codec and dimensions are unchanged', () => {
    const refs = makeRefs();
    const callbacks = makeCallbacks();
    const container = document.createElement('div');
    const init = { codec: 'avc1.42E01F', width: 320, height: 180 };

    expect(applyWcStreamInit(init, container, refs, callbacks)).toBe(true);
    const decoder = refs.decoderRef.current;
    const canvas = refs.canvasRef.current;
    expect(decoder).toBeTruthy();
    expect(canvas).toBeTruthy();
    expect(FakeVideoDecoder.constructions).toBe(1);

    // stream_reset → reset+reconfigure (the hook path), then stream_init.
    expect(resetWcDecoder(decoder, refs.lastWcConfigRef.current)).toBe(true);
    expect(decoder!.state).toBe('configured');
    expect(applyWcStreamInit(init, container, refs, callbacks)).toBe(true);

    // The ABR step allocated nothing: same VideoDecoder, same canvas element,
    // no second canvas appended to the container.
    expect(FakeVideoDecoder.constructions).toBe(1);
    expect(refs.decoderRef.current).toBe(decoder);
    expect(refs.canvasRef.current).toBe(canvas);
    expect(container.querySelectorAll('canvas')).toHaveLength(1);
    // A test frame decodes on the reused decoder.
    expect(() => decoder!.decode(new FakeEncodedVideoChunk({
      type: 'key',
      timestamp: 0,
      data: AUS[0].data,
    }))).not.toThrow();
  });

  it('reuses the decoder and resizes the canvas in place when geometry changes', () => {
    const refs = makeRefs();
    const callbacks = makeCallbacks();
    const container = document.createElement('div');

    applyWcStreamInit({ codec: 'avc1.42E01F', width: 320, height: 180 }, container, refs, callbacks);
    const decoder = refs.decoderRef.current;
    const canvas = refs.canvasRef.current!;
    canvas.style.opacity = '1';

    // Scale-down ABR step: 320x180 → 160x90.
    expect(applyWcStreamInit({ codec: 'avc1.42E01F', width: 160, height: 90 }, container, refs, callbacks)).toBe(true);
    expect(refs.decoderRef.current).toBe(decoder);
    expect(refs.canvasRef.current).toBe(canvas);
    expect(canvas.width).toBe(160);
    expect(canvas.height).toBe(90);
    expect(canvas.style.opacity).toBe('1'); // not re-hidden on a step
    expect(FakeVideoDecoder.constructions).toBe(1);
  });

  it('reconfigures in place when the codec string changes', () => {
    const refs = makeRefs();
    const callbacks = makeCallbacks();
    const container = document.createElement('div');
    applyWcStreamInit({ codec: 'avc1.42E01F', width: 320, height: 180 }, container, refs, callbacks);
    const decoder = refs.decoderRef.current as FakeVideoDecoder;

    expect(applyWcStreamInit({ codec: 'avc1.640028', width: 320, height: 180 }, container, refs, callbacks)).toBe(true);
    expect(refs.decoderRef.current).toBe(decoder);
    expect(decoder.configureCalls).toEqual(['avc1.42E01F', 'avc1.640028']);
    expect(FakeVideoDecoder.constructions).toBe(1);
  });

  it('a bare reset is not decodable — the old stream_reset path killed the stream', () => {
    const refs = makeRefs();
    const callbacks = makeCallbacks();
    applyWcStreamInit({ codec: 'avc1.42E01F', width: 320, height: 180 }, document.createElement('div'), refs, callbacks);
    const decoder = refs.decoderRef.current as FakeVideoDecoder;

    // Pre-fix behaviour: reset() and no reconfigure. Per WebCodecs the state is
    // "unconfigured", so every later decode() throws and the client — which
    // checked state === 'configured' first — silently refused every access
    // unit until the geometry happened to change.
    decoder.reset();
    expect(decoder.state).toBe('unconfigured');
    expect(() => decoder.decode(new FakeEncodedVideoChunk({
      type: 'key', timestamp: 0, data: AUS[0].data,
    }))).toThrow(/InvalidStateError/);

    // Fixed path: the same decoder object, reconfigured in place.
    expect(resetWcDecoder(decoder, refs.lastWcConfigRef.current)).toBe(true);
    expect(() => decoder.decode(new FakeEncodedVideoChunk({
      type: 'key', timestamp: 0, data: AUS[0].data,
    }))).not.toThrow();
  });
});
