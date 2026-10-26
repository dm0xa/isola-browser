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
import { createWcDecoder as buildWcDecoder, setupWebCodecsVideoPath } from './isola/webCodecsVideoPath';
import { isPlaceholderUrl, seedForActiveTab, shouldCarryUrlEdit } from './isola/addressBar';
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

// Browser-chrome palette for the in-container tab strip and the page
// context menu. Both are built with imperative DOM nodes (not React), so they
// cannot use Tailwind's dark: variant and have to pick colors from the active
// theme explicitly.
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

// Background painted inside the capture element behind the decoded frame.
// object-fit:contain letterboxes the remote frame whenever the viewer's aspect
// ratio differs from the capture's (which fullscreen always changes), and that
// padding shows this fill. It must match the app background — dark:gray-950 /
// light:zinc-200, the same colors as the viewer root — or the letterbox reads as
// glaring dark bars (the old hardcoded #030712 was invisible in dark theme but
// looked like black lines in light theme).
const CAPTURE_LETTERBOX_BG = { dark: '#030712', light: '#e4e4e7' } as const;


// Input backlog (see sendEvent): events buffered while no socket is
// sendable are replayed on open if younger than the TTL.
const INPUT_BACKLOG_TTL_MS = 600;
const INPUT_BACKLOG_CAP = 32;
// Consecutive reconnect attempts for the input channel before giving up (a
// successful open resets the counter).
const INPUT_RECONNECT_ATTEMPTS = 5;
// The streamer writes a keepalive on the input channel every 20s. Three
// missing keepalives mean the socket is half-open (a close event never
// fires for those) and every tab command would silently no-op.
const INPUT_ZOMBIE_MS = 65000;

// IsolaStreamMetrics, ABR, MSE/WebCodecs decode helpers → ./isola/*

// ── Canvas insets for the network panel (bottom or side split) ──────────
// The canvas is created imperatively by the hook, so the panel layout is
// communicated through these module-level insets, applied at creation and
// adjustable live via setIsolaInsets().
let isolaInsets = { right: 0, bottom: 0 };
// Reserve 36px at the top for the isola browser chrome (tab strip, URL
// field, reload button). When the user's profile disables the strip (no
// tabs support on this session) we collapse this to 0 and render a full
// viewport capture. The value is read by applyInsets() so both video and
// canvas accounts for it automatically, and kept in sync by rebuildTabBar
// when it creates/destroys the strip element.
//
// EAGER INIT (non-viewOnly): on host (non-share) connects we set this to
// 36px immediately in doConnect, BEFORE any canvas/video element is
// created. This avoids a ~1-frame layout race where the first decoded
// frame renders at top=0 with the tab strip overlaying the capture's top
// 36px, then the first tabs broadcast fixes it late (user-visible flash).
let isolaTopOffset = 0;
function getIsolaTopOffset() { return isolaTopOffset; }
// Direct refs to the currently-mounted capture elements. Stored at creation
// time; cleared on disconnect. Avoids the fragility of global
// document.querySelector('#vnc-container canvas') selectors that silently
// return the wrong element when multiple container nodes exist in the DOM
// (e.g. RestartSession transition, share+viewer overlays) or the element
// lives outside an id'ed wrapper for legacy transports.
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
  // Replaced elements (<canvas>, <video>) do NOT honor the four-corner
  // absolute inset model when width/height are auto — CSS spec picks the
  // intrinsic capture size instead. Use explicit calc() percentages so the
  // capture stretches to fill the full viewport, then position with top/left.
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

// ── Copy/select-all routing ──────────────────────────────────────────────
// Ctrl+C / Ctrl+A on the viewer can mean two different things: copy the
// remote selection, or copy host-side UI chrome (toolbar, tab strip, network
// panel, debug overlay). Focus decides — clicking the stream container puts
// focus on it, clicking local chrome moves focus away. The live DOM selection
// must NOT decide: the container's mousedown handler preventDefaults (so the
// remote caret stays authoritative), which also leaves any previous host
// selection intact. Judging by that stale selection made Ctrl+A fall through
// to the host document — selecting the toolbar and tab strip instead of the
// remote page — and Ctrl+C then kept copying that host chrome.
export function copyChordStaysLocal(container: Node, active: Element | null): boolean {
  return !active || !(active === container || container.contains(active));
}

// mapClientToCapture maps a client-space pointer position onto capture-space
// pixels for CDP input. The capture element renders with object-fit: contain,
// so the image is scaled by a single factor and centred (letterboxed) inside
// its box: undo the centring, divide by the scale to reach frame pixels, then
// rescale frame pixels into capture pixels (ABR may serve a scaled-down
// encode while input must address the full-resolution capture).
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

// Frame-rate stream counters. The frame path runs at the decode rate (up to the
// encode ceiling, ~25/s), so these are plain data the caller keeps in a ref and
// the 1 Hz metrics tick publishes — never setMetrics per frame. Publishing them
// per frame re-rendered the whole viewer at the frame rate for two numbers only
// the opt-in debug overlay reads.
export interface FrameCounters {
  /** Latest packet sequence number seen on the wire. */
  sequenceNumber: number;
  /** Running total of IDR packets received this view. */
  keyframeCount: number;
}

export function createFrameCounters(): FrameCounters {
  return { sequenceNumber: 0, keyframeCount: 0 };
}

/** Record one streamed packet: advance the sequence number and, for a keyframe,
 *  the IDR total. */
