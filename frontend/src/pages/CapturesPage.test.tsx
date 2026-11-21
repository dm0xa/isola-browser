import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
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

import CapturesPage from './CapturesPage';
import { initAppConfig } from '../lib/config';
import {
  archiveFileUrl,
  fetchArchiveBlob,
  fetchArchiveFile,
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
    filename: 'a.example_a1b2c3d4e5f6.js',
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

function renderPage(entry = '/captures/sess-1') {
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <Routes>
        <Route path="/captures/:id" element={<CapturesPage />} />
        <Route path="/session/:id/captures" element={<CapturesPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe('CapturesPage', () => {
  beforeEach(() => {
    vi.stubGlobal('localStorage', memoryStorage());
    initAppConfig({ capture_enabled: true });
    setArchiveKey('sess-1', 'key-1');
    vi.mocked(getArchive).mockReset();
    vi.mocked(fetchArchiveBlob).mockReset();
    vi.mocked(fetchArchiveFile).mockReset();
    vi.mocked(saveBlob).mockReset();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
  });

  it('dedupes one row per sha256 and reports URL sharing and versions', async () => {
    const shaShared = 'a'.repeat(64);
    const shaSecond = 'b'.repeat(64);
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([
        item({ id: 'r1', url: 'https://a.example/app.js', sha256: shaShared, filename: 'app.js', ts_saved: 1000 }),
        item({
          id: 'r2',
          url: 'https://cdn.example/app.js',
          sha256: shaShared,
          filename: 'app.js',
          ts_saved: 1100,
        }),
        item({ id: 'r3', url: 'https://a.example/app.js', sha256: shaSecond, filename: 'app.v2.js', ts_saved: 2000 }),
      ]),
    );

    renderPage();

    await screen.findByText('app.v2.js');
    // Two distinct contents → two rows, even though three items were captured.
    expect(screen.getAllByText(/^app(\.v2)?\.js$/)).toHaveLength(2);
    expect(screen.getByText(/shared by 2 URLs/)).toBeInTheDocument();
    // The same URL produced two hashes: the newer one is v2.
    expect(screen.getByText('v2')).toBeInTheDocument();
    expect(screen.getByText('v1')).toBeInTheDocument();
  });

  it('downloads by sha256 when the item has no readable path', async () => {
    vi.mocked(getArchive).mockResolvedValue(archiveOf([item({ filename: 'app.js' })]));
    const blob = new Blob(['x'], { type: 'text/javascript' });
    vi.mocked(fetchArchiveBlob).mockResolvedValue(blob);

    renderPage();
    fireEvent.click(await screen.findByTitle('Save this file'));

    await waitFor(() =>
      expect(fetchArchiveBlob).toHaveBeenCalledWith('sess-1', 'a'.repeat(64), 'key-1'),
    );
    await waitFor(() => expect(saveBlob).toHaveBeenCalledWith(blob, 'app.js'));
    expect(fetchArchiveFile).not.toHaveBeenCalled();
  });

  it('shows the readable path in the row and downloads through the file route', async () => {
    const file = 'files/example.com/s.js';
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([item({ filename: 's.js', file })]),
    );
    const blob = new Blob(['x'], { type: 'text/javascript' });
    vi.mocked(fetchArchiveFile).mockResolvedValue(blob);

    renderPage();

    const label = await screen.findByText(file);
    expect(label).toHaveAttribute('title', file);

    fireEvent.click(screen.getByTitle('Save this file'));

    // The page hands the item's archive-root path to the API helper unchanged;
    // the helper drops the leading `files/` the route already scopes.
    await waitFor(() => expect(fetchArchiveFile).toHaveBeenCalledWith('sess-1', file, 'key-1'));
    const [calledId, calledFile] = vi.mocked(fetchArchiveFile).mock.calls[0];
    expect(archiveFileUrl(calledId, calledFile)).toBe('/api/captures/sess-1/files/example.com/s.js');
    await waitFor(() => expect(saveBlob).toHaveBeenCalledWith(blob, 's.js'));
    expect(fetchArchiveBlob).not.toHaveBeenCalled();
  });

  it('truncates a long path from the left, keeping the file name', async () => {
    const file = 'files/example.com/very/deep/tree/that/keeps/going/w/load.php';
    vi.mocked(getArchive).mockResolvedValue(archiveOf([item({ filename: 'load.php', file })]));

    renderPage();

    const label = await screen.findByTitle(file);
    expect(label.textContent?.startsWith('…/')).toBe(true);
    expect(label.textContent?.endsWith('/w/load.php')).toBe(true);
    expect(label.textContent?.length).toBeLessThan(file.length);
  });

  it('copies the readable path from the expanded detail', async () => {
    const file = 'files/a.example/app.js';
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    });
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([item({ filename: 'app.js', file })]),
    );
    vi.mocked(fetchArchiveBlob).mockResolvedValue(new Blob(['x'], { type: 'text/javascript' }));

    renderPage();
    fireEvent.click(await screen.findByRole('button', { name: 'Open' }));

    const button = await screen.findByRole('button', { name: 'Copy file path' });
    fireEvent.click(button);

    await waitFor(() => expect(writeText).toHaveBeenCalledWith(file));
    expect(await screen.findByText('copied')).toBeInTheDocument();
  });

  it('shows archived HTML as source text, never as markup', async () => {
    const html = '<img src=x onerror="window.__pwned=1">';
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([
        item({ id: 'r1', url: 'https://a.example/page.html', mime: 'text/html', filename: 'page.html' }),
      ]),
    );
    vi.mocked(fetchArchiveBlob).mockResolvedValue(new Blob([html], { type: 'text/html' }));

    renderPage();
    fireEvent.click(await screen.findByRole('button', { name: 'Open' }));

    await screen.findByText(html);
    expect(document.querySelector('img')).toBeNull();
    expect(screen.getByText(html).tagName).toBe('PRE');
  });

  it('shows an empty state when nothing was saved', async () => {
    vi.mocked(getArchive).mockResolvedValue(archiveOf([]));
    renderPage();
    expect(await screen.findByText('No resources saved yet.')).toBeInTheDocument();
  });

  it('renders the backend error inline and retries', async () => {
    vi.mocked(getArchive).mockRejectedValueOnce({
      response: { status: 404, data: { error: { code: 'archive_not_found', message: 'archive is gone' } } },
    });
    vi.mocked(getArchive).mockResolvedValueOnce(archiveOf([]));

    renderPage();
    expect(await screen.findByText('archive is gone')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    expect(await screen.findByText('No resources saved yet.')).toBeInTheDocument();
    expect(getArchive).toHaveBeenCalledTimes(2);
  });

  it('explains a missing archive key instead of calling the API', async () => {
    localStorage.clear();
    renderPage();

    expect(await screen.findByText(/does not hold the archive key/)).toBeInTheDocument();
    expect(getArchive).not.toHaveBeenCalled();
  });

  it('filters rows by url, filename, or mime', async () => {
    vi.mocked(getArchive).mockResolvedValue(
      archiveOf([
        item({ id: 'r1', filename: 'app.js', url: 'https://a.example/app.js' }),
        item({
          id: 'r2',
          filename: 'photo.png',
          url: 'https://a.example/photo.png',
          mime: 'image/png',
          sha256: 'c'.repeat(64),
        }),
      ]),
    );

    renderPage();
    await screen.findByText('photo.png');

    fireEvent.change(screen.getByLabelText('Filter saved resources'), { target: { value: 'image/png' } });
    expect(screen.queryByText('app.js')).toBeNull();
    expect(screen.getByText('photo.png')).toBeInTheDocument();
  });
});
