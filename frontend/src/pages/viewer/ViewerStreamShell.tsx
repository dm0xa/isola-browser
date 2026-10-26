import React from 'react';
import { IsolaNetworkPanel, type ConsoleEntry, type CookieEntry, type NetEntry, type PanelLayout, type PanelTab } from '../../components/IsolaNetworkPanel';
import { ISOLA_LOGO_KEYFRAMES } from '../../components/IsolaLogo';
import type { useIsolaStream } from '../../hooks/useIsolaStream';
import type { DocumentHeaders, IntelFetch } from '../../lib/intel';

export type ViewerStreamShellProps = {
  shellRef: React.RefObject<HTMLDivElement>;
  canvasRef: React.RefObject<HTMLDivElement>;
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
  setNetPanel: React.Dispatch<React.SetStateAction<PanelLayout | 'off'>>;
  openNetworkPopOut: () => void;
  browserTabs: ReturnType<typeof useIsolaStream>['browserTabs'];
  theme: 'dark' | 'light';
};

export function ViewerStreamShell(props: ViewerStreamShellProps) {
  const {
    shellRef, canvasRef, containerStyle, networkCaptureEnabled, netPanel, netEntries,
    consoleEntries, cookieEntries, docHeaders, onIntel, intelDisabled, netTab, setNetTab, setNetPanel, openNetworkPopOut,
    browserTabs, theme,
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
            tabs={browserTabs}
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
}