export function noteStreamedPacket(counters: FrameCounters, seq: number, isKeyframe: boolean): void {
  counters.sequenceNumber = seq;
  if (isKeyframe) counters.keyframeCount += 1;
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
  // Latest browser tab strip state, mirrored into React so the network panel
  // can list every open tab even before (or without) captured traffic for it.
  const [browserTabs, setBrowserTabs] = useState<Array<{ id: number; url: string; title: string; active: boolean }>>([]);
  const [metrics, setMetrics] = useState<IsolaStreamMetrics>({
    connectMs: null,
    firstFrameMs: null,
    recentFps: 0,
    sequenceNumber: 0,
    keyframeCount: 0,
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
  // ── WebCodecs mode: raw Annex-B access units → VideoDecoder → canvas.
  // No MediaSource container, no re-buffering: frames render the moment
  // the decoder emits them, so glass-to-glass latency is one frame.
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
  // ── On-demand keyframe recovery ──
  // A decode error or a reset with no IDR re-arms a single recovery attempt:
  // one request_keyframe if none arrived within the timeout, then the recovery
  // duration is measured until the next IDR flips keyframeSeenRef true.
  const decodeErrorCountRef = useRef(0);
  const lastRecoveryMsRef = useRef<number | null>(null);
  const recoveryStartedAtRef = useRef<number | null>(null);
  const recoveryRequestedRef = useRef(false);
  const keyframeRequestTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Consecutive keyframe requests sent without an IDR landing. A few are
  // normal (the streamer restarts its encoder per request); too many mean the
  // restarts are failing server-side and only a fresh connection helps.
  const recoveryRequestsRef = useRef(0);
  const wcFramesRef = useRef(0);
  const wcDecodedThisSecondRef = useRef(0);
  // Frame-rate counters live in a ref, not React state; the 1 Hz metrics tick
  // publishes them via frameMetricsPatch. See FrameCounters above.
  const frameCountersRef = useRef<FrameCounters>(createFrameCounters());
  const tabbarRef = useRef<HTMLDivElement | null>(null);
  // When set, the tab strip is hosted here instead of inside the capture
  // container. The viewer uses this in fullscreen to fold the browser chrome
  // (tabs + address bar) into its own toolbar row so everything sits on one
  // line; null restores the default in-container placement.
  const tabBarHostRef = useRef<HTMLElement | null>(null);
  // URL box gets focus once, when the tab bar first appears after page open.
  const urlAutofocusRef = useRef(false);
  // Seed URL from the session config (whatever the user typed on the landing
  // page). Used when the runner hasn't emitted a real tabs broadcast yet or
  // the active tab still reports empty/about:blank during cold start.
  const seedTargetUrlRef = useRef(defaultTargetUrl || '');
  // The id of the tab the session started with. The seed above describes THAT
  // tab only — see seedForActiveTab.
  const initialTabIdRef = useRef<number | null>(null);
  // Pending user-initiated navigation URL. When the user presses Enter in the
  // URL field, the blur handler runs immediately after but the runner hasn't
  // yet broadcast the updated tabs array — without this, the blur handler
  // overwrites the URL box with the stale active.url (about:blank) and the
  // user sees a ~1s flash of about:blank before the next tabs repaint.
  // Cleared by the msg.type==='tabs' handler once the active tab URL no longer
  // matches the placeholder list (i.e. the runner confirmed the navigation).
  const pendingNavRef = useRef<string>('');
  const menuRef = useRef<HTMLDivElement | null>(null);
  const menuCleanupRef = useRef<(() => void) | null>(null);
  const wcContainerRef = useRef<HTMLElement | null>(null);
  const wcViewOnlyRef = useRef(false);
  const hoverURLRef = useRef(''); // last link under the pointer (from cursor broadcast)
  // Computed cursor shape under the remote pointer, from the streamer's cursor
  // broadcast. Used to tell a text drag ("text" = I-beam) from every other
  // left drag, so only the former takes the DOM select_range path.
  const remoteCursorRef = useRef('default');
  // Drag-selection state. `text` latches whether the drag is over selectable
  // text; the DOM-API select_range path replaces CDP mouse replay only for
  // such drags (Input.dispatchMouseEvent with buttons doesn't produce selection
  // in Electron offscreen mode). All other left drags stream raw mousemove.
  const dragStartRef = useRef<{ x: number; y: number; text: boolean } | null>(null);
  const lastSelectRangeRef = useRef(0);
  // ── Audio: Opus packets (marker 0x03) → AudioDecoder → AudioContext ──
  const audioCtxRef = useRef<AudioContext | null>(null);
  const audioDecoderRef = useRef<WCAudioDecoder | null>(null);
  const audioChunkCtorRef = useRef<WCAudioChunkCtor | null>(null);
  // decoder reset guard: 3+ consecutive decode errors within 200ms tear the
  // decoder down and build a fresh one — this recovers from corrupted Opus
  // streams after reconnects without losing future frames.
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
  const sourceBufferRef = useRef<SourceBuffer | null>(null);
  const pendingSegmentsRef = useRef<ArrayBuffer[]>([]);
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
  // Live encoder target (from server metrics) vs. session base (stream_init).
  // Congestion is measured as arrival/server-egress rather than
  // arrival/target: the encoder uses capped VBR, so an idle page legitimately
  // produces far less than the target and must not be read as congestion.
  const targetKbpsRef = useRef(0);
  const sentKbpsRef = useRef(0);
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
  // Failed (re)connect cycles for the stream WS. Lives on a ref, not a local
  // in doConnect: each reconnect spawns a fresh doConnect closure, so a local
  // counter reset every cycle and the max-attempts cap never triggered — the
  // client then hammered an unreachable/rejecting endpoint forever. Reset on
  // a successful open and on an explicit connect().
  const reconnectAttemptRef = useRef(0);

  const pasteToRemote = useCallback(async (text?: string) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      try {
        const textToPaste = text || await navigator.clipboard.readText();
        ws.send(JSON.stringify({ type: 'clipboard_sync', text: textToPaste }));
      } catch (err) {
        console.error('Failed to read clipboard', err);
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

  // Applies the persisted manual quality once the server has reported the
  // session's base bitrate (stream_init). No-op in auto mode. A set_bitrate
  // for the already-active rung is ignored server-side, so this can't loop
  // when the restart it triggers sends a fresh stream_init.
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
    if (!v || v === 'about:blank') input.value = next;
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
    // Recovery state must not leak across connections: a stale armed timer
    // would fire request_keyframe on the new socket (harmless but noisy), and
    // a stale counter could escalate a fresh connection prematurely.
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
    firstFrameRecordedRef.current = false;
    appendCountRef.current = 0;
    appendFailRef.current = 0;
    if (appendRetryTimerRef.current) {
      clearTimeout(appendRetryTimerRef.current);
      appendRetryTimerRef.current = null;
    }
    setFirstFrame(false);
    setConnected(false);
    // Clear module-level capture element refs (set during canvas/video
    // creation) so subsequent reconnects don't try to re-layout removed
    // elements, and the shared globals reset to a clean state.
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
    // Unlike sendEvent there is no stream-WS fallback here: the stream
    // channel hands unknown control types to the input dispatcher, which
    // drops them. Buffer instead so a tab command issued while the channel
    // is reconnecting is replayed on open rather than silently lost.
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
    // The strip normally lives inside the capture container as browser chrome
    // overlaid on the top of the stream. In fullscreen the viewer points
    // tabBarHostRef at a slot in its toolbar row, so the strip is re-homed
    // there and laid out inline instead (see below).
    const externalHost = tabBarHostRef.current;
    const host = externalHost ?? wcContainerRef.current;
    // Operator kill-switch from config.yaml (settings.isola_tabs) — the
    // strip is a UI feature, not part of the wire protocol.
    if (!host || wcViewOnlyRef.current || !getIsolaTabs()) return;
    // A broadcast landing while the user is mid-typing in the URL field
    // must not rebuild the bar: innerHTML='' destroys the focused input,
    // focus falls to body, and the window-capture key handlers relay the
    // remaining keystrokes into the remote page. Skip only broadcasts that
    // don't change the tab set, though: a tab open/close/switch must repaint
    // even while the field is focused, or clicking a chip's ✕ would close the
    // remote tab while leaving its chip on screen (focus never leaves the
    // field — the ✕ handler calls preventDefault).
    const prevUrl = tabbarRef.current?.querySelector<HTMLInputElement>('input');
    const urlFocused = !!prevUrl && document.activeElement === prevUrl;
    // A host change (fullscreen toggled) must always repaint so the strip is
    // re-homed, even while the URL field is focused.
    const hostChanged = !!tabbarRef.current && tabbarRef.current.parentElement !== host;
    const prevTabs = lastTabsRef.current;
    const structureChanged = prevTabs.length !== tabs.length
      || prevTabs.some((t, i) => t.id !== tabs[i].id || t.active !== tabs[i].active);
    if (urlFocused && !structureChanged && !hostChanged) return;
    // The field belongs to the ACTIVE tab. When that changes (a new tab, a tab
    // switch) whatever is in the box describes the tab we just left, so it must
    // not be carried over — otherwise opening a tab from the address bar leaves
    // the previous URL sitting there instead of the new tab's blank address.
    const activeTabChanged = (prevTabs.find((t) => t.active)?.id ?? null)
      !== (tabs.find((t) => t.active)?.id ?? null);
    if (activeTabChanged) {
      // The pending navigation also belonged to the tab we left.
      pendingNavRef.current = '';
    }
    // Carry an in-progress edit across the rebuild below (the input element is
    // replaced, taking its value and caret with it).
    const keepEdit = shouldCarryUrlEdit({ urlFocused, activeTabChanged }) && prevUrl
      ? { value: prevUrl.value, start: prevUrl.selectionStart, end: prevUrl.selectionEnd }
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
      // The bar overlays the canvas top — remote clicks on it should not
      // fall through to the page. Both mousedown AND contextmenu must be
      // stopped: without the contextmenu guard, a right-click on the URL
      // field bubbles to the container's onContextMenu → focusViewer(),
      // which blurs the input and drops the user's text selection (so Copy
      // from the native menu no longer works) while popping the remote
      // Copy/Paste overlay over the local browser chrome.
      bar.addEventListener('mousedown', (e) => e.stopPropagation());
      bar.addEventListener('contextmenu', (e) => e.stopPropagation());
    }
    // (Re)apply the layout for whichever host currently owns the strip. In
    // the capture container it is an absolutely-positioned 36px overlay with
    // its own chrome background; in the toolbar row it is a plain inline flex
    // child (no own background, so the toolbar's shows through) that takes
    // the free space between the session info and the action buttons.
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
    // Re-home the strip if its host changed since the last rebuild (e.g. the
    // viewer toggled fullscreen). appendChild moves the existing node.
    if (bar.parentElement !== host) host.appendChild(bar);
    // A strip living inside the capture container overlays the top 36px, so
    // the canvas/video inset around it. One hosted in the toolbar does not
    // overlap the stream, which then uses the full viewport.
    setIsolaTopOffset(external ? 0 : 36);
    bar.innerHTML = '';

    // Fallback URL: use the seed (from session config, the URL the user typed on
    // the landing page) for the tab the session STARTED with, while that tab
    // still reports a meaningless placeholder (browser cold start, first tabs
    // broadcast before the runner has navigated anywhere). A tab opened later is
    // blank on purpose and must not inherit it.
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
    for (const t of tabsDisplay) {
      const chip = document.createElement('div');
      chip.style.display = 'flex';
      chip.style.alignItems = 'center';
      chip.style.gap = '6px';
      chip.style.padding = '4px 8px';
      chip.style.borderRadius = '6px';
      chip.style.cursor = 'pointer';
      chip.style.maxWidth = '180px';
      chip.style.background = t.active ? pal.chipActiveBg : 'transparent';
      chip.style.color = t.active ? pal.chipActiveText : pal.barText;
      chip.style.transition = 'background 120ms ease, color 120ms ease';
      chip.title = t.url;

      const label = document.createElement('span');
      label.textContent = t.title || t.url.replace(/^https?:\/\//, '').split('/')[0] || `Tab ${t.id}`;
      label.style.overflow = 'hidden';
      label.style.textOverflow = 'ellipsis';
      label.style.whiteSpace = 'nowrap';
      chip.appendChild(label);

      const x = document.createElement('span');
      x.textContent = '✕';
      x.style.opacity = '0.6';
      x.style.padding = '0 2px';
      x.style.borderRadius = '4px';
      x.style.transition = 'opacity 120ms ease, background 120ms ease';
      x.onmouseenter = () => { x.style.opacity = '1'; x.style.background = pal.buttonBgHover; };
      x.onmouseleave = () => { x.style.opacity = '0.6'; x.style.background = 'transparent'; };
      x.onmousedown = (e) => {
        e.preventDefault();
        e.stopPropagation();
        sendCtrl({ type: 'tab_close', id: t.id });
      };
      chip.appendChild(x);

      chip.onmousedown = (e) => {
        e.preventDefault();
        e.stopPropagation();
        if (e.target !== x) sendCtrl({ type: 'tab_switch', id: t.id });
      };
      // Hover highlight only for background tabs — the active tab already
      // wears chipActiveBg and must not visually react to the pointer.
      if (!t.active) {
        chip.onmouseenter = () => { chip.style.background = pal.buttonBg; };
        chip.onmouseleave = () => { chip.style.background = 'transparent'; };
      }
      bar.appendChild(chip);
    }

    const plus = document.createElement('div');
    plus.textContent = '+';
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
    bar.appendChild(plus);

    // Back sits immediately left of reload, both left of the address bar.
    // Same chip as the plus button. mousedown (not click) so the browser's
    // own drag/selection flow isn't disturbed — the URL field handles its
    // own captures. tab_back walks the active tab's history; a tab with
    // nowhere to go is a no-op on the runner.
    const back = document.createElement('div');
    back.setAttribute('title', 'Back');
    back.setAttribute('role', 'button');
    back.innerHTML = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 12H5"/><polyline points="12 19 5 12 12 5"/></svg>`;
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
    bar.appendChild(back);

    const reload = document.createElement('div');
    reload.setAttribute('title', 'Reload page');
    reload.innerHTML = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-3-6.7"/><polyline points="21 3 21 9 15 9"/></svg>`;
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
    bar.appendChild(reload);

    // URL field for the active tab
    const url = document.createElement('input');
    url.type = 'text';
    url.placeholder = 'Search or enter address';
    url.value = active ? active.url : '';
    url.style.flex = '1';
    url.style.minWidth = '120px';
    url.style.marginLeft = '6px';
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
    // programmatic focus (autofocus after page load) must not clear
    // about:blank or select text — only explicit user focus (mousedown /
    // Tab key) clears/selects so the remote page's current URL stays in
    // view after the page already opened.
    let userFocus = false;
    const markUserFocus = () => { userFocus = true; };
    url.addEventListener('mousedown', markUserFocus);
    url.addEventListener('keydown', (e) => { if (e.key !== 'Enter') markUserFocus(); });
    url.addEventListener('focus', () => {
      if (!userFocus) return; // autofocus: leave current value alone
      if (url.value === 'about:blank') url.value = '';
      requestAnimationFrame(() => url.select());
    });
    url.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') navigate();
    });
    url.addEventListener('blur', () => {
      userFocus = false;
      // If user just pressed Enter (pendingNav set), keep the URL they
      // submitted visible instead of flashing back to the stale active.url
      // (still about:blank until runner sends next tabs broadcast).
      if (pendingNavRef.current) {
        url.value = pendingNavRef.current;
      } else {
        url.value = active ? active.url : '';
      }
    });
    bar.appendChild(url);

    // Restore an edit that was in flight when this rebuild started: put the
    // typed text and caret back into the fresh input and re-focus it, so a tab
    // open/close/switch performed from the address bar neither loses what was
    // typed nor drops focus to <body> (which would leak keystrokes remote-ward).
    if (keepEdit) {
      url.value = keepEdit.value;
      url.focus();
      try {
        const end = keepEdit.end ?? url.value.length;
        url.setSelectionRange(keepEdit.start ?? end, end);
      } catch { /* selection unsupported on this input type */ }
    } else if (urlFocused) {
      // The active tab changed but the field had focus: keep it there (and out of
      // the remote page) while it shows the NEW tab's address.
      url.focus();
    }

    // Autofocus ONCE — on the very first render of the viewer page, when
    // the active tab is still empty/about:blank (i.e. the user hasn't
    // navigated yet). After a page is loaded (real URL in the active tab)
    // we never move focus to the box — the user can still click it.
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

  // Repaint the imperative tab strip when the theme toggles. The strip is
  // built outside React's render tree, so it does not pick up Tailwind's
  // dark: variant on its own — we replay the last tabs broadcast to restyle it.
  useEffect(() => {
    themeRef.current = theme;
    // Repaint the letterbox fill behind the capture so a theme toggle is
    // reflected live (it matches the app background and must follow it).
    const letterbox = CAPTURE_LETTERBOX_BG[theme];
    if (videoRef.current) videoRef.current.style.backgroundColor = letterbox;
    if (canvasRef.current) canvasRef.current.style.backgroundColor = letterbox;
    if (lastTabsRef.current.length > 0) rebuildTabBar(lastTabsRef.current);
  }, [theme, rebuildTabBar]);

  // Re-home the tab strip. The viewer passes its toolbar slot while
  // fullscreen (so the session id, tabs, address bar and action buttons share
  // one line) and null otherwise, which returns the strip to its usual
  // in-container chrome position. Repaints immediately so the move is instant
  // instead of waiting for the next tabs broadcast.
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
    // Reset per-connection ABR state. The server starts a fresh per-viewer
    // encoder at the session base on every connection, so the controller
    // restarts at L0; a manual override persists and is re-applied from
    // localStorage once stream_init reports base_bitrate.
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
    // Eagerly reserve 36px of vertical space for the isola browser chrome
    // (tab strip, URL field, back and reload buttons) on HOST sessions (viewOnly =
    // share viewer has no in-container tab strip so it uses full viewport).
    // Without this, the FIRST decoded frame renders at top=0 and the tab
    // strip overlays on top of the capture's top 36px until the first tabs
    // WS broadcast arrives — which is exactly what user reports as "tabs
    // are hovering the website". We set this BEFORE any canvas/video
    // element is created so applyInsets() at creation time already uses
    // top=36 (no visible layout flash). Collapses back to 0 on disconnect,
    // and stays 0 when the strip is hosted in the toolbar (fullscreen).
    if (!viewOnly && getIsolaTabs() && !tabBarHostRef.current) {
      isolaTopOffset = 36;
    } else {
      isolaTopOffset = 0;
    }
    isolaInsets = { right: 0, bottom: 0 };
    setError(null);
    // The first connection is not a reconnect: showing "Reconnecting" while a
    // fresh session is still being established made the launch look like it
    // had already failed once. Only retries after a dropped/failed attempt
    // (reconnectAttemptRef > 0) count as reconnecting.
    setReconnecting(reconnectAttemptRef.current > 0);
    connectStartedAtRef.current = performance.now();
    firstFrameRecordedRef.current = false;
    appendCountRef.current = 0;
    bytesReceivedRef.current = 0;
    segmentsThisSecondRef.current = 0;
    lastSegmentAtRef.current = 0;
    lastBitrateWindowRef.current = { at: performance.now(), bytes: 0 };

    // Stream WS: capability negotiation rides the handshake — a
    // WebCodecs-capable client asks for raw Annex-B via mode=webcodecs
    // (the server's fMP4/MSE path serves everyone else). First-message
    // negotiation raced slow links and poisoned legacy WS connections.
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
          // The sandbox had to replace a wedged page renderer (or recovered
          // from one). Surface it: this turns an unexplained frozen frame into
          // an explained, self-healing one.
          const stalled = msg.stalled === true;
          setMetrics((m) => ({
            ...m,
            rendererStalled: stalled,
            rendererStalledMs: stalled && typeof msg.ms === 'number' ? msg.ms : 0,
            rendererRecoveries: typeof msg.recoveries === 'number' ? msg.recoveries : m.rendererRecoveries,
          }));
        } else if (msg.type === 'clipboard_sync' && typeof msg.text === 'string' && msg.text) {
          // Auto-sync: the sandbox clipboard changed (Ctrl+C in the remote
          // browser) — mirror it onto the host clipboard.
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
          // Network monitor entry (metadata only). Dispatch as a DOM event
          // so the panel can live in React without the hook owning it.
          window.dispatchEvent(new CustomEvent('isola-net', { detail: msg.e }));
        } else if (msg.type === 'headers' && msg.e && typeof msg.e === 'object') {
          // Document headers + posture/TLS, computed by the streamer. Same
          // dispatch pattern as net/console so the panel stays React-owned.
          window.dispatchEvent(new CustomEvent('isola-headers', { detail: msg.e }));
        } else if (msg.type === 'console' && typeof msg.l === 'string' && typeof msg.t === 'string') {
          window.dispatchEvent(new CustomEvent('isola-console', { detail: { i: typeof msg.i === 'number' ? msg.i : undefined, level: msg.l, text: msg.t, tb: msg.tb, ts: typeof msg.ts === 'number' ? msg.ts : undefined } }));
        } else if (msg.type === 'cookie' && typeof msg.n === 'string') {
          window.dispatchEvent(new CustomEvent('isola-cookie', { detail: msg }));
        } else if (msg.type === 'cursor' && typeof msg.c === 'string') {
          // Cursor-shape feedback: the offscreen capture has no cursor, so
          // the streamer reports the page's computed cursor under the
          // pointer and we shape the native CSS cursor accordingly. Keep the
          // shape too — the mouse handlers use it to tell a text drag apart.
          remoteCursorRef.current = msg.c;
          const canvas = canvasRef.current;
          if (canvas) canvas.style.cursor = msg.c;
          hoverURLRef.current = typeof msg.u === 'string' ? msg.u : '';
        } else if (msg.type === 'net_init' && Array.isArray(msg.entries)) {
          // Replay of everything captured before this viewer attached.
          window.dispatchEvent(new CustomEvent('isola-net-init', { detail: msg.entries }));
        } else if (msg.type === 'net_clear') {
          // Reserved: the streamer does not currently wipe buffers on tab
          // change — entries carry a per-tab tag (tb) so the panel can
          // filter them instead.
          window.dispatchEvent(new CustomEvent('isola-net-clear'));
        }
      };
      inputWs.onerror = () => {
        console.warn('[isola] input WS error, falling back to inline events');
      };
      // Reconnect. The relay closes this channel when the container has been
      // silent for 90s, and a network blip closes it too. Without a retry the
      // ref is left holding a closed socket for the rest of the session, and
      // because sendCtrl has no stream-WS fallback every later tab command
      // (new tab, tab switch, "open link in new tab") becomes a silent no-op.
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
    // Tab-bar top-offset (36px when enabled) is already applied here — no
    // width/height 100% because absolute inset layout sizes the capture
    // perfectly without clipping the bottom or hiding behind the tab strip
    // when the network panel toggles.
    applyInsets(video);
    // contain, not cover: the remote viewport's aspect ratio (the requested
    // launch resolution, e.g. 3456x2234) rarely matches the viewer's window,
    // and cover cropped the overflow — which hid the top of every page right
    // under the address/tab bar. contain letterboxes instead, so the whole
    // remote page is always visible and clickable.
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
    video.src = URL.createObjectURL(mediaSource);

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
          console.error('[isola] SourceBuffer error', {
            codec,
            updating: sb.updating,
            buffered: sb.buffered.length,
            mediaError: video.error?.code,
            mediaMessage: video.error?.message,
          });
          sourceBufferRef.current = null;
        });
        sourceBufferRef.current = sb;
        flushPending();
      } catch (e) {
        console.error('[isola] failed to add SourceBuffer', e);
        setError('MSE not supported or codec unavailable');
        setReconnecting(false);
      }
    };

    mediaSource.addEventListener('sourceopen', () => {
      if (gotStreamInit) addSourceBuffer(mediaSource);
    });

    function appendSegment(seg: ArrayBuffer) {
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
      // If repeated keyframe requests still yield no IDR, the streamer's
      // encoder restarts are failing — a fresh connection is the only way
      // out, and the onclose handler already owns that path.
      if (recoveryRequestsRef.current >= MAX_KEYFRAME_REQUESTS) {
        console.error('[isola] no IDR after repeated keyframe requests — reconnecting');
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

    // beginRecovery arms the keyframe-recovery watchdog. It re-arms itself on
    // every tick while no IDR has landed: a lost request_keyframe or a failed
    // encoder restart must retry, not wedge the stream forever (a closed
    // decoder plus a spent one-shot request used to freeze the picture
    // permanently). recoveryStartedAtRef records when the streak began for
    // the lastRecoveryMs diagnostic; it clears when an IDR arrives.
    //
    // reschedule starts the wait over. stream_reset must use it: the timer
    // armed when the request was sent has already burned part of its window
    // on the encoder that was just torn down, and firing it immediately
    // kills the replacement before that one can emit an IDR.
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
    const wcPathCallbacks = {
      setFirstFrame,
      setMetrics,
      beginRecovery,
      requestKeyframeNow,
      applyInsets,
      letterboxBg: CAPTURE_LETTERBOX_BG[themeRef.current],
      onVideoRemoved: () => { currentCaptureVideo = null; },
      onCanvasMounted: (canvas: HTMLCanvasElement) => { currentCaptureCanvas = canvas; },
    };

    function createWcDecoder(): WCDecoder | null {
      return buildWcDecoder(wcPathRefs, wcPathCallbacks);
    }

    function setupWebCodecs(msg: { codec?: string; width?: number; height?: number }): boolean {
      return setupWebCodecsVideoPath(msg, container, wcPathRefs, wcPathCallbacks);
    }

    metricsIntervalRef.current = setInterval(() => {
      const now = performance.now();
      // ── Input-WS liveness: the streamer keepalives the input channel every
      // 20s, and the relay kills silent channels after 90s. A half-open
      // socket never fires onclose, so without this check every tab command
      // (switch/new/close, URL bar) silently no-ops for the rest of the
      // session — the picture freezes on the old tab and looks like a
      // rendering bug. Replace the zombie and let the reconnect cycle run.
      const liveInput = inputWsRef.current;
      if (
        liveInput && liveInput.readyState === WebSocket.OPEN &&
        inputLastMsgAtRef.current > 0 &&
        Date.now() - inputLastMsgAtRef.current > INPUT_ZOMBIE_MS
      ) {
        console.warn('[isola] input WS unresponsive — forcing reconnect');
        inputLastMsgAtRef.current = Date.now(); // once per window
        inputAttempt = 0; // the attempts cap must not block the replacement
        liveInput.close();
      }
      // ── RTT probe every 2s, on the video channel itself (that's the one
      // that degrades). App-level JSON ping — the JS WebSocket API can't
      // send protocol-level ping frames. One outstanding probe at a time.
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
        setMetrics((m) => ({
          ...m,
          recentFps: fps,
          videoWidth: canvas ? canvas.width : m.videoWidth,
          videoHeight: canvas ? canvas.height : m.videoHeight,
        }));
      }
      const lastSegmentAgeMs = lastSegmentAtRef.current > 0
        ? Math.round(now - lastSegmentAtRef.current)
        : -1;

      // ── Network quality estimation + ABR controller (1 Hz, ref state
      // only so it never re-renders). While paused (just after a stream
      // reset) the stream is re-booting — don't read that as degradation.
      const abr = abrRef.current;
      const abrResult = abrTick(abr, {
        nowMs: Date.now(),
        sentKbps: sentKbpsRef.current,
        arrivalKbps: bitrateKbps,
        targetKbps: targetKbpsRef.current,
        rttMs: rttEmaRef.current,
        baseKbps: baseKbpsRef.current,
        ratioEma: ratioEmaRef.current,
      });
      ratioEmaRef.current = abrResult.ratioEma;
      const netQuality = abrResult.netQuality;
      const netRatio = abrResult.netRatio;
      // libx264 cannot re-target live, so a step is an encoder restart; the
      // client recovers from the stream_reset + stream_init that follows.
      if (abrResult.stepToKbps !== null) {
        sendControl({ type: 'set_bitrate', kbps: abrResult.stepToKbps });
      }

      setMetrics((m) => ({
        ...m,
        // Frame-rate counters and decode-recovery state are published here, at
        // 1 Hz, instead of on every frame/segment.
        ...frameMetricsPatch(frameCountersRef.current),
        // MSE playout state (ref) — skipped in WebCodecs mode, where the canvas
        // block above publishes the decoded geometry instead.
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
      reconnectAttemptRef.current = 0;
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
        const msg = JSON.parse(ev.data);
        switch (msg.type) {
          case 'stream_init':
            if (wcModeRef.current && msg.mode === 'webcodecs') {
              // Rebuild canvas+decoder only when geometry actually changed:
              // an ABR bitrate restart keeps codec/dimensions identical, and
              // a rebuild would blank the canvas until the next frame.
              // stream_reset already reset the decoder — it just waits for
              // the fresh IDR that the new encoder emits first.
              const cv = canvasRef.current;
              const sameGeometry = !!cv
                && typeof msg.width === 'number' && cv.width === msg.width
                && typeof msg.height === 'number' && cv.height === msg.height;
              if (!sameGeometry && !setupWebCodecs(msg)) {
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
            // The server answered with MSE rather than the raw Annex-B
            // (webcodecs) mode this client asked for — a mismatched/downgraded
            // handshake. Keep wcModeRef honest so getCoords and the metrics read
            // the <video> element that actually exists instead of a canvas that
            // never got created.
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
            setRemoteClipboard(msg.text || '');
            if (msg.text) {
              navigator.clipboard.writeText(msg.text).catch(() => {});
            }
            break;
          case 'stream_reset':
            console.log('[isola] stream reset:', msg.reason);
            {
              // The restart itself isn't degradation: pause the estimator
              // for a beat so the encoder spin-up gap isn't read as a stall.
              const a = abrRef.current;
              a.pausedUntil = Date.now() + ABR_RESTART_PAUSE_MS;
              a.badSecs = 0;
              a.goodSecs = 0;
            }
            if (wcModeRef.current) {
              // Encoder restarted (resize/ABR): drop decoder state, the
              // next IDR re-initializes it. ts restarts with the new stream.
              try { decoderRef.current?.reset(); } catch { /* ignore */ }
              keyframeSeenRef.current = false;
              tsUsRef.current = 0;
              beginRecovery(true); // full window for the replacement encoder's IDR
              if (msg.reason !== 'bitrate_change' && msg.reason !== 'keyframe_request') {
                // ABR restarts keep the same canvas — don't flash the
                // loading overlay for a sub-second quality step. A forced
                // keyframe is just as quick, so it must not flash either.
                firstFrameRecordedRef.current = false;
                setFirstFrame(false);
              }
              break;
            }
            // Encoder was restarted (e.g. after resize). MSE needs a new
            // init segment. Re-create MediaSource.
            if (mediaSourceRef.current && mediaSourceRef.current.readyState === 'open') {
              try { mediaSourceRef.current.endOfStream(); } catch { /* ignore */ }
            }
            sourceBufferRef.current = null;
            pendingSegmentsRef.current = [];
            firstFrameRecordedRef.current = false;
    appendCountRef.current = 0;
            setFirstFrame(false);
            // Re-create MediaSource so the next segment (which includes
            // a new init segment) gets parsed correctly.
            if (videoRef.current) {
              const ms = new MediaSource();
              mediaSourceRef.current = ms;
              videoRef.current.src = URL.createObjectURL(ms);
              ms.addEventListener('sourceopen', () => {
                addSourceBuffer(ms);
              });
            }
            break;
          case 'pong': {
            // RTT probe reply: EMA-smooth the sample (refs — no re-render
            // here; the metrics interval publishes it).
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
              inputBackend: msg.input_backend || m.inputBackend,
              inputTarget: typeof msg.input_target === 'string' ? msg.input_target : m.inputTarget,
              inputTargetOK: msg.input_target_ok !== false,
              activityMode: msg.activity_mode || m.activityMode,
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
            }));
            if (typeof msg.bitrate_kbps === 'number') {
              targetKbpsRef.current = msg.bitrate_kbps;
            }
            if (typeof msg.sent_kbps === 'number') {
              sentKbpsRef.current = msg.sent_kbps;
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
        // Access unit: H.264 deltas depend on their reference chain, so
        // decode everything in order — the decoder is far faster than the
        // wire at these bitrates. Timestamps are synthetic CFR ticks.
        let decoder = decoderRef.current;
        const ChunkCtor = wcChunkCtorRef.current;
        if (!ChunkCtor) return;
        if (decoder && decoder.state === 'closed') {
          // A fatal decode error closes the VideoDecoder, and nothing else
          // rebuilt it — every packet was silently dropped from then on and
          // the picture froze for good. Re-create it from the last negotiated
          // config; deltas are ignored below until the next IDR re-syncs.
          console.warn('[isola-wc] decoder closed — rebuilding');
          decoder = createWcDecoder();
        }
        if (!decoder || decoder.state !== 'configured') return;
        if (!keyframeSeenRef.current) {
          if (!isKeyframe) return; // join mid-GOP: wait for an IDR
          keyframeSeenRef.current = true;
          tsUsRef.current = 0;
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
        try {
          decoder.decode(new ChunkCtor({
            type: isKeyframe ? 'key' : 'delta',
            timestamp: tsUsRef.current,
            data: bytes.subarray(10),
          }));
          tsUsRef.current += frameStepUsRef.current;
          // Ref only — a per-frame setMetrics re-rendered the whole viewer at
          // the decode rate. The 1 Hz metrics tick publishes these.
          noteStreamedPacket(frameCountersRef.current, seq, isKeyframe);
        } catch (e) {
          console.error('[isola-wc] decode error:', e);
          decodeErrorCountRef.current++;
          keyframeSeenRef.current = false; // out of sync until the next IDR
          beginRecovery();
          requestKeyframeNow();
          setMetrics((m) => ({ ...m, decodeErrorCount: decodeErrorCountRef.current }));
        }
        return;
      }

      if (bytes[0] === 0x01) {
        segmentsThisSecondRef.current++;
        appendSegment(buf.slice(10));
        // Ref only — published by the 1 Hz metrics tick, not per segment.
        noteStreamedPacket(frameCountersRef.current, seq, isKeyframe);
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
          console.error('[isola-audio] decode error:', e);
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
        // If the session is gone (deleted, expired, token rejected by a
        // backend state reset, or revoked), retrying only hammers a
        // 401/403/404 forever — stop and tell the user.
        const sid = connectParamsRef.current?.sessionId;
        if (sid) {
          try {
            const token = getSessionToken(sid);
            const res = await fetch(`/api/sessions/${sid}`, {
              headers: token ? { 'X-Session-Token': token } : undefined,
            });
            if (res.status === 404 || res.status === 401 || res.status === 403) {
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
        // Always measure the actual capture element's bounding box (video for
        // MSE, canvas for WebCodecs) — both are absolutely positioned, so the
        // rect already includes the tab strip top offset, the network side/bottom
        // panel offsets, etc. Using the container rect used to produce a 36px
        // offset in video mode because the video no longer fills the full
        // container height (it starts 36px lower to avoid the tab strip).
        //
        // Prefer the mode's element but fall back to whichever element exists:
        // the server can answer a `mode=webcodecs` handshake with an MSE stream
        // (and a decoder can fail to configure), and reading canvas.width off a
        // null canvas collapsed every click to (0,0) — dead mouse input while
        // keyboard kept working.
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

      // macOS delivers the secondary click (Control-click, used by
      // one-button mice and trackpads) as a LEFT press carrying ctrlKey
      // instead of button=2. Without this, such a "right click" was
      // forwarded as a plain left click and the remote page navigated.
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
        // Neither socket is sendable. Buffer briefly instead of dropping:
        // a click whose press and release span a reconnect must land as a
        // pair. mousemove is not worth replaying.
        if (iws && iws.readyState === WebSocket.CONNECTING && event.type !== 'mousemove') {
          const q = inputBacklogRef.current;
          q.push({ ev: event, at: Date.now() });
          while (q.length > INPUT_BACKLOG_CAP) q.shift();
        }
      };

      const focusViewer = () => {
        // Drop any host-side text selection (toolbar, tab strip, network
        // panel) on the way in. The mousedown handlers call preventDefault, so
        // the browser never collapses the old selection itself, and leaving it
        // live means a host Ctrl+C copies UI chrome instead of the remote
        // selection the user just made.
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
          container.focus({ preventScroll: true });
        } catch {
          container.focus();
        }
      };

      const onMouseMove = (e: MouseEvent) => {
        const { x, y } = getCoords(e);
        const st = dragStartRef.current;
        if (st && (e.buttons & 1) !== 0) {
          // Left-button drag. The DOM select_range path exists only for TEXT
          // drags (CDP button-pinned moves are unreliable for selection in
          // Electron's offscreen compositor). Every other left drag — video
          // seek/volume sliders, canvas apps, maps, drag handles, scrollbar
          // thumbs — must reach the page as a real button-pinned mousemove:
          // swallowing it left the page with mousedown and mouseup at
          // different points and no motion between them, so a slider snapped
          // back to wherever it was first pressed instead of tracking.
          // The streamer broadcasts the computed cursor under the pointer, so
          // "is this a text cursor" is known without guessing coordinates.
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

      const onMouseDown = (e: MouseEvent) => {
        e.preventDefault();
        focusViewer();
        const { x, y } = getCoords(e);
        if (e.button === 0) {
          // Arm the drag path. `text` is latched from the remote cursor shape
          // and may also be upgraded mid-drag (see onMouseMove) once the
          // cursor probe confirms the pointer sits on selectable text.
          dragStartRef.current = { x, y, text: remoteCursorRef.current === 'text' };
        }
        sendEvent({ type: 'mousedown', button: buttonName(e), x, y, buttons: e.buttons });
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
        sendEvent({ type: 'mouseup', button: buttonName(e), x, y, buttons: e.buttons });
      };

      // Wheel chunking: CDP wheel deltas apply instantly (no native smooth
      // scrolling offscreen), so spread each gesture over ~120ms in three
      // slices — same total delta, visibly smoother motion.
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
        // Electron's offscreen renderer can't show its native context menu
        // (it never renders into captured frames), so right-click opens a
        // client-side menu: Copy reads the remote selection over CDP,
        // Paste pushes the host clipboard in.
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
        // The tab bar's URL field and other editable chrome take priority:
        // capture-phase handlers would otherwise steal every keystroke
        // before the field receives it.
        const t = e.target as HTMLElement | null;
        if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) {
          return;
        }
        if (e.repeat) return;
        // Copy/select-all belong to the remote browser when the viewer has
        // focus, and to the host UI otherwise (see copyChordStaysLocal).
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
        if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) {
          return;
        }
        e.preventDefault();
      };

      container.addEventListener('mousemove', onMouseMove);
      container.addEventListener('mousedown', onMouseDown);
      container.addEventListener('wheel', onWheel, { passive: false });
      container.addEventListener('contextmenu', onContextMenu);
      // mouseup on window only: it catches releases inside the container
      // (bubbling) and outside it (dragging off-canvas). Registering both
      // sent every mouseup to the remote twice.
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

  // enableAudio wires the Opus path: AudioContext (must be created in a
  // user-gesture handler — the mute button), AudioDecoder, and the buffer
  // pump. audioMuted=false afterwards routes decoded frames to the speakers.
  const enableAudioRef = useRef<() => Promise<void>>(null as any);
  const enableAudio: () => Promise<void> = useCallback(async () => {
    const DecoderCtor = wcHost.AudioDecoder;
    const ChunkCtor = wcHost.EncodedAudioChunk;
    if (!DecoderCtor || !ChunkCtor) {
      console.warn('[isola-audio] WebCodecs audio unavailable in this browser');
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
          let buf: AudioBuffer | null = null;
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
            console.warn('[isola-audio] output copy failed:', e);
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
            console.warn('[isola-audio] playout failed:', e);
          }
          data.close();
        },
        error: (err) => {
          console.error('[isola-audio] decoder error:', err);
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
