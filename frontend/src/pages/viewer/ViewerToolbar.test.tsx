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
  onOpenCaptures: () => {},
  netPanel: 'off',
  setNetPanel: () => {},
  openNetworkPopOut: () => {},
  shareCopied: false,
  handleShare: () => {},
  toolbarButtons: [],
  restarting: false,
  sessionTargetUrl: 'https://example.com',
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

  it('shows the saved chip after a stop and opens the review page from it', () => {
    const onOpenCaptures = vi.fn();
    renderToolbar({
      captureSummary: { items: 3, bytes: 4096, deduped: 1, complete: true },
      onOpenCaptures,
    });

    const chip = screen.getByRole('button', { name: /Saved 3 files · Review/ });
    fireEvent.click(chip);
    expect(onOpenCaptures).toHaveBeenCalledTimes(1);
  });

  it('surfaces a capture error inline', () => {
    renderToolbar({ captureError: 'already recording', captureState: 'error' });
    const chip = screen.getByText('capture failed');
    expect(chip).toHaveAttribute('title', 'already recording');
  });
});
