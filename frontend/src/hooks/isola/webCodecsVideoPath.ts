import type { Dispatch, SetStateAction } from 'react';
import type { WCDecoder, WCEncodedVideoChunkCtor } from './webCodecsHost';
import { wcHost } from './webCodecsHost';
import type { IsolaStreamMetrics } from './streamMetrics';
import { DECODE_CATCHUP_MIN_INTERVAL_MS, MAX_DECODE_BACKLOG_MS } from './decodeConstants';

export interface WcStreamConfig {
  codec: string;
  w: number;
  h: number;
}

export interface WebCodecsVideoPathRefs {
  lastWcConfigRef: { current: WcStreamConfig | null };
  canvasRef: { current: HTMLCanvasElement | null };
  wcFramesRef: { current: number };
  wcDecodedThisSecondRef: { current: number };
  firstFrameRecordedRef: { current: boolean };
  connectStartedAtRef: { current: number };
  decodeErrorCountRef: { current: number };
  keyframeSeenRef: { current: boolean };
  decoderRef: { current: WCDecoder | null };
  tsUsRef: { current: number };
  wcChunkCtorRef: { current: WCEncodedVideoChunkCtor | null };
  frameStepUsRef: { current: number };
  mediaSourceRef: { current: MediaSource | null };
  sourceBufferRef: { current: SourceBuffer | null };
  pendingSegmentsRef: { current: ArrayBuffer[] };
  videoRef: { current: HTMLVideoElement | null };
}

export interface WebCodecsVideoPathCallbacks {
  setFirstFrame: (v: boolean) => void;
  setMetrics: Dispatch<SetStateAction<IsolaStreamMetrics>>;
  beginRecovery: () => void;
  requestKeyframeNow: () => void;
  applyInsets: (el: HTMLVideoElement | HTMLCanvasElement) => void;
  letterboxBg: string;
  onVideoRemoved: () => void;
  onCanvasMounted: (canvas: HTMLCanvasElement) => void;
  /** A frame reached the canvas. The access-unit sink uses this to know how
   *  much of the decode queue has drained (see MAX_DECODE_BACKLOG_MS). */
  onFramePainted?: () => void;
}

export function createWcDecoder(
  refs: WebCodecsVideoPathRefs,
  callbacks: WebCodecsVideoPathCallbacks,
): WCDecoder | null {
  const WCDecoderCtor = wcHost.VideoDecoder;
  const cfg = refs.lastWcConfigRef.current;
  if (!WCDecoderCtor || !cfg) return null;
  let decoder: WCDecoder;
  try {
    decoder = new WCDecoderCtor({
      output: (frame) => {
        refs.wcFramesRef.current++;
        refs.wcDecodedThisSecondRef.current++;
        const c = refs.canvasRef.current;
        if (c) {
          const ctx = c.getContext('2d');
          if (ctx) ctx.drawImage(frame as unknown as CanvasImageSource, 0, 0, c.width, c.height);
        }
        frame.close();
        callbacks.onFramePainted?.();
        if (c && c.style.opacity !== '1') c.style.opacity = '1';
        if (!refs.firstFrameRecordedRef.current) {
          refs.firstFrameRecordedRef.current = true;
          callbacks.setFirstFrame(true);
          if (refs.connectStartedAtRef.current) {
            const elapsed = Math.round(performance.now() - refs.connectStartedAtRef.current);
            callbacks.setMetrics((m) => (m.firstFrameMs === elapsed ? m : { ...m, firstFrameMs: elapsed }));
          }
        }
      },
      error: (e) => {
        console.error('[isola-wc] decoder error:', e);
        refs.decodeErrorCountRef.current++;
        refs.keyframeSeenRef.current = false; // out of sync until the next IDR
        callbacks.beginRecovery();
        callbacks.requestKeyframeNow();
        callbacks.setMetrics((m) => ({ ...m, decodeErrorCount: refs.decodeErrorCountRef.current }));
      },
    });
    decoder.configure({ codec: cfg.codec, optimizeForLatency: true });
  } catch (e) {
    console.error('[isola-wc] configure failed:', e);
    return null;
  }
  refs.decoderRef.current = decoder;
  refs.keyframeSeenRef.current = false;
  refs.tsUsRef.current = 0;
  return decoder;
}

