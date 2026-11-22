// Debug info collection for the viewer.
//
// One source of truth for everything a "Copy debug info" report needs: the
// browser and machine running the viewer, the session being watched, and the
// full stream metrics. The overlay text and the `window.__isolaViewerDebug`
// hook are both rendered from this, so the two can never drift apart.
//
// Everything here is pure (aside from the optional clipboard/`navigator`
// reads) so it can be unit-tested without mounting the viewer.

import type { IsolaStreamMetrics, IsolaQuality } from '../../hooks/useIsolaStream';
import { copyToClipboard } from '../../lib/clipboard';

/** Session facts the viewer knows but the stream metrics do not carry. */
export type DebugSessionInfo = {
  id: string;
  idShort: string;
  targetUrl: string;
  resolution: string;
  frameRate: number;
  browserType: string;
  /** Page-visible identity; empty = the image default (UTC / en-US). */
  timezone: string;
  locale: string;
  torEnabled: boolean;
  torCountry: string;
  status: string;
  viewOnly: boolean;
  shadow: boolean;
  viewerCount: number;
  remainingSec: number;
  isUnlimited: boolean;
  quality: IsolaQuality;
  networkCaptureEnabled: boolean;
  browserTabCount: number;
  audioActive: boolean;
  audioMuted: boolean;
  restarting: boolean;
  /** How many times the transport has reset/reconnected this view. */
  reconnects: number;
};

/** Which build is running — the first thing support needs to know. */
export type DebugAppInfo = {
  version: string;
  commit: string;
};

/** The viewer's own machine/browser, which the backend never sees. */
export type DebugRuntimeInfo = {
  href: string;
  userAgent: string;
  platform: string;
  language: string;
  timezone: string;
  cores: number | null;
  memoryGb: number | null;
  devicePixelRatio: number;
  screen: string;
  viewport: string;
  visibility: string;
  theme: string;
  connection: string;
  online: boolean;
  mobile: boolean;
  secureContext: boolean;
  webCodecs: boolean;
  mediaSource: boolean;
  readyState: string;
  /** Seconds since the viewer page loaded. */
  uptimeSec: number;
};

/** Everything the debug button collects, in structured form. */
export type ViewerDebugInfo = {
  /** Epoch ms — kept for backwards compatibility with external tooling. */
  ts: number;
  generatedAt: string;
  // Existing flat fields (kept as-is; external consumers may read them).
  status: string;
  transport: string;
  isConnected: boolean;
  isFirstFrame: boolean;
  streamError: string | null;
  isReconnecting: boolean;
  showOverlay: boolean;
  blankCover: boolean;
  showError: boolean;
  clipboardSynced: boolean;
  loadMs: number | null;
  codec: string;
  /** The raw metrics blob (kept under its historical `isola` key). */
  isola: IsolaStreamMetrics;
  // Enrichments.
  latencyText: string;
  fpsText: string;
  errorText: string;
  sessionError: string;
  session: DebugSessionInfo;
  runtime: DebugRuntimeInfo;
  app: DebugAppInfo;
};

export type ViewerDebugInput = {
  transport: string;
  sessionId?: string;
  targetUrl?: string;
  resolution?: string;
  frameRate?: number;
  browserType?: string;
  timezone?: string;
  locale?: string;
  torEnabled?: boolean;
  torCountry?: string;
  status: string;
  viewOnly: boolean;
  shadow: boolean;
  viewerCount: number;
  remainingSec: number;
  isUnlimited: boolean;
  quality: IsolaQuality;
  loadMs: number | null;
  networkCaptureEnabled: boolean;
  browserTabCount: number;
  isConnected: boolean;
  isFirstFrame: boolean;
  streamError: string | null;
  isReconnecting: boolean;
  clipboardSynced: boolean;
  showOverlay: boolean;
  blankCover: boolean;
  showError: boolean;
  latencyText: string;
  fpsText: string;
  metrics: IsolaStreamMetrics;
  version?: string;
  commit?: string;
  errorText?: string;
  sessionError?: string;
  audioActive?: boolean;
  audioMuted?: boolean;
  restarting?: boolean;
  reconnects?: number;
  now?: number;
};

type NavigatorWithExtras = Navigator & {
  connection?: { effectiveType?: string; downlink?: number; rtt?: number; saveData?: boolean };
  deviceMemory?: number;
};

/** Query/fragment parameter names that carry credentials and must never be
 *  copied into a debug report — the report is designed to be pasted/shared. */
