// Client for the capture (record downloaded resources) endpoints.
//
// Two credentials are in play and they are not interchangeable:
//   * per-session calls (`/api/sessions/{id}/capture*`) are owner calls and use
//     `X-Session-Token`, exactly like the viewer's session polls.
//   * archive calls (`/api/captures/{archiveID}*`) use the archive key from the
//     start response as `Authorization: Bearer`, so a review tab opened later
//     does not need the session token.
// The archive key lives in localStorage, never in the URL: a query string ends
// up in history, logs and Referer headers.
import { api } from './api';
import { sessionCreateAuthHeaders } from './createAuth';
import { apiErrorMessage } from './apiError';

export interface CaptureItem {
  id: string;
  url: string;
  final_url?: string;
  method?: string;
  status?: number;
  mime?: string;
  type?: string;
  size_wire?: number;
  size_decoded?: number;
  sha256: string;
  sha1?: string;
  filename?: string;
  tab?: number;
  origin_host?: string;
  ts_requested?: number;
  ts_finished?: number;
  ts_saved?: number;
  from_disk_cache?: boolean;
  from_service_worker?: boolean;
  /** captured | too_large | missing_body | excluded_type | failed */
  state: string;
}

export interface CaptureStatus {
  recording: boolean;
  session_id?: string;
  started_at?: string | number | null;
  items: number;
  bytes: number;
  deduped: number;
  truncated: boolean;
  counts?: Record<string, number>;
}

export interface CaptureSessionStatus extends CaptureStatus {
  archive_id?: string;
}

export interface CaptureStartResult {
  archive_id: string;
  archive_key: string;
  started_at?: string;
}

export interface CaptureStopResult {
  items: number;
  bytes: number;
  deduped: number;
  complete: boolean;
}

export interface ArchiveMeta {
  schema_version?: number;
  archive_id: string;
  session_id?: string;
  target_url?: string;
  created_at?: string;
  ended_at?: string;
  complete?: boolean;
  stop_reason?: string;
  counts?: Record<string, number>;
  sizes?: Record<string, number>;
}

export interface CaptureArchive {
  meta: ArchiveMeta;
  items: CaptureItem[];
}

export interface CaptureSummary extends ArchiveMeta {
  items: number;
}

const SESSION_TOKEN_HEADER = 'X-Session-Token';
const ARCHIVE_KEY_PREFIX = 'isola.capture.';

export function archiveStorageKey(archiveId: string): string {
  return `${ARCHIVE_KEY_PREFIX}${archiveId}`;
}

export function getArchiveKey(archiveId: string): string {
  if (typeof localStorage === 'undefined') return '';
  return localStorage.getItem(archiveStorageKey(archiveId)) || '';
}

export function setArchiveKey(archiveId: string, key: string): void {
  if (typeof localStorage === 'undefined') return;
  if (!key) localStorage.removeItem(archiveStorageKey(archiveId));
  else localStorage.setItem(archiveStorageKey(archiveId), key);
}

export function clearArchiveKey(archiveId: string): void {
  if (typeof localStorage === 'undefined') return;
  localStorage.removeItem(archiveStorageKey(archiveId));
}

/** Human-readable message for a failed capture call (inline errors, no toasts). */
export function captureErrorText(err: unknown): string {
  if (err && typeof err === 'object' && 'response' in err) {
    const resp = err.response;
    if (resp && typeof resp === 'object') {
      const status = 'status' in resp && typeof resp.status === 'number' ? resp.status : 0;
      const data = 'data' in resp ? resp.data : undefined;
      return apiErrorMessage(data, status);
    }
  }
  if (err instanceof Error && err.message) return err.message;
  return 'Capture request failed';
}

function ownerConfig(token: string) {
  return token ? { headers: { [SESSION_TOKEN_HEADER]: token } } : undefined;
}

function keyConfig(key: string) {
  return { headers: { Authorization: `Bearer ${key}` } };
}

export async function startCapture(
  sessionId: string,
  token: string,
): Promise<CaptureStartResult> {
  const res = await api.post(`/api/sessions/${sessionId}/capture/start`, undefined, ownerConfig(token));
  return res.data as CaptureStartResult;
}

export async function stopCapture(
  sessionId: string,
  token: string,
): Promise<CaptureStopResult> {
  const res = await api.post(`/api/sessions/${sessionId}/capture/stop`, undefined, ownerConfig(token));
  return res.data as CaptureStopResult;
}

export async function getCaptureStatus(
  sessionId: string,
  token: string,
): Promise<CaptureSessionStatus> {
  const res = await api.get(`/api/sessions/${sessionId}/capture`, ownerConfig(token));
  return res.data as CaptureSessionStatus;
}

/** Operator-level archive list (same bearer posture as the session create gate). */
export async function listCaptures(): Promise<CaptureSummary[]> {
  const res = await api.get('/api/captures', { headers: sessionCreateAuthHeaders() });
  const data = res.data;
  if (Array.isArray(data)) return data as CaptureSummary[];
  const items = (data as { captures?: unknown })?.captures;
  return Array.isArray(items) ? (items as CaptureSummary[]) : [];
}

// The host archive endpoint returns meta.json with its `items` array; accept
// both the flat shape and a `{meta, items}` envelope so a backend refactor does
// not break the review page. Network input: the only unchecked step is the
// record view below, after the object check.
export function parseArchive(data: unknown): CaptureArchive {
  if (!data || typeof data !== 'object') return { meta: { archive_id: '' }, items: [] };
  const rec = data as Record<string, unknown>;
  const metaSource = 'meta' in rec && rec.meta && typeof rec.meta === 'object' ? rec.meta : rec;
  const meta = metaSource as unknown as ArchiveMeta;
  const items = Array.isArray(rec.items) ? (rec.items as CaptureItem[]) : [];
  return { meta, items };
}

export async function getArchive(archiveId: string, key: string): Promise<CaptureArchive> {
  const res = await api.get(`/api/captures/${archiveId}`, keyConfig(key));
  return parseArchive(res.data);
}

export function archiveBlobUrl(archiveId: string, sha256: string): string {
  return `/api/captures/${archiveId}/blobs/${sha256}`;
}

export function archiveDownloadUrl(archiveId: string): string {
  return `/api/captures/${archiveId}/download`;
}

// Body bytes cannot ride an <a href>: a plain navigation sends no Authorization
// header, so every download goes through an authenticated fetch into a Blob.
async function fetchAuthed(url: string, key: string, failMessage: string): Promise<Blob> {
  const res = await fetch(url, { headers: { Authorization: `Bearer ${key}` } });
  if (!res.ok) throw new Error(`${failMessage} (${res.status})`);
  return res.blob();
}

export async function fetchArchiveBlob(
  archiveId: string,
  sha256: string,
  key: string,
): Promise<Blob> {
  return fetchAuthed(archiveBlobUrl(archiveId, sha256), key, 'Could not download file');
}

export async function fetchArchiveZip(archiveId: string, key: string): Promise<Blob> {
  return fetchAuthed(archiveDownloadUrl(archiveId), key, 'Could not download archive');
}

export async function deleteArchive(archiveId: string, key: string): Promise<void> {
  await api.delete(`/api/captures/${archiveId}`, keyConfig(key));
}
