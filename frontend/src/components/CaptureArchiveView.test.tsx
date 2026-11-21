import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import type * as captureApiModule from '../lib/captureApi';
import type * as captureFormatModule from '../lib/captureFormat';

vi.mock('../lib/captureApi', async (importOriginal) => {
  const actual = await importOriginal<typeof captureApiModule>();
  return {
    ...actual,
    getArchive: vi.fn(),
    fetchArchiveBlob: vi.fn(),
    fetchArchiveFile: vi.fn(),
    fetchArchiveZip: vi.fn(),
  };
});

vi.mock('../lib/captureFormat', async (importOriginal) => {
  const actual = await importOriginal<typeof captureFormatModule>();
  return { ...actual, saveBlob: vi.fn() };
});

import { CaptureArchiveView } from './CaptureArchiveView';
import { initAppConfig } from '../lib/config';
import {
  fetchArchiveBlob,
  fetchArchiveFile,
  fetchArchiveZip,
  getArchive,
  setArchiveKey,
  type CaptureItem,
} from '../lib/captureApi';
import { saveBlob } from '../lib/captureFormat';

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

function item(overrides: Partial<CaptureItem>): CaptureItem {
  return {
    id: 'r1',
    url: 'https://a.example/app.js',
    method: 'GET',
    status: 200,
    mime: 'text/javascript',
    size_decoded: 1234,
    sha256: 'a'.repeat(64),
    sha1: 'b'.repeat(40),
    filename: 'app.js',
    ts_saved: 1_700_000_000_000,
    state: 'captured',
    ...overrides,
  };
}

function archiveOf(items: CaptureItem[]) {
  return {
    meta: {
      archive_id: 'sess-1',
      session_id: 'sess-1',
      target_url: 'https://a.example',
      created_at: '2026-11-21T10:00:00Z',
      complete: true,
    },
    items,
  };
}

function renderView(overrides: Partial<Parameters<typeof CaptureArchiveView>[0]> = {}) {
  return render(<CaptureArchiveView archiveId="sess-1" {...overrides} />);
}

describe('CaptureArchiveView', () => {
  beforeEach(() => {
    vi.stubGlobal('localStorage', memoryStorage());
    initAppConfig({ capture_enabled: true });
    setArchiveKey('sess-1', 'key-1');
    vi.mocked(getArchive).mockReset();
    vi.mocked(fetchArchiveBlob).mockReset();
    vi.mocked(fetchArchiveFile).mockReset();
    vi.mocked(fetchArchiveZip).mockReset();
    vi.mocked(saveBlob).mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('dedupes rows by sha256 and reports the deduped count', async () => {
    const onArchive = vi.fn();
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([
        item({ id: 'r1', sha256: 'a'.repeat(64) }),
        item({ id: 'r2', url: 'https://cdn.example/app.js', sha256: 'a'.repeat(64) }),
        item({ id: 'r3', sha256: 'b'.repeat(64), filename: 'app.v2.js' }),
      ]),
    );

    renderView({ onArchive });

    expect(await screen.findByText('app.v2.js')).toBeInTheDocument();
    expect(screen.getAllByText(/^app(\.v2)?\.js$/)).toHaveLength(2);
    await waitFor(() => expect(onArchive).toHaveBeenCalled());
    const last = onArchive.mock.calls[onArchive.mock.calls.length - 1];
    expect(last?.[1]).toBe(2);
  });

  it('downloads an item through its readable files route', async () => {
    const file = 'files/example.com/lib/app.js';
    vi.mocked(getArchive).mockResolvedValue(archiveOf([item({ file })]));
    const blob = new Blob(['x'], { type: 'text/javascript' });
    vi.mocked(fetchArchiveFile).mockResolvedValue(blob);

    renderView();
    fireEvent.click(await screen.findByTitle('Save this file'));

    await waitFor(() => expect(fetchArchiveFile).toHaveBeenCalledWith('sess-1', file, 'key-1'));
    await waitFor(() => expect(saveBlob).toHaveBeenCalledWith(blob, 'app.js'));
    expect(fetchArchiveBlob).not.toHaveBeenCalled();
  });

  it('fetches the zip with the archive key and saves it under the archive id', async () => {
    vi.mocked(getArchive).mockResolvedValue(archiveOf([item({})]));
    const zip = new Blob(['PK'], { type: 'application/zip' });
    vi.mocked(fetchArchiveZip).mockResolvedValue(zip);

    renderView();
    fireEvent.click(await screen.findByTitle('Download every saved file as a zip'));

    await waitFor(() => expect(fetchArchiveZip).toHaveBeenCalledWith('sess-1', 'key-1'));
    await waitFor(() => expect(saveBlob).toHaveBeenCalledWith(zip, 'isola-capture-sess-1.zip'));
  });

  it('shows a load error inline and retries', async () => {
    vi.mocked(getArchive).mockRejectedValueOnce({
      response: { status: 404, data: { error: { code: 'archive_not_found', message: 'archive is gone' } } },
    });
    vi.mocked(getArchive).mockResolvedValueOnce(archiveOf([]));

    renderView();
    expect(await screen.findByText('archive is gone')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(await screen.findByText('No resources saved yet.')).toBeInTheDocument();
    expect(getArchive).toHaveBeenCalledTimes(2);
  });

  it('shows a recording-specific empty state', async () => {
    vi.mocked(getArchive).mockResolvedValue(archiveOf([]));
    renderView({ recording: true });

    expect(await screen.findByText('Recording — no files yet.')).toBeInTheDocument();
    expect(screen.queryByText('No resources saved yet.')).toBeNull();
  });

  it('refetches every 5 s while recording and stops when it ends', async () => {
    vi.useFakeTimers();
    vi.mocked(getArchive).mockResolvedValue(archiveOf([item({})]));
    const { rerender } = renderView({ recording: true });

    await vi.advanceTimersByTimeAsync(0);
    expect(getArchive).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(5000);
    expect(getArchive).toHaveBeenCalledTimes(2);

    await vi.advanceTimersByTimeAsync(5000);
    expect(getArchive).toHaveBeenCalledTimes(3);

    rerender(<CaptureArchiveView archiveId="sess-1" recording={false} />);
    await vi.advanceTimersByTimeAsync(0);
    expect(getArchive).toHaveBeenCalledTimes(4);
    await vi.advanceTimersByTimeAsync(15000);
    expect(getArchive).toHaveBeenCalledTimes(4);
  });
});