const SENSITIVE_PARAM = /(token|secret|passwd|password|apikey|api_key|auth)/i;

/** The viewer's own URL with credential-bearing query and fragment parameters
 *  removed, so "Copy debug info" can never leak a session or admin token. */
function safeHref(win: Window | undefined): string {
  if (!win?.location) return '';
  try {
    const url = new URL(win.location.href);
    for (const key of Array.from(url.searchParams.keys())) {
      if (SENSITIVE_PARAM.test(key)) url.searchParams.delete(key);
    }
    const hash = url.hash.replace(/^#/, '');
    if (hash) {
      const params = new URLSearchParams(hash);
      let changed = false;
      for (const key of Array.from(params.keys())) {
        if (SENSITIVE_PARAM.test(key)) {
          params.delete(key);
          changed = true;
        }
      }
      if (changed) {
        const rest = params.toString();
        url.hash = rest ? `#${rest}` : '';
      }
    }
    return url.toString();
  } catch {
    return '';
  }
}

/** Reads the viewer's browser/machine facts, degrading gracefully when a browser
 *  API is unavailable (SSR, insecure origin, older engine). */
export function collectRuntimeInfo(): DebugRuntimeInfo {
  const win = typeof window !== 'undefined' ? window : undefined;
  const nav = (typeof navigator !== 'undefined' ? navigator : undefined) as
    | NavigatorWithExtras
    | undefined;
  const conn = nav?.connection;

  const netParts: string[] = [];
  if (conn?.effectiveType) netParts.push(conn.effectiveType);
  if (typeof conn?.downlink === 'number') netParts.push(`${conn.downlink} Mbps`);
  if (typeof conn?.rtt === 'number') netParts.push(`rtt ${conn.rtt} ms`);
  if (conn?.saveData) netParts.push('saveData');

  const screenStr =
    win && win.screen
      ? `${win.screen.width}x${win.screen.height}`
      : '?';
  const viewport =
    win ? `${win.innerWidth}x${win.innerHeight}` : '?';

  let theme = '?';
  try {
    if (win && typeof win.matchMedia === 'function') {
      theme = win.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
    }
  } catch {
    theme = '?';
  }

  // Capability probes. Some are undefined on a secure-context-only or older
  // engine, so guard each by name rather than reading it directly.
  const mobile =
    (nav as (Navigator & { userAgentData?: { mobile?: boolean } }) | undefined)?.userAgentData?.mobile
    ?? /Mobi|Android|iPhone|iPad/i.test(nav?.userAgent ?? '');
  let uptimeSec = 0;
  try {
    if (win && typeof win.performance?.now === 'function') {
      uptimeSec = Math.round(win.performance.now() / 1000);
    }
  } catch {
    uptimeSec = 0;
  }

  return {
    href: safeHref(win),
    userAgent: nav?.userAgent ?? '',
    platform: nav?.platform ?? '',
    language: nav?.language ?? '',
    timezone: safeTimezone(),
    cores: typeof nav?.hardwareConcurrency === 'number' ? nav.hardwareConcurrency : null,
    memoryGb: typeof nav?.deviceMemory === 'number' ? nav.deviceMemory : null,
    devicePixelRatio: win?.devicePixelRatio ?? 1,
    screen: screenStr,
    viewport,
    visibility: (typeof document !== 'undefined' && document.visibilityState) || 'unknown',
    theme,
    connection: netParts.join(' · '),
    online: nav?.onLine ?? true,
    mobile,
    secureContext: !!win?.isSecureContext,
    webCodecs: typeof VideoDecoder !== 'undefined',
    mediaSource: typeof MediaSource !== 'undefined',
    readyState: (typeof document !== 'undefined' && document.readyState) || 'unknown',
    uptimeSec,
  };
}

function safeTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || '';
  } catch {
    return '';
  }
}

