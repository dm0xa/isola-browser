import { createRef } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';

import { ThemeProvider } from '../../lib/ThemeContext';
import { ViewerToolbar, type ViewerToolbarProps } from './ViewerToolbar';

function memoryStorage(): Storage {
  const map = new Map<string, string>();
  return {
    get length() {
      return map.size;
    },
    clear: () => map.clear(),
    getItem: (key: string) => (map.has(key) ? map.get(key)! : null),
    key: (index: number) => Array.from(map.keys())[index] ?? null,
    removeItem: (key: string) => {
      map.delete(key);
    },
    setItem: (key: string, value: string) => {
      map.set(key, String(value));
    },
  };
}

// The toolbar only reads metrics for the signal/quality indicators and the tab
// strip; a real stream hook is unnecessary for these assertions.
function fakeIsola(): ViewerToolbarProps['isola'] {
  const metrics = {
    netQuality: 'good',
    rttMs: 22,
    captureEmptyWindow: 0,
    captureFallback: false,
  };
  return { metrics, browserTabs: [] } as unknown as ViewerToolbarProps['isola'];
}

const baseProps: ViewerToolbarProps = {
  hideToolbar: false,
  id: 'sess-1234abcd',
  showOverlay: false,
  statusText: 'Running',
  isReconnecting: false,
  isConnected: true,
  isola: fakeIsola(),
  viewerCount: 0,
  remainingSec: 300,
  isUnlimited: false,
  status: 'running',
  extendAvailable: true,
  viewOnly: false,
  onExtend: async () => true,
  isFullscreen: false,
  tabBarSlotRef: createRef<HTMLDivElement>(),
  rendererStallVisible: false,
  rendererStalledSec: 0,
  captureStallVisible: false,
  clipboardSynced: false,
  menuOpen: true,
  setMenuOpen: () => {},
  audioMuted: false,
  toggleAudio: () => {},
  toggleFullscreen: () => {},
  debugEnabled: false,
  toggleDebug: () => {},
  networkCaptureEnabled: false,
  captureEnabled: true,
  captureState: 'idle',
  captureStatus: null,
  captureSummary: null,
  captureError: '',
  onToggleCapture: () => {},
  onToggleCapturePanel: () => {},
  onOpenCaptures: () => {},
  captureHasKey: true,
  netPanel: 'off',
  setNetPanel: () => {},
  capturePanel: 'off',
  openNetworkPopOut: () => {},
  onOpenSessionData: () => {},
  shareCopied: false,
  handleShare: () => {},
  toolbarButtons: [],
  restarting: false,
  sessionTargetUrl: 'https://example.com',
  sessionParams: {
    resolution: '1280x720',
    frameRate: 30,
    browserType: 'chromium',
    timezone: 'Europe/Stockholm',
    locale: 'sv-SE',
    torEnabled: false,
    torCountry: '',
  },
  handleRestart: () => {},
  handleEndSession: () => {},
  onRecoverRenderer: () => {},
};

function renderToolbar(overrides: Partial<ViewerToolbarProps> = {}) {
  return render(
    <MemoryRouter>
      <ThemeProvider>
        <ViewerToolbar {...baseProps} {...overrides} />
      </ThemeProvider>
    </MemoryRouter>,
  );
}

