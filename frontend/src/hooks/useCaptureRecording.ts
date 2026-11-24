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
import { apiErrorStatus } from '../lib/apiError';
import { getSessionToken } from '../lib/sessionToken';

export type CaptureState = 'idle' | 'starting' | 'recording' | 'stopping' | 'stopped' | 'error';

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
  refresh: () => Promise<CaptureSessionStatus | null>;
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

  const refresh = useCallback(async (): Promise<CaptureSessionStatus | null> => {
    if (!sessionId) return null;
    try {
      const next: CaptureSessionStatus = await getCaptureStatus(sessionId, getSessionToken(sessionId));
      if (!mountedRef.current || !next) return null;
      setStatus(next);
      if (next.archive_id) setArchiveId(next.archive_id);
      setError('');
      if (next.recording) {
        setState((cur) => (cur === 'stopping' || cur === 'error' ? cur : 'recording'));
      } else {
        // An archive exists once a recording has started, even after a clean
        // stop. Recording is one-shot per session (the archive id is the
        // session id), so the row must read "saved" instead of offering a start
        // that the backend answers with archive_exists.
        setState((cur) => {
          if (cur === 'stopping') return 'stopped';
          if (next.items > 0 || next.started_at) return 'stopped';
          return cur === 'error' ? 'error' : 'idle';
        });
      }
      return next;
    } catch (err) {
      if (!mountedRef.current) return null;
      // 404 = never recorded (or the session is already gone); 401/403 = this
      // viewer has no control token. Both are a stable idle state, not a
      // capture error — the session-level error already explains a no-token
      // deep link, and a share viewer never owns capture.
      const status = apiErrorStatus(err);
      if (status === 400 || status === 404 || status === 401 || status === 403) {
        setState((cur) => (cur === 'stopping' ? cur : 'idle'));
        return null;
      }
      setError(captureErrorText(err));
      return null;
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

  // Reloading the viewer (or opening one for a recording started elsewhere)
  // must re-read the authoritative status; the hook used to start in idle and
  // only poll while it believed it was recording.
  useEffect(() => {
    if (!sessionId) return;
    void refresh();
  }, [sessionId, refresh]);

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
      // A 409 means the server already has a recording or a final archive for
      // this session (another tab, or a Stop this viewer has not seen yet).
      // Adopt that state instead of showing a dead-end error.
      if (apiErrorStatus(err) === 409) {
        const next = await refresh();
        if (!mountedRef.current) return;
        if (!next) {
          // The status read failed too: report the conflict instead of leaving
          // the row stuck on "starting".
          setError(captureErrorText(err));
          setState('error');
        }
        return;
      }
      setError(captureErrorText(err));
      setState('error');
    } finally {
      busyRef.current = false;
    }
  }, [sessionId, refresh]);

  const stop = useCallback(async () => {
    if (!sessionId || busyRef.current) return;
    busyRef.current = true;
    setError('');
    setState('stopping');
    try {
      const res = await stopCapture(sessionId, getSessionToken(sessionId));
      if (!mountedRef.current) return;
      setSummary(res);
      // Leave `stopping` for refresh() to resolve into `stopped`.
      await refresh();
      if (mountedRef.current) {
        setState((cur) => (cur === 'stopping' ? 'stopped' : cur));
      }
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
