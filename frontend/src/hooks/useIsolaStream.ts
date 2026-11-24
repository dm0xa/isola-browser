import { useState, useRef, useCallback, useEffect } from 'react';
import { getSessionToken } from '../lib/sessionToken';
import { getIsolaTabs } from '../lib/config';
import { useTheme } from '../lib/ThemeContext';
import {
  abrTick,
  ABR_RESTART_PAUSE_MS,
  RTT_EMA_ALPHA,
  abrLadderKbps,
  loadPersistedQuality,
  QUALITY_RUNG,
  type IsolaQuality,
} from './isola/abr';
import {
  KEYFRAME_REQUEST_TIMEOUT_MS,
  MAX_KEYFRAME_REQUESTS,
  MAX_PENDING_SEGMENTS,
} from './isola/decodeConstants';
import { pickMseCodec, MSE_CODEC_FALLBACK } from './isola/mseCodec';
import {
  flushPendingSegments,
  wirePlayoutSync,
  wireVideoFps as wireVideoFpsHelper,
  createPlayoutMetrics,
  playoutMetricsPatch,
  type PlayoutMetrics,
} from './isola/msePlayout';
import {
  applyWcStreamInit,
  createWcAccessUnitSink,
  createWcDecoder as buildWcDecoder,
  resetWcDecoder,
  setupWebCodecsVideoPath,
} from './isola/webCodecsVideoPath';
import { isPlaceholderUrl, seedForActiveTab, shouldCarryUrlEdit, shouldSkipTabBarRebuild, tabLabels } from './isola/addressBar';
import { isSessionTerminated } from './isola/sessionLiveness';
import type { IsolaStreamMetrics } from './isola/streamMetrics';
import {
  wcHost,
  type WCAudioChunkCtor,
  type WCAudioDecoder,
  type WCChunk,
  type WCDecoder,
} from './isola/webCodecsHost';

export type { IsolaQuality, IsolaNetQuality, AbrState, AbrTick, AbrResult } from './isola/abr';
export { abrTick } from './isola/abr';
export type { IsolaStreamMetrics } from './isola/streamMetrics';

// setAddressValue stores the rendered value so a later tabs broadcast can tell an untouched field from an in-flight edit.
function setAddressValue(input: HTMLInputElement, value: string) {
  input.value = value;
  input.dataset.shownUrl = value;
}

// Imperative DOM (not React) — no Tailwind dark: variant, colors come from this palette.
const TABBAR_PALETTE = {
  dark: {
    barBg: 'rgba(17, 24, 39, 0.88)',
    barText: '#e5e7eb',
    chipActiveBg: '#374151',
    chipActiveText: '#ffffff',
    buttonBg: 'rgba(255, 255, 255, 0.08)',
    buttonBgHover: 'rgba(255, 255, 255, 0.16)',
    buttonText: '#e5e7eb',
    buttonTextHover: '#ffffff',
    inputBg: '#111827',
    inputBorder: '#4b5563',
    inputText: '#e5e7eb',
    menuBg: '#1f2937',
    menuBorder: '#374151',
    menuText: '#e5e7eb',
    menuItemHoverBg: '#374151',
    menuShadow: '0 8px 24px rgba(0, 0, 0, 0.4)',
  },
  light: {
    barBg: 'rgba(244, 244, 245, 0.92)',
    barText: '#18181b',
    chipActiveBg: '#d4d4d8',
    chipActiveText: '#09090b',
    buttonBg: 'rgba(24, 24, 27, 0.07)',
    buttonBgHover: 'rgba(24, 24, 27, 0.14)',
    buttonText: '#3f3f46',
    buttonTextHover: '#09090b',
    inputBg: '#ffffff',
    inputBorder: '#d4d4d8',
    inputText: '#18181b',
    menuBg: '#ffffff',
    menuBorder: '#e4e4e7',
    menuText: '#18181b',
    menuItemHoverBg: '#f4f4f5',
    menuShadow: '0 8px 24px rgba(0, 0, 0, 0.12)',
  },
} as const;

// Letterbox fill behind the contain-fitted frame; must match the viewer root background or the padding reads as dark bars.
const CAPTURE_LETTERBOX_BG = { dark: '#030712', light: '#e4e4e7' } as const;

// Tab-strip overflow chrome. The chip row scrolls sideways; its scrollbar is
// hidden because the in-container strip is a 36px overlay with no room for one,
// so the affordance is an edge fade on whichever side still holds tabs.
const TABBAR_CSS = `
#isola-tabbar [data-chips]{overflow-x:auto;overflow-y:hidden;scrollbar-width:none;-ms-overflow-style:none;}
#isola-tabbar [data-chips]::-webkit-scrollbar{width:0;height:0;}
#isola-tabbar [data-chips][data-fade="r"]{mask-image:linear-gradient(to right,#000 calc(100% - 16px),transparent);-webkit-mask-image:linear-gradient(to right,#000 calc(100% - 16px),transparent);}
#isola-tabbar [data-chips][data-fade="l"]{mask-image:linear-gradient(to right,transparent,#000 16px);-webkit-mask-image:linear-gradient(to right,transparent,#000 16px);}
#isola-tabbar [data-chips][data-fade="lr"]{mask-image:linear-gradient(to right,transparent,#000 16px,#000 calc(100% - 16px),transparent);-webkit-mask-image:linear-gradient(to right,transparent,#000 16px,#000 calc(100% - 16px),transparent);}
`;
function ensureTabBarStyles() {
  if (document.getElementById('isola-tabbar-styles')) return;
  const style = document.createElement('style');
  style.id = 'isola-tabbar-styles';
  style.textContent = TABBAR_CSS;
  document.head.appendChild(style);
}

// Input backlog (see sendEvent): events buffered while no socket is
// sendable are replayed on open if younger than the TTL.
const INPUT_BACKLOG_TTL_MS = 600;
const INPUT_BACKLOG_CAP = 32;
// Consecutive reconnect attempts for the input channel before giving up (a
// successful open resets the counter).
const INPUT_RECONNECT_ATTEMPTS = 5;
// Three missing 20s keepalives = half-open socket; every tab command would silently no-op.
const INPUT_ZOMBIE_MS = 65000;

// Two missing metrics messages: fall back to the free-running window instead of a stale rate.
const ARRIVAL_WINDOW_MAX_AGE_MS = 2500;

// IsolaStreamMetrics, ABR, MSE/WebCodecs decode helpers → ./isola/*

// Canvas insets for the network panel, applied imperatively via setIsolaInsets().
let isolaInsets = { right: 0, bottom: 0 };
// 36px reserved for the in-container tab strip; set eagerly in doConnect so the first frame never flashes at top=0.
let isolaTopOffset = 0;
function getIsolaTopOffset() { return isolaTopOffset; }
// Direct refs to the mounted capture elements; querySelector breaks with multiple containers.
let currentCaptureVideo: HTMLVideoElement | null = null;
let currentCaptureCanvas: HTMLCanvasElement | null = null;
function applyInsetsToBoth() {
  if (currentCaptureVideo) applyInsets(currentCaptureVideo);
  if (currentCaptureCanvas) applyInsets(currentCaptureCanvas);
}
function setIsolaTopOffset(px: number) {
  isolaTopOffset = px;
  applyInsetsToBoth();
}
export function setIsolaInsets(insets: { right?: number; bottom?: number }) {
  isolaInsets = { ...isolaInsets, ...insets };
  applyInsetsToBoth();
}
function applyInsets(el: HTMLVideoElement | HTMLCanvasElement) {
  const { right, bottom } = isolaInsets;
  const top = getIsolaTopOffset();
// Replaced elements ignore the inset model with auto sizing — use explicit calc() percentages.
  el.style.position = 'absolute';
  el.style.left = '0';
  el.style.top = `${top}px`;
  el.style.right = '0';
  el.style.bottom = '0';
  el.style.width = right > 0 ? `calc(100% - ${right}px)` : '100%';
  el.style.height = bottom > 0
    ? `calc(100% - ${top}px - ${bottom}px)`
    : `calc(100% - ${top}px)`;
  el.style.marginTop = '0';
  el.style.marginLeft = '0';
  el.style.marginRight = '0';
  el.style.marginBottom = '0';
  el.style.maxWidth = 'none';
  el.style.maxHeight = 'none';
  el.style.transform = 'none';
}

// Copy/select-all routing: focus decides (container vs host chrome); the live DOM selection must not — mousedown preventDefault leaves stale host selections.
export function copyChordStaysLocal(container: Node, active: Element | null): boolean {
  return !active || !(active === container || container.contains(active));
}

// mapClientToCapture: undo contain-letterbox centring, scale to frame pixels, rescale to full capture (ABR may serve a smaller encode).
export function mapClientToCapture(
  clientX: number,
  clientY: number,
  rect: { left: number; top: number; width: number; height: number },
  srcW: number,
  srcH: number,
  capW: number,
  capH: number,
): { x: number; y: number } {
  if (!srcW || !srcH) return { x: 0, y: 0 };
  const scale = Math.min(rect.width / srcW, rect.height / srcH);
  if (!scale) return { x: 0, y: 0 };
  const displayW = srcW * scale;
  const displayH = srcH * scale;
  const offsetX = (rect.width - displayW) / 2;
  const offsetY = (rect.height - displayH) / 2;
  const vx = Math.max(0, Math.min(srcW - 1, Math.floor((clientX - rect.left - offsetX) / scale)));
  const vy = Math.max(0, Math.min(srcH - 1, Math.floor((clientY - rect.top - offsetY) / scale)));
  const w = capW > 0 ? capW : srcW;
  const h = capH > 0 ? capH : srcH;
  return {
    x: Math.max(0, Math.min(w - 1, Math.round(vx * w / srcW))),
    y: Math.max(0, Math.min(h - 1, Math.round(vy * h / srcH))),
  };
}

// Arrival metering closed on the server's metrics messages so the sample and
// sent_kbps describe the same second; a free-running window fabricates loss on bursty streams.
export interface ArrivalWindow {
  /** When the last metrics message arrived (performance.now), 0 before the first. */
  atMs: number;
  /** Cumulative bytes received at that message. */
  bytes: number;
  /** Bitrate over the window the message closed. */
  kbps: number;
  /** True once `kbps` covers a real interval (needs two messages). */
  fresh: boolean;
}

export function createArrivalWindow(): ArrivalWindow {
  return { atMs: 0, bytes: 0, kbps: 0, fresh: false };
}

/** Close the arrival window at a server metrics message. Mutates in place. */
export function advanceArrivalWindow(win: ArrivalWindow, nowMs: number, totalBytes: number): void {
  if (win.atMs > 0) {
    const dtSec = (nowMs - win.atMs) / 1000;
    // A zero/negative interval (clock hiccup, two messages in one task) carries
    // no rate, so it must not replace the last usable sample with 0.
    if (dtSec > 0) {
      win.kbps = Math.round(((totalBytes - win.bytes) * 8) / dtSec / 1000);
      win.fresh = true;
    }
  }
  win.atMs = nowMs;
  win.bytes = totalBytes;
}

// Frame-rate counters: plain data in a ref, published by the 1 Hz tick — never per frame.
export interface FrameCounters {
  /** Latest packet sequence number seen on the wire. */
  sequenceNumber: number;
  /** Running total of IDR packets received this view. */
  keyframeCount: number;
  /** lastSeq: baseline for gap arithmetic; `seq === lastSeq + 1` is in order. */
  lastSeq: number | null;
  /** Missing packets accumulated in the current loss burst (since the last
   *  IDR or run reset). Drives the one-shot keyframe request. */
  burstLost: number;
  /** True once the current burst has already spent its keyframe request, so a
   *  long gap fires recovery exactly once instead of once per packet. */
  burstRequested: boolean;
  /** Cumulative packets counted missing this view (survives a run reset). */
  packetsLost: number;
  /** Cumulative packets accepted on the wire this view, including reordered
   *  and duplicate ones — they were received, just not in sequence. */
  packetsReceived: number;
}

export function createFrameCounters(): FrameCounters {
  return {
    sequenceNumber: 0,
    keyframeCount: 0,
    lastSeq: null,
    burstLost: 0,
    burstRequested: false,
    packetsLost: 0,
    packetsReceived: 0,
  };
}

/** LOSS_BURST_PACKETS: five missing = the streamer shed a real GOP slice; only an IDR heals. */
export const LOSS_BURST_PACKETS = 5;

/** What one packet's gap arithmetic implies for the caller. */
export interface PacketLossNotice {
  /** Packets this packet revealed as missing (0 when in order or on reset). */
  lost: number;
  /** True exactly once per burst: the caller should kick keyframe recovery. */
  requestKeyframe: boolean;
}

/** Record one packet; seq restarts per encoder run, so backward/duplicate = run change, not a gap. */
export function noteStreamedPacket(
  counters: FrameCounters,
  seq: number,
  isKeyframe: boolean,
): PacketLossNotice {
  const notice: PacketLossNotice = { lost: 0, requestKeyframe: false };
  counters.sequenceNumber = seq;
  counters.packetsReceived += 1;
  if (isKeyframe) counters.keyframeCount += 1;
  const last = counters.lastSeq;
  if (last !== null && seq > last + 1) {
    const missing = seq - (last + 1);
    counters.packetsLost += missing;
    counters.burstLost += missing;
    notice.lost = missing;
    // Only a non-keyframe packet can leave the decoder waiting: an IDR resyncs
    // it, so the burst ends below instead of firing a request.
    if (!isKeyframe && counters.burstLost >= LOSS_BURST_PACKETS && !counters.burstRequested) {
      counters.burstRequested = true;
      notice.requestKeyframe = true;
    }
  }
  // Backward/duplicate seq (seq <= last) and a simple in-order step both
  // simply move the baseline forward.
  counters.lastSeq = seq;
  if (isKeyframe) {
    // An IDR resyncs the decoder, which ends any burst in progress: whatever
    // was lost before it no longer needs recovery, and the next gap starts a
    // fresh burst with its own request budget.
    counters.burstLost = 0;
    counters.burstRequested = false;
  }
  return notice;
}

/** Drop the gap baseline on stream_init/stream_reset: the new run's seq restarts at 0. */
export function resetPacketLoss(counters: FrameCounters): void {
  counters.lastSeq = null;
  counters.burstLost = 0;
  counters.burstRequested = false;
}

/** Per-view cumulative totals last sampled by the 1 Hz metrics tick. The ratio
 *  is windowed over this delta, never per frame, and never cumulative. */
export interface PacketLossWindow {
  lost: number;
  received: number;
}

export function createPacketLossWindow(): PacketLossWindow {
  return { lost: 0, received: 0 };
}