describe('ViewerToolbar capture row', () => {
  beforeEach(() => {
    vi.stubGlobal('localStorage', memoryStorage());
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('offers Record resources and calls the toggle', () => {
    const onToggleCapture = vi.fn();
    renderToolbar({ onToggleCapture });

    const row = screen.getByRole('menuitem', { name: /Record resources/ });
    expect(row).toBeEnabled();

    fireEvent.click(row);
    expect(onToggleCapture).toHaveBeenCalledTimes(1);
  });

  it('disables the row when capture is off or the stream is not connected', () => {
    renderToolbar({ captureEnabled: false });
    expect(screen.getByRole('menuitem', { name: /Record resources/ })).toBeDisabled();
    cleanup();

    renderToolbar({ isConnected: false });
    expect(screen.getByRole('menuitem', { name: /Record resources/ })).toBeDisabled();
  });

  it('disables the row while starting or stopping', () => {
    renderToolbar({ captureState: 'starting' });
    expect(screen.getByRole('menuitem', { name: /Record resources/ })).toBeDisabled();
    cleanup();

    renderToolbar({ captureState: 'stopping' });
    expect(screen.getByRole('menuitem', { name: /Record resources/ })).toBeDisabled();
  });

  it('shows Stop recording, a toggled row, and the REC chip while recording', () => {
    renderToolbar({
      captureState: 'recording',
      captureStatus: {
        recording: true,
        started_at: new Date(Date.now() - 5000).toISOString(),
        items: 2,
        bytes: 2048,
        deduped: 0,
        truncated: false,
      },
    });

    expect(screen.getByRole('menuitem', { name: /Stop recording/ })).toBeEnabled();
    expect(screen.getByRole('button', { name: /REC 00:0[45] · 2\.0 KB/ })).toBeInTheDocument();
  });

  it('shows the saved chip after a stop and opens the panel from it', () => {
    const onToggleCapturePanel = vi.fn();
    renderToolbar({
      captureSummary: { items: 3, bytes: 4096, deduped: 1, complete: true },
      onToggleCapturePanel,
    });

    const chip = screen.getByRole('button', { name: /Saved 3 files · 4\.0 KB/ });
    fireEvent.click(chip);
    expect(onToggleCapturePanel).toHaveBeenCalledTimes(1);
  });

  it('offers Saved files and calls the panel toggle', () => {
    const onToggleCapturePanel = vi.fn();
    renderToolbar({ onToggleCapturePanel });

    const row = screen.getByRole('menuitem', { name: /Saved files/ });
    expect(row).toBeEnabled();

    fireEvent.click(row);
    expect(onToggleCapturePanel).toHaveBeenCalledTimes(1);
  });

  it('marks the Saved files row toggled while the panel is open', () => {
    renderToolbar({ capturePanel: 'side' });
    // The label flips to Hide while the panel is docked, matching the
    // network row's wording.
    const row = screen.getByRole('menuitem', { name: /Hide saved files/ });
    expect(row).toBeEnabled();
    expect(screen.queryByRole('menuitem', { name: /^Saved files$/ })).toBeNull();
  });

  it('disables the Saved files row without an archive key or capture support', () => {
    renderToolbar({ captureHasKey: false });
    expect(screen.getByRole('menuitem', { name: /Saved files/ })).toBeDisabled();
    cleanup();

    renderToolbar({ captureEnabled: false });
    expect(screen.getByRole('menuitem', { name: /Saved files/ })).toBeDisabled();
  });

  it('offers Custom data to the owner and calls the handler', () => {
    const onOpenSessionData = vi.fn();
    renderToolbar({ onOpenSessionData });

    const row = screen.getByRole('menuitem', { name: /Custom data/ });
    expect(row).toBeEnabled();

    fireEvent.click(row);
    expect(onOpenSessionData).toHaveBeenCalledTimes(1);
  });

  it('hides Custom data from a view-only share and disables it while disconnected', () => {
    renderToolbar({ viewOnly: true });
    expect(screen.queryByRole('menuitem', { name: /Custom data/ })).toBeNull();
    cleanup();

    renderToolbar({ isConnected: false });
    expect(screen.getByRole('menuitem', { name: /Custom data/ })).toBeDisabled();
  });

  it('shows the stored launch parameters on hover over the session id', () => {
    renderToolbar();

    const chip = screen.getByText('sess-123');
    expect(chip).toHaveAttribute('title');
    const title = chip.getAttribute('title') || '';
    expect(title).toContain('1280x720 · 30 fps · chromium');
    expect(title).toContain('timezone Europe/Stockholm · locale sv-SE');
    expect(title).toContain('tor off');
  });

  it('surfaces a capture error inline', () => {
    renderToolbar({ captureError: 'already recording', captureState: 'error' });
    const chip = screen.getByText('capture failed');
    expect(chip).toHaveAttribute('title', 'already recording');
  });
});
