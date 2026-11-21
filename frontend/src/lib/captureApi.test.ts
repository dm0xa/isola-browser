import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AxiosResponse } from 'axios';

vi.mock('./api', () => ({
  api: { get: vi.fn(), post: vi.fn(), delete: vi.fn() },
}));

import { api } from './api';
import {
  archiveBlobUrl,
  archiveDownloadUrl,
  archiveFileUrl,
  captureErrorText,
  clearArchiveKey,
  deleteArchive,
  fetchArchiveBlob,
  fetchArchiveFile,
  getArchive,
  getArchiveKey,
  getCaptureStatus,
  parseArchive,
  setArchiveKey,
  startCapture,
  stopCapture,
} from './captureApi';

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

// A response body is all these calls read; the axios fields are irrelevant.
function ok(data: unknown): AxiosResponse {
  return { data } as AxiosResponse;
}

// api.post(url, body, config) / api.get(url, config) / api.delete(url, config):
// the config is the last argument of every call.
function headersOf(call: unknown[]): Record<string, string> {
  const config = call[call.length - 1] as { headers?: Record<string, string> } | undefined;
  return config?.headers ?? {};
}

describe('per-session capture calls', () => {
  beforeEach(() => {
    vi.mocked(api.post).mockReset();
    vi.mocked(api.get).mockReset();
    vi.mocked(api.delete).mockReset();
  });

  it('starts with the owner session token', async () => {
    vi.mocked(api.post).mockResolvedValue(
      ok({ archive_id: 'sess-1', archive_key: 'k-1', started_at: '2026-11-21T10:00:00Z' }),
    );
    const res = await startCapture('sess-1', 'token-1');
    expect(res.archive_id).toBe('sess-1');
    const [url, , config] = vi.mocked(api.post).mock.calls[0];
    expect(url).toBe('/api/sessions/sess-1/capture/start');
    expect((config as { headers?: Record<string, string> })?.headers).toEqual({
      'X-Session-Token': 'token-1',
    });
  });

  it('stops and reads status with the owner session token', async () => {
    vi.mocked(api.post).mockResolvedValue(ok({ items: 3, bytes: 10, deduped: 1, complete: true }));
    vi.mocked(api.get).mockResolvedValue(
      ok({ recording: true, items: 3, bytes: 10, deduped: 1, truncated: false }),
    );
    await stopCapture('sess-1', 'token-1');
    const status = await getCaptureStatus('sess-1', 'token-1');
    expect(status.recording).toBe(true);

    const stopCall = vi.mocked(api.post).mock.calls[0];
    expect(stopCall[0]).toBe('/api/sessions/sess-1/capture/stop');
    expect(headersOf(stopCall)).toEqual({ 'X-Session-Token': 'token-1' });
    const statusCall = vi.mocked(api.get).mock.calls[0];
    expect(statusCall[0]).toBe('/api/sessions/sess-1/capture');
    expect(headersOf(statusCall)).toEqual({ 'X-Session-Token': 'token-1' });
  });

  it('reports the backend error message verbatim', async () => {
    vi.mocked(api.post).mockRejectedValue({
      response: { status: 409, data: { error: { code: 'capture_failed', message: 'already recording' } } },
    });
    await expect(startCapture('sess-1', 'token-1')).rejects.toEqual(
      expect.objectContaining({ response: expect.objectContaining({ status: 409 }) }),
    );
    expect(
      captureErrorText({
        response: { status: 409, data: { error: { code: 'capture_failed', message: 'already recording' } } },
      }),
    ).toBe('already recording');
  });
});