/** Publish windowed loss at 1 Hz: ratio over this tick's delta, clamped, 0 when empty. */
export function samplePacketLoss(
  counters: FrameCounters,
  win: PacketLossWindow,
): { packetsLost: number; packetLossRatio: number } {
  const lost = Math.max(0, counters.packetsLost - win.lost);
  const received = Math.max(0, counters.packetsReceived - win.received);
  win.lost = counters.packetsLost;
  win.received = counters.packetsReceived;
  const total = lost + received;
  return {
    packetsLost: counters.packetsLost,
    packetLossRatio: total > 0 ? lost / total : 0,
  };
}

/** The metrics fields the 1 Hz tick publishes from the frame counters. */
export function frameMetricsPatch(
  counters: FrameCounters,
): Pick<IsolaStreamMetrics, 'sequenceNumber' | 'keyframeCount'> {
  return {
    sequenceNumber: counters.sequenceNumber,
    keyframeCount: counters.keyframeCount,
  };
}

export function useIsolaStream(defaultTargetUrl?: string) {
  const { theme } = useTheme();
  // The tab strip is imperative DOM, so it reads the theme through a ref and
  // is repainted by the effect below whenever the theme toggles.
  const themeRef = useRef(theme);
  const lastTabsRef = useRef<Array<{ id: number; url: string; title: string; active: boolean }>>([]);
  const [connected, setConnected] = useState(false);
  const [firstFrame, setFirstFrame] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [reconnecting, setReconnecting] = useState(false);
  const [remoteClipboard, setRemoteClipboard] = useState<string>('');
  const [audioMuted, setAudioMutedState] = useState(true);
  const [audioActive, setAudioActive] = useState(false);
  // Tab strip state mirrored into React so the network panel can list every open tab.
  const [browserTabs, setBrowserTabs] = useState<Array<{ id: number; url: string; title: string; active: boolean }>>([]);
  const [metrics, setMetrics] = useState<IsolaStreamMetrics>({
    connectMs: null,
    firstFrameMs: null,
    recentFps: 0,
    sequenceNumber: 0,
    keyframeCount: 0,
    packetsLost: 0,
    packetLossRatio: 0,
    serverDropped: 0,
    wsState: WebSocket.CLOSED,
    inputBackend: '',
    inputTarget: '',
    inputTargetOK: true,
    activityMode: '',
    playoutLagMs: 0,
    codec: '',
    streamWidth: 0,
    streamHeight: 0,
    targetFps: 0,
    targetBitrateKbps: 0,
    bytesReceived: 0,
    bitrateKbps: 0,
    segmentRate: 0,
    bufferedSec: 0,
    pendingSegments: 0,
    playbackRate: 1,
    videoWidth: 0,
    videoHeight: 0,
    lastSegmentAgeMs: -1,
    appendCount: 0,
    encoderSeq: 0,
    captureWidth: 0,
    captureHeight: 0,
    rttMs: null,
    netQuality: 'good',
    netRatio: 0,
    abrLevel: 0,
    decodeErrorCount: 0,
    lastRecoveryMs: null,
    decodeBacklogMs: 0,
    decodeDroppedFrames: 0,
    decodeCatchUps: 0,
    encodeScale: 0,
    staticSecs: 0,
    skippedTicks: 0,
    keyframeReason: '',
    keyframeAgeMs: -1,
    rendererStalled: false,
    rendererStalledMs: 0,
    rendererRecoveries: 0,
    cdpProbeFailStreak: 0,
    cdpProbeFailAgoMs: -1,
    captureHealthOk: false,
    captureEmptyFrames: 0,
    captureEmptyWindow: 0,
    captureFallback: false,
    captureLastPaintMs: -1,
    captureLastFrameMs: -1,
    changesPerSec: 0,
    fullFramesPerSec: 0,
    damagedPct: 0,
    captureLastSentMs: -1,
    captureSentFrames: 0,
    captureDedupFrames: 0,
  });

  const wsRef = useRef<WebSocket | null>(null);
  const captureSizeRef = useRef({ w: 0, h: 0 });
  // WebCodecs mode: raw Annex-B → VideoDecoder → canvas; latency is one frame.
  const wcModeRef = useRef(false);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const decoderRef = useRef<WCDecoder | null>(null);
  const wcChunkCtorRef = useRef<(new (init: WCChunk) => WCChunk) | null>(null);
  // Last negotiated WebCodecs stream config — lets a decoder that a fatal
  // error closed be rebuilt with the same codec/geometry (see the binary
  // access-unit path).
  const lastWcConfigRef = useRef<{ codec: string; w: number; h: number } | null>(null);
  const tsUsRef = useRef(0);
  const frameStepUsRef = useRef(33333);
  const keyframeSeenRef = useRef(false);
  // One armed recovery attempt per decode error / IDR-less reset.
  const decodeErrorCountRef = useRef(0);
  const lastRecoveryMsRef = useRef<number | null>(null);
  const recoveryStartedAtRef = useRef<number | null>(null);
  const recoveryRequestedRef = useRef(false);
  const keyframeRequestTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Too many unanswered requests = streamer restarts failing; only a fresh connection helps.
  const recoveryRequestsRef = useRef(0);
  const wcFramesRef = useRef(0);
  const wcDecodedThisSecondRef = useRef(0);
  // Frame-rate counters live in a ref, not React state; the 1 Hz metrics tick
  // publishes them via frameMetricsPatch. See FrameCounters above.
  const frameCountersRef = useRef<FrameCounters>(createFrameCounters());
  // Cumulative totals the last 1 Hz tick published; the windowed loss ratio is
  // this tick's delta, so a burst shows up for one tick instead of decaying
  // forever in a lifetime average.
  const lossWindowRef = useRef<PacketLossWindow>(createPacketLossWindow());
  const tabbarRef = useRef<HTMLDivElement | null>(null);
  // Fullscreen host slot for the browser chrome; null restores in-container placement.
  const tabBarHostRef = useRef<HTMLElement | null>(null);
  // URL box gets focus once, when the tab bar first appears after page open.
  const urlAutofocusRef = useRef(false);
  // Seed URL from session config, used before a real tabs broadcast arrives.
  const seedTargetUrlRef = useRef(defaultTargetUrl || '');
  // The id of the tab the session started with. The seed above describes THAT
  // tab only — see seedForActiveTab.
  const initialTabIdRef = useRef<number | null>(null);
  // Enter's blur runs before the runner broadcasts the new URL; without this the blur
  // handler flashes about:blank. Cleared once the tabs handler confirms the navigation.
  const pendingNavRef = useRef<string>('');
  const menuRef = useRef<HTMLDivElement | null>(null);
  const menuCleanupRef = useRef<(() => void) | null>(null);
  const wcContainerRef = useRef<HTMLElement | null>(null);
  const wcViewOnlyRef = useRef(false);
  const hoverURLRef = useRef(''); // last link under the pointer (from cursor broadcast)
  // Remote cursor shape; only "text" drags take the DOM select_range path.
  const remoteCursorRef = useRef('default');
  // Text drags use DOM select_range (CDP button-pinned moves don't select in
  // Electron offscreen mode); every other left drag streams raw mousemove.
  const dragStartRef = useRef<{ x: number; y: number; text: boolean } | null>(null);
  const lastSelectRangeRef = useRef(0);
  // ── Audio: Opus packets (marker 0x03) → AudioDecoder → AudioContext ──
  const audioCtxRef = useRef<AudioContext | null>(null);
  const audioDecoderRef = useRef<WCAudioDecoder | null>(null);
  const audioChunkCtorRef = useRef<WCAudioChunkCtor | null>(null);
  // 3+ decode errors in 200ms tear the Opus decoder down and rebuild it.
  const audioErrRef = useRef<{ count: number; at: number }>({ count: 0, at: 0 });
  const audioNextRef = useRef(0);
  const audioTsUsRef = useRef(0);
  const audioOnRef = useRef(false);
  const audioPendingRef = useRef<Float32Array[][]>([]);
  const audioAnnouncedRef = useRef(false);
  const inputWsRef = useRef<WebSocket | null>(null); // separate WS for input events (channel=input)
  // Input buffered while neither the input WS nor the stream WS is sendable
  // (reconnect windows): replayed in order when a socket opens, so a
  // mousedown/mouseup pair spanning a blip still lands as a click.
  const inputBacklogRef = useRef<Array<{ ev: Record<string, unknown>; at: number }>>([]);
  // Last message seen on the input WS (the streamer's 20s keepalive keeps
  // this fresh on a healthy socket) — drives the zombie-socket watchdog.
  const inputLastMsgAtRef = useRef(0);
  const videoRef = useRef<HTMLVideoElement | null>(null);
  const mediaSourceRef = useRef<MediaSource | null>(null);
  // Every stream_reset builds a new MediaSource; revoke the old URL or memory grows per quality change.
  const mediaObjectUrlRef = useRef<string | null>(null);
  const sourceBufferRef = useRef<SourceBuffer | null>(null);
  const pendingSegmentsRef = useRef<Uint8Array<ArrayBuffer>[]>([]);
  // Latest MSE playout state, written by syncPlayout on each append and
  // published by the 1 Hz metrics tick. A ref, not state — see msePlayout.ts.
  const playoutRef = useRef<PlayoutMetrics>(createPlayoutMetrics());
  const containerRef = useRef<HTMLElement | null>(null);
  const inputCleanupRef = useRef<(() => void) | null>(null);
  // The stream reconnect timer. Held in a ref (not a doConnect-local) so
  // disconnect() and the unmount cleanup can cancel it for every viewer,
  // including view-only ones — whose input cleanup never registered the clear.
  const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const connectStartedAtRef = useRef(0);
  const firstFrameRecordedRef = useRef(false);
  const appendCountRef = useRef(0);
  // Consecutive appendBuffer failures and the retry nudge timer (see
  // flushPending): transient throws must degrade to a retry, not a dead
  // SourceBuffer.
  const appendFailRef = useRef(0);
  const appendRetryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const bytesReceivedRef = useRef(0);
  const segmentsThisSecondRef = useRef(0);
  const lastSegmentAtRef = useRef(0);
  const lastBitrateWindowRef = useRef({ at: 0, bytes: 0 });
  const metricsIntervalRef = useRef<ReturnType<typeof setInterval> | null>(null);
  // ── ABR state (refs: mutated at 1 Hz, must not re-render) ──
  // performance.now() of the in-flight RTT ping, or null when none.
  const pingOutstandingRef = useRef<number | null>(null);
  const rttEmaRef = useRef<number | null>(null);
  // Congestion = arrival/egress, not arrival/target: capped VBR legitimately undershoots the target on idle pages.
  const targetKbpsRef = useRef(0);
  const sentKbpsRef = useRef(0);
  // Arrival metering closed on the server's metrics messages, so the sample and
  // `sent_kbps` describe the same second (see advanceArrivalWindow).
  const arrivalWindowRef = useRef(createArrivalWindow());
  // Smoothed arrival/egress ratio (see RATIO_EMA_ALPHA), null before the
  // first measurable tick. Published as netRatio.
  const ratioEmaRef = useRef<number | null>(null);
  const baseKbpsRef = useRef(0);
  const metricsTickRef = useRef(0);
  const abrRef = useRef<{
    level: number;       // current ladder rung L0..L3
    badSecs: number;     // consecutive bad seconds
    goodSecs: number;    // consecutive good seconds
    lastChangeAt: number; // Date.now() of the last rung change (anti-oscillation)
    pausedUntil: number;  // estimator pause across encoder restarts
    manual: boolean;      // manual quality override active
  }>({ level: 0, badSecs: 0, goodSecs: 0, lastChangeAt: 0, pausedUntil: 0, manual: loadPersistedQuality() !== 'auto' });

  // Saved connect params for reconnect
  const connectParamsRef = useRef<{
    streamWsUrl: string;
    inputWsUrl: string;
    container: HTMLElement;
    viewOnly: boolean;
    bandwidthKbps?: number;
    sessionId?: string;
  } | null>(null);
  const sessionIdRef = useRef<string | undefined>(undefined);
  // A ref, not a doConnect local: each reconnect spawns a fresh closure, so a local counter never reached the cap.
  const reconnectAttemptRef = useRef(0);

  const pasteToRemote = useCallback(async (text?: string) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      try {
        const textToPaste = text || await navigator.clipboard.readText();
        ws.send(JSON.stringify({ type: 'clipboard_sync', text: textToPaste }));
      } catch (err) {
        console.error('[isola] clipboard read', err);
      }
    }
  }, []);

  useEffect(() => {
    const target = window as Window & {
      __isolaPasteToRemote?: (text?: string) => Promise<void>;
    };
    target.__isolaPasteToRemote = pasteToRemote;
    return () => {
      delete target.__isolaPasteToRemote;
    };
  }, [pasteToRemote]);

  // Control channel: messages on the stream WS that bypass input routing
  // (input may have no dedicated socket in view-only mode; control must not
  // depend on it).
  const sendControl = useCallback((msg: Record<string, unknown>) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      try { ws.send(JSON.stringify(msg)); } catch { /* ignore */ }
    }
  }, []);

  // Applies the persisted rung once stream_init reports base bitrate; a duplicate set_bitrate is ignored server-side.
  const applyPersistedQuality = useCallback(() => {
    const q = loadPersistedQuality();
    if (q === 'auto') return;
    const base = baseKbpsRef.current;
    if (!base) return;
    abrRef.current.manual = true;
    sendControl({ type: 'set_bitrate', kbps: abrLadderKbps(base)[QUALITY_RUNG[q]] });
  }, [sendControl]);

  // Manual quality override (Auto returns control to the ABR controller).
  const setQuality = useCallback((q: IsolaQuality) => {
    try { localStorage.setItem('isola_quality', q); } catch { /* ignore */ }
    const abr = abrRef.current;
    if (q === 'auto') {
      abr.manual = false;
      return;
    }
    abr.manual = true;
    abr.badSecs = 0;
    abr.goodSecs = 0;
    const base = baseKbpsRef.current;
    if (!base) return; // stream_init applies the persisted rung after connect
    sendControl({ type: 'set_bitrate', kbps: abrLadderKbps(base)[QUALITY_RUNG[q]] });
  }, [sendControl]);

  // Keep seed URL in sync with upstream param (which is updated from the viewer
  // poll's data.target_url). If the tab bar exists and URL field is still
  // placeholder/empty (no tabs broadcast yet), overwrite with the seed.
  useEffect(() => {
    const next = defaultTargetUrl || '';
    seedTargetUrlRef.current = next;
    if (!next) return;
    const el = tabbarRef.current;
    if (!el) return;
    const input = el.querySelector<HTMLInputElement>('input[type="text"]');
    if (!input) return;
    // Only overwrite when the current value is a known placeholder state (no
    // browser-sourced URL yet) AND the user is not actively typing there now.
    if (document.activeElement === input) return;
    const v = input.value.trim();
    if (!v || v === 'about:blank') setAddressValue(input, next);
  }, [defaultTargetUrl]);

  const hideContextMenu = useCallback(() => {
    if (menuRef.current) {
      menuRef.current.remove();
      menuRef.current = null;
    }
    if (menuCleanupRef.current) {
      menuCleanupRef.current();
      menuCleanupRef.current = null;
    }
  }, []);

  const disconnect = useCallback(() => {
    const ws = wsRef.current;
    if (ws) {
      ws.onmessage = null;
      ws.onclose = null;
      ws.onerror = null;
      ws.close();
      wsRef.current = null;
    }
    const iws = inputWsRef.current;
    if (iws) {
      iws.onopen = null;
      iws.onclose = null;
      iws.onerror = null;
      iws.close();
      inputWsRef.current = null;
    }
    inputBacklogRef.current = [];
    if (mediaSourceRef.current) {
      try {
        const ms = mediaSourceRef.current;
        if (ms.readyState === 'open') {
          ms.endOfStream();
        }
      } catch { /* ignore */ }
      mediaSourceRef.current = null;
    }
    sourceBufferRef.current = null;
    pendingSegmentsRef.current = [];
    if (mediaObjectUrlRef.current) {
      URL.revokeObjectURL(mediaObjectUrlRef.current);
      mediaObjectUrlRef.current = null;
    }
    if (videoRef.current) {
      videoRef.current.src = '';
      if (videoRef.current.parentNode) {
        videoRef.current.parentNode.removeChild(videoRef.current);
      }
      videoRef.current = null;
    }
    if (decoderRef.current) {
      try { decoderRef.current.close(); } catch { /* ignore */ }
      decoderRef.current = null;
    }
    wcChunkCtorRef.current = null;
    lastWcConfigRef.current = null;
    // Recovery state must not leak across connections.
    recoveryStartedAtRef.current = null;
    recoveryRequestedRef.current = false;
    recoveryRequestsRef.current = 0;
    if (keyframeRequestTimerRef.current) {
      clearTimeout(keyframeRequestTimerRef.current);
      keyframeRequestTimerRef.current = null;
    }
    if (audioDecoderRef.current) {
      try { audioDecoderRef.current.close(); } catch { /* ignore */ }
      audioDecoderRef.current = null;
    }
    if (audioCtxRef.current) {
      void audioCtxRef.current.close().catch(() => {});
      audioCtxRef.current = null;
    }
    audioPendingRef.current = [];
    audioOnRef.current = false;
    audioNextRef.current = 0;
    audioTsUsRef.current = 0;
    audioAnnouncedRef.current = false;
    setAudioActive(false);
    wcModeRef.current = false;
    keyframeSeenRef.current = false;
    tsUsRef.current = 0;
    // Clear in-flight keyframe recovery so a reconnect starts clean.
    if (keyframeRequestTimerRef.current !== null) {
      clearTimeout(keyframeRequestTimerRef.current);
      keyframeRequestTimerRef.current = null;
    }
    recoveryStartedAtRef.current = null;
    recoveryRequestedRef.current = false;
    decodeErrorCountRef.current = 0;
    lastRecoveryMsRef.current = null;
    wcFramesRef.current = 0;
    wcDecodedThisSecondRef.current = 0;
    if (canvasRef.current) {
      if (canvasRef.current.parentNode) {
        canvasRef.current.parentNode.removeChild(canvasRef.current);
      }
      canvasRef.current = null;
    }
    if (inputCleanupRef.current) {
      inputCleanupRef.current();
      inputCleanupRef.current = null;
    }
    // Cancel any pending reconnect so a dropped socket cannot resurrect a
    // stream after disconnect()/unmount.
    if (reconnectTimerRef.current) {
      clearTimeout(reconnectTimerRef.current);
      reconnectTimerRef.current = null;
    }
    hideContextMenu();
    if (tabbarRef.current) {
      tabbarRef.current.remove();
      tabbarRef.current = null;
    }
    if (metricsIntervalRef.current) {
      clearInterval(metricsIntervalRef.current);
      metricsIntervalRef.current = null;
    }
    bytesReceivedRef.current = 0;
    segmentsThisSecondRef.current = 0;
    lastSegmentAtRef.current = 0;
    lastBitrateWindowRef.current = { at: 0, bytes: 0 };
    arrivalWindowRef.current = createArrivalWindow();
    firstFrameRecordedRef.current = false;
    appendCountRef.current = 0;
    appendFailRef.current = 0;
    if (appendRetryTimerRef.current) {
      clearTimeout(appendRetryTimerRef.current);
      appendRetryTimerRef.current = null;
    }
    setFirstFrame(false);
    setConnected(false);
    // Clear module-level capture element refs so reconnects start clean.
    currentCaptureVideo = null;
    currentCaptureCanvas = null;
  }, [hideContextMenu]);

  const msePlayoutRefs = useCallback(() => ({
    sourceBufferRef,
    pendingSegmentsRef,
    videoRef,
    appendCountRef,
    appendFailRef,
    appendRetryTimerRef,
    playoutRef,
  }), []);

  const flushPendingRef = useRef<() => void>(() => {});
  const flushPending = useCallback(() => {
    flushPendingSegments(msePlayoutRefs(), sendControl, () => flushPendingRef.current());
  }, [msePlayoutRefs, sendControl]);
  flushPendingRef.current = flushPending;

  const wireVideoFps = useCallback(
    (video: HTMLVideoElement) => wireVideoFpsHelper(video, setMetrics),
    [],
  );

  // ── Remote UI chrome: tab strip + right-click menu ──────────────────
  const sendCtrl = useCallback((obj: Record<string, unknown>) => {
    const iws = inputWsRef.current;
    if (iws && iws.readyState === WebSocket.OPEN) {
      iws.send(JSON.stringify(obj));
      return;
    }
    // No stream-WS fallback here: buffer so a tab command issued mid-reconnect is replayed, not lost.
    const q = inputBacklogRef.current;
    q.push({ ev: obj, at: Date.now() });
    while (q.length > INPUT_BACKLOG_CAP) q.shift();
  }, []);


  const showContextMenu = useCallback((clientX: number, clientY: number) => {
    const host = wcContainerRef.current;
    if (!host) return;
    hideContextMenu();

    const pal = TABBAR_PALETTE[themeRef.current];
    const menu = document.createElement('div');
    menu.style.position = 'fixed';
    menu.style.left = `${clientX}px`;
    menu.style.top = `${clientY}px`;
    menu.style.zIndex = '9999';
    menu.style.minWidth = '160px';
    menu.style.background = pal.menuBg;
    menu.style.border = `1px solid ${pal.menuBorder}`;
    menu.style.borderRadius = '8px';
    menu.style.padding = '4px';
    menu.style.boxShadow = pal.menuShadow;
    menu.style.fontFamily = 'system-ui, sans-serif';
    menu.style.fontSize = '13px';
    menu.style.color = pal.menuText;

    const item = (label: string, action: () => void) => {
      const el = document.createElement('div');
      el.textContent = label;
      el.style.padding = '7px 12px';
      el.style.borderRadius = '5px';
      el.style.cursor = 'pointer';
      el.onmouseenter = () => { el.style.background = pal.menuItemHoverBg; };
      el.onmouseleave = () => { el.style.background = 'transparent'; };
      el.onmousedown = (e) => {
        e.preventDefault();
        e.stopPropagation();
        hideContextMenu();
        action();
      };
      menu.appendChild(el);
    };

    item('Copy', () => sendCtrl({ type: 'get_selection' }));
    item('Paste', () => { void pasteToRemote(); });
    item('Select all', () => sendCtrl({ type: 'chord', combo: 'ctrl+a' }));
    if (hoverURLRef.current) {
      const u = hoverURLRef.current;
      item('Open link in new tab', () => sendCtrl({ type: 'tab_new', url: u }));
    }

    document.body.appendChild(menu);
    menuRef.current = menu;

    const close = (e: Event) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) {
        hideContextMenu();
      }
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') hideContextMenu();
    };
    // use capture + delay so the initiating contextmenu event doesn't close it
    setTimeout(() => {
      window.addEventListener('mousedown', close, true);
      window.addEventListener('wheel', close, { capture: true, passive: true });
      window.addEventListener('keydown', onKey, true);
    }, 0);
    menuCleanupRef.current = () => {
      window.removeEventListener('mousedown', close, true);
      window.removeEventListener('wheel', close, true);
      window.removeEventListener('keydown', onKey, true);
    };
  }, [hideContextMenu, pasteToRemote, sendCtrl]);

  const rebuildTabBar = useCallback((tabs: Array<{ id: number; url: string; title: string; active: boolean }>) => {
    // Fullscreen re-homes the strip into the viewer's toolbar slot.
    const externalHost = tabBarHostRef.current;
    const host = externalHost ?? wcContainerRef.current;
    // Operator kill-switch from config.yaml (settings.isola_tabs) — the
    // strip is a UI feature, not part of the wire protocol.
    if (!host || wcViewOnlyRef.current || !getIsolaTabs()) return;
    // innerHTML='' would destroy a focused URL field and leak keystrokes remote-ward;
    // skip only no-change broadcasts — a tab open/close/switch must repaint even mid-typing.
    const prevUrl = tabbarRef.current?.querySelector<HTMLInputElement>('input');
    const urlFocused = !!prevUrl && document.activeElement === prevUrl;
    // Only a real edit may block a repaint; an autofocused untouched field must follow warm-claim navigations.
    const urlEdited = urlFocused && !!prevUrl && prevUrl.value !== (prevUrl.dataset.shownUrl ?? '');
    // A host change (fullscreen toggled) must always repaint so the strip is
    // re-homed, even while the URL field is focused.
    const hostChanged = !!tabbarRef.current && tabbarRef.current.parentElement !== host;
    const prevTabs = lastTabsRef.current;
    const structureChanged = prevTabs.length !== tabs.length
      || prevTabs.some((t, i) => t.id !== tabs[i].id || t.active !== tabs[i].active);
    if (shouldSkipTabBarRebuild({ urlFocused, urlEdited, structureChanged, hostChanged })) return;
    // The field describes the ACTIVE tab; on a switch its content must not carry over.
    const activeTabChanged = (prevTabs.find((t) => t.active)?.id ?? null)
      !== (tabs.find((t) => t.active)?.id ?? null);
    if (activeTabChanged) {
      // The pending navigation also belonged to the tab we left.
      pendingNavRef.current = '';
    }
    // Carry an in-progress edit across the rebuild; an untouched field shows the active tab's URL.
    const keepEdit = urlEdited && shouldCarryUrlEdit({ urlFocused, activeTabChanged }) && prevUrl
      ? { value: prevUrl.value, start: prevUrl.selectionStart, end: prevUrl.selectionEnd }
      : null;
    // Autofocused but untouched: keep the caret where the user left it.
    const keepCaret = urlFocused && !urlEdited && !activeTabChanged && prevUrl
      ? { start: prevUrl.selectionStart, end: prevUrl.selectionEnd }
      : null;
    lastTabsRef.current = tabs;
    const pal = TABBAR_PALETTE[themeRef.current];
    const external = !!externalHost;
    let bar = tabbarRef.current;
    if (!bar) {
      bar = document.createElement('div');
      bar.id = 'isola-tabbar';
      bar.style.zIndex = '20';
      bar.style.display = 'flex';
      bar.style.alignItems = 'center';
      bar.style.fontFamily = 'system-ui, sans-serif';
      bar.style.fontSize = '12px';
      bar.style.userSelect = 'none';
      host.appendChild(bar);
      tabbarRef.current = bar;
      // Stop mousedown AND contextmenu, or a right-click blurs the input and pops the remote menu over local chrome.
      bar.addEventListener('mousedown', (e) => e.stopPropagation());
      bar.addEventListener('contextmenu', (e) => e.stopPropagation());
    }
    // In-container: absolute 36px overlay. In the toolbar row: plain inline flex child.
    if (external) {
      bar.style.position = 'static';
      bar.style.top = '';
      bar.style.left = '';
      bar.style.right = '';
      bar.style.height = 'auto';
      bar.style.flex = '1 1 auto';
      bar.style.minWidth = '0';
      bar.style.gap = '6px';
      bar.style.padding = '0';
      bar.style.background = 'transparent';
    } else {
      bar.style.position = 'absolute';
      bar.style.top = '0';
      bar.style.left = '0';
      bar.style.right = '0';
      bar.style.height = '36px';
      bar.style.flex = '';
      bar.style.minWidth = '';
      bar.style.gap = '2px';
      bar.style.padding = '4px 6px';
      bar.style.background = pal.barBg;
    }
    bar.style.color = pal.barText;
    // appendChild re-homes the existing node when the host changed.
    if (bar.parentElement !== host) host.appendChild(bar);
    // In-container strip overlays the top 36px; toolbar-hosted does not.
    setIsolaTopOffset(external ? 0 : 36);
    ensureTabBarStyles();
    // Every broadcast rebuilds the strip (a tab's title alone triggers one), so
    // the chip row's scroll position has to survive the wipe — otherwise the
    // strip snaps back to the first tab while the user is reading the last one.
    const prevChips = bar.querySelector<HTMLElement>('[data-chips]');
    const prevScrollLeft = prevChips ? prevChips.scrollLeft : 0;
    bar.innerHTML = '';

    // Seed URL only for the tab the session started with, while it still reports a placeholder.
    const activeRaw = tabs.find((t) => t.active);
    if (initialTabIdRef.current === null && tabs.length > 0) {
      initialTabIdRef.current = tabs.reduce((min, t) => (t.id < min ? t.id : min), tabs[0].id);
    }
    const seed = seedForActiveTab({
      activeTabId: activeRaw ? activeRaw.id : null,
      initialTabId: initialTabIdRef.current,
      seed: seedTargetUrlRef.current,
    });
    const active = activeRaw && (activeRaw.url !== '' || !seed)
      ? activeRaw
      : (activeRaw
          ? { ...activeRaw, url: isPlaceholderUrl(activeRaw.url) && seed ? seed : activeRaw.url }
          : (seed ? { id: 1, url: seed, title: '', active: true } : undefined));
    const tabsDisplay = tabs.length > 0
      ? (seed
          ? tabs.map((t) => t.active && isPlaceholderUrl(t.url) ? { ...t, url: seed } : t)
          : tabs)
      : (seed ? [active!] : []);
    // Chip row: fixed-width chips in a horizontally scrolling row, so a busy
    // tab set never shrinks a chip below legibility. The address bar is pinned
    // to the left as a fixed-width sibling; back/reload/`+` and the chips take
    // the remaining space to its right.
    const chips = document.createElement('div');
    chips.setAttribute('data-chips', '');
    chips.style.display = 'flex';
    chips.style.alignItems = 'center';
    chips.style.flex = '1 1 auto';
    chips.style.minWidth = '0';
    chips.style.marginLeft = '6px';
    chips.style.justifyContent = 'flex-start';
    chips.style.gap = external ? '6px' : '2px';
    const labels = tabLabels(tabsDisplay);
    let activeChip: HTMLElement | null = null;
    for (const t of tabsDisplay) {
      const label = labels.get(t.id) || `Tab ${t.id}`;
      const chip = document.createElement('div');
      chip.setAttribute('role', 'tab');
      chip.setAttribute('aria-selected', t.active ? 'true' : 'false');
      // Roving tabindex: Tab reaches the current tab, arrows walk the rest.
      chip.tabIndex = t.active ? 0 : -1;
      chip.style.display = 'flex';
      chip.style.alignItems = 'center';
      chip.style.gap = '2px';
      chip.style.padding = '4px 4px 4px 8px';
      chip.style.borderRadius = '6px';
      chip.style.cursor = 'pointer';
      chip.style.flex = '0 0 auto';
      chip.style.minWidth = '90px';
      chip.style.maxWidth = '200px';
      chip.style.background = t.active ? pal.chipActiveBg : 'transparent';
      chip.style.color = t.active ? pal.chipActiveText : pal.barText;
      chip.style.transition = 'background 120ms ease, color 120ms ease';
      chip.title = t.title && t.title !== label ? `${t.title}\n${t.url}` : (t.url || label);
      if (t.active) activeChip = chip;

      const text = document.createElement('span');
      text.textContent = label;
      text.style.flex = '1 1 auto';
      text.style.minWidth = '0';
      text.style.overflow = 'hidden';
      text.style.textOverflow = 'ellipsis';
      text.style.whiteSpace = 'nowrap';
      chip.appendChild(text);

      // Close is a fixed 16px slot in every chip, shown on the active tab and
      // on hover — reserving the space keeps the strip from reflowing as the
      // pointer moves. A real button so Enter/Space work when it is focused.
      const x = document.createElement('button');
      x.type = 'button';
      x.setAttribute('aria-label', `Close ${label}`);
      // Out of the tab order: the chip itself owns Enter (switch) and Delete
      // (close), so Tab walks the strip, not one ✕ per tab.
      x.tabIndex = -1;
      x.textContent = '✕';
      x.style.flex = '0 0 16px';
      x.style.width = '16px';
      x.style.height = '16px';
      x.style.display = 'grid';
      x.style.placeItems = 'center';
      x.style.padding = '0';
      x.style.border = '0';
      x.style.borderRadius = '4px';
      x.style.background = 'transparent';
      x.style.color = 'inherit';
      x.style.cursor = 'pointer';
      x.style.font = 'inherit';
      x.style.lineHeight = '1';
      x.style.opacity = t.active ? '0.55' : '0';
      x.style.transition = 'opacity 120ms ease, background 120ms ease';
      x.onmouseenter = () => { x.style.opacity = '1'; x.style.background = pal.buttonBgHover; };
      x.onmouseleave = () => { x.style.opacity = t.active ? '0.55' : '0'; x.style.background = 'transparent'; };
      x.onclick = (e) => {
        e.stopPropagation();
        sendCtrl({ type: 'tab_close', id: t.id });
      };
      chip.appendChild(x);

      chip.onmousedown = (e) => {
        e.preventDefault();
        e.stopPropagation();
        if (e.target !== x) sendCtrl({ type: 'tab_switch', id: t.id });
      };
      chip.onkeydown = (e) => {
        // Keys aimed at the close button are the browser's (it is not tabbable,
        // but a pointer click can focus it in some engines).
        if (e.target !== chip) return;
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          sendCtrl({ type: 'tab_switch', id: t.id });
          return;
        }
        if (e.key === 'Delete') {
          e.preventDefault();
          sendCtrl({ type: 'tab_close', id: t.id });
          return;
        }
        if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
        e.preventDefault();
        const all = Array.from(chips.querySelectorAll<HTMLElement>('[role="tab"]'));
        const next = all[all.indexOf(chip) + (e.key === 'ArrowRight' ? 1 : -1)];
        next?.focus({ preventScroll: true });
        next?.scrollIntoView({ inline: 'nearest', block: 'nearest' });
      };
      // Hover highlight only for background tabs — the active tab already
      // wears chipActiveBg and must not visually react to the pointer.
      chip.onmouseenter = () => {
        x.style.opacity = '0.55';
        if (!t.active) chip.style.background = pal.buttonBg;
      };
      chip.onmouseleave = () => {
        if (!t.active) { chip.style.background = 'transparent'; x.style.opacity = '0'; }
      };
      chips.appendChild(chip);
    }
    // New-tab button rides at the end of the tab row, right after the last chip.
    const plus = document.createElement('div');
    plus.textContent = '+';
    plus.style.flex = '0 0 auto';
    plus.style.marginLeft = '2px';
    plus.style.padding = '3px 9px';
    plus.style.borderRadius = '6px';
    plus.style.cursor = 'pointer';
    plus.style.fontSize = '14px';
    plus.style.background = pal.buttonBg;
    plus.style.color = pal.barText;
    plus.style.transition = 'background 120ms ease, color 120ms ease';
    plus.onmouseenter = () => { plus.style.background = pal.buttonBgHover; plus.style.color = pal.buttonTextHover; };
    plus.onmouseleave = () => { plus.style.background = pal.buttonBg; plus.style.color = pal.barText; };
    plus.onmousedown = (e) => {
      e.preventDefault();
      e.stopPropagation();
      sendCtrl({ type: 'tab_new' });
    };

    // mousedown (not click) so the browser's drag/selection flow isn't disturbed.
    const back = document.createElement('div');
    back.setAttribute('title', 'Back');
    back.setAttribute('role', 'button');
    back.innerHTML = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 12H5"/><polyline points="12 19 5 12 12 5"/></svg>`;
    back.style.flex = '0 0 auto';
    back.style.padding = '3px 7px';
    back.style.marginLeft = '6px';
    back.style.borderRadius = '6px';
    back.style.cursor = 'pointer';
    back.style.fontSize = '14px';
    back.style.color = pal.buttonText;
    back.style.background = pal.buttonBg;
    back.style.display = 'flex';
    back.style.alignItems = 'center';
    back.style.justifyContent = 'center';
    back.style.transition = 'background 120ms ease, color 120ms ease';
    back.onmouseenter = () => { back.style.background = pal.buttonBgHover; back.style.color = pal.buttonTextHover; };
    back.onmouseleave = () => { back.style.background = pal.buttonBg; back.style.color = pal.buttonText; };
    back.onmousedown = (e) => {
      e.preventDefault();
      e.stopPropagation();
      sendCtrl({ type: 'tab_back' });
    };

    const reload = document.createElement('div');
    reload.setAttribute('title', 'Reload page');
    reload.innerHTML = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-3-6.7"/><polyline points="21 3 21 9 15 9"/></svg>`;
    reload.style.flex = '0 0 auto';
    reload.style.padding = '3px 7px';
    reload.style.marginLeft = '2px';
    reload.style.borderRadius = '6px';
    reload.style.cursor = 'pointer';
    reload.style.fontSize = '14px';
    reload.style.color = pal.buttonText;
    reload.style.background = pal.buttonBg;
    reload.style.display = 'flex';
    reload.style.alignItems = 'center';
    reload.style.justifyContent = 'center';
    reload.style.transition = 'background 120ms ease, color 120ms ease';
    reload.onmouseenter = () => { reload.style.background = pal.buttonBgHover; reload.style.color = pal.buttonTextHover; };
    reload.onmouseleave = () => { reload.style.background = pal.buttonBg; reload.style.color = pal.buttonText; };
    reload.onmousedown = (e) => {
      e.preventDefault();
      e.stopPropagation();
      const id = active ? active.id : 1;
      sendCtrl({ type: 'tab_reload', id });
    };

    // URL field for the active tab
    const url = document.createElement('input');
    url.type = 'text';
    url.placeholder = 'Search or enter address';
    setAddressValue(url, active ? active.url : '');
    // Leading, fixed-width field: it never grows or shrinks, so the tab row
    // keeps a stable starting edge. Capped to half the strip when narrow.
    url.style.flex = '0 0 auto';
    url.style.width = '350px';
    url.style.maxWidth = '50%';
    url.style.minWidth = '140px';
    url.style.padding = '4px 10px';
    url.style.borderRadius = '999px';
    url.style.border = `1px solid ${pal.inputBorder}`;
    url.style.background = pal.inputBg;
    url.style.color = pal.inputText;
    url.style.outline = 'none';
    url.style.transition = 'background 120ms ease, border-color 120ms ease, color 120ms ease';
    // Keys typed here must never reach the remote as chords — stop them at
    // the capture phase before the window-level handlers see them.
    url.addEventListener('keydown', (e) => e.stopPropagation());
    url.addEventListener('keyup', (e) => e.stopPropagation());
    url.addEventListener('mousedown', (e) => e.stopPropagation());
    const navigate = () => {
      const v = url.value.trim();
      if (v && active) {
        sendCtrl({ type: 'tab_navigate', url: v });
        pendingNavRef.current = v;
      }
      url.blur();
    };
    // Only explicit user focus clears/selects; autofocus leaves the current value alone.
    let userFocus = false;
    let typedSinceFocus = false;
    const markUserFocus = () => { userFocus = true; };
    url.addEventListener('mousedown', markUserFocus);
    url.addEventListener('keydown', (e) => {
      if (e.key !== 'Enter') {
        markUserFocus();
        typedSinceFocus = true;
      }
    });
    url.addEventListener('focus', () => {
      if (!userFocus) return; // autofocus: leave current value alone
      if (url.value === 'about:blank') url.value = '';
      typedSinceFocus = false;
      // Select immediately: deferring this solely to the next frame let a fast
      // first keystroke land before the select, after which select() re-selected
      // the field and the next character replaced what was typed.
      url.select();
      // A click's mouseup can still collapse the selection made in focus, so
      // restore it once the click has finished — unless the user started typing.
      requestAnimationFrame(() => {
        if (url.isConnected && document.activeElement === url && !typedSinceFocus) url.select();
      });
    });
    url.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') navigate();
    });
    url.addEventListener('blur', () => {
      userFocus = false;
      // Keep the submitted URL visible instead of flashing back to the stale active.url.
      if (pendingNavRef.current) {
        setAddressValue(url, pendingNavRef.current);
      } else {
        setAddressValue(url, active ? active.url : '');
      }
    });

    // Left-to-right order: address bar (fixed width) → history/reload → tabs
    // (with `+` at the end of the row), which take the remaining width and
    // scroll internally.
    bar.appendChild(url);
    bar.appendChild(back);
    bar.appendChild(reload);
    chips.appendChild(plus);
    bar.appendChild(chips);

    // Restore the previous scroll offset, then reveal the active chip only on a
    // real tab switch — a title-only broadcast must not yank the row back.
    const updateFade = () => {
      const max = chips.scrollWidth - chips.clientWidth;
      if (max <= 1) {
        chips.removeAttribute('data-fade');
        return;
      }
      const atStart = chips.scrollLeft <= 1;
      const atEnd = chips.scrollLeft >= max - 1;
      chips.dataset.fade = atStart ? 'r' : atEnd ? 'l' : 'lr';
    };
    chips.addEventListener('scroll', updateFade, { passive: true });
    chips.addEventListener('wheel', (e) => {
      // No overflow: leave the wheel to the page, as before.
      if (chips.scrollWidth <= chips.clientWidth) return;
      const delta = e.deltaX !== 0 ? e.deltaX : e.deltaY;
      if (!delta) return;
      e.preventDefault();
      e.stopPropagation();
      chips.scrollLeft += delta;
    }, { passive: false });
    chips.scrollLeft = prevScrollLeft;
    if (activeChip && (activeTabChanged || prevTabs.length === 0)) {
      const chipRect = activeChip.getBoundingClientRect();
      const viewRect = chips.getBoundingClientRect();
      if (chipRect.left < viewRect.left) {
        chips.scrollLeft -= viewRect.left - chipRect.left + 8;
      } else if (chipRect.right > viewRect.right) {
        chips.scrollLeft += chipRect.right - viewRect.right + 8;
      }
    }
    updateFade();

    // Restore an in-flight edit into the fresh input so keystrokes never leak to <body>.
    if (keepEdit) {
      setAddressValue(url, keepEdit.value);
      url.focus();
      try {
        const end = keepEdit.end ?? url.value.length;
        url.setSelectionRange(keepEdit.start ?? end, end);
      } catch { /* selection unsupported on this input type */ }
    } else if (urlFocused) {
      // Keep focus out of the remote page while the field shows the new tab's address.
      url.focus();
      if (keepCaret) {
        try {
          const end = keepCaret.end ?? url.value.length;
          url.setSelectionRange(keepCaret.start ?? end, end);
        } catch { /* selection unsupported on this input type */ }
      }
    }

    // Autofocus once, only before the first navigation; never steal focus after a page loads.
    if (!urlAutofocusRef.current) {
      const ae = document.activeElement;
      const pageReady = active && active.url !== '' && active.url !== 'about:blank';
      if (pageReady || (ae && ae !== document.body)) {
        urlAutofocusRef.current = true; // not eligible, consider it "done"
      } else {
        requestAnimationFrame(() => {
          if (urlAutofocusRef.current || !url.isConnected) return;
          urlAutofocusRef.current = true;
          url.focus();
        });
      }
    }
  }, [sendCtrl]);

  // The strip lives outside React's render tree; replay the last broadcast to restyle it.
  useEffect(() => {
    themeRef.current = theme;
    // Letterbox fill must follow the app background through theme toggles.
    const letterbox = CAPTURE_LETTERBOX_BG[theme];
    if (videoRef.current) videoRef.current.style.backgroundColor = letterbox;
    if (canvasRef.current) canvasRef.current.style.backgroundColor = letterbox;
    if (lastTabsRef.current.length > 0) rebuildTabBar(lastTabsRef.current);
  }, [theme, rebuildTabBar]);

  // Fullscreen passes its toolbar slot; null restores the in-container position.
  const setTabBarHost = useCallback((el: HTMLElement | null) => {
    if (tabBarHostRef.current === el) return;
    tabBarHostRef.current = el;
    if (tabbarRef.current) rebuildTabBar(lastTabsRef.current);
  }, [rebuildTabBar]);

  const doConnect = useCallback((
    streamWsUrl: string,
    _inputWsUrl: string,
    container: HTMLElement,
    viewOnly: boolean,
    _bandwidthKbps?: number,
    _sessionId?: string,
  ) => {
    disconnect();
    // Fresh encoder per connection: restart at L0; a manual override re-applies on stream_init.
    pingOutstandingRef.current = null;
    rttEmaRef.current = null;
    targetKbpsRef.current = 0;
    sentKbpsRef.current = 0;
    ratioEmaRef.current = null;
    baseKbpsRef.current = 0;
    metricsTickRef.current = 0;
    const abr = abrRef.current;
    abr.level = 0;
    abr.badSecs = 0;
    abr.goodSecs = 0;
    abr.lastChangeAt = 0;
    abr.pausedUntil = 0;
    containerRef.current = container;
    wcContainerRef.current = container;
    wcViewOnlyRef.current = viewOnly;
    if (container && getComputedStyle(container).position === 'static') {
      container.style.position = 'relative';
    }
    // Eager 36px for host sessions (viewOnly has no strip) so the first frame
    // never renders under the tab strip. 0 on disconnect or toolbar-hosted.
    if (!viewOnly && getIsolaTabs() && !tabBarHostRef.current) {
      isolaTopOffset = 36;
    } else {
      isolaTopOffset = 0;
    }
    isolaInsets = { right: 0, bottom: 0 };
    setError(null);
    // Only retries after a failed attempt count as reconnecting.
    setReconnecting(reconnectAttemptRef.current > 0);
    connectStartedAtRef.current = performance.now();
    firstFrameRecordedRef.current = false;
    appendCountRef.current = 0;
    bytesReceivedRef.current = 0;
    segmentsThisSecondRef.current = 0;
    lastSegmentAtRef.current = 0;
    lastBitrateWindowRef.current = { at: performance.now(), bytes: 0 };
    arrivalWindowRef.current = createArrivalWindow();

    // Capability negotiation rides the handshake (?mode=webcodecs); first-message negotiation raced slow links.
    const wsUrl = new URL(streamWsUrl);
    wsUrl.searchParams.set('channel', 'isola');
    const wcCapable = !!(wcHost.VideoDecoder && wcHost.EncodedAudioChunk && wcHost.EncodedVideoChunk);
    if (wcCapable) {
      wsUrl.searchParams.set('mode', 'webcodecs');
      wcModeRef.current = true;
    }
    const ws = new WebSocket(wsUrl.toString());
    ws.binaryType = 'arraybuffer';
    wsRef.current = ws;

    // Input WS: separate connection for keyboard/mouse events (channel=input).
    // This avoids head-of-line blocking from video segments on the same TCP stream.
    let inputReconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let inputAttempt = 0;
    const openInputWs = () => {
      const iUrl = new URL(_inputWsUrl);
      iUrl.searchParams.set('channel', 'input');
      const inputWs = new WebSocket(iUrl.toString());
      inputWsRef.current = inputWs;
      inputWs.onopen = () => {
        inputAttempt = 0;
        inputLastMsgAtRef.current = Date.now();
        setMetrics((m) => ({ ...m, inputBackend: 'cdp' }));
        // Replay input buffered during the reconnect window (drop stale).
        const now = Date.now();
        const backlog = inputBacklogRef.current.splice(0)
          .filter((b) => now - b.at <= INPUT_BACKLOG_TTL_MS);
        for (const b of backlog) {
          try { inputWs.send(JSON.stringify(b.ev)); } catch { break; }
        }
      };
      inputWs.onmessage = (ev) => {
        inputLastMsgAtRef.current = Date.now();
        if (typeof ev.data !== 'string') return;
        let msg: Record<string, unknown>;
        try { msg = JSON.parse(ev.data); } catch { return; }
        if (msg.type === 'selection' && typeof msg.text === 'string' && msg.text) {
          navigator.clipboard.writeText(msg.text).catch(() => {});
        } else if (msg.type === 'renderer_stall') {
          // A wedged renderer was replaced; surface it so the frozen frame is explained and self-healing.
          const stalled = msg.stalled === true;
          setMetrics((m) => ({
            ...m,
            rendererStalled: stalled,
            rendererStalledMs: stalled && typeof msg.ms === 'number' ? msg.ms : 0,
            rendererRecoveries: typeof msg.recoveries === 'number' ? msg.recoveries : m.rendererRecoveries,
          }));
        } else if (msg.type === 'clipboard_sync' && typeof msg.text === 'string' && msg.text) {
          // Sandbox clipboard changed — mirror it onto the host clipboard.
          setRemoteClipboard(msg.text);
          navigator.clipboard.writeText(msg.text).catch(() => {});
        } else if (msg.type === 'tabs' && Array.isArray(msg.tabs)) {
          const arr = msg.tabs as Array<{ id: number; url: string; title: string; active: boolean }>;
          setBrowserTabs(arr);
          const activeT = arr.find((t) => t.active);
          if (pendingNavRef.current && activeT && (!isPlaceholderUrl(activeT.url) || activeT.url === pendingNavRef.current)) {
            pendingNavRef.current = '';
          }
          rebuildTabBar(arr);
        } else if (msg.type === 'net' && msg.e && typeof msg.e === 'object') {
          // DOM-event dispatch keeps the panel React-owned.
          window.dispatchEvent(new CustomEvent('isola-net', { detail: msg.e }));
        } else if (msg.type === 'headers' && msg.e && typeof msg.e === 'object') {
          // Document headers + posture/TLS; same dispatch pattern as net/console.
          window.dispatchEvent(new CustomEvent('isola-headers', { detail: msg.e }));
        } else if (msg.type === 'console' && typeof msg.l === 'string' && typeof msg.t === 'string') {
          window.dispatchEvent(new CustomEvent('isola-console', { detail: { i: typeof msg.i === 'number' ? msg.i : undefined, level: msg.l, text: msg.t, tb: msg.tb, ts: typeof msg.ts === 'number' ? msg.ts : undefined } }));
        } else if (msg.type === 'cookie' && typeof msg.n === 'string') {
          window.dispatchEvent(new CustomEvent('isola-cookie', { detail: msg }));
        } else if (msg.type === 'cursor' && typeof msg.c === 'string') {
          // Offscreen capture has no cursor: shape the CSS cursor from the broadcast; keep the shape for text-drag routing.
          remoteCursorRef.current = msg.c;
          const canvas = canvasRef.current;
          if (canvas) canvas.style.cursor = msg.c;
          hoverURLRef.current = typeof msg.u === 'string' ? msg.u : '';
        } else if (msg.type === 'net_init' && Array.isArray(msg.entries)) {
          // Replay of everything captured before this viewer attached.
          window.dispatchEvent(new CustomEvent('isola-net-init', { detail: msg.entries }));
        } else if (msg.type === 'net_clear') {
          // Reserved: entries carry a per-tab tag (tb) so the panel filters instead.
          window.dispatchEvent(new CustomEvent('isola-net-clear'));
        }
      };
      inputWs.onerror = () => {
        console.warn('[isola] input WS down, inline');
      };
      // Without a retry every later tab command silently no-ops on the closed socket.
      inputWs.onclose = () => {
        if (inputWsRef.current === inputWs) inputWsRef.current = null;
        if (inputReconnectTimer || inputAttempt >= INPUT_RECONNECT_ATTEMPTS) return;
        const delay = Math.min(500 * Math.pow(2, inputAttempt), 5000);
        inputAttempt++;
        inputReconnectTimer = setTimeout(() => {
          inputReconnectTimer = null;
          openInputWs();
        }, delay);
      };
    };
    if (!viewOnly && _inputWsUrl) openInputWs();

    const video = document.createElement('video');
    video.autoplay = true;
    video.muted = true;
    video.playsInline = true;
    // Inset layout already sizes the capture; no width/height 100%.
    applyInsets(video);
    // contain, not cover — cover cropped the top of every page under the tab strip.
    video.style.objectFit = 'contain';
    video.style.backgroundColor = CAPTURE_LETTERBOX_BG[themeRef.current];
    video.style.cursor = 'none';
    video.style.opacity = '0';
    video.style.transition = 'opacity 200ms ease-out';
    container.appendChild(video);
    videoRef.current = video;
    currentCaptureVideo = video;

    const mediaSource = new MediaSource();
    mediaSourceRef.current = mediaSource;
    if (mediaObjectUrlRef.current) URL.revokeObjectURL(mediaObjectUrlRef.current);
    mediaObjectUrlRef.current = URL.createObjectURL(mediaSource);
    video.src = mediaObjectUrlRef.current;

    let streamCodec = pickMseCodec(MSE_CODEC_FALLBACK);
    let gotStreamInit = false;

    const addSourceBuffer = (ms: MediaSource) => {
      const codec = pickMseCodec(streamCodec);
      streamCodec = codec;
      try {
        const sb = ms.addSourceBuffer(`video/mp4; codecs="${codec}"`);
        sb.mode = 'segments';
        sb.addEventListener('updateend', () => flushPending());
        sb.addEventListener('error', () => {
          console.error('[isola] sourcebuffer error', video.error?.code);
          sourceBufferRef.current = null;
        });
        sourceBufferRef.current = sb;
        flushPending();
      } catch (e) {
        console.error('[isola] addSourceBuffer', e);
        setError('MSE not supported or codec unavailable');
        setReconnecting(false);
      }
    };

    mediaSource.addEventListener('sourceopen', () => {
      if (gotStreamInit) addSourceBuffer(mediaSource);
    });

    function appendSegment(seg: Uint8Array<ArrayBuffer>) {
      const pending = pendingSegmentsRef.current;
      pending.push(seg);
      while (pending.length > MAX_PENDING_SEGMENTS) {
        pending.shift();
      }
      flushPending();
    }

    const cleanupFps = wireVideoFps(video);
    const cleanupPlayout = wirePlayoutSync(video);

    // requestKeyframeNow asks for an IDR immediately (decode-error path); the
    // timer in beginRecovery covers the no-IDR-within-timeout path.
    function requestKeyframeNow() {
      if (recoveryRequestedRef.current) return; // one request per attempt
      recoveryRequestedRef.current = true;
      recoveryRequestsRef.current++;
      // Repeated requests without an IDR: restarts failing server-side; reconnect.
      if (recoveryRequestsRef.current >= MAX_KEYFRAME_REQUESTS) {
        console.error('[isola] no IDR, reconnect');
        recoveryRequestsRef.current = 0;
        recoveryRequestedRef.current = false;
        const stalled = wsRef.current;
        if (stalled && stalled.readyState === WebSocket.OPEN) {
          stalled.close();
        }
        return;
      }
      sendControl({ type: 'request_keyframe' });
    }

    // beginRecovery arms the keyframe watchdog and re-arms until an IDR lands.
    // reschedule=true (stream_reset): the old timer's window was burned on the torn-down encoder.
    function beginRecovery(reschedule = false) {
      if (recoveryStartedAtRef.current === null) {
        recoveryStartedAtRef.current = performance.now();
      }
      recoveryRequestedRef.current = false;
      if (reschedule && keyframeRequestTimerRef.current !== null) {
        clearTimeout(keyframeRequestTimerRef.current);
        keyframeRequestTimerRef.current = null;
      }
      if (keyframeRequestTimerRef.current !== null) return;
      keyframeRequestTimerRef.current = setTimeout(() => {
        keyframeRequestTimerRef.current = null;
        if (keyframeSeenRef.current === false) {
          requestKeyframeNow();
          beginRecovery(); // stay armed until an IDR lands
        }
      }, KEYFRAME_REQUEST_TIMEOUT_MS);
    }

    // notePacket: a TCP gap means the streamer shed packets; a burst of
    // LOSS_BURST_PACKETS routes into the shared recovery path, once per burst.
    function notePacket(seq: number, isKeyframe: boolean) {
      const loss = noteStreamedPacket(frameCountersRef.current, seq, isKeyframe);
      if (!loss.requestKeyframe) return;
      // A gap breaks the reference chain; a stale true would stop the watchdog from escalating.
      keyframeSeenRef.current = false;
      beginRecovery();
      requestKeyframeNow();
    }

    const wcPathRefs = {
      lastWcConfigRef,
      canvasRef,
      wcFramesRef,
      wcDecodedThisSecondRef,
      firstFrameRecordedRef,
      connectStartedAtRef,
      decodeErrorCountRef,
      keyframeSeenRef,
      decoderRef,
      tsUsRef,
      wcChunkCtorRef,
      frameStepUsRef,
      mediaSourceRef,
      sourceBufferRef,
      pendingSegmentsRef,
      videoRef,
    };
    function noteDecodeError(e: unknown) {
      console.error('[isola-wc] decode:', e);
      decodeErrorCountRef.current++;
      keyframeSeenRef.current = false; // out of sync until the next IDR
      beginRecovery();
      requestKeyframeNow();
      setMetrics((m) => ({ ...m, decodeErrorCount: decodeErrorCountRef.current }));
    }

    // Bounded decode admission: the sink drops backlog past MAX_DECODE_BACKLOG_MS and skips to the next IDR.
    const auSink = createWcAccessUnitSink({
      getDecoder: () => decoderRef.current,
      getChunkCtor: () => wcChunkCtorRef.current,
      frameStepUs: () => frameStepUsRef.current,
      tsUs: tsUsRef,
      isKeyframeSeen: () => keyframeSeenRef.current,
      setKeyframeSeen: (v: boolean) => { keyframeSeenRef.current = v; },
      onSkipAhead: () => {
        // Abandon queued frames and ask for a fresh IDR — an encoder restart, not a whole GOP.
        resetWcDecoder(decoderRef.current, lastWcConfigRef.current);
        beginRecovery();
        requestKeyframeNow();
      },
      onDecodeError: noteDecodeError,
    });

    const wcPathCallbacks = {
      setFirstFrame,
      setMetrics,
      beginRecovery,
      requestKeyframeNow,
      applyInsets,
      letterboxBg: CAPTURE_LETTERBOX_BG[themeRef.current],
      onVideoRemoved: () => { currentCaptureVideo = null; },
      onCanvasMounted: (canvas: HTMLCanvasElement) => { currentCaptureCanvas = canvas; },
      onFramePainted: () => auSink.notePainted(),
    };

    function createWcDecoder(): WCDecoder | null {
      return buildWcDecoder(wcPathRefs, wcPathCallbacks);
    }

    function setupWebCodecs(msg: { codec?: string; width?: number; height?: number }): boolean {
      return setupWebCodecsVideoPath(msg, container, wcPathRefs, wcPathCallbacks);
    }

    metricsIntervalRef.current = setInterval(() => {
      const now = performance.now();
      // Half-open input sockets never fire onclose; replace the zombie so tab commands work.
      const liveInput = inputWsRef.current;
      if (
        liveInput && liveInput.readyState === WebSocket.OPEN &&
        inputLastMsgAtRef.current > 0 &&
        Date.now() - inputLastMsgAtRef.current > INPUT_ZOMBIE_MS
      ) {
        console.warn('[isola] input WS unresponsive, reconnect');
        inputLastMsgAtRef.current = Date.now(); // once per window
        inputAttempt = 0; // the attempts cap must not block the replacement
        liveInput.close();
      }
      // RTT probe every 2s on the video channel; app-level JSON ping, one outstanding.
      metricsTickRef.current++;
      const wsNow = wsRef.current;
      if (
        metricsTickRef.current % 2 === 0 &&
        wsNow && wsNow.readyState === WebSocket.OPEN &&
        pingOutstandingRef.current === null
      ) {
        const sentAt = performance.now();
        try {
          wsNow.send(JSON.stringify({ type: 'ping', ts: sentAt }));
          pingOutstandingRef.current = sentAt;
        } catch { /* ignore */ }
      }
      if (pingOutstandingRef.current !== null && now - pingOutstandingRef.current > 5000) {
        pingOutstandingRef.current = null; // reply lost — don't wedge the probe
      }
      const win = lastBitrateWindowRef.current;
      if (win.at === 0) {
        lastBitrateWindowRef.current = { at: now, bytes: bytesReceivedRef.current };
        return;
      }
      const dtSec = (now - win.at) / 1000;
      const bitrateKbps = dtSec > 0
        ? Math.round(((bytesReceivedRef.current - win.bytes) * 8) / dtSec / 1000)
        : 0;
      lastBitrateWindowRef.current = { at: now, bytes: bytesReceivedRef.current };
      const segmentRate = segmentsThisSecondRef.current;
      segmentsThisSecondRef.current = 0;
      if (wcModeRef.current) {
        const fps = wcFramesRef.current;
        wcFramesRef.current = 0;
        const canvas = canvasRef.current;
        // Annex-B decode-queue state as of this tick.
        const wcStats = auSink.stats();
        setMetrics((m) => ({
          ...m,
          recentFps: fps,
          videoWidth: canvas ? canvas.width : m.videoWidth,
          videoHeight: canvas ? canvas.height : m.videoHeight,
          decodeBacklogMs: wcStats.backlogMs,
          decodeDroppedFrames: wcStats.droppedFrames,
          decodeCatchUps: wcStats.catchUps,
        }));
      }
      const lastSegmentAgeMs = lastSegmentAtRef.current > 0
        ? Math.round(now - lastSegmentAtRef.current)
        : -1;
      // Windowed to this tick's packets; sampled after the early return so window and interval match.
      const packetLoss = samplePacketLoss(frameCountersRef.current, lossWindowRef.current);

      // ABR: 1 Hz, ref state. Paused right after a reset. Arrival must be the
      // metrics-aligned sample or bursty streams fabricate loss; free-running bitrateKbps is the fallback.
      const abr = abrRef.current;
      const arrivalWin = arrivalWindowRef.current;
      const arrivalAligned = arrivalWin.fresh && now - arrivalWin.atMs <= ARRIVAL_WINDOW_MAX_AGE_MS;
      const abrResult = abrTick(abr, {
        nowMs: Date.now(),
        sentKbps: sentKbpsRef.current,
        arrivalKbps: arrivalAligned ? arrivalWin.kbps : bitrateKbps,
        targetKbps: targetKbpsRef.current,
        rttMs: rttEmaRef.current,
        baseKbps: baseKbpsRef.current,
        ratioEma: ratioEmaRef.current,
      });
      ratioEmaRef.current = abrResult.ratioEma;
      const netQuality = abrResult.netQuality;
      const netRatio = abrResult.netRatio;
      // A step is an encoder restart; the client recovers via stream_reset + stream_init.
      if (abrResult.stepToKbps !== null) {
        sendControl({ type: 'set_bitrate', kbps: abrResult.stepToKbps });
      }

      setMetrics((m) => ({
        ...m,
        // Frame-rate counters published here at 1 Hz, not per frame.
        ...frameMetricsPatch(frameCountersRef.current),
        packetsLost: packetLoss.packetsLost,
        packetLossRatio: packetLoss.packetLossRatio,
        // MSE playout state; WebCodecs mode publishes canvas geometry instead.
        ...(wcModeRef.current ? {} : playoutMetricsPatch(playoutRef.current)),
        decodeErrorCount: decodeErrorCountRef.current,
        lastRecoveryMs: lastRecoveryMsRef.current,
        bytesReceived: bytesReceivedRef.current,
        bitrateKbps,
        segmentRate,
        pendingSegments: pendingSegmentsRef.current.length,
        appendCount: appendCountRef.current,
        lastSegmentAgeMs,
        rttMs: rttEmaRef.current,
        netQuality,
        netRatio,
        abrLevel: abr.level,
      }));
    }, 1000);

    const markDecodedFrame = () => {
      const v = videoRef.current;
      if (!v || v.videoWidth <= 0 || firstFrameRecordedRef.current) return;
      firstFrameRecordedRef.current = true;
      // A decoded frame — not onopen — earns a fresh backoff window.
      reconnectAttemptRef.current = 0;
      setFirstFrame(true);
      void v.play().catch(() => {});
      if (connectStartedAtRef.current) {
        const elapsed = Math.round(performance.now() - connectStartedAtRef.current);
        setMetrics((m) => (m.firstFrameMs === elapsed ? m : { ...m, firstFrameMs: elapsed }));
      }
    };
    video.addEventListener('loadeddata', markDecodedFrame);
    video.addEventListener('resize', markDecodedFrame);

    ws.onopen = () => {
      setError(null);
      setMetrics((m) => ({ ...m, wsState: ws.readyState }));
      setConnected(true);
      // Capability negotiation happens on the handshake via ?mode=webcodecs
      // (set in doConnect) — no hello message on this socket.
      if (connectStartedAtRef.current) {
        const elapsed = Math.round(performance.now() - connectStartedAtRef.current);
        setMetrics((m) => (m.connectMs === elapsed ? m : { ...m, connectMs: elapsed }));
      }
    };

    ws.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        // A malformed text frame must not throw out of onmessage.
        let msg: Record<string, unknown>;
        try {
          msg = JSON.parse(ev.data) as Record<string, unknown>;
        } catch {
          return;
        }
        if (typeof msg !== 'object' || msg === null) return;
        switch (msg.type) {
          case 'stream_init':
            // New run: seq restarts at 0.
            resetPacketLoss(frameCountersRef.current);
            // No IDR of this run has landed yet.
            keyframeSeenRef.current = false;
            if (wcModeRef.current && msg.mode === 'webcodecs') {
              // An ABR step reuses the same decoder; only the first init tears anything down.
              const firstEntry = !canvasRef.current || !decoderRef.current;
              const ok = firstEntry ? setupWebCodecs(msg) : applyWcStreamInit(
                msg,
                container,
                wcPathRefs,
                wcPathCallbacks,
              );
              if (!ok) {
                setError('WebCodecs H.264 decode unavailable in this browser');
                setReconnecting(false);
              }
              setReconnecting(false);
              setMetrics((m) => ({
                ...m,
                codec: typeof msg.codec === 'string' ? msg.codec : 'avc1',
                streamWidth: typeof msg.width === 'number' ? msg.width : m.streamWidth,
                streamHeight: typeof msg.height === 'number' ? msg.height : m.streamHeight,
                captureWidth: typeof msg.capture_width === 'number' ? msg.capture_width : m.captureWidth,
                captureHeight: typeof msg.capture_height === 'number' ? msg.capture_height : m.captureHeight,
                targetFps: typeof msg.encode_fps === 'number' ? msg.encode_fps : m.targetFps,
              }));
              if (typeof msg.encode_fps === 'number' && msg.encode_fps > 0) {
                frameStepUsRef.current = Math.round(1_000_000 / msg.encode_fps);
              }
              if (typeof msg.capture_width === 'number' && typeof msg.capture_height === 'number') {
                captureSizeRef.current = { w: msg.capture_width, h: msg.capture_height };
              }
              if (typeof msg.base_bitrate === 'number' && msg.base_bitrate > 0) {
                baseKbpsRef.current = msg.base_bitrate;
              }
              applyPersistedQuality();
              break;
            }
            // Server answered MSE despite mode=webcodecs; keep wcModeRef honest.
            wcModeRef.current = false;
            if (typeof msg.codec === 'string' && msg.codec) {
              streamCodec = pickMseCodec(msg.codec);
            }
            gotStreamInit = true;
            if (mediaSource.readyState === 'open' && !sourceBufferRef.current) {
              addSourceBuffer(mediaSource);
            }
            setReconnecting(false);
            setMetrics((m) => ({
              ...m,
              codec: streamCodec,
              streamWidth: typeof msg.width === 'number' ? msg.width : m.streamWidth,
              streamHeight: typeof msg.height === 'number' ? msg.height : m.streamHeight,
              captureWidth: typeof msg.capture_width === 'number' ? msg.capture_width : m.captureWidth,
              captureHeight: typeof msg.capture_height === 'number' ? msg.capture_height : m.captureHeight,
            }));
            if (typeof msg.capture_width === 'number' && typeof msg.capture_height === 'number') {
              captureSizeRef.current = { w: msg.capture_width, h: msg.capture_height };
            } else if (typeof msg.width === 'number' && typeof msg.height === 'number') {
              captureSizeRef.current = { w: msg.width, h: msg.height };
            }
            if (typeof msg.base_bitrate === 'number' && msg.base_bitrate > 0) {
              baseKbpsRef.current = msg.base_bitrate;
            }
            applyPersistedQuality();
            if (videoRef.current) {
              videoRef.current.style.opacity = '1';
            }
            break;
          case 'clipboard_sync':
            setRemoteClipboard(typeof msg.text === 'string' ? msg.text : '');
            if (typeof msg.text === 'string' && msg.text) {
              navigator.clipboard.writeText(msg.text).catch(() => {});
            }
            break;
          case 'stream_reset':
            // Replacement run: seq starts over.
            resetPacketLoss(frameCountersRef.current);
            {
              // The restart itself isn't degradation; pause the estimator.
              const a = abrRef.current;
              a.pausedUntil = Date.now() + ABR_RESTART_PAUSE_MS;
              a.badSecs = 0;
              a.goodSecs = 0;
            }
            if (wcModeRef.current) {
              // reset() leaves the decoder unconfigured; resetWcDecoder re-arms it for the next run.
              resetWcDecoder(decoderRef.current, lastWcConfigRef.current);
              auSink.resetForNewRun();
              keyframeSeenRef.current = false;
              tsUsRef.current = 0;
              beginRecovery(true); // full window for the replacement encoder's IDR
              if (msg.reason !== 'bitrate_change' && msg.reason !== 'keyframe_request') {
                // Quick restarts keep the same canvas — no loading flash.
                firstFrameRecordedRef.current = false;
                setFirstFrame(false);
              }
              break;
            }
            // MSE needs a new init segment after an encoder restart.
            if (mediaSourceRef.current && mediaSourceRef.current.readyState === 'open') {
              try { mediaSourceRef.current.endOfStream(); } catch { /* ignore */ }
            }
            sourceBufferRef.current = null;
            pendingSegmentsRef.current = [];
            firstFrameRecordedRef.current = false;
    appendCountRef.current = 0;
            setFirstFrame(false);
            // New MediaSource so the new init segment parses correctly.
            if (videoRef.current) {
              const ms = new MediaSource();
              mediaSourceRef.current = ms;
              if (mediaObjectUrlRef.current) URL.revokeObjectURL(mediaObjectUrlRef.current);
              mediaObjectUrlRef.current = URL.createObjectURL(ms);
              videoRef.current.src = mediaObjectUrlRef.current;
              ms.addEventListener('sourceopen', () => {
                addSourceBuffer(ms);
              });
            }
            break;
          case 'pong': {
            // EMA-smooth into a ref; the metrics interval publishes it.
            const sentAt = pingOutstandingRef.current;
            if (sentAt !== null && typeof msg.ts === 'number') {
              const sample = performance.now() - msg.ts;
              pingOutstandingRef.current = null;
              rttEmaRef.current = rttEmaRef.current === null
                ? Math.round(sample)
                : Math.round(rttEmaRef.current * (1 - RTT_EMA_ALPHA) + sample * RTT_EMA_ALPHA);
            }
            break;
          }
          case 'metrics':
            setMetrics((m) => ({
              ...m,
              inputBackend: typeof msg.input_backend === 'string' ? msg.input_backend : m.inputBackend,
              inputTarget: typeof msg.input_target === 'string' ? msg.input_target : m.inputTarget,
              inputTargetOK: msg.input_target_ok !== false,
              activityMode: typeof msg.activity_mode === 'string' ? msg.activity_mode : m.activityMode,
              targetFps: typeof msg.capture_fps === 'number' ? msg.capture_fps : m.targetFps,
              targetBitrateKbps: typeof msg.bitrate_kbps === 'number' ? msg.bitrate_kbps : m.targetBitrateKbps,
              encoderSeq: typeof msg.encoder_seq === 'number' ? msg.encoder_seq : m.encoderSeq,
              streamWidth: typeof msg.width === 'number' ? msg.width : m.streamWidth,
              streamHeight: typeof msg.height === 'number' ? msg.height : m.streamHeight,
              encodeScale: typeof msg.encode_scale === 'number' ? msg.encode_scale : m.encodeScale,
              staticSecs: typeof msg.static_secs === 'number' ? msg.static_secs : m.staticSecs,
              skippedTicks: typeof msg.skipped_ticks === 'number' ? msg.skipped_ticks : m.skippedTicks,
              keyframeReason: typeof msg.keyframe_reason === 'string' ? msg.keyframe_reason : m.keyframeReason,
              keyframeAgeMs: typeof msg.keyframe_age_ms === 'number' ? msg.keyframe_age_ms : m.keyframeAgeMs,
              rendererStalled: typeof msg.renderer_stalled === 'boolean' ? msg.renderer_stalled : m.rendererStalled,
              rendererStalledMs: typeof msg.renderer_stalled_ms === 'number' ? msg.renderer_stalled_ms : m.rendererStalledMs,
              rendererRecoveries: typeof msg.renderer_recoveries === 'number' ? msg.renderer_recoveries : m.rendererRecoveries,
              cdpProbeFailStreak: typeof msg.cdp_probe_fail_streak === 'number' ? msg.cdp_probe_fail_streak : m.cdpProbeFailStreak,
              cdpProbeFailAgoMs: typeof msg.cdp_probe_fail_ago_ms === 'number' ? msg.cdp_probe_fail_ago_ms : m.cdpProbeFailAgoMs,
              captureHealthOk: typeof msg.capture_health_ok === 'boolean' ? msg.capture_health_ok : m.captureHealthOk,
              captureEmptyFrames: typeof msg.capture_empty_frames === 'number' ? msg.capture_empty_frames : m.captureEmptyFrames,
              captureEmptyWindow: typeof msg.capture_empty_window === 'number' ? msg.capture_empty_window : m.captureEmptyWindow,
              captureFallback: typeof msg.capture_fallback === 'boolean' ? msg.capture_fallback : m.captureFallback,
              captureLastPaintMs: typeof msg.capture_last_paint_ms === 'number' ? msg.capture_last_paint_ms : m.captureLastPaintMs,
              captureLastFrameMs: typeof msg.capture_last_frame_ms === 'number' ? msg.capture_last_frame_ms : m.captureLastFrameMs,
              changesPerSec: typeof msg.changes_per_sec === 'number' ? msg.changes_per_sec : m.changesPerSec,
              fullFramesPerSec: typeof msg.full_frames_per_sec === 'number' ? msg.full_frames_per_sec : m.fullFramesPerSec,
              damagedPct: typeof msg.damaged_pct === 'number' ? msg.damaged_pct : m.damagedPct,
              captureLastSentMs: typeof msg.capture_last_sent_ms === 'number' ? msg.capture_last_sent_ms : m.captureLastSentMs,
              captureSentFrames: typeof msg.capture_sent_frames === 'number' ? msg.capture_sent_frames : m.captureSentFrames,
              captureDedupFrames: typeof msg.capture_dedup_frames === 'number' ? msg.capture_dedup_frames : m.captureDedupFrames,
              // Packet-shed counter from newer streamers; keep the last value on older servers.
              serverDropped: typeof msg.dropped === 'number' ? msg.dropped : m.serverDropped,
            }));
            if (typeof msg.bitrate_kbps === 'number') {
              targetKbpsRef.current = msg.bitrate_kbps;
            }
            if (typeof msg.sent_kbps === 'number') {
              sentKbpsRef.current = msg.sent_kbps;
              // One ordered TCP stream: closing here puts both numbers on the same second.
              advanceArrivalWindow(arrivalWindowRef.current, performance.now(), bytesReceivedRef.current);
            }
            break;
        }
        return;
      }
      // Binary: 10-byte header, then fMP4 fragment (marker 0x01) or raw
      // Annex-B access unit (marker 0x02, WebCodecs mode).
      const buf = ev.data as ArrayBuffer;
      if (buf.byteLength <= 10) return;
      const bytes = new Uint8Array(buf);
      const view = new DataView(buf);
      const seq = view.getUint32(2, true);
      const isKeyframe = (bytes[1] & 0x01) !== 0;
      bytesReceivedRef.current += buf.byteLength;
      lastSegmentAtRef.current = performance.now();

      if (bytes[0] === 0x02 && wcModeRef.current) {
        // In-order delivery, bounded backlog: the sink drops past
        // MAX_DECODE_BACKLOG_MS and resumes on the next IDR. Synthetic CFR timestamps.
        if (decoderRef.current && decoderRef.current.state === 'closed') {
          // A closed decoder otherwise drops every packet; rebuild from the last config.
          console.warn('[isola-wc] decoder closed, rebuilding');
          createWcDecoder();
          auSink.resetForNewRun();
        }
        const outcome = auSink.push(isKeyframe, bytes.subarray(10));
        if (outcome === 'resynced' || outcome === 'decoded') {
          if (outcome === 'resynced') {
            // Recovery complete: a usable IDR landed; record how long it took.
            const recoveryStart = recoveryStartedAtRef.current;
            if (recoveryStart !== null) {
              lastRecoveryMsRef.current = Math.round(performance.now() - recoveryStart);
              recoveryStartedAtRef.current = null;
              recoveryRequestedRef.current = false;
              recoveryRequestsRef.current = 0;
              if (keyframeRequestTimerRef.current !== null) {
                clearTimeout(keyframeRequestTimerRef.current);
                keyframeRequestTimerRef.current = null;
              }
            }
          }
          // Ref only; the 1 Hz tick publishes.
          notePacket(seq, isKeyframe);
        }
        return;
      }

      if (bytes[0] === 0x01) {
        segmentsThisSecondRef.current++;
        // One view over the buffer avoids copying the header off every fragment.
        appendSegment(new Uint8Array(buf, 10));
        // MSE must see appended IDRs as the resync signal too.
        if (isKeyframe) {
          keyframeSeenRef.current = true;
          const recoveryStart = recoveryStartedAtRef.current;
          if (recoveryStart !== null) {
            lastRecoveryMsRef.current = Math.round(performance.now() - recoveryStart);
            recoveryStartedAtRef.current = null;
          }
          recoveryRequestedRef.current = false;
          recoveryRequestsRef.current = 0;
          if (keyframeRequestTimerRef.current !== null) {
            clearTimeout(keyframeRequestTimerRef.current);
            keyframeRequestTimerRef.current = null;
          }
        }
        // Ref only — published by the 1 Hz metrics tick, not per segment.
        notePacket(seq, isKeyframe);
        return;
      }

      if (bytes[0] === 0x03) {
        // Opus packet (20ms): decode → schedule into the AudioContext.
        const dec = audioDecoderRef.current;
        const ChunkCtor = wcHost.EncodedAudioChunk;
        if (!dec || dec.state !== 'configured' || !ChunkCtor) return;
        if (!audioOnRef.current) return; // muted server-side gate anyway
        try {
          dec.decode(new ChunkCtor({
            type: 'key', // Opus packets are independently decodable
            timestamp: audioTsUsRef.current,
            data: bytes.subarray(10),
          }));
          audioTsUsRef.current += 20000; // 20ms frames
          if (!audioAnnouncedRef.current) {
            audioAnnouncedRef.current = true;
            setAudioActive(true);
          }
        } catch (e) {
          console.error('[isola-audio] decode:', e);
        }
      }
    };

    ws.onerror = () => {
      console.warn('[isola] stream WS error');
    };

    // Reconnect backoff state lives in reconnectAttemptRef so the cap
    // survives across doConnect cycles.
    const maxReconnectAttempts = 5;
    const baseDelay = 500;
    reconnectTimerRef.current = null;

    ws.onclose = () => {
      setConnected(false);
      if (reconnectAttemptRef.current >= maxReconnectAttempts) {
        setError('Connection lost after max reconnect attempts');
        setReconnecting(false);
        return;
      }
      const delay = baseDelay * Math.pow(2, reconnectAttemptRef.current);
      reconnectAttemptRef.current++;
      setReconnecting(true);
      reconnectTimerRef.current = setTimeout(async () => {
        reconnectTimerRef.current = null;
        // A dead session: retrying only hammers the relay.
        const sid = connectParamsRef.current?.sessionId;
        if (sid) {
          try {
            // Share viewers have no control token: probe their view-only share
            // link instead of the owner-only session row.
            let shareToken = '';
            try {
              shareToken = new URL(connectParamsRef.current?.streamWsUrl || '', window.location.href)
                .searchParams.get('share_token') || '';
            } catch {
              shareToken = '';
            }
            const token = shareToken ? '' : getSessionToken(sid);
            // 4s abort: a half-open probe must not park the viewer on "Reconnecting…" forever.
            const ac = new AbortController();
            const abortTimer = setTimeout(() => ac.abort(), 4000);
            const res = await fetch(
              shareToken ? `/api/share/${encodeURIComponent(shareToken)}` : `/api/sessions/${sid}`,
              {
                headers: token ? { 'X-Session-Token': token } : undefined,
                signal: ac.signal,
              },
            ).finally(() => clearTimeout(abortTimer));
            if (res.status === 404 || res.status === 401 || res.status === 403 || res.status === 410) {
              setConnected(false);
              setReconnecting(false);
              setError('Session is no longer available');
              return;
            }
            // Dead containers keep a 200 until the TTL; only the status says retrying is pointless.
            const body = (await res.json().catch(() => null)) as { status?: unknown } | null;
            if (body && isSessionTerminated(body.status)) {
              setConnected(false);
              setReconnecting(false);
              setError('Session is no longer available');
              return;
            }
          } catch {
            /* backend unreachable — the attempt cap above ends the loop */
          }
        }
        const params = connectParamsRef.current;
        if (params) {
          doConnect(
            params.streamWsUrl,
            params.inputWsUrl,
            params.container,
            params.viewOnly,
            params.bandwidthKbps,
            params.sessionId,
          );
        }
      }, delay);
    };

    // Save the reconnect cleanup
    const prevCleanup = inputCleanupRef.current;
    if (prevCleanup) {
      const extendedCleanup = () => {
        if (reconnectTimerRef.current) {
          clearTimeout(reconnectTimerRef.current);
          reconnectTimerRef.current = null;
        }
        if (metricsIntervalRef.current) {
          clearInterval(metricsIntervalRef.current);
          metricsIntervalRef.current = null;
        }
        if (keyframeRequestTimerRef.current !== null) {
          clearTimeout(keyframeRequestTimerRef.current);
          keyframeRequestTimerRef.current = null;
        }
        cleanupFps();
        cleanupPlayout();
        prevCleanup();
      };
      inputCleanupRef.current = extendedCleanup;
    }

    // Input handling — match WebRTC: global keys, reliable mouse button tracking.
    if (!viewOnly) {
      if (!container.hasAttribute('tabindex')) {
        container.setAttribute('tabindex', '-1');
      }
      container.style.outline = 'none';

      const getCoords = (e: MouseEvent | WheelEvent) => {
        // Measure the actual capture element's rect (it carries every inset);
        // fall back to whichever element exists — a null canvas collapsed clicks to (0,0).
        const el: HTMLCanvasElement | HTMLVideoElement | null = wcModeRef.current
          ? (canvasRef.current ?? videoRef.current)
          : (videoRef.current ?? canvasRef.current);
        if (!el) return { x: 0, y: 0 };
        const isCanvas = el instanceof HTMLCanvasElement;
        const srcW = isCanvas ? el.width : el.videoWidth;
        const srcH = isCanvas ? el.height : el.videoHeight;
        const cap = captureSizeRef.current;
        return mapClientToCapture(
          e.clientX,
          e.clientY,
          el.getBoundingClientRect(),
          srcW,
          srcH,
          cap.w,
          cap.h,
        );
      };

      // macOS Control-click is a LEFT press with ctrlKey; map it to 'right'.
      const isMacPlatform = typeof navigator !== 'undefined'
        && /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent);
      const buttonName = (e: MouseEvent) => (
        e.button === 1 ? 'middle'
          : e.button === 2 || (isMacPlatform && e.ctrlKey && e.button === 0) ? 'right'
            : 'left'
      );

      const sendEvent = (event: Record<string, unknown>) => {
        const sws = wsRef.current;
        const iws = inputWsRef.current;
        const payload = JSON.stringify(event);
        // Route all input via dedicated input WS (avoids HOL-blocking video).
        if (iws && iws.readyState === WebSocket.OPEN) {
          iws.send(payload);
          return;
        }
        if (sws && sws.readyState === WebSocket.OPEN) {
          sws.send(payload);
          return;
        }
        // Buffer a click spanning a reconnect; mousemove is not worth replaying.
        if (iws && iws.readyState === WebSocket.CONNECTING && event.type !== 'mousemove') {
          const q = inputBacklogRef.current;
          q.push({ ev: event, at: Date.now() });
          while (q.length > INPUT_BACKLOG_CAP) q.shift();
        }
      };

      // A hidden editable is the only way the host IME can compose: composition
      // events need a focused editable target, and the viewer itself has none.
      // Normal keys are still handled by onKeyDown (with preventDefault); only
      // composition text is read here and sent as one `type` message.
      const ime = document.createElement('textarea');
      ime.setAttribute('aria-hidden', 'true');
      ime.tabIndex = -1;
      ime.autocomplete = 'off';
      ime.spellcheck = false;
      Object.assign(ime.style, {
        position: 'fixed', left: '-9999px', top: '0', width: '1px', height: '1px',
        opacity: '0', pointerEvents: 'none', resize: 'none', border: '0', padding: '0',
      });
      container.appendChild(ime);
      let composing = false;
      const onCompositionStart = () => { composing = true; };
      const onCompositionEnd = (e: CompositionEvent) => {
        composing = false;
        ime.value = '';
        if (e.data) sendEvent({ type: 'type', text: e.data });
      };
      const onImeInput = () => {
        // Non-composition inserts (emoji picker, autofill) are also text the
        // page should receive; composition commits are handled above.
        if (composing) return;
        const text = ime.value;
        ime.value = '';
        if (text) sendEvent({ type: 'type', text });
      };
      ime.addEventListener('compositionstart', onCompositionStart);
      ime.addEventListener('compositionend', onCompositionEnd);
      ime.addEventListener('input', onImeInput);

      const focusViewer = () => {
        // Drop host-side selections on the way in, or host Ctrl+C copies chrome.
        const sel = window.getSelection();
        if (sel && !sel.isCollapsed && sel.rangeCount > 0) {
          const node = sel.anchorNode;
          const el = node
            ? node.nodeType === Node.ELEMENT_NODE
              ? (node as Element)
              : node.parentElement
            : null;
          if (!el || !container.contains(el)) sel.removeAllRanges();
        }
        try {
          ime.focus({ preventScroll: true });
        } catch {
          ime.focus();
        }
      };

      const onMouseMove = (e: MouseEvent) => {
        const { x, y } = getCoords(e);
        const st = dragStartRef.current;
        if (st && (e.buttons & 1) !== 0) {
          // Text drags use select_range; every other left drag must reach the
          // page as a real button-pinned mousemove or sliders snap back.
          if (st.text || remoteCursorRef.current === 'text') {
            st.text = true;
            const now = Date.now();
            if (now - lastSelectRangeRef.current >= 100) {
              lastSelectRangeRef.current = now;
              sendEvent({ type: 'select_range', x1: st.x, y1: st.y, x2: x, y2: y });
            }
          }
        }
        sendEvent({ type: 'mousemove', x, y, buttons: e.buttons });
      };

      // Multi-click sequence: Chromium selects a word on clickCount 2 and a
      // line on 3, and only fires `dblclick` for the second press. Every press
      // used to report 1, so double/triple-click did nothing.
      const clickSeq = { at: 0, x: 0, y: 0, count: 0 };
      let pressClickCount = 1;
      const nextClickCount = (x: number, y: number): number => {
        const now = performance.now();
        const near = Math.abs(x - clickSeq.x) <= 4 && Math.abs(y - clickSeq.y) <= 4;
        clickSeq.count = now - clickSeq.at <= 400 && near ? Math.min(clickSeq.count + 1, 3) : 1;
        clickSeq.at = now;
        clickSeq.x = x;
        clickSeq.y = y;
        return clickSeq.count;
      };

      const onMouseDown = (e: MouseEvent) => {
        e.preventDefault();
        focusViewer();
        const { x, y } = getCoords(e);
        if (e.button === 0) {
          // text latches the remote cursor shape; upgradable mid-drag.
          dragStartRef.current = { x, y, text: remoteCursorRef.current === 'text' };
        }
        pressClickCount = e.button === 0 ? nextClickCount(x, y) : 1;
        sendEvent({ type: 'mousedown', button: buttonName(e), x, y, buttons: e.buttons, click_count: pressClickCount });
      };

      const onMouseUp = (e: MouseEvent) => {
        e.preventDefault();
        const { x, y } = getCoords(e);
        const st = dragStartRef.current;
        if (buttonName(e) === 'left' && st) {
          if (st.text) {
            const moved = Math.abs(st.x - x) > 3 || Math.abs(st.y - y) > 3;
            if (moved) {
              // Finalize the text selection via the DOM API.
              sendEvent({ type: 'select_range', x1: st.x, y1: st.y, x2: x, y2: y });
            }
          }
          dragStartRef.current = null;
        }
        sendEvent({ type: 'mouseup', button: buttonName(e), x, y, buttons: e.buttons, click_count: pressClickCount });
      };

      // CDP wheel deltas apply instantly; spread each gesture over ~120ms in three slices.
      const wq = { dx: 0, dy: 0, x: 0, y: 0, timer: 0 as ReturnType<typeof setInterval> | 0 };
      const onWheel = (e: WheelEvent) => {
        e.preventDefault();
        const { x, y } = getCoords(e);
        wq.dx += Math.round(e.deltaX);
        wq.dy += Math.round(e.deltaY);
        wq.x = x;
        wq.y = y;
        if (wq.timer) return;
        let chunk = 0;
        wq.timer = setInterval(() => {
          chunk++;
          const sdx = Math.round(wq.dx / (4 - chunk));
          const sdy = Math.round(wq.dy / (4 - chunk));
          wq.dx -= sdx;
          wq.dy -= sdy;
          sendEvent({ type: 'mousewheel', deltaX: sdx, deltaY: sdy, x: wq.x, y: wq.y });
          if (chunk >= 3) {
            clearInterval(wq.timer);
            wq.timer = 0;
          }
        }, 40);
      };

      const onContextMenu = (e: MouseEvent) => {
        e.preventDefault();
        e.stopPropagation();
        focusViewer();
        // Offscreen renderer has no native context menu; the client-side menu reads/pushes over CDP.
        showContextMenu(e.clientX, e.clientY);
      };

      const MODIFIER_KEYS = new Set([
        'Meta', 'Control', 'Alt', 'Shift', 'AltGraph', 'OS',
        'Symbol', 'Fn',
      ]);

      const buildChord = (e: KeyboardEvent): string | null => {
        if (e.key === 'Meta' || e.key === 'OS') return null;
        if (e.key === 'Dead') return null;

        const parts: string[] = [];
        if (e.ctrlKey || e.metaKey) parts.push('ctrl');
        if (e.shiftKey) parts.push('shift');
        if (e.altKey) parts.push('alt');
        if (!MODIFIER_KEYS.has(e.key)) {
          parts.push(e.key);
        }
        if (parts.length === 0) return null;
        return parts.join('+');
      };

      const onKeyDown = (e: KeyboardEvent) => {
        // Editable chrome takes priority over the capture-phase key handlers.
        // The hidden IME target is the one exception: its keys are forwarded
        // like a canvas key, and prevented from inserting locally.
        const t = e.target as HTMLElement | null;
        const onIme = t === ime;
        // The strip is local chrome: its chips are focusable (role=tab), and a
        // key aimed at a chip must never be relayed into the remote page.
        if (t && t.closest('#isola-tabbar')) return;
        if (t && !onIme && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) {
          return;
        }
        // During composition let the IME own the key; the commit arrives as
        // compositionend and is sent as text.
        if (e.isComposing || e.key === 'Process') return;
        if (e.repeat) return;
        // Copy/select-all follow focus (see copyChordStaysLocal).
        if (e.metaKey || e.ctrlKey) {
          const key = e.key.toLowerCase();
          if (key === 'c' || key === 'a') {
            if (copyChordStaysLocal(container, document.activeElement)) return;
          }
        }
        if (
          (e.key === 'v' || e.key === 'V') &&
          (e.metaKey || e.ctrlKey) &&
          !e.altKey
        ) {
          e.preventDefault();
          e.stopPropagation();
          void pasteToRemote();
          return;
        }
        const chord = buildChord(e);
        if (chord !== null) {
          e.preventDefault();
          sendEvent({ type: 'chord', combo: chord });
        } else if (e.key.length === 1 && !e.ctrlKey && !e.altKey && !e.metaKey) {
          e.preventDefault();
          sendEvent({ type: 'type', text: e.key });
        }
      };

      const onKeyUp = (e: KeyboardEvent) => {
        const t = e.target as HTMLElement | null;
        if (t === ime) return;
        if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) {
          return;
        }
        e.preventDefault();
      };

      container.addEventListener('mousemove', onMouseMove);
      container.addEventListener('mousedown', onMouseDown);
      container.addEventListener('wheel', onWheel, { passive: false });
      container.addEventListener('contextmenu', onContextMenu);
      // window-level only: also catches releases off-canvas; registering both sent mouseup twice.
      window.addEventListener('mouseup', onMouseUp);
      window.addEventListener('keydown', onKeyDown, true);
      window.addEventListener('keyup', onKeyUp, true);

      const prevCleanupFn = inputCleanupRef.current;
      inputCleanupRef.current = () => {
        container.removeEventListener('mousemove', onMouseMove);
        container.removeEventListener('mousedown', onMouseDown);
        container.removeEventListener('wheel', onWheel);
        if (wq.timer) clearInterval(wq.timer);
        container.removeEventListener('contextmenu', onContextMenu);
        window.removeEventListener('mouseup', onMouseUp);
        window.removeEventListener('keydown', onKeyDown, true);
        window.removeEventListener('keyup', onKeyUp, true);
        ime.removeEventListener('compositionstart', onCompositionStart);
        ime.removeEventListener('compositionend', onCompositionEnd);
        ime.removeEventListener('input', onImeInput);
        ime.remove();
        if (reconnectTimerRef.current) {
          clearTimeout(reconnectTimerRef.current);
          reconnectTimerRef.current = null;
        }
        if (inputReconnectTimer) clearTimeout(inputReconnectTimer);
        if (metricsIntervalRef.current) {
          clearInterval(metricsIntervalRef.current);
          metricsIntervalRef.current = null;
        }
        if (keyframeRequestTimerRef.current !== null) {
          clearTimeout(keyframeRequestTimerRef.current);
          keyframeRequestTimerRef.current = null;
        }
        cleanupFps();
        cleanupPlayout();
        if (prevCleanupFn) prevCleanupFn();
      };
    }
  }, [applyPersistedQuality, disconnect, flushPending, pasteToRemote, rebuildTabBar, sendControl, showContextMenu, wireVideoFps]);

  const connect = useCallback((
    streamWsUrl: string,
    inputWsUrl: string,
    container: HTMLElement,
    viewOnly: boolean,
    bandwidthKbps?: number,
    sessionId?: string,
  ) => {
    // Save params for reconnect
    connectParamsRef.current = {
      streamWsUrl, inputWsUrl, container, viewOnly, bandwidthKbps, sessionId,
    };
    sessionIdRef.current = sessionId;
    reconnectAttemptRef.current = 0;
    doConnect(streamWsUrl, inputWsUrl, container, viewOnly, bandwidthKbps, sessionId);
  }, [doConnect]);

  // enableAudio: AudioContext must be created in a user gesture; afterwards decoded frames reach the speakers.
  const enableAudioRef = useRef<() => Promise<void>>(null as any);
  const enableAudio: () => Promise<void> = useCallback(async () => {
    const DecoderCtor = wcHost.AudioDecoder;
    const ChunkCtor = wcHost.EncodedAudioChunk;
    if (!DecoderCtor || !ChunkCtor) {
      console.warn('[isola-audio] webcodecs unavailable');
      return;
    }
    audioChunkCtorRef.current = ChunkCtor;
    if (!audioCtxRef.current) {
      audioCtxRef.current = new AudioContext({ sampleRate: 48000 });
    }
    await audioCtxRef.current.resume().catch(() => {});
    const buildDecoder = () => {
      const dec = new DecoderCtor({
        output: (data) => {
          audioErrRef.current.count = 0;
          const ctx = audioCtxRef.current;
          if (!ctx || !audioOnRef.current) {
            data.close();
            return;
          }
          const frames = data.numberOfFrames;
          const ch = data.numberOfChannels;
          const sr = data.sampleRate;
          let buf: AudioBuffer;
          try {
            buf = ctx.createBuffer(ch, frames, sr);
            const fmt = data.format || 'f32-planar';
            if (fmt.endsWith('-planar')) {
              for (let p = 0; p < ch; p++) {
                const byteLen = typeof data.allocationSize === 'function'
                  ? data.allocationSize({ planeIndex: p })
                  : frames * 4;
                const byteBuf = new Uint8Array(byteLen);
                const plane = new Float32Array(
                  byteBuf.buffer,
                  byteBuf.byteOffset,
                  Math.max(frames, byteLen / 4),
                );
                data.copyTo(plane as unknown as Float32Array, { planeIndex: p });
                buf.copyToChannel(plane.subarray(0, frames), p);
              }
            } else {
              // Interleaved output (some decoder paths emit 'f32'/'u8')
              // exposes a single plane — allocationSize/copyTo throw
              // Invalid planeIndex for any planeIndex > 0. Copy plane 0
              // whole, then de-interleave into the AudioBuffer.
              const byteLen = data.allocationSize({ planeIndex: 0 });
              if (fmt === 'u8') {
                const raw = new Uint8Array(byteLen);
                data.copyTo(raw as unknown as Float32Array, { planeIndex: 0 });
                for (let c = 0; c < ch; c++) {
                  const plane = new Float32Array(frames);
                  for (let i = 0; i < frames; i++) {
                    plane[i] = (raw[i * ch + c] - 128) / 128;
                  }
                  buf.copyToChannel(plane, c);
                }
              } else {
                const byteBuf = new Uint8Array(byteLen);
                const inter = new Float32Array(
                  byteBuf.buffer,
                  byteBuf.byteOffset,
                  byteLen / 4,
                );
                data.copyTo(inter as unknown as Float32Array, { planeIndex: 0 });
                for (let c = 0; c < ch; c++) {
                  const plane = new Float32Array(frames);
                  for (let i = 0; i < frames; i++) {
                    plane[i] = inter[i * ch + c];
                  }
                  buf.copyToChannel(plane, c);
                }
              }
            }
          } catch (e) {
            console.warn('[isola-audio] output copy:', e);
            data.close();
            return;
          }
          const now = ctx.currentTime;
          if (audioNextRef.current < now + 0.005 || audioNextRef.current > now + 0.25) {
            audioNextRef.current = now + 0.04;
          }
          try {
            const src = ctx.createBufferSource();
            src.buffer = buf;
            src.connect(ctx.destination);
            src.start(audioNextRef.current);
            audioNextRef.current += frames / sr;
          } catch (e) {
            console.warn('[isola-audio] playout:', e);
          }
          data.close();
        },
        error: (err) => {
          console.error('[isola-audio] decoder:', err);
          const now = Date.now();
          const r = audioErrRef.current;
          r.count = (now - r.at < 200) ? r.count + 1 : 1;
          r.at = now;
          if (r.count >= 3) {
            r.count = 0;
            const old = audioDecoderRef.current;
            audioDecoderRef.current = null;
            try { old?.close(); } catch { /* ignore */ }
            queueMicrotask(() => {
              if (audioOnRef.current && enableAudioRef.current) {
                void enableAudioRef.current();
              }
            });
          }
        },
      });
      dec.configure({
        codec: 'opus',
        sampleRate: 48000,
        numberOfChannels: 2,
        optimizeForLatency: true,
      });
      return dec;
    };
    if (!audioDecoderRef.current) {
      audioDecoderRef.current = buildDecoder();
    }
    audioOnRef.current = true;
  }, []);
  enableAudioRef.current = enableAudio;

  const disableAudio = useCallback(() => {
    audioOnRef.current = false;
    audioNextRef.current = 0;
  }, []);

  // Mute flips both the local gate and the server-side forward gate
  // (isola-streamer /audio).
  const setAudioMuted = useCallback((muted: boolean) => {
    setAudioMutedState(muted);
    if (muted) {
      disableAudio();
    } else {
      void enableAudio();
    }
  }, [disableAudio, enableAudio]);

  const setupInputForwarding = useCallback((_container: HTMLElement, _viewOnly: boolean) => {
    // Input forwarding is handled in connect() via DOM event listeners.
  }, []);

  // recoverRenderer asks the sandbox to replace the active tab's renderer
  // process and reload it — the manual escape hatch when a heavy page has
  // wedged it (the toolbar offers it while a stall is showing). The runner
  // rate-limits its own automatic recoveries, so this exists precisely for the
  // window where the user is staring at a frozen picture and does not want to
  // wait for the cooldown.
  const recoverRenderer = useCallback(() => {
    const iws = inputWsRef.current;
    if (iws && iws.readyState === WebSocket.OPEN) {
      iws.send(JSON.stringify({ type: 'tab_recover' }));
    }
  }, []);

  return {
    connected,
    firstFrame,
    error,
    reconnecting,
    remoteClipboard,
    audioMuted,
    setAudioMuted,
    audioActive,
    browserTabs,
    enableAudio,
    disableAudio,
    metrics,
    connect,
    disconnect,
    pasteToRemote,
    setupInputForwarding,
    setQuality,
    recoverRenderer,
    setTabBarHost,
  };
}
