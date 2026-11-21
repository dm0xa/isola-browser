// Recording state for one session's resource capture.
//
// The server is authoritative: `start`/`stop` only record what the backend
// answered, and while recording the hook re-reads status every 5 s so a capture
// stopped elsewhere (session end, quota) shows up here. Unmounting does NOT stop
// the recording — it lives on the host — so the cleanup only clears timers.
import { useCallback, useEffect, useRef, useState } from 'react';

import {
  captureErrorText,
  getArchive,
  getArchiveKey,
  getCaptureStatus,
  setArchiveKey,
  startCapture,
  stopCapture,
  type CaptureSessionStatus,
  type CaptureStatus,
  type CaptureStopResult,
} from '../lib/captureApi';
import { getSessionToken } from '../lib/sessionToken';

export type CaptureState = 'idle' | 'starting' | 'recording' | 'stopping' | 'error';

const STATUS_POLL_MS = 5000;
// The manifest is heavier than the status line (one object per file), so the
// panel's `saved` markers refresh on their own cadence.
const MANIFEST_POLL_MS = 15000;

export interface UseCaptureRecording {
  state: CaptureState;
  status: CaptureStatus | null;
  summary: CaptureStopResult | null;
  error: string;
  archiveId: string;
  /** sha1 digests of item bodies already persisted, for the network panel. */
  savedHashes: ReadonlySet<string>;
  start: () => Promise<void>;
  stop: () => Promise<void>;
  refresh: () => Promise<void>;
}

export function useCaptureRecording(sessionId: string | undefined): UseCaptureRecording {
  const [state, setState] = useState<CaptureState>('idle');
  const [status, setStatus] = useState<CaptureStatus | null>(null);
  const [summary, setSummary] = useState<CaptureStopResult | null>(null);
  const [error, setError] = useState('');
  const [archiveId, setArchiveId] = useState('');
  const [savedHashes, setSavedHashes] = useState<ReadonlySet<string>>(() => new Set());
  const mountedRef = useRef(true);
  // Guards a double-click on the menu row: the row is disabled while starting or
  // stopping, but two mutations can still land in the same tick.
  const busyRef = useRef(false);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  // A different session means a different recording: drop the previous state
  // instead of showing its counters and archive id.
  const sessionRef = useRef(sessionId);
  useEffect(() => {
    if (sessionRef.current === sessionId) return;
    sessionRef.current = sessionId;
    setState('idle');
    setStatus(null);
    setSummary(null);
    setError('');
    setArchiveId('');
    setSavedHashes(new Set());
  }, [sessionId]);

  const refresh = useCallback(async () => {
    if (!sessionId) return;
    try {
      const next: CaptureSessionStatus = await getCaptureStatus(sessionId, getSessionToken(sessionId));
      if (!mountedRef.current) return;
      setStatus(next);
      if (next.archive_id) setArchiveId(next.archive_id);
      setError('');
      if (next.recording) {
        setState((cur) => (cur === 'stopping' || cur === 'error' ? cur : 'recording'));
      } else {
        setState((cur) => (cur === 'stopping' ? cur : 'idle'));
      }
    } catch (err) {
      if (!mountedRef.current) return;
      // 404 = never recorded (or the session is already gone): a stable idle
      // state, not an error worth shouting about.
      if (err && typeof err === 'object' && 'response' in err) {
        const resp = err.response;
        if (resp && typeof resp === 'object' && 'status' in resp && resp.status === 404) {
          setState((cur) => (cur === 'stopping' ? cur : 'idle'));
          return;
        }
      }
      setError(captureErrorText(err));
    }
  }, [sessionId]);

  const refreshManifest = useCallback(async () => {
    if (!sessionId) return;
    const id = archiveId || sessionId;
    const key = getArchiveKey(id) || getArchiveKey(sessionId);
    if (!key) return;
    try {
      const archive = await getArchive(id, key);
      if (!mountedRef.current) return;
      const hashes = new Set<string>();
      for (const item of archive.items) {
        if (item.sha1 && item.state === 'captured') hashes.add(item.sha1);
      }
      setSavedHashes(hashes);
    } catch {
      // Manifest refresh is best-effort; the recording itself is unaffected.
    }
  }, [sessionId, archiveId]);

  const start = useCallback(async () => {
    if (!sessionId || busyRef.current) return;
    const token = getSessionToken(sessionId);
    if (!token) {
      setError('Session token unavailable — reopen the session to record.');
      setState('error');
      return;
    }
    busyRef.current = true;
    setError('');
    setSummary(null);
    setState('starting');
    try {
      const res = await startCapture(sessionId, token);
      if (!mountedRef.current) return;
      setArchiveId(res.archive_id);
      setArchiveKey(res.archive_id, res.archive_key);
      setStatus({
        recording: true,
        session_id: sessionId,
        started_at: res.started_at || new Date().toISOString(),
        items: 0,
        bytes: 0,
        deduped: 0,
        truncated: false,
      });
      setState('recording');
    } catch (err) {
      if (!mountedRef.current) return;
      setError(captureErrorText(err));
      setState('error');
    } finally {
      busyRef.current = false;
    }
  }, [sessionId]);

  const stop = useCallback(async () => {
    if (!sessionId || busyRef.current) return;
    busyRef.current = true;
    setError('');
    setState('stopping');
    try {
      const res = await stopCapture(sessionId, getSessionToken(sessionId));
      if (!mountedRef.current) return;
      setSummary(res);
      setState('idle');
      await refresh();
      await refreshManifest();
    } catch (err) {
      if (!mountedRef.current) return;
      setError(captureErrorText(err));
      setState('error');
    } finally {
      busyRef.current = false;
    }
  }, [sessionId, refresh, refreshManifest]);

  useEffect(() => {
    if (state !== 'recording') return;
    const timer = setInterval(() => {
      void refresh();
    }, STATUS_POLL_MS);
    return () => clearInterval(timer);
  }, [state, refresh]);

  useEffect(() => {
    if (state !== 'recording') return;
    void refreshManifest();
    const timer = setInterval(() => {
      void refreshManifest();
    }, MANIFEST_POLL_MS);
    return () => clearInterval(timer);
  }, [state, refreshManifest]);

  return { state, status, summary, error, archiveId, savedHashes, start, stop, refresh };
}
