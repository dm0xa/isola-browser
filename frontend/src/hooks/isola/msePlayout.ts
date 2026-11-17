import type { Dispatch, SetStateAction } from 'react';
import {
  APPEND_MAX_RETRIES,
  APPEND_RETRY_MS,
  LIVE_EDGE_SEC,
  MAX_AHEAD_SEC,
  MAX_PLAYOUT_LAG_SEC,
} from './decodeConstants';
import type { IsolaStreamMetrics } from './streamMetrics';

export type MetricsSetter = Dispatch<SetStateAction<IsolaStreamMetrics>>;

// Playout state that used to be pushed through setMetrics on every appended
// segment (roughly the segment rate). syncPlayout now writes it to a ref and the
// viewer's 1 Hz metrics tick publishes it, so buffered video arriving no longer
// re-renders the whole viewer per segment.
export interface PlayoutMetrics {
  playoutLagMs: number;
  bufferedSec: number;
  playbackRate: number;
  videoWidth: number;
  videoHeight: number;
}

export function createPlayoutMetrics(): PlayoutMetrics {
  return { playoutLagMs: 0, bufferedSec: 0, playbackRate: 1, videoWidth: 0, videoHeight: 0 };
}

/** The metrics fields the 1 Hz tick publishes from the playout state. */
export function playoutMetricsPatch(
  p: PlayoutMetrics,
): Pick<IsolaStreamMetrics, 'playoutLagMs' | 'bufferedSec' | 'playbackRate' | 'videoWidth' | 'videoHeight'> {
  return {
    playoutLagMs: p.playoutLagMs,
    bufferedSec: p.bufferedSec,
    playbackRate: p.playbackRate,
    videoWidth: p.videoWidth,
    videoHeight: p.videoHeight,
  };
}

export interface MsePlayoutRefs {
  sourceBufferRef: { current: SourceBuffer | null };
  pendingSegmentsRef: { current: Uint8Array<ArrayBuffer>[] };
  videoRef: { current: HTMLVideoElement | null };
  appendCountRef: { current: number };
  appendFailRef: { current: number };
  appendRetryTimerRef: { current: ReturnType<typeof setTimeout> | null };
  playoutRef: { current: PlayoutMetrics };
}

export function syncPlayout(refs: MsePlayoutRefs): void {
  const sb = refs.sourceBufferRef.current;
  const video = refs.videoRef.current;
  if (!sb || sb.updating || !video || sb.buffered.length === 0) return;

  const end = sb.buffered.end(sb.buffered.length - 1);
  const lagSec = end - video.currentTime;
  const start = sb.buffered.start(0);
  // Playout state goes to a ref, never setMetrics: this runs once per appended
  // segment (~the segment rate), so publishing here re-rendered the viewer at
  // the frame rate. The 1 Hz metrics tick publishes it (playoutMetricsPatch).
  const p = refs.playoutRef.current;
  p.playoutLagMs = Math.round(lagSec * 1000);
  p.bufferedSec = Math.round((end - start) * 100) / 100;
  p.playbackRate = video.playbackRate;
  p.videoWidth = video.videoWidth;
  p.videoHeight = video.videoHeight;

  // Stay near live edge — large buffers cause stutter/blink on catch-up.
  if (lagSec > MAX_PLAYOUT_LAG_SEC) {
    video.playbackRate = 1.0;
    video.currentTime = Math.max(start, end - LIVE_EDGE_SEC);
  } else if (video.playbackRate !== 1.0) {
    video.playbackRate = 1.0;
  }

  const ct = video.currentTime;
  const trimEnd = ct - 2.0;
  if (trimEnd > start + 0.5 && !sb.updating) {
    try {
      sb.remove(start, trimEnd);
    } catch {
      /* ignore */
    }
  }
}

export function flushPendingSegments(
  refs: MsePlayoutRefs,
  sendControl: (msg: Record<string, unknown>) => void,
  onFlushAgain: () => void,
): void {
  const sb = refs.sourceBufferRef.current;
  const pending = refs.pendingSegmentsRef.current;
  if (!sb || sb.updating) return;

  syncPlayout(refs);

  // syncPlayout's trim (sb.remove) flips sb.updating synchronously; appending
  // behind a pending removal throws InvalidStateError. Bail — the removal's
  // updateend re-enters flushPending and appends then.
  if (sb.updating) return;

  if (pending.length === 0) return;

  const video = refs.videoRef.current;
  if (video && sb.buffered.length > 0) {
    const ahead = sb.buffered.end(sb.buffered.length - 1) - video.currentTime;
    if (ahead > MAX_AHEAD_SEC) {
      refs.pendingSegmentsRef.current = [];
      video.currentTime = Math.max(
        sb.buffered.start(0),
        sb.buffered.end(sb.buffered.length - 1) - LIVE_EDGE_SEC,
      );
      return;
    }
  }

  const seg = pending.shift()!;
  try {
    sb.appendBuffer(seg);
    refs.appendCountRef.current++;
    refs.appendFailRef.current = 0;
  } catch (e) {
    // A single throw used to null the SourceBuffer, which froze the stream
    // permanently (every later segment just queued up). Transient throws —
    // a remove settling, quota pressure — get a bounded retry instead.
    refs.appendFailRef.current++;
    pending.unshift(seg);
    if (refs.appendFailRef.current < APPEND_MAX_RETRIES) {
      if (refs.appendRetryTimerRef.current === null) {
        refs.appendRetryTimerRef.current = setTimeout(() => {
          refs.appendRetryTimerRef.current = null;
          onFlushAgain();
        }, APPEND_RETRY_MS);
      }
      return;
    }
    console.error('[isola] appendBuffer retry limit, resetting', e);
    refs.appendFailRef.current = 0;
    refs.sourceBufferRef.current = null;
    refs.pendingSegmentsRef.current = [];
    // request_keyframe restarts the encoder; its stream_reset + stream_init
    // rebuild the MediaSource through the normal path.
    sendControl({ type: 'request_keyframe' });
  }
}

export function wireVideoFps(
  video: HTMLVideoElement,
  setMetrics: MetricsSetter,
): () => void {
  let frames = 0;
  let lastReport = performance.now();
  let rafId = 0;
  let tick: ReturnType<typeof setInterval> | null = null;

  const report = () => {
    const now = performance.now();
    const fps = Math.round((frames * 1000) / (now - lastReport));
    frames = 0;
    lastReport = now;
    setMetrics((m) => (m.recentFps === fps ? m : { ...m, recentFps: fps }));
  };

  const onFrame = () => {
    frames++;
    if (performance.now() - lastReport >= 1000) report();
    if (video.isConnected) {
      rafId = video.requestVideoFrameCallback(onFrame);
    }
  };

  if (typeof video.requestVideoFrameCallback === 'function') {
    rafId = video.requestVideoFrameCallback(onFrame);
  } else {
    const onTime = () => { frames++; };
    video.addEventListener('timeupdate', onTime);
    tick = setInterval(report, 1000);
    return () => {
      if (tick) clearInterval(tick);
      video.removeEventListener('timeupdate', onTime);
    };
  }

  return () => {
    if (rafId && typeof video.cancelVideoFrameCallback === 'function') {
      video.cancelVideoFrameCallback(rafId);
    }
  };
}

/** MSE playout sync hook placeholder — playout is driven via syncPlayout on append. */
export function wirePlayoutSync(_video: HTMLVideoElement): () => void {
  return () => {};
}
