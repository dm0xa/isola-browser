import React from 'react';
import { Link } from 'react-router-dom';
import {
  Share2, X, Check, RotateCw, Bug, Maximize2, Minimize2, MoreVertical,
  Activity, Gauge, SignalHigh, SignalLow, SignalMedium, ExternalLink, AlertTriangle,
} from 'lucide-react';
import { AudioToggleButton } from '../../components/AudioToggleButton';
import { IsolaLogo } from '../../components/IsolaLogo';
import SessionTimeControl from '../../components/SessionTimeControl';
import type { useIsolaStream } from '../../hooks/useIsolaStream';
import type { ToolbarButton } from '../../plugins/types';
import { ViewerMenuItem } from './ViewerMenuItem';

export type ViewerToolbarProps = {
  hideToolbar: boolean;
  id: string | undefined;
  showOverlay: boolean;
  statusText: string;
  isReconnecting: boolean;
  isConnected: boolean;
  isola: ReturnType<typeof useIsolaStream>;
  viewerCount: number;
  remainingSec: number;
  isUnlimited: boolean;
  status: string;
  extendAvailable: boolean;
  viewOnly: boolean;
  onExtend: () => Promise<boolean>;
  isFullscreen: boolean;
  tabBarSlotRef: React.RefObject<HTMLDivElement>;
  rendererStallVisible: boolean;
  rendererStalledSec: number;
  captureStallVisible: boolean;
  clipboardSynced: boolean;
  menuOpen: boolean;
  setMenuOpen: React.Dispatch<React.SetStateAction<boolean>>;
  audioMuted: boolean;
  toggleAudio: () => void;
  toggleFullscreen: () => void;
  debugEnabled: boolean;
  toggleDebug: () => void;
  networkCaptureEnabled: boolean;
  netPanel: import('../../components/IsolaNetworkPanel').PanelLayout | 'off';
  setNetPanel: React.Dispatch<React.SetStateAction<import('../../components/IsolaNetworkPanel').PanelLayout | 'off'>>;
  openNetworkPopOut: () => void;
  shareCopied: boolean;
  handleShare: () => void;
  toolbarButtons: ToolbarButton[];
  restarting: boolean;
  sessionTargetUrl: string;
  handleRestart: () => void;
  handleEndSession: () => void;
  onRecoverRenderer: () => void;
};