/**
 * Abandon the decoder's queued access units and leave it configured for the
 * next run.
 *
 * `VideoDecoder.reset()` sets `state` to `"unconfigured"` (WebCodecs, "Reset
 * VideoDecoder"), not back to "configured": a bare `reset()` on a
 * stream_reset therefore made every later `decode()` throw, and the caller
 * silently refused every access unit (`decoder.state !== 'configured'`), so the
 * picture never came back — an ABR bitrate step killed WebCodecs mode for the
 * rest of the connection. Re-arming the same decoder object keeps the hardware
 * decoder allocated: `configure()` reuses it, a new VideoDecoder pays the init
 * again and drops every frame the old one had already queued.
 *
 * Returns false when there was nothing to reset or the reconfigure failed.
 */
export function resetWcDecoder(
  decoder: WCDecoder | null,
  cfg: WcStreamConfig | null,
): boolean {
  if (!decoder || !cfg || decoder.state === 'closed') return false;
  try {
    decoder.reset();
  } catch (e) {
    console.error('[isola-wc] reset failed:', e);
    return false;
  }
  try {
    decoder.configure({ codec: cfg.codec, optimizeForLatency: true });
    return true;
  } catch (e) {
    console.error('[isola-wc] reconfigure failed:', e);
    return false;
  }
}

// ── Access-unit admission: bounded decode backpressure ──────────────────
//
// The decoder path used to be a straight pipe: every access unit on the socket
// went into VideoDecoder.decode() in order, and the output callback painted it
// whenever it came out. Nothing bounded that queue, so a burst (or a tab that
// was throttled) put the past into the queue and the viewer watched it replay.
// The sink below is the bound: it tracks AUs submitted but not yet painted, and
// once that backlog exceeds MAX_DECODE_BACKLOG_MS it stops replaying it. An
// H.264 delta is worthless without the frames that were just dropped, so
// "skip" means the next decodable picture — an IDR. The sink abandons the
// queued AUs, tells the caller to drop and reconfigure the decoder and to ask
// the streamer for a keyframe, and refuses deltas until that IDR lands. A
// keyframe that arrives while backed up is decoded immediately (it is the
// newest decodable frame).
//
// Everything here is pure bookkeeping over an injected decoder, so the
// admission policy is testable without a browser.

export type WcPushOutcome =
  | 'decoded'      // delta submitted; the chain is intact
  | 'resynced'     // keyframe submitted after a gap — caller records recovery
  | 'waiting-key'  // dropped: no IDR yet, deltas are undecodable
  | 'dropped'      // dropped: the backlog policy skipped ahead
  | 'unavailable'; // no decoder/ctor, or decode() threw

export interface WcAccessUnitSinkDeps {
  getDecoder: () => WCDecoder | null;
  getChunkCtor: () => WCEncodedVideoChunkCtor | null;
  frameStepUs: () => number;
  /** Synthetic CFR timestamp handed to the next chunk; the sink advances it. */
  tsUs: { current: number };
  isKeyframeSeen: () => boolean;
  setKeyframeSeen: (v: boolean) => void;
  /** Called when the policy skipped ahead. The caller must abandon the
   *  decoder queue (reset + reconfigure) and request a fresh IDR. */
  onSkipAhead: (abandonedAccessUnits: number) => void;
  /** A delivery error — the caller's decode-error/recovery path. */
  onDecodeError: (e: unknown) => void;
  now?: () => number;
}

export interface WcSinkStats {
  /** Stream time between the oldest unpainted AU and the newest submitted. */
  backlogMs: number;
  backlogFrames: number;
  /** Access units refused or abandoned because of the backlog policy. */
  droppedFrames: number;
  catchUps: number;
}

export class WcAccessUnitSink {
  private readonly deps: WcAccessUnitSinkDeps;
  private readonly clock: () => number;
  // Timestamps (µs) of AUs submitted to the decoder and not yet painted, in
  // submission order. Output is FIFO, so the head is the frame in flight.
  private outstanding: number[] = [];
  private newestTsUs = 0;
  private droppedFrames = 0;
  private catchUps = 0;
  private lastSkipAt = -Infinity;
  // True from a policy skip until the replacement IDR lands: the AUs refused
  // in that window are part of the same loss and are counted as such.
  private skipPending = false;

  constructor(deps: WcAccessUnitSinkDeps) {
    this.deps = deps;
    this.clock = deps.now ?? (() => performance.now());
  }

  private stepUs(): number {
    return Math.max(1, this.deps.frameStepUs());
  }

  /** Current presentation delay carried by the decode queue, in ms. */
  backlogMs(): number {
    const n = this.outstanding.length;
    if (n === 0) return 0;
    return (this.newestTsUs - this.outstanding[0] + this.stepUs()) / 1000;
  }

  stats(): WcSinkStats {
    return {
      backlogMs: Math.round(this.backlogMs()),
      backlogFrames: this.outstanding.length,
      droppedFrames: this.droppedFrames,
      catchUps: this.catchUps,
    };
  }