/** Assembles the structured debug report from the viewer's current state. */
export function buildDebugInfo(input: ViewerDebugInput): ViewerDebugInfo {
  const now = input.now ?? Date.now();
  const session: DebugSessionInfo = {
    id: input.sessionId ?? '',
    idShort: input.sessionId ? input.sessionId.slice(0, 8) : '',
    targetUrl: input.targetUrl ?? '',
    resolution: input.resolution ?? '',
    frameRate: input.frameRate ?? 0,
    browserType: input.browserType ?? '',
    timezone: input.timezone ?? '',
    locale: input.locale ?? '',
    torEnabled: input.torEnabled ?? false,
    torCountry: input.torCountry ?? '',
    status: input.status,
    viewOnly: input.viewOnly,
    shadow: input.shadow,
    viewerCount: input.viewerCount,
    remainingSec: input.remainingSec,
    isUnlimited: input.isUnlimited,
    quality: input.quality,
    networkCaptureEnabled: input.networkCaptureEnabled,
    browserTabCount: input.browserTabCount,
    audioActive: input.audioActive ?? false,
    audioMuted: input.audioMuted ?? false,
    restarting: input.restarting ?? false,
    reconnects: input.reconnects ?? 0,
  };

  return {
    ts: now,
    generatedAt: new Date(now).toISOString(),
    status: input.status,
    transport: input.transport,
    isConnected: input.isConnected,
    isFirstFrame: input.isFirstFrame,
    streamError: input.streamError,
    isReconnecting: input.isReconnecting,
    showOverlay: input.showOverlay,
    blankCover: input.blankCover,
    showError: input.showError,
    clipboardSynced: input.clipboardSynced,
    loadMs: input.loadMs,
    codec: input.metrics.codec,
    isola: input.metrics,
    latencyText: input.latencyText,
    fpsText: input.fpsText,
    errorText: input.errorText ?? '',
    sessionError: input.sessionError ?? '',
    session,
    runtime: collectRuntimeInfo(),
    app: {
      version: input.version || 'dev',
      commit: input.commit || 'unknown',
    },
  };
}

const yesNo = (b: boolean) => (b ? 'yes' : 'no');

function remainingText(sec: number, unlimited: boolean): string {
  if (unlimited) return 'unlimited';
  if (!Number.isFinite(sec) || sec <= 0) return '0:00';
  const m = Math.floor(sec / 60);
  const s = Math.floor(sec % 60);
  return `${m}:${String(s).padStart(2, '0')}`;
}

/** Renders the report as the human-readable text the debug button copies,
 *  ending with the full JSON so the paste works for a person and a parser. */
