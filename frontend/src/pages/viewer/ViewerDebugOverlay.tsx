import { X, Check, Copy, AlertCircle } from 'lucide-react';
import type { IsolaQuality } from '../../hooks/useIsolaStream';
import type { IsolaStreamMetrics } from '../../hooks/useIsolaStream';
import { shortInputTarget } from './viewerUtils';

export type ViewerDebugOverlayProps = {
  enabled: boolean;
  transport: string;
  id: string | undefined;
  debugCopied: boolean;
  debugCopyFailed: boolean;
  copyDebugOverlay: () => void | Promise<void>;
  toggleDebug: () => void;
  latencyColor: string;
  latencyText: string;
  fpsColor: string;
  fpsText: string;
  isolaMetrics: IsolaStreamMetrics;
  loadMs: number | null;
  loadSec: string | null;
  quality: IsolaQuality;
  rendererStallVisible: boolean;
  rendererStalledSec: number;
};

export function ViewerDebugOverlay(props: ViewerDebugOverlayProps) {
  const {
    enabled, transport, id, debugCopied, debugCopyFailed, copyDebugOverlay, toggleDebug,
    latencyColor, latencyText, fpsColor, fpsText, isolaMetrics, loadMs, loadSec,
    quality, rendererStallVisible, rendererStalledSec,
  } = props;
  if (!enabled) return null;
  // Layout facts for "the viewer does not fill the screen" reports: the
  // viewport the page is actually laid out in, the physical screen, whether the
  // browser is really in element fullscreen, the app's painted height and the
  // capture element's box. A fullscreen viewer whose `view` is smaller than
  // `screen` is a browser/top-layer problem; one where `view` == `screen` shows
  // any remaining bars as the letterbox of the remote capture's aspect ratio.
  const view = `${window.innerWidth}x${window.innerHeight}`;
  const outer = `${window.outerWidth}x${window.outerHeight}`;
  const screenSize = window.screen ? `${window.screen.width}x${window.screen.height}` : '?';
  // avail* excludes the OS menu bar / dock, so a viewport that matches it but
  // not `screen` means the app is inside a window-managed (not fullscreen) box.
  const avail = window.screen ? `${window.screen.availWidth}x${window.screen.availHeight}` : '?';
  const fs = !!document.fullscreenElement;
  const rootEl = document.getElementById('root');
  const appEl = rootEl?.firstElementChild as HTMLElement | null;
  const toolbarTop = Math.round(appEl?.firstElementChild?.getBoundingClientRect().top ?? -1);
  const capEl = document.querySelector<HTMLVideoElement | HTMLCanvasElement>('#vnc-container video, #vnc-container canvas');
  const capBox = capEl ? `${capEl.clientWidth}x${capEl.clientHeight}` : '?';
  return (
        <div
          className="absolute bottom-3 right-3 z-40 rounded-lg border border-cyan-500/20 bg-slate-950/70 px-3 py-2 text-[11px] font-mono text-cyan-100 shadow-lg backdrop-blur min-w-[14rem] max-w-[min(24rem,calc(100vw-1.5rem))] opacity-60 hover:opacity-100 transition-opacity duration-200"
          title="Debug overlay — hover to focus"
        >
          <div className="flex items-center justify-between gap-2 mb-1">
            <div className="text-cyan-300">{transport} · {id?.slice(0, 8)}</div>
            <div className="flex shrink-0 items-center gap-0.5">
              <button
                type="button"
                onClick={() => { void copyDebugOverlay(); }}
                className="shrink-0 rounded p-1 text-cyan-400 hover:text-cyan-200 hover:bg-cyan-500/10 transition-colors"
                title={debugCopyFailed ? 'Copy failed — copy manually' : 'Copy debug info'}
                aria-label={debugCopyFailed ? 'Copy failed' : 'Copy debug info'}
              >
                {debugCopyFailed ? (
                  <AlertCircle className="w-3.5 h-3.5 text-red-400" />
                ) : debugCopied ? (
                  <Check className="w-3.5 h-3.5" />
                ) : (
                  <Copy className="w-3.5 h-3.5" />
                )}
              </button>
              <button
                type="button"
                onClick={toggleDebug}
                className="shrink-0 rounded p-1 text-cyan-400 hover:text-cyan-200 hover:bg-cyan-500/10 transition-colors"
                title="Close debug overlay"
                aria-label="Close debug overlay"
              >
                <X className="w-3.5 h-3.5" />
              </button>
            </div>
          </div>
          <div className="mb-1 text-[10px] text-cyan-400">
            <span className="font-bold">view</span> {view}
            {' · '}<span className="font-bold">outer</span> {outer}
            {' · '}<span className="font-bold">fs</span> {fs ? 'yes' : 'no'}
          </div>
          <div className="mb-1 text-[10px] text-cyan-400">
            <span className="font-bold">screen</span> {screenSize}
            {' · '}<span className="font-bold">avail</span> {avail}
          </div>
          <div className="mb-2 text-[10px] text-cyan-400">
            <span className="font-bold">root</span> {rootEl?.clientHeight ?? 0}px
            {' · '}<span className="font-bold">toolbar top</span> {toolbarTop}px
            {' · '}<span className="font-bold">capture box</span> {capBox}
          </div>
          {/* Latency + FPS — surfaced prominently here so the toolbar stays clean */}
          <div className="flex items-baseline gap-3 mb-2 text-base">
            <span className="flex items-baseline gap-1">
              <span className="text-[10px] text-cyan-400">lat</span>
              <span className={latencyColor}>{latencyText}</span>
            </span>
            <span className="flex items-baseline gap-1">
              <span className="text-[10px] text-cyan-400">fps</span>
              <span className={fpsColor}>{fpsText}</span>
            </span>
          </div>
          <>
              <div><span className="font-bold">ws</span> {isolaMetrics.wsState === WebSocket.OPEN ? 'open' : isolaMetrics.wsState}</div>
              <div><span className="font-bold">connect</span> {isolaMetrics.connectMs ?? '...'} ms</div>
              <div><span className="font-bold">first frame</span> {isolaMetrics.firstFrameMs ?? '...'} ms</div>
              <div>
                <span className="font-bold">load</span>{' '}
                {loadMs !== null ? `${loadMs} ms (${loadSec} s)` : '...'}
              </div>
              {/* Network score — arrival ratio (measured receive bitrate vs.
                  what the server pushed) plus app-level RTT; the ABR rung
                  shows the adaptive response to that score. */}
              <div>
                <span className="font-bold">net</span> {isolaMetrics.netQuality}
                {' · '}<span className="font-bold">arrival</span>{' '}
                {isolaMetrics.netRatio > 0 ? `${(isolaMetrics.netRatio * 100).toFixed(0)}%` : 'n/a'}
                {' · '}<span className="font-bold">rtt</span>{' '}
                {isolaMetrics.rttMs !== null ? `${isolaMetrics.rttMs} ms` : '...'}
                {' · '}<span className="font-bold">abr</span> L{isolaMetrics.abrLevel}
                {quality !== 'auto' ? ' manual' : ''}
              </div>
              <div>
                <span className="font-bold">codec</span> {isolaMetrics.codec || '...'}
              </div>
              <div>
                <span className="font-bold">stream</span>{' '}
                {isolaMetrics.streamWidth || '?'}x{isolaMetrics.streamHeight || '?'}
                {isolaMetrics.targetFps ? ` @ ${isolaMetrics.targetFps}fps cap` : ''}
                {isolaMetrics.encodeScale > 0 && isolaMetrics.encodeScale < 1
                  ? ` · ${isolaMetrics.encodeScale.toFixed(2)}x`
                  : ''}
              </div>
              <div>
                <span className="font-bold">decoded</span>{' '}
                {isolaMetrics.videoWidth || '?'}x{isolaMetrics.videoHeight || '?'}
                {isolaMetrics.recentFps > 0 ? ` · ${isolaMetrics.recentFps} fps` : ''}
              </div>
              <div>
                <span className="font-bold">playout lag</span> {isolaMetrics.playoutLagMs} ms
                {isolaMetrics.playbackRate !== 1 ? (
                  <span className="ml-1 opacity-70">×{isolaMetrics.playbackRate.toFixed(2)}</span>
                ) : ''}
              </div>
              <div>
                <span className="font-bold">buffer</span> {isolaMetrics.bufferedSec.toFixed(2)} s
                {' · '}<span className="font-bold">pending</span> {isolaMetrics.pendingSegments}
              </div>
              <div>
                <span className="font-bold">segments</span> {isolaMetrics.segmentRate}/s
                {' · '}<span className="font-bold">seq</span> {isolaMetrics.sequenceNumber}
                {isolaMetrics.keyframeCount > 0 ? (
                  <span className="ml-1 opacity-70">({isolaMetrics.keyframeCount} IDR)</span>
                ) : ''}
              </div>
              <div>
                <span className="font-bold">rx</span> {(isolaMetrics.bytesReceived / 1024).toFixed(0)} KB
                {isolaMetrics.bitrateKbps > 0 ? (
                  <span className="ml-2">{(isolaMetrics.bitrateKbps / 1000).toFixed(2)} Mbps</span>
                ) : ''}
                {isolaMetrics.targetBitrateKbps > 0 ? (
                  <span className="ml-1 opacity-60">/ {(isolaMetrics.targetBitrateKbps / 1000).toFixed(1)} cap</span>
                ) : ''}
              </div>
              <div>
                <span className="font-bold">last segment</span>{' '}
                {isolaMetrics.lastSegmentAgeMs >= 0 ? `${isolaMetrics.lastSegmentAgeMs} ms ago` : '...'}
                {' · '}<span className="font-bold">appends</span> {isolaMetrics.appendCount}
              </div>
              <div>
                <span className="font-bold">static</span> {isolaMetrics.staticSecs} s
                {' · '}<span className="font-bold">skipped</span> {isolaMetrics.skippedTicks}
              </div>
              <div>
                <span className="font-bold">renderer</span>{' '}
                {rendererStallVisible
                  ? (rendererStalledSec > 0 ? `stalled ${rendererStalledSec}s` : 'not responding')
                  : 'responsive'}
                {' · '}<span className="font-bold">recoveries</span> {isolaMetrics.rendererRecoveries}
                {' · '}<span className="font-bold">probe streak</span> {isolaMetrics.cdpProbeFailStreak}
              </div>
              <div>
                <span className="font-bold">changed</span> {isolaMetrics.changesPerSec}/s
                {' · '}<span className="font-bold">of frame</span> {isolaMetrics.damagedPct}%
                {isolaMetrics.fullFramesPerSec > 0 ? ` · ${isolaMetrics.fullFramesPerSec} full/s` : ''}
              </div>
              <div>
                <span className="font-bold">capture</span>{' '}
                {isolaMetrics.captureHealthOk ? 'reporting' : 'no report'}
                {' · '}<span className="font-bold">empty</span> {isolaMetrics.captureEmptyFrames}
                {' · '}<span className="font-bold">empty/10s</span> {isolaMetrics.captureEmptyWindow}
                {' · '}<span className="font-bold">fallback</span> {isolaMetrics.captureFallback ? 'on' : 'off'}
                {' · '}<span className="font-bold">last paint</span> {isolaMetrics.captureLastPaintMs >= 0 ? `${isolaMetrics.captureLastPaintMs} ms` : '...'}
                {' · '}<span className="font-bold">paints→sent/dedup</span> {isolaMetrics.captureSentFrames}/{isolaMetrics.captureDedupFrames}
              </div>
              <div>
                <span className="font-bold">keyframe</span> {isolaMetrics.keyframeReason || '...'}
                {' · '}
                {isolaMetrics.keyframeAgeMs >= 0 ? `${isolaMetrics.keyframeAgeMs} ms ago` : '...'}
              </div>
              <div>
                <span className="font-bold">decode errors</span> {isolaMetrics.decodeErrorCount}
                {' · '}<span className="font-bold">last recovery</span>{' '}
                {isolaMetrics.lastRecoveryMs !== null ? `${isolaMetrics.lastRecoveryMs} ms` : '...'}
              </div>
              <div><span className="font-bold">input</span> {isolaMetrics.inputBackend || '...'}</div>
              <div>
                <span className="font-bold">input target</span>{' '}
                <span className={isolaMetrics.inputTargetOK ? '' : 'text-red-500'}>
                  {shortInputTarget(isolaMetrics.inputTarget)}
                </span>
                {!isolaMetrics.inputTargetOK && <span className="ml-1 opacity-80">≠ active tab</span>}
              </div>
              <div><span className="font-bold">activity</span> {isolaMetrics.activityMode || '...'}</div>
              <div>
                <span className="font-bold">capture fps</span>{' '}
                {isolaMetrics.targetFps ? String(isolaMetrics.targetFps) : '?'}
              </div>
              <div>
                <span className="font-bold">encoder seq</span> {isolaMetrics.encoderSeq > 0 ? isolaMetrics.encoderSeq : '...'}
              </div>
          </>
        </div>
  );
}