  /** The decoder painted a frame (its output callback ran). */
  notePainted(): void {
    if (this.outstanding.length > 0) this.outstanding.shift();
  }

  /** A new encoder run (stream_reset / new decoder): no AU in flight survives
   *  it. Cumulative counters stay — they describe the whole view. */
  resetForNewRun(): void {
    this.outstanding.length = 0;
    this.newestTsUs = this.deps.tsUs.current ?? 0;
    this.skipPending = false;
  }

  push(isKey: boolean, data: Uint8Array): WcPushOutcome {
    const ctor = this.deps.getChunkCtor();
    const decoder = this.deps.getDecoder();
    if (!ctor || !decoder || decoder.state !== 'configured') return 'unavailable';

    if (!this.deps.isKeyframeSeen()) {
      if (!isKey) {
        if (this.skipPending) this.droppedFrames++;
        return 'waiting-key';
      }
      // An IDR starts a new chain and a new stream-time origin.
      this.deps.tsUs.current = 0;
      this.outstanding.length = 0;
      this.newestTsUs = 0;
      this.skipPending = false;
      this.deps.setKeyframeSeen(true);
      return this.submit(ctor, decoder, true, data) ? 'resynced' : 'unavailable';
    }

    const now = this.clock();
    if (this.backlogMs() > MAX_DECODE_BACKLOG_MS) {
      if (isKey) {
        return this.skipAhead(decoder, ctor, data, now);
      }
      if (now - this.lastSkipAt >= DECODE_CATCHUP_MIN_INTERVAL_MS) {
        return this.skipAhead(decoder, ctor, null, now);
      }
      // Over budget but a skip just happened: keep refusing rather than
      // replaying behind it. Refused AUs need a new IDR anyway.
      this.deps.setKeyframeSeen(false);
      this.skipPending = true;
      this.droppedFrames++;
      return 'dropped';
    }

    return this.submit(ctor, decoder, isKey, data) ? 'decoded' : 'unavailable';
  }

  private submit(
    ctor: WCEncodedVideoChunkCtor,
    decoder: WCDecoder,
    isKey: boolean,
    data: Uint8Array,
  ): boolean {
    const tsUs = this.deps.tsUs.current;
    try {
      decoder.decode(new ctor({
        type: isKey ? 'key' : 'delta',
        timestamp: tsUs,
        data,
      }));
    } catch (e) {
      this.deps.onDecodeError(e);
      return false;
    }
    this.outstanding.push(tsUs);
    this.newestTsUs = tsUs;
    this.deps.tsUs.current = tsUs + this.stepUs();
    return true;
  }

  /** Abandon the decoder queue. `keyData` re-enters the chain immediately with
   *  a keyframe; without it the sink waits for one. */
  private skipAhead(
    decoder: WCDecoder,
    ctor: WCEncodedVideoChunkCtor,
    keyData: Uint8Array | null,
    now: number,
  ): WcPushOutcome {
    const abandoned = this.outstanding.length;
    this.outstanding.length = 0;
    this.newestTsUs = this.deps.tsUs.current;
    this.catchUps++;
    this.lastSkipAt = now;
    this.deps.onSkipAhead(abandoned);
    this.droppedFrames += abandoned;
    if (keyData === null) {
      // No decodable picture in hand: refuse deltas until an IDR arrives.
      this.deps.setKeyframeSeen(false);
      this.skipPending = true;
      this.droppedFrames++;
      return 'dropped';
    }
    this.deps.setKeyframeSeen(true);
    this.deps.tsUs.current = 0;
    this.skipPending = false;
    return this.submit(ctor, decoder, true, keyData) ? 'resynced' : 'unavailable';
  }
}

export function createWcAccessUnitSink(deps: WcAccessUnitSinkDeps): WcAccessUnitSink {
  return new WcAccessUnitSink(deps);
}

/**
 * The canvas is per-viewer, not per-run: an ABR step that changes the encode
 * scale used to throw the element away and build a new one at opacity 0, which
 * blinked the picture and re-ran inset math for nothing. Resize in place
 * instead. Only the first call creates an element.
 */
