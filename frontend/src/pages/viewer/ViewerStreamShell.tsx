import React from 'react';
import { CapturePanel } from '../../components/CapturePanel';
import { SessionDataPanel } from '../../components/SessionDataPanel';
import type { ConsoleEntry, CookieEntry, NetEntry, PanelLayout, PanelTab } from '../../components/IsolaNetworkPanel';

// The panel (and ResourceGraph) is ~100 kB raw that most sessions never open;
// load it on first open instead of on viewer start.
const IsolaNetworkPanel = React.lazy(() =>
  import('../../components/IsolaNetworkPanel').then((m) => ({ default: m.IsolaNetworkPanel })),
);
import { ISOLA_LOGO_KEYFRAMES } from '../../components/IsolaLogo';
import type { useIsolaStream } from '../../hooks/useIsolaStream';
import type { CaptureStatus } from '../../lib/captureApi';
import type { DocumentHeaders, IntelFetch } from '../../lib/intel';

export type ViewerStreamShellProps = {
  shellRef: React.RefObject<HTMLDivElement | null>;
  canvasRef: React.RefObject<HTMLDivElement | null>;
  containerStyle: React.CSSProperties;
  networkCaptureEnabled: boolean;
  netPanel: PanelLayout | 'off';
  netEntries: NetEntry[];
  consoleEntries: ConsoleEntry[];
  cookieEntries: CookieEntry[];
  docHeaders: Record<string, DocumentHeaders>;
  onIntel: IntelFetch;
  intelDisabled: boolean;
  netTab: PanelTab;
  setNetTab: React.Dispatch<React.SetStateAction<PanelTab>>;
  setNetPanel: (next: PanelLayout | 'off') => void;
  openNetworkPopOut: () => void;
  /** Saved-resources overlay (docks in the same corner as the network panel). */
  capturePanel: PanelLayout | 'off';
  setCapturePanel: (next: PanelLayout | 'off') => void;
  captureArchiveId: string;
  captureStatus: CaptureStatus | null;
  openCaptures: () => void;
  /** SHA-1 digests already persisted to the capture archive (row markers). */
  savedHashes?: ReadonlySet<string>;
  browserTabs: ReturnType<typeof useIsolaStream>['browserTabs'];
  theme: 'dark' | 'light';
  /**
   * Live custom-data panel, docked to the right of the stream. Empty id hides
   * it (closed, or a view-only share with no session token to seed with).
   */
  sessionDataSessionId: string;
  sessionDataToken: string;
  onCloseSessionData: () => void;
};

// Memoised: the shell renders the canvas host, the panel and the logo styles,
// none of which read stream metrics. Without this every 1 Hz metrics publish
// re-rendered the open panel (measured 37 ms at the 1000-entry cap).
export const ViewerStreamShell = React.memo(function ViewerStreamShell(props: ViewerStreamShellProps) {
  const {
    shellRef, canvasRef, containerStyle, networkCaptureEnabled, netPanel, netEntries,
    consoleEntries, cookieEntries, docHeaders, onIntel, intelDisabled, netTab, setNetTab, setNetPanel, openNetworkPopOut,
    capturePanel, setCapturePanel, captureArchiveId, captureStatus, openCaptures,
    savedHashes, browserTabs, theme,
    sessionDataSessionId, sessionDataToken, onCloseSessionData,
  } = props;
  return (
    <>
<div ref={shellRef} className="flex-1 relative min-h-0 overflow-hidden bg-zinc-100 dark:bg-gray-900 flex items-center justify-center">
        <div
          id="vnc-container"
          ref={canvasRef}
          className="relative overflow-hidden bg-zinc-100 dark:bg-gray-900 shrink-0"
          style={containerStyle}
        />
        {networkCaptureEnabled && netPanel !== 'off' && (
          <React.Suspense fallback={null}>
          <IsolaNetworkPanel
            entries={netEntries}
            consoleEntries={consoleEntries}
            cookieEntries={cookieEntries}
            docHeaders={docHeaders}
            onIntel={onIntel}
            intelDisabled={intelDisabled}
            layout={netPanel}
            tab={netTab}
            onTab={setNetTab}
            onLayout={setNetPanel}
            onClose={() => setNetPanel('off')}
            onPopOut={openNetworkPopOut}
            savedHashes={savedHashes}
            tabs={browserTabs}
          />
          </React.Suspense>
        )}
        {capturePanel !== 'off' && (
          <CapturePanel
            archiveId={captureArchiveId}
            status={captureStatus}
            layout={capturePanel}
            onLayout={setCapturePanel}
            onClose={() => setCapturePanel('off')}
            onPopOut={openCaptures}
          />
        )}
        {sessionDataSessionId && (
          <SessionDataPanel
            sessionId={sessionDataSessionId}
            token={sessionDataToken}
            onClose={onCloseSessionData}
          />
        )}
        {/* Old inline "Network (N)" floating button removed — access via the
            3-dots menu (Show/hide network panel) */}
      </div>
      <style>{ISOLA_LOGO_KEYFRAMES + `
        #vnc-container {
          display: flex;
          align-items: center;
          justify-content: center;
          background: var(--vnc-container-bg);
        }
        #vnc-container > div:not(#isola-tabbar) {
          width: 100% !important;
          height: 100% !important;
          display: flex !important;
          align-items: center !important;
          justify-content: center !important;
          background: var(--vnc-container-bg) !important;
          overflow: hidden !important;
        }
        /* The isola tab strip is browser chrome, not page content: it keeps
           its own fixed height at the top instead of the legacy 100% stretch. */
        #vnc-container > div#isola-tabbar {
          width: auto !important;
          height: 36px !important;
          justify-content: flex-start !important;
          background: ${theme === 'dark' ? 'rgba(17, 24, 39, 0.88)' : 'rgba(244, 244, 245, 0.92)'} !important;
          overflow: visible !important;
        }
        #vnc-container > div > canvas {
          margin: 0 auto !important;
          max-width: 100% !important;
          max-height: 100% !important;
        }
      `}</style>
    </>
  );
});
