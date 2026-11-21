import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import type * as captureApiModule from '../lib/captureApi';

vi.mock('../lib/captureApi', async (importOriginal) => {
  const actual = await importOriginal<typeof captureApiModule>();
  return {
    ...actual,
    getArchive: vi.fn().mockResolvedValue({
      meta: { archive_id: 'sess-1', session_id: 'sess-1', complete: true },
      items: [],
    }),
  };
});

import { CapturePanel } from './CapturePanel';
import { ThemeProvider } from '../lib/ThemeContext';
import { initAppConfig } from '../lib/config';
import { getArchive, setArchiveKey, type CaptureItem, type CaptureStatus } from '../lib/captureApi';

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

const recordingStatus: CaptureStatus = {
  recording: true,
  items: 4,
  bytes: 2048,
  deduped: 0,
  truncated: false,
};

function archiveOf(items: CaptureItem[]) {
  return { meta: { archive_id: 'sess-1', session_id: 'sess-1', complete: true }, items };
}

function captureItem(id: string, sha256: string, size: number): CaptureItem {
  return {
    id,
    url: `https://a.example/${id}.js`,
    mime: 'text/javascript',
    size_decoded: size,
    sha256,
    state: 'captured',
  };
}

function renderPanel(overrides: Partial<Parameters<typeof CapturePanel>[0]> = {}) {
  const props = {
    archiveId: 'sess-1',
    status: null,
    layout: 'bottom' as const,
    onLayout: vi.fn(),
    onClose: vi.fn(),
    onPopOut: vi.fn(),
    ...overrides,
  };
  render(
    <ThemeProvider>
      <CapturePanel {...props} />
    </ThemeProvider>,
  );
  return props;
}

describe('CapturePanel', () => {
  beforeEach(() => {
    vi.stubGlobal('localStorage', memoryStorage());
    initAppConfig({ capture_enabled: true });
    setArchiveKey('sess-1', 'key-1');
    vi.mocked(getArchive).mockResolvedValue(archiveOf([]));
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('titles the overlay Resources and shows the live recording counts', () => {
    renderPanel({ status: recordingStatus });
    expect(screen.getByText('Resources')).toBeInTheDocument();
    expect(screen.getByTestId('capture-header-counts')).toHaveTextContent('Recording — 4 files');
    // No hover fade: the panel must stay readable over a live page.
    expect(document.querySelector('.z-30')?.className).not.toContain('opacity-');
  });

  it('shows deduped files and bytes once the recording has stopped', async () => {
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([
        captureItem('r1', 'a'.repeat(64), 2048),
        captureItem('r2', 'b'.repeat(64), 1024),
      ]),
    );
    renderPanel({ status: { ...recordingStatus, recording: false } });
    await waitFor(() =>
      expect(screen.getByTestId('capture-header-counts')).toHaveTextContent('2 files · 3.0 KB'),
    );
    expect(screen.getByTestId('capture-header-counts').querySelector('.animate-pulse')).toBeNull();
  });

  it('focuses the filter and exposes the sort control when the panel opens', () => {
    renderPanel();
    expect(screen.getByLabelText('Filter saved resources')).toHaveFocus();
    const byPath = screen.getByRole('button', { name: 'path' });
    expect(byPath).toHaveAttribute('aria-pressed', 'true');
    fireEvent.click(screen.getByRole('button', { name: 'time' }));
    expect(screen.getByRole('button', { name: 'time' })).toHaveAttribute('aria-pressed', 'true');
    expect(byPath).toHaveAttribute('aria-pressed', 'false');
  });

  it('switches the dock side through the layout toggle', () => {
    const props = renderPanel({ layout: 'bottom' });
    fireEvent.click(screen.getByTitle('Move panel to the side'));
    expect(props.onLayout).toHaveBeenCalledWith('side');
  });

  it('offers the opposite toggle when docked to the side', () => {
    const props = renderPanel({ layout: 'side' });
    fireEvent.click(screen.getByTitle('Move panel to the bottom'));
    expect(props.onLayout).toHaveBeenCalledWith('bottom');
  });

  it('closes through the ✕ button', () => {
    const props = renderPanel();
    fireEvent.click(screen.getByTitle('Close panel'));
    expect(props.onClose).toHaveBeenCalledTimes(1);
  });

  it('opens the standalone page through ↗ new tab', () => {
    const props = renderPanel();
    fireEvent.click(screen.getByTitle('Open saved resources in a new browser tab'));
    expect(props.onPopOut).toHaveBeenCalledTimes(1);
  });

  it('hides the new-tab action when no pop-out is wired', () => {
    renderPanel({ onPopOut: undefined });
    expect(screen.queryByTitle('Open saved resources in a new browser tab')).toBeNull();
  });
});