describe('archive calls', () => {
  beforeEach(() => {
    vi.mocked(api.get).mockReset();
    vi.mocked(api.delete).mockReset();
  });

  it('reads the archive with a bearer key, not a session token', async () => {
    vi.mocked(api.get).mockResolvedValue(ok({ meta: { archive_id: 'sess-1' }, items: [] }));
    const archive = await getArchive('sess-1', 'archive-key');
    expect(archive.meta.archive_id).toBe('sess-1');
    const [url, config] = vi.mocked(api.get).mock.calls[0];
    expect(url).toBe('/api/captures/sess-1');
    expect((config as { headers?: Record<string, string> })?.headers).toEqual({
      Authorization: 'Bearer archive-key',
    });
  });

  it('deletes with the archive key', async () => {
    vi.mocked(api.delete).mockResolvedValue(ok({}));
    await deleteArchive('sess-1', 'archive-key');
    const [url, config] = vi.mocked(api.delete).mock.calls[0];
    expect(url).toBe('/api/captures/sess-1');
    expect((config as { headers?: Record<string, string> })?.headers).toEqual({
      Authorization: 'Bearer archive-key',
    });
  });

  it('builds blob, file and zip URLs', () => {
    expect(archiveBlobUrl('sess-1', 'a'.repeat(64))).toBe(
      `/api/captures/sess-1/blobs/${'a'.repeat(64)}`,
    );
    // `item.file` is archive-root relative; the route takes the path INSIDE
    // files/, so the leading `files/` is stripped and slashes stay slashes.
    expect(archiveFileUrl('sess-1', 'files/example.com/lib/app.js')).toBe(
      '/api/captures/sess-1/files/example.com/lib/app.js',
    );
    // A caller that already passes the route-relative shape is left alone.
    expect(archiveFileUrl('sess-1', 'example.com/lib/app.js')).toBe(
      '/api/captures/sess-1/files/example.com/lib/app.js',
    );
    // Each segment is encoded on its own, never the whole path.
    expect(archiveFileUrl('sess-1', 'files/example.com/a b/c#d?e.js')).toBe(
      '/api/captures/sess-1/files/example.com/a%20b/c%23d%3Fe.js',
    );
    expect(archiveDownloadUrl('sess-1')).toBe('/api/captures/sess-1/download');
  });

  it('downloads by readable path through an authenticated fetch', async () => {
    const blob = new Blob(['hello'], { type: 'text/plain' });
    const fetchMock = vi.fn(async () => ({ ok: true, status: 200, blob: async () => blob }));
    vi.stubGlobal('fetch', fetchMock);

    const out = await fetchArchiveFile('sess-1', 'files/a.example/w/load.php', 'archive-key');
    expect(out).toBe(blob);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe('/api/captures/sess-1/files/a.example/w/load.php');
    expect(init.headers).toEqual({ Authorization: 'Bearer archive-key' });

    vi.unstubAllGlobals();
  });

  it('downloads bytes through an authenticated fetch', async () => {
    const blob = new Blob(['hello'], { type: 'text/plain' });
    const fetchMock = vi.fn(async () => ({ ok: true, status: 200, blob: async () => blob }));
    vi.stubGlobal('fetch', fetchMock);

    const out = await fetchArchiveBlob('sess-1', 'b'.repeat(64), 'archive-key');
    expect(out).toBe(blob);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe(`/api/captures/sess-1/blobs/${'b'.repeat(64)}`);
    expect(init.headers).toEqual({ Authorization: 'Bearer archive-key' });

    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 404, blob: async () => blob })));
    await expect(fetchArchiveBlob('sess-1', 'b'.repeat(64), 'archive-key')).rejects.toThrow('404');
  });

  it('accepts a flat meta.json and a {meta, items} envelope', () => {
    const flat = parseArchive({ archive_id: 's1', items: [{ sha256: 'x', state: 'captured' }] });
    expect(flat.meta.archive_id).toBe('s1');
    expect(flat.items).toHaveLength(1);

    const envelope = parseArchive({ meta: { archive_id: 's2' }, items: [] });
    expect(envelope.meta.archive_id).toBe('s2');
    expect(envelope.items).toEqual([]);

    expect(parseArchive(null)).toEqual({ meta: { archive_id: '' }, items: [] });
  });
});

describe('archive key storage', () => {
  beforeEach(() => {
    vi.stubGlobal('localStorage', memoryStorage());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('stores the key under isola.capture.<archiveId>', () => {
    setArchiveKey('sess-1', 'k-1');
    expect(localStorage.getItem('isola.capture.sess-1')).toBe('k-1');
    expect(getArchiveKey('sess-1')).toBe('k-1');

    setArchiveKey('sess-1', '');
    expect(getArchiveKey('sess-1')).toBe('');

    setArchiveKey('sess-2', 'k-2');
    clearArchiveKey('sess-2');
    expect(localStorage.getItem('isola.capture.sess-2')).toBeNull();
  });
});