export function ViewerToolbar(props: ViewerToolbarProps) {
  const {
    hideToolbar, id, showOverlay, statusText, isReconnecting, isConnected, isola, viewerCount,
    remainingSec, isUnlimited, status, extendAvailable, viewOnly, onExtend, isFullscreen,
    tabBarSlotRef, rendererStallVisible, rendererStalledSec, captureStallVisible, clipboardSynced,
    menuOpen, setMenuOpen, audioMuted, toggleAudio, toggleFullscreen, debugEnabled,
    toggleDebug, networkCaptureEnabled, netPanel, setNetPanel, openNetworkPopOut,
    shareCopied, handleShare, toolbarButtons, restarting, sessionTargetUrl, handleRestart, handleEndSession,
    onRecoverRenderer,
  } = props;
  // End session takes two clicks. The first arms the button; the second, while
  // it stays armed, actually ends. A stray single click no longer kills the
  // session. A quick double-click still counts, because the first click arms
  // and the second lands inside the window.
  const [endArmed, setEndArmed] = React.useState(false);
  const endArmedRef = React.useRef(false);
  const endBtnRef = React.useRef<HTMLButtonElement>(null);
  const disarmEnd = React.useCallback(() => {
    endArmedRef.current = false;
    setEndArmed(false);
  }, []);
  React.useEffect(() => {
    if (!endArmed) return;
    const timer = setTimeout(disarmEnd, 1800);
    const onPointer = (e: MouseEvent) => {
      if (endBtnRef.current && endBtnRef.current.contains(e.target as Node)) return;
      disarmEnd();
    };
    window.addEventListener('mousedown', onPointer, true);
    return () => {
      clearTimeout(timer);
      window.removeEventListener('mousedown', onPointer, true);
    };
  }, [endArmed, disarmEnd]);
  const onEndClick = () => {
    // Ref, not state: the second click of a double-click can arrive before
    // the re-render that would show the armed state.
    if (!endArmedRef.current) {
      endArmedRef.current = true;
      setEndArmed(true);
      return;
    }
    disarmEnd();
    handleEndSession();
  };
  if (hideToolbar) return null;
  return (
        <div className="flex items-center px-4 py-1 bg-zinc-100 dark:bg-gray-900 shrink-0 relative z-50">
        <div className={`flex items-center gap-2 min-w-0 ${isFullscreen ? 'shrink-0' : 'flex-1'}`}>
        <Link to="/" className="text-zinc-500 dark:text-gray-400 hover:text-zinc-900 dark:hover:text-white transition-colors shrink-0" title="Home">
          <IsolaLogo className="w-9 h-9 mt-1" mini />
        </Link>
        <div className="flex items-center gap-2">
          <span className="text-xs text-zinc-600 dark:text-gray-500 font-mono">
            {id?.slice(0, 8)}
          </span>
          {showOverlay && (
            <span className="text-xs text-zinc-600 dark:text-gray-500">{statusText}</span>
          )}
          {isReconnecting && (
            <span className="text-xs text-yellow-600 dark:text-yellow-400 font-mono animate-pulse">Reconnecting...</span>
          )}
          {isConnected && (
            <span
              className={`flex items-center ${
                isola.metrics.netQuality === 'good'
                  ? 'text-emerald-500'
                  : isola.metrics.netQuality === 'bad'
                  ? 'text-red-500'
                  : 'text-yellow-500'
              }`}
              title={`Stream quality: ${isola.metrics.netQuality}${isola.metrics.rttMs !== null ? ` · RTT ${isola.metrics.rttMs} ms` : ''}`}
            >
              {isola.metrics.netQuality === 'good'
                ? <SignalHigh size={13} />
                : isola.metrics.netQuality === 'bad'
                ? <SignalLow size={13} />
                : <SignalMedium size={13} />}
            </span>
          )}
          {viewerCount > 0 && (
            <span className="text-xs text-zinc-500 dark:text-gray-400 font-mono ml-1" title={`${viewerCount} watcher${viewerCount !== 1 ? 's' : ''}`}>
              {viewerCount}
            </span>
          )}
        </div>
        </div>
        <div className="flex items-center gap-3 shrink-0">
          <SessionTimeControl
            remainingSec={remainingSec}
            isUnlimited={isUnlimited}
            status={status}
            extendAvailable={extendAvailable}
            viewOnly={viewOnly}
            onExtend={onExtend}
          />
          {viewOnly && (
            <span className="text-xs text-zinc-600 dark:text-gray-500 font-mono bg-zinc-300 dark:bg-gray-800 px-2 py-0.5 rounded">
              watch only
            </span>
          )}
        </div>
        {/* Fullscreen only: the isola tab strip is re-homed into this slot by
            the stream hook so the tabs and address bar sit on the same line as
            the session controls. Hidden (and unused) outside fullscreen. */}
        <div className={`items-center min-w-0 mx-2 ${isFullscreen ? 'flex flex-1' : 'hidden'}`}>
          <div ref={tabBarSlotRef} className="w-full min-w-0" />
        </div>
        <div className={`flex items-center gap-1 min-w-0 ${isFullscreen ? 'shrink-0' : 'flex-1 justify-end'}`}>
          {rendererStallVisible && (
            <button
              onClick={() => onRecoverRenderer()}
              title={
                rendererStalledSec > 0
                  ? `The sandbox's page renderer stopped producing frames ${rendererStalledSec}s ago, which freezes the picture. Click to replace the renderer and reload the tab.`
                  : 'The sandbox is not answering its own probes for the page renderer. Click to replace the renderer and reload the tab.'
              }
              className="flex items-center gap-1 whitespace-nowrap rounded px-1.5 py-0.5 text-xs font-mono text-amber-700 ring-1 ring-inset ring-amber-500/40 bg-amber-500/10 hover:bg-amber-500/20 dark:text-amber-300 mr-1"
            >
              <AlertTriangle size={12} aria-hidden />
              {rendererStalledSec > 0 ? `renderer stuck ${rendererStalledSec}s` : 'renderer not responding'}
              <span className="font-sans font-medium">recover</span>
            </button>
          )}
          {captureStallVisible && !rendererStallVisible && (
            <span
              title={`The sandbox's compositor is returning empty frames (${isola.metrics.captureEmptyWindow} in the last 10s)${
                isola.metrics.captureFallback ? ', so frames are being driven from screenshots' : ''
              }. The page itself is running; the picture recovers on its own.`}
              className="flex items-center gap-1 whitespace-nowrap rounded px-1.5 py-0.5 text-xs font-mono text-amber-700 ring-1 ring-inset ring-amber-500/40 bg-amber-500/10 dark:text-amber-300 mr-1"
            >
              <AlertTriangle size={12} aria-hidden />
              capture stalled{isola.metrics.captureFallback ? ' — recovering' : ''}
            </span>
          )}
          {clipboardSynced && (
            <span className="text-xs text-emerald-600 dark:text-emerald-400 font-mono transition-opacity mr-1">Synced</span>
          )}

          <AudioToggleButton
            muted={audioMuted}
            onToggle={() => { void toggleAudio(); }}
            disabled={!isConnected}
          />

          {/* Secondary session actions behind a 3-dots menu. The
              time-critical actions are pinned out instead: End stays as its
              own button here, and Extend lives inside the session-time
              control above so the owner can add time before the clock runs
              out. */}
          <div className="relative">
            <button
              onClick={() => setMenuOpen((v) => !v)}
              title="Session actions"
              aria-label="Session actions"
              aria-haspopup="menu"
              aria-expanded={menuOpen}
              className={`p-1.5 rounded transition-colors ${
                menuOpen
                  ? 'bg-zinc-300 dark:bg-gray-800 text-zinc-900 dark:text-white'
                  : 'text-zinc-500 dark:text-gray-400 hover:text-zinc-900 dark:hover:text-white hover:bg-zinc-300 dark:hover:bg-gray-800'
              }`}
            >
              <MoreVertical size={16} />
            </button>
            {menuOpen && (
              <>
                <div
                  className="fixed inset-0 z-40"
                  onClick={() => setMenuOpen(false)}
                  onContextMenu={(e) => { e.preventDefault(); setMenuOpen(false); }}
                />
                <div
                  role="menu"
                  className="absolute right-0 top-full z-50 mt-1 w-52 overflow-hidden rounded-xl border border-zinc-200 dark:border-gray-700 bg-white/95 dark:bg-gray-900/95 py-1 shadow-2xl backdrop-blur text-[13px]"
                >
                  {isConnected && (
                    <ViewerMenuItem
                      icon={document.fullscreenElement ? <Minimize2 size={15} /> : <Maximize2 size={15} />}
                      label={document.fullscreenElement ? 'Exit fullscreen' : 'Enter fullscreen'}
                      dimmed
                      onClick={() => { setMenuOpen(false); toggleFullscreen(); }}
                    />
                  )}
                  <ViewerMenuItem
                    icon={<Bug size={15} />}
                    label={debugEnabled ? 'Hide debug overlay' : 'Show debug overlay'}
                    dimmed
                    toggled={debugEnabled}
                    onClick={() => { setMenuOpen(false); toggleDebug(); }}
                  />
                  {/* Network panel toggle — only shown when the sandbox was
                      launched with CDP network/console capture enabled (every
                      isola session). Default is OFF; user must explicitly
                      opt in from this menu row to see the panel split, even
                      though the capture itself has been streaming all along
                      so their history is already populated when they opt in. */}
                  {networkCaptureEnabled && (
                    <div className={`flex items-center ${netPanel !== 'off' ? 'bg-cyan-500/10' : ''}`}>
                      <div className="min-w-0 flex-1">
                        <ViewerMenuItem
                          icon={<Activity size={15} />}
                          label={netPanel !== 'off' ? 'Hide network panel' : 'Show network panel'}
                          dimmed
                          toggled={netPanel !== 'off'}
                          onClick={() => {
                            setMenuOpen(false);
                            setNetPanel((cur) => (cur === 'off' ? 'bottom' : 'off'));
                          }}
                        />
                      </div>
                      {/* Trailing action: open the same monitor in a standalone
                          browser tab. Kept as a sibling button (not nested)
                          so the row stays semantically valid. */}
                      <button
                        type="button"
                        title="Open network monitor in a new tab"
                        aria-label="Open network monitor in a new tab"
                        onClick={() => { setMenuOpen(false); openNetworkPopOut(); }}
                        className="mr-1.5 flex h-7 w-7 shrink-0 items-center justify-center rounded-md text-zinc-400 transition-colors hover:bg-zinc-300/70 hover:text-cyan-500 dark:text-gray-500 dark:hover:bg-gray-700 dark:hover:text-cyan-400"
                      >
                        <ExternalLink size={15} />
                      </button>
                    </div>
                  )}
                  {/* Stream quality (isola): adaptive Auto is the only mode
                      exposed, so the row is informational — no selected state. */}
                  {isConnected && (
                    <ViewerMenuItem
                      icon={<Gauge size={15} />}
                      label="Quality (Auto)"
                      dimmed
                      static
                    />
                  )}
                  {!viewOnly && (
                    <ViewerMenuItem
                      icon={shareCopied ? <Check size={15} /> : <Share2 size={15} />}
                      label={shareCopied ? 'Share link copied' : 'Share session link'}
                      dimmed
                      onClick={() => { setMenuOpen(false); void handleShare(); }}
                    />
                  )}
                  {/* Dynamic plugin toolbar buttons (kept in the menu, same order as before) */}
                  {toolbarButtons.length > 0 && (
                    <div className="my-1 h-px bg-zinc-200/70 dark:bg-gray-700/70" />
                  )}
                  {toolbarButtons.map((btn) => (
                    <div key={btn.id} className="px-1 py-0.5">
                      <btn.component sessionId={id!} viewOnly={viewOnly} />
                    </div>
                  ))}
                  {!viewOnly && (
                    <>
                      <div className="my-1 h-px bg-zinc-200/70 dark:bg-gray-700/70" />
                      <ViewerMenuItem
                        icon={<RotateCw size={15} className={restarting ? 'animate-spin' : ''} />}
                        label={restarting ? 'Restarting session…' : 'Restart session'}
                        dimmed
                        disabled={restarting || !sessionTargetUrl}
                        onClick={() => { setMenuOpen(false); void handleRestart(); }}
                      />
                    </>
                  )}
                </div>
              </>
            )}
          </div>

          {/* Exit fullscreen — shown only while fullscreen, next to the close
              button, so the toolbar can still collapse back on one click. */}
          {isFullscreen && (
            <button
              onClick={toggleFullscreen}
              title="Exit fullscreen"
              aria-label="Exit fullscreen"
              className="p-1.5 rounded text-zinc-500 dark:text-gray-400 hover:text-zinc-900 dark:hover:text-white hover:bg-zinc-300 dark:hover:bg-gray-800 transition-colors"
            >
              <Minimize2 size={16} />
            </button>
          )}

          {/* End session — pinned to the RIGHT of the 3-dots menu. Most
              destructive action, so it arms on the first click and ends on the
              second. */}
          {!viewOnly && (
            <button
              ref={endBtnRef}
              onClick={onEndClick}
              title={endArmed ? 'Click again to end session' : 'End session'}
              aria-label={endArmed ? 'Click again to end session' : 'End session'}
              className={`p-1.5 rounded transition-colors ${
                endArmed
                  ? 'bg-red-600 text-white hover:bg-red-500'
                  : 'text-red-600 dark:text-red-400 hover:bg-red-500/10'
              }`}
            >
              <X size={16} />
            </button>
          )}
        </div>
      </div>
  );
}