export function ensureWcCanvas(
  msg: { width?: number; height?: number },
  container: HTMLElement,
  refs: WebCodecsVideoPathRefs,
  callbacks: WebCodecsVideoPathCallbacks,
): HTMLCanvasElement {
  const w = typeof msg.width === 'number' && msg.width > 0 ? msg.width : 1280;
  const h = typeof msg.height === 'number' && msg.height > 0 ? msg.height : 720;
  const existing = refs.canvasRef.current;
  if (existing) {
    if (existing.width !== w || existing.height !== h) {
      existing.width = w;
      existing.height = h;
      callbacks.applyInsets(existing);
    }
    // Already showing a picture (or about to): never re-hide it on a step.
    existing.style.opacity = '1';
    return existing;
  }
  const canvas = document.createElement('canvas');
  canvas.width = w;
  canvas.height = h;
  callbacks.applyInsets(canvas);
  canvas.style.objectFit = 'contain';
  canvas.style.backgroundColor = callbacks.letterboxBg;
  canvas.style.cursor = 'default';
  canvas.style.opacity = '0';
  canvas.style.transition = 'opacity 200ms ease-out';
  container.appendChild(canvas);
  refs.canvasRef.current = canvas;
  callbacks.onCanvasMounted(canvas);
  return canvas;
}

/**
 * Apply a `stream_init` to the WebCodecs path, reusing what is still valid.
 *
 * The streamer always follows `stream_reset` with `stream_init`, and on an ABR
 * bitrate step (or a forced keyframe) the codec string and the geometry are
 * unchanged. There is nothing to rebuild in that case: the decoder was already
 * reconfigured by resetWcDecoder and is waiting for the next IDR, and the
 * canvas still holds the last picture. Recreating them cost a VideoDecoder
 * init (hardware decoder allocation) plus a blank canvas on every step.
 */
export function applyWcStreamInit(
  msg: { codec?: string; width?: number; height?: number },
  container: HTMLElement,
  refs: WebCodecsVideoPathRefs,
  callbacks: WebCodecsVideoPathCallbacks,
): boolean {
  const WCChunkCtor = wcHost.EncodedVideoChunk;
  if (!wcHost.VideoDecoder || !WCChunkCtor) return false;
  refs.wcChunkCtorRef.current = WCChunkCtor;
  const codec = typeof msg.codec === 'string' && msg.codec.startsWith('avc1') ? msg.codec : 'avc1.42E01F';
  const canvas = ensureWcCanvas(msg, container, refs, callbacks);
  const prev = refs.lastWcConfigRef.current;
  const decoder = refs.decoderRef.current;
  refs.lastWcConfigRef.current = { codec, w: canvas.width, h: canvas.height };

  if (decoder && decoder.state !== 'closed') {
    // Same codec string: the decoder is already configured for this run (the
    // stream_reset path reconfigured it). A changed codec — or a decoder some
    // other path left unconfigured — does need configure.
    if (!prev || prev.codec !== codec || decoder.state !== 'configured') {
      try {
        decoder.configure({ codec, optimizeForLatency: true });
      } catch (e) {
        console.error('[isola-wc] reconfigure failed:', e);
        try { decoder.close(); } catch { /* ignore */ }
        refs.decoderRef.current = null;
        return false;
      }
    }
    callbacks.beginRecovery();
    return true;
  }

  if (!createWcDecoder(refs, callbacks)) return false;
  callbacks.beginRecovery(); // no IDR yet — recover if one does not arrive
  return true;
}

export function setupWebCodecsVideoPath(
  msg: { codec?: string; width?: number; height?: number },
  container: HTMLElement,
  refs: WebCodecsVideoPathRefs,
  callbacks: WebCodecsVideoPathCallbacks,
): boolean {
  const WCDecoderCtor = wcHost.VideoDecoder;
  const WCChunkCtor = wcHost.EncodedVideoChunk;
  if (!WCDecoderCtor || !WCChunkCtor) return false;

  // First entry into WebCodecs mode: drop the MSE plumbing, which this mode
  // never uses. A decoder that already exists belongs to the previous mode.
  try {
    if (refs.mediaSourceRef.current && refs.mediaSourceRef.current.readyState === 'open') {
      refs.mediaSourceRef.current.endOfStream();
    }
  } catch { /* ignore */ }
  refs.mediaSourceRef.current = null;
  refs.sourceBufferRef.current = null;
  refs.pendingSegmentsRef.current = [];
  if (refs.decoderRef.current) {
    try { refs.decoderRef.current.close(); } catch { /* ignore */ }
    refs.decoderRef.current = null;
  }
  if (refs.videoRef.current) {
    refs.videoRef.current.src = '';
    refs.videoRef.current.remove();
    refs.videoRef.current = null;
    callbacks.onVideoRemoved();
  }

  if (!applyWcStreamInit(msg, container, refs, callbacks)) return false;
  refs.frameStepUsRef.current = Math.round(1_000_000 / 30);
  return true;
}