export function formatDebugInfo(info: ViewerDebugInfo): string {
  const m = info.isola;
  const s = info.session;
  const r = info.runtime;
  const wsState = m.wsState === WebSocket.OPEN ? 'open' : String(m.wsState);

  const lines: string[] = [
    `isola viewer debug · ${info.generatedAt}`,
    'app',
    `  version       ${info.app.version} (${info.app.commit})`,
    'runtime',
    `  href          ${r.href || '?'}`,
    `  ua            ${r.userAgent || '?'}`,
    `  platform      ${[r.platform, r.language && `lang ${r.language}`, r.timezone && `tz ${r.timezone}`]
      .filter(Boolean)
      .join(' · ') || '?'}`,
    `  screen        ${r.screen} @${r.devicePixelRatio}x · viewport ${r.viewport}`,
    `  cores         ${r.cores ?? '?'} · mem ${r.memoryGb !== null ? `${r.memoryGb} GB` : '?'} · uptime ${r.uptimeSec}s`,
    `  network       ${r.connection || 'n/a'} · online ${yesNo(r.online)} · mobile ${yesNo(r.mobile)}`,
    `  browser       secure context ${yesNo(r.secureContext)} · webcodecs ${yesNo(r.webCodecs)} · mse ${yesNo(r.mediaSource)} · ready ${r.readyState}`,
    `  theme         ${r.theme} · visibility ${r.visibility}`,
    'session',
    `  ${info.transport} · ${s.idShort || '?'}`,
    `  target        ${s.targetUrl || '?'}`,
    `  launch        ${s.resolution || '?'} · ${s.frameRate > 0 ? `${s.frameRate} fps` : 'fps default'} · ${s.browserType || '?'}`,
    `  identity      tz ${s.timezone || 'UTC (default)'} · locale ${s.locale || 'en-US (default)'}`,
    `  tor           ${s.torEnabled ? `on${s.torCountry ? ` (${s.torCountry})` : ''}` : 'off'}`,
    `  status        ${s.status} · viewonly ${yesNo(s.viewOnly)} · shadow ${yesNo(s.shadow)} · viewers ${s.viewerCount}`,
    `  timer         ${remainingText(s.remainingSec, s.isUnlimited)} · unlimited ${yesNo(s.isUnlimited)}`,
    `  quality       ${s.quality} · net capture ${yesNo(s.networkCaptureEnabled)} · tabs ${s.browserTabCount}`,
    `  audio         active ${yesNo(s.audioActive)} · muted ${yesNo(s.audioMuted)}`,
    `  reconnects    ${s.reconnects} · restarting ${yesNo(s.restarting)}`,
    `  load          ${info.loadMs !== null ? `${info.loadMs} ms` : '...'}`,
    'stream',
    `  lat           ${info.latencyText}`,
    `  fps           ${info.fpsText}`,
    `  ws            ${wsState}`,
    `  connect       ${m.connectMs ?? '...'} ms`,
    `  first frame   ${m.firstFrameMs ?? '...'} ms`,
    `  codec         ${m.codec || '...'}`,
    `  stream        ${m.streamWidth || '?'}x${m.streamHeight || '?'} @ ${m.targetFps || '?'}fps cap`,
    `  decoded       ${m.videoWidth || '?'}x${m.videoHeight || '?'} · ${m.recentFps || 0} fps`,
    `  net           ${m.netQuality} · arrival ${m.netRatio > 0 ? `${(m.netRatio * 100).toFixed(0)}%` : 'n/a'} · rtt ${m.rttMs !== null ? `${m.rttMs} ms` : '...'} · abr L${m.abrLevel}${s.quality !== 'auto' ? ' manual' : ''}`,
    `  playout lag   ${m.playoutLagMs} ms${m.playbackRate !== 1 ? ` ×${m.playbackRate.toFixed(2)}` : ''}`,
    `  buffer        ${m.bufferedSec.toFixed(2)} s · pending ${m.pendingSegments}`,
    `  segments      ${m.segmentRate}/s · seq ${m.sequenceNumber}${m.keyframeCount > 0 ? ` (${m.keyframeCount} IDR)` : ''}`,
    `  rx            ${(m.bytesReceived / 1024).toFixed(0)} KB · ${(m.bitrateKbps / 1000).toFixed(2)} Mbps · ${(m.targetBitrateKbps / 1000).toFixed(1)} cap`,
    `  last segment  ${m.lastSegmentAgeMs >= 0 ? `${m.lastSegmentAgeMs} ms ago` : '...'} · appends ${m.appendCount}`,
    `  static        ${m.staticSecs} s · skipped ${m.skippedTicks}`,
    `  renderer      ${m.rendererStalled ? `stalled ${Math.round(m.rendererStalledMs / 1000)}s` : 'responsive'} · recoveries ${m.rendererRecoveries} · probe streak ${m.cdpProbeFailStreak}`,
    `  changed       ${m.changesPerSec}/s · ${m.damagedPct}% of frame${m.fullFramesPerSec > 0 ? ` · ${m.fullFramesPerSec} full/s` : ''}`,
    `  capture       ${m.captureHealthOk ? 'reporting' : 'no report'} · empty ${m.captureEmptyFrames} · empty/10s ${m.captureEmptyWindow} · fallback ${m.captureFallback ? 'on' : 'off'} · last paint ${m.captureLastPaintMs >= 0 ? `${m.captureLastPaintMs} ms` : '...'} · paints→sent/dedup ${m.captureSentFrames}/${m.captureDedupFrames}`,
    `  keyframe      ${m.keyframeReason || '...'} · ${m.keyframeAgeMs >= 0 ? `${m.keyframeAgeMs} ms ago` : '...'}`,
    `  decode errors ${m.decodeErrorCount} · last recovery ${m.lastRecoveryMs !== null ? `${m.lastRecoveryMs} ms` : '...'}`,
    `  input         ${m.inputBackend || '...'}`,
    `  input target  ${m.inputTarget || '...'}${m.inputTargetOK ? '' : ' (NOT THE ACTIVE TAB)'}`,
    `  activity      ${m.activityMode || '...'} · capture fps ${m.targetFps ? String(m.targetFps) : '?'} · encoder seq ${m.encoderSeq > 0 ? m.encoderSeq : '...'}`,
    'errors',
    `  stream        ${info.streamError || 'none'}`,
    `  session       ${info.sessionError || 'none'}`,
    `  display       ${info.errorText || 'none'}`,
  ];

  lines.push('', '--- json ---', JSON.stringify(info, null, 2));
  return lines.join('\n');
}

/** Copies text to the clipboard, falling back to a hidden textarea + execCommand
 *  on insecure origins (plain-http self-hosted deployments, where
 *  `navigator.clipboard` is undefined). Resolves true on success. */
export async function copyTextToClipboard(text: string): Promise<boolean> {
  return copyToClipboard(text);
}
