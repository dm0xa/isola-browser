// Docked "Resources" overlay for the live viewer: the saved-files archive,
// reviewable without leaving the stream. Same dock geometry and header
// affordances as IsolaNetworkPanel (layout toggle, ↗ new tab, ✕) so the two
// overlays behave alike — they share the dock size (and its localStorage keys)
// because only one can be open at a time.
import { useCallback, useEffect, useRef, useState, type CSSProperties } from 'react';

import type { CaptureArchive, CaptureStatus } from '../lib/captureApi';
import { formatBytes, groupCaptureFiles } from '../lib/captureFormat';
import { useTheme } from '../lib/ThemeContext';
import { CaptureArchiveView } from './CaptureArchiveView';
import {
  clampBottom,
  clampSide,
  DEFAULT_BOTTOM_HEIGHT,
  DEFAULT_SIDE_WIDTH,
  LS_BOTTOM_HEIGHT,
  LS_SIDE_WIDTH,
  type PanelLayout,
} from './IsolaNetworkPanel';

const FLOAT_INSET = 12;

export type CapturePanelProps = {
  /** Archive id (the session id on this deployment). */
  archiveId: string;
  /** Live counters from useCaptureRecording; also decides the polling mode. */
  status: CaptureStatus | null;
  layout: PanelLayout;
  onLayout: (next: PanelLayout) => void;
  onClose: () => void;
  /** Offer the "open in a new browser tab" action. */
  onPopOut?: () => void;
};

