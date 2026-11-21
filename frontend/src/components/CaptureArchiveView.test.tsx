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
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
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
    expect(await screen.findByText('No captured files')).toBeInTheDocument();
    expect(getArchive).toHaveBeenCalledTimes(2);
  });

  it('shows a recording-specific empty state', async () => {
    vi.mocked(getArchive).mockResolvedValue(archiveOf([]));
    renderView({ recording: true });

    expect(await screen.findByText('Recording — nothing captured yet')).toBeInTheDocument();
    expect(screen.queryByText('No captured files')).toBeNull();
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

  it('groups files by host and collapses a host section', async () => {
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([
        item({
          id: 'r1',
          sha256: 'a'.repeat(64),
          url: 'https://a.example/',
          filename: 'index.html',
          file: 'files/a.example/index.html',
          mime: 'text/html',
        }),
        item({
          id: 'r2',
          sha256: 'b'.repeat(64),
          url: 'https://b.example/app.js',
          filename: 'app.js',
          file: 'files/b.example/app.js',
        }),
      ]),
    );

    renderView();
    const hostA = await screen.findByRole('button', { name: /^a\.example/ });
    expect(hostA).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByTitle('files/a.example/index.html')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^b\.example/ })).toHaveAttribute('aria-expanded', 'true');

    fireEvent.click(hostA);
    expect(hostA).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByTitle('files/a.example/index.html')).toBeNull();
    expect(screen.getByTitle('files/b.example/app.js')).toBeInTheDocument();

    fireEvent.click(hostA);
    expect(hostA).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByTitle('files/a.example/index.html')).toBeInTheDocument();
  });

  it('filters by host, hiding the other host sections', async () => {
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([
        item({ id: 'r1', sha256: 'a'.repeat(64), file: 'files/a.example/index.html', url: 'https://a.example/' }),
        item({ id: 'r2', sha256: 'b'.repeat(64), file: 'files/b.example/app.js', url: 'https://b.example/app.js' }),
      ]),
    );

    renderView();
    await screen.findByTitle('files/a.example/index.html');

    fireEvent.change(screen.getByLabelText('Filter saved resources'), { target: { value: 'b.example' } });
    expect(screen.queryByTitle('files/a.example/index.html')).toBeNull();
    expect(screen.getByTitle('files/b.example/app.js')).toBeInTheDocument();
    expect(screen.getByText('1/2 files')).toBeInTheDocument();
  });

  it('shows a badge only for anomalous states and hides their actions', async () => {
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([
        item({ id: 'r1', sha256: 'a'.repeat(64), file: 'files/a.example/ok.js', state: 'captured' }),
        item({
          id: 'r2',
          sha256: 'b'.repeat(64),
          file: 'files/a.example/huge.bin',
          url: 'https://a.example/huge.bin',
          mime: 'application/octet-stream',
          state: 'too_large',
        }),
      ]),
    );

    renderView();
    await screen.findByText('too_large');
    // The normal case costs no badge.
    expect(screen.queryByText('captured')).toBeNull();
    // Only the captured row may offer actions.
    expect(screen.getAllByTitle('Save this file')).toHaveLength(1);
    expect(screen.getAllByRole('button', { name: 'Open' })).toHaveLength(1);
  });

  it('drops the duplicate count in panel mode but keeps it on the full page', async () => {
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([item({ sha256: 'a'.repeat(64), file: 'files/a.example/app.js' })]),
    );

    const panel = renderView({ layout: 'panel' });
    await screen.findByTitle('files/a.example/app.js');
    expect(screen.queryByText('1 file')).toBeNull();
    panel.unmount();

    renderView();
    expect(await screen.findByText('1 file')).toBeInTheDocument();
  });

  it('renders two-line panel rows with a host-relative tail-truncated path', async () => {
    const full = 'files/a.example/get-inspire/3c1d19c74beb41e2bc872f156bd30c54/grid.webp';
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([
        item({
          sha256: 'a'.repeat(64),
          url: 'https://a.example/get-inspire/3c1d19c74beb41e2bc872f156bd30c54/grid.webp',
          filename: 'grid.webp',
          file: full,
          mime: 'image/webp',
        }),
      ]),
    );

    renderView({ layout: 'panel' });
    const label = await screen.findByTitle(full);
    expect(label).toHaveAttribute('aria-label', full);
    expect(label.textContent).not.toContain('files/a.example/');
    // Tail kept: the hash and file name distinguish sibling images.
    expect(label.textContent).toBe('…/3c1d19c74beb41e2bc872f156bd30c54/grid.webp');
    // Compact rows are not a table; actions are icon-only.
    expect(document.querySelector('table')).toBeNull();
    expect(screen.queryByText('Download')).toBeNull();
    expect(screen.getAllByLabelText('Download')).toHaveLength(1);
    expect(screen.getAllByLabelText('Open')).toHaveLength(1);

    cleanup();
    renderView();
    expect(await screen.findByRole('columnheader', { name: 'file / url' })).toBeInTheDocument();
    expect(screen.getAllByTitle('Save this file')).toHaveLength(1);
  });

  it('lists every shared URL in the detail and copies the chosen one', async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    const first = 'https://a.example/app.js';
    const second = 'https://cdn.example/app.js';
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([
        item({ id: 'r1', sha256: 'a'.repeat(64), url: first, file: 'files/a.example/app.js' }),
        item({ id: 'r2', sha256: 'a'.repeat(64), url: second, file: 'files/cdn.example/app.js' }),
      ]),
    );

    renderView();
    fireEvent.click(await screen.findByText('shared by 2 URLs'));

    expect(screen.getByText(first)).toBeInTheDocument();
    expect(screen.getByText(second)).toBeInTheDocument();
    fireEvent.click(screen.getByLabelText(`Copy URL ${second}`));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(second));
  });

  it('sorts by natural path by default and can switch to size', async () => {
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([
        item({ id: 'r1', sha256: 'a'.repeat(64), file: 'files/a.example/a10.js', url: 'https://a.example/a10.js', size_decoded: 100 }),
        item({ id: 'r2', sha256: 'b'.repeat(64), file: 'files/a.example/a2.js', url: 'https://a.example/a2.js', size_decoded: 300 }),
        item({ id: 'r3', sha256: 'c'.repeat(64), file: 'files/a.example/b.js', url: 'https://a.example/b.js', size_decoded: 200 }),
      ]),
    );

    renderView();
    const labels = () => screen.getAllByTestId('capture-file-path').map((el) => el.getAttribute('title'));
    await waitFor(() => expect(labels()).toEqual([
      'files/a.example/a2.js',
      'files/a.example/a10.js',
      'files/a.example/b.js',
    ]));

    fireEvent.click(screen.getByRole('button', { name: 'size' }));
    expect(labels()).toEqual([
      'files/a.example/a2.js',
      'files/a.example/b.js',
      'files/a.example/a10.js',
    ]);
  });

  it('closes the preview on Escape', async () => {
    vi.mocked(getArchive).mockResolvedValue(archiveOf([item({ mime: 'text/plain' })]));
    vi.mocked(fetchArchiveBlob).mockResolvedValue(new Blob(['LET IT BE'], { type: 'text/plain' }));

    renderView();
    fireEvent.click(await screen.findByRole('button', { name: 'Open' }));
    expect(await screen.findByText('LET IT BE')).toBeInTheDocument();

    fireEvent.keyDown(window, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByText('LET IT BE')).toBeNull());
  });
});
