import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type * as captureApiModule from '../lib/captureApi';

vi.mock('../lib/captureApi', async (importOriginal) => {
  const actual = await importOriginal<typeof captureApiModule>();
  return {
    ...actual,
    startCapture: vi.fn(),
    stopCapture: vi.fn(),
    getCaptureStatus: vi.fn(),
    getArchive: vi.fn(),
  };
});

import {
  getArchive,
  getCaptureStatus,
  getArchiveKey,
  startCapture,
  stopCapture,
  type CaptureArchive,
} from '../lib/captureApi';
import { useCaptureRecording } from './useCaptureRecording';

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

function emptyArchive(items: CaptureArchive['items'] = []): CaptureArchive {
  return { meta: { archive_id: 'sess-1' }, items };
}

describe('useCaptureRecording', () => {
  beforeEach(() => {
    vi.stubGlobal('localStorage', memoryStorage());
    vi.stubGlobal('sessionStorage', memoryStorage());
    sessionStorage.setItem('isola_session_token:sess-1', 'token-1');
    vi.mocked(startCapture).mockReset();
    vi.mocked(stopCapture).mockReset();
    vi.mocked(getCaptureStatus).mockReset();
    vi.mocked(getArchive).mockReset();
    vi.mocked(getArchive).mockResolvedValue(emptyArchive());
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('starts with the session token and remembers the archive key', async () => {
    vi.mocked(startCapture).mockResolvedValue({
      archive_id: 'sess-1',
      archive_key: 'key-1',
      started_at: '2026-11-21T10:00:00Z',
    });
    const { result } = renderHook(() => useCaptureRecording('sess-1'));
    expect(result.current.state).toBe('idle');

    await act(async () => {
      await result.current.start();
    });

    expect(startCapture).toHaveBeenCalledWith('sess-1', 'token-1');
    expect(result.current.state).toBe('recording');
    expect(result.current.status?.recording).toBe(true);
    expect(result.current.archiveId).toBe('sess-1');
    expect(getArchiveKey('sess-1')).toBe('key-1');
  });

  it('surfaces a start failure and lands in error', async () => {
    vi.mocked(startCapture).mockRejectedValue({
      response: { status: 409, data: { error: { code: 'capture_failed', message: 'already recording' } } },
    });
    const { result } = renderHook(() => useCaptureRecording('sess-1'));

    await act(async () => {
      await result.current.start();
    });

    expect(result.current.state).toBe('error');
    expect(result.current.error).toBe('already recording');
  });

  it('stops, keeps the summary, and lands in stopped', async () => {
    vi.mocked(startCapture).mockResolvedValue({ archive_id: 'sess-1', archive_key: 'key-1' });
    vi.mocked(stopCapture).mockResolvedValue({ items: 4, bytes: 2048, deduped: 2, complete: true });
    vi.mocked(getCaptureStatus).mockResolvedValue({
      recording: false,
      items: 4,
      bytes: 2048,
      deduped: 2,
      truncated: false,
    });
    const { result } = renderHook(() => useCaptureRecording('sess-1'));

    await act(async () => {
      await result.current.start();
      await result.current.stop();
    });

    expect(stopCapture).toHaveBeenCalledWith('sess-1', 'token-1');
    expect(result.current.state).toBe('stopped');
    expect(result.current.summary).toEqual({ items: 4, bytes: 2048, deduped: 2, complete: true });
  });

  it('polls status every 5 s and follows the server when recording stops', async () => {
    vi.useFakeTimers();
    vi.mocked(startCapture).mockResolvedValue({ archive_id: 'sess-1', archive_key: 'key-1' });
    vi.mocked(getCaptureStatus).mockResolvedValue({
      recording: true,
      items: 2,
      bytes: 512,
      deduped: 0,
      truncated: false,
    });
    const { result } = renderHook(() => useCaptureRecording('sess-1'));

    await act(async () => {
      await result.current.start();
    });
    // The manifest effect also runs once on entering `recording`.
    await act(async () => {});

    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    // One call on mount (restore), one per 5 s poll.
    expect(getCaptureStatus).toHaveBeenCalledTimes(2);
    expect(result.current.state).toBe('recording');

    vi.mocked(getCaptureStatus).mockResolvedValue({
      recording: false,
      items: 2,
      bytes: 512,
      deduped: 0,
      truncated: false,
    });
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    expect(getCaptureStatus).toHaveBeenCalledTimes(3);
    expect(result.current.state).toBe('stopped');
  });

  it('marks saved hashes from the archive manifest', async () => {
    vi.mocked(startCapture).mockResolvedValue({ archive_id: 'sess-1', archive_key: 'key-1' });
    vi.mocked(getArchive).mockResolvedValue(
      emptyArchive([
        { id: 'r1', url: 'https://a.example/app.js', sha256: 'a'.repeat(64), sha1: 'h1', state: 'captured' },
        { id: 'r2', url: 'https://a.example/big.bin', sha256: '', sha1: 'h2', state: 'too_large' },
      ]),
    );
    const { result } = renderHook(() => useCaptureRecording('sess-1'));

    await act(async () => {
      await result.current.start();
    });
    await waitFor(() => expect(result.current.savedHashes.has('h1')).toBe(true));

    expect(result.current.savedHashes.has('h2')).toBe(false);
    expect(getArchive).toHaveBeenCalledWith('sess-1', 'key-1');
  });

  it('stops polling on unmount and never stops the recording implicitly', async () => {
    vi.useFakeTimers();
    vi.mocked(startCapture).mockResolvedValue({ archive_id: 'sess-1', archive_key: 'key-1' });
    vi.mocked(getCaptureStatus).mockResolvedValue({
      recording: true,
      items: 0,
      bytes: 0,
      deduped: 0,
      truncated: false,
    });
    const { result, unmount } = renderHook(() => useCaptureRecording('sess-1'));

    await act(async () => {
      await result.current.start();
    });
    const callsBefore = vi.mocked(getCaptureStatus).mock.calls.length;
    unmount();
    await act(async () => {
      vi.advanceTimersByTime(20000);
    });

    expect(vi.mocked(getCaptureStatus).mock.calls.length).toBe(callsBefore);
    expect(stopCapture).not.toHaveBeenCalled();
  });
});

describe('useCaptureRecording mount restore', () => {
  beforeEach(() => {
    vi.stubGlobal('localStorage', memoryStorage());
    vi.stubGlobal('sessionStorage', memoryStorage());
    sessionStorage.setItem('isola_session_token:sess-1', 'token-1');
    vi.mocked(getCaptureStatus).mockReset();
    vi.mocked(getArchive).mockReset();
    vi.mocked(getArchive).mockResolvedValue(emptyArchive());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('adopts a recording that was started in another tab', async () => {
    vi.mocked(getCaptureStatus).mockResolvedValue({
      recording: true,
      items: 3,
      bytes: 900,
      deduped: 0,
      truncated: false,
      started_at: '2026-11-21T10:00:00Z',
    });
    const { result } = renderHook(() => useCaptureRecording('sess-1'));
    await waitFor(() => expect(result.current.state).toBe('recording'));
  });

  it('shows a finished archive as stopped instead of offering a start', async () => {
    vi.mocked(getCaptureStatus).mockResolvedValue({
      recording: false,
      items: 7,
      bytes: 4096,
      deduped: 1,
      truncated: false,
      started_at: '2026-11-21T10:00:00Z',
    });
    const { result } = renderHook(() => useCaptureRecording('sess-1'));
    await waitFor(() => expect(result.current.state).toBe('stopped'));
    expect(result.current.status?.items).toBe(7);
  });
});