export function CapturePanel({ archiveId, status, layout, onLayout, onClose, onPopOut }: CapturePanelProps) {
  const { theme } = useTheme();
  const recording = !!status?.recording;
  const [fileCount, setFileCount] = useState(0);
  const [fileBytes, setFileBytes] = useState(0);
  const draggingRef = useRef(false);
  const startRectRef = useRef<{ width: number; height: number; clientY: number; clientX: number } | null>(null);

  const [bottomHeight, setBottomHeight] = useState<number>(() => {
    try {
      const v = localStorage.getItem(LS_BOTTOM_HEIGHT);
      // Clamp restored value: guards against oversized values saved earlier.
      return v ? clampBottom(parseInt(v, 10)) : DEFAULT_BOTTOM_HEIGHT();
    } catch { return DEFAULT_BOTTOM_HEIGHT(); }
  });
  const [sideWidth, setSideWidth] = useState<number>(() => {
    try {
      const v = localStorage.getItem(LS_SIDE_WIDTH);
      return v ? clampSide(parseInt(v, 10)) : DEFAULT_SIDE_WIDTH;
    } catch { return DEFAULT_SIDE_WIDTH; }
  });

  // Re-clamp when the window itself shrinks (orientation change, browser resize).
  useEffect(() => {
    const onWinResize = () => {
      setBottomHeight((h) => clampBottom(h));
      setSideWidth((w) => clampSide(w));
    };
    window.addEventListener('resize', onWinResize);
    return () => window.removeEventListener('resize', onWinResize);
  }, []);

  useEffect(() => {
    try { localStorage.setItem(LS_BOTTOM_HEIGHT, String(bottomHeight)); } catch {}
  }, [bottomHeight]);
  useEffect(() => {
    try { localStorage.setItem(LS_SIDE_WIDTH, String(sideWidth)); } catch {}
  }, [sideWidth]);

  const onResizeStart = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    e.stopPropagation();
    draggingRef.current = true;
    startRectRef.current = {
      width: sideWidth,
      height: bottomHeight,
      clientY: e.clientY,
      clientX: e.clientX,
    };
    document.body.style.userSelect = 'none';
    document.body.style.cursor = layout === 'bottom' ? 'ns-resize' : 'ew-resize';
  }, [sideWidth, bottomHeight, layout]);

  useEffect(() => {
    const onMove = (e: MouseEvent) => {
      if (!draggingRef.current || !startRectRef.current) return;
      const start = startRectRef.current;
      if (layout === 'bottom') {
        const delta = start.clientY - e.clientY;
        setBottomHeight(clampBottom(start.height + delta));
      } else {
        const delta = start.clientX - e.clientX;
        setSideWidth(clampSide(start.width + delta));
      }
    };
    const onUp = () => {
      if (draggingRef.current) {
        draggingRef.current = false;
        startRectRef.current = null;
        document.body.style.userSelect = '';
        document.body.style.cursor = '';
      }
    };
    window.addEventListener('mousemove', onMove, true);
    window.addEventListener('mouseup', onUp, true);
    return () => {
      window.removeEventListener('mousemove', onMove, true);
      window.removeEventListener('mouseup', onUp, true);
    };
  }, [layout]);

  const onArchive = useCallback((archive: CaptureArchive | null, count: number) => {
    setFileCount(count);
    setFileBytes(
      archive
        ? groupCaptureFiles(archive.items).reduce((sum, file) => sum + (file.size || 0), 0)
        : 0,
    );
  }, []);

  const panelBg = theme === 'light' ? 'rgba(255, 255, 255, 0.97)' : 'rgba(17, 24, 39, 0.97)';
  const panelStyle: CSSProperties = layout === 'bottom'
    ? { left: FLOAT_INSET, right: FLOAT_INSET, bottom: FLOAT_INSET, height: `${bottomHeight}px` }
    // Clear the 36px isola browser chrome (tab strip) plus the float margin.
    : { top: FLOAT_INSET + 32, right: FLOAT_INSET, bottom: FLOAT_INSET, width: `${sideWidth}px` };

  // Live counters come from the capture status; once stopped the archive is the
  // source of truth (its deduped rows and unique bytes), so a reopened panel
  // still reads `N files · X MB`.
  const recorded = status?.items ?? 0;
  const counts = recording
    ? `Recording — ${recorded} ${recorded === 1 ? 'file' : 'files'}`
    : `${fileCount} ${fileCount === 1 ? 'file' : 'files'} · ${formatBytes(fileBytes)}`;

  return (
    <div
      className="absolute z-30 flex flex-col overflow-hidden rounded-xl border border-cyan-500/30 bg-white font-mono text-xs text-zinc-800 shadow-2xl dark:border-cyan-500/20 dark:bg-slate-950 dark:text-gray-200"
      style={{ ...panelStyle, backgroundColor: panelBg }}
    >
      {layout === 'bottom' && (
        <div
          onMouseDown={onResizeStart}
          onDoubleClick={() => setBottomHeight(DEFAULT_BOTTOM_HEIGHT())}
          className="absolute top-0 left-0 right-0 z-40 h-1.5 cursor-ns-resize hover:bg-cyan-500/20 transition-colors"
          title="Drag to resize · double-click to reset"
        >
          <div className="pointer-events-none mx-auto mt-px h-0.5 w-10 rounded-full bg-zinc-400 dark:bg-gray-600" />
        </div>
      )}
      {layout === 'side' && (
        <div
          onMouseDown={onResizeStart}
          onDoubleClick={() => setSideWidth(DEFAULT_SIDE_WIDTH)}
          className="absolute left-0 top-0 bottom-0 z-40 w-1.5 cursor-ew-resize hover:bg-cyan-500/20 transition-colors"
          title="Drag to resize · double-click to reset"
        >
          <div className="pointer-events-none my-auto ml-px h-10 w-0.5 rounded-full bg-zinc-400 dark:bg-gray-600" />
        </div>
      )}

      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 border-b border-zinc-300 px-2 py-1.5 dark:border-gray-700">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-bold text-zinc-900 dark:text-gray-100">Resources</span>
          <span
            data-testid="capture-header-counts"
            className={`whitespace-nowrap ${recording ? 'text-red-700 dark:text-red-300' : 'text-zinc-500 dark:text-gray-500'}`}
          >
            {recording && <span className="mr-1 inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-red-500 align-middle" aria-hidden />}
            {counts}
          </span>
        </div>
        <div className="ml-auto flex items-center gap-2">
          {onPopOut && (
            <button
              type="button"
              onClick={onPopOut}
              className="whitespace-nowrap rounded px-1.5 py-0.5 text-cyan-600 hover:bg-zinc-200 hover:text-cyan-700 dark:text-cyan-400 dark:hover:bg-gray-700 dark:hover:text-cyan-200"
              title="Open saved resources in a new browser tab"
            >
              ↗ new tab
            </button>
          )}
          <button
            type="button"
            onClick={() => onLayout(layout === 'bottom' ? 'side' : 'bottom')}
            className="whitespace-nowrap rounded px-1.5 py-0.5 text-zinc-500 hover:bg-zinc-200 hover:text-zinc-900 dark:text-gray-400 dark:hover:bg-gray-700 dark:hover:text-gray-100"
            title={layout === 'bottom' ? 'Move panel to the side' : 'Move panel to the bottom'}
          >
            {layout === 'bottom' ? '⇥ side' : '⤓ bottom'}
          </button>
          <button
            type="button"
            onClick={onClose}
            className="whitespace-nowrap rounded px-1.5 py-0.5 text-zinc-500 hover:bg-zinc-200 hover:text-zinc-900 dark:text-gray-400 dark:hover:bg-gray-700 dark:hover:text-gray-100"
            title="Close panel"
          >
            ✕
          </button>
        </div>
      </div>

      <CaptureArchiveView
        archiveId={archiveId}
        layout="panel"
        recording={recording}
        onArchive={onArchive}
      />
    </div>
  );
}
