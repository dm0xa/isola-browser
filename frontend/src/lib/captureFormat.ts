// Pure formatting/grouping helpers for the capture review surface. No React, no
// network: the page fetches, this module decides what to show.
import type { CaptureItem } from './captureApi';

export function formatBytes(bytes: number | null | undefined): string {
  if (!bytes || bytes <= 0) return '0 B';
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}

/** `mm:ss` (or `h:mm:ss` past an hour) for the REC chip. */
export function formatDuration(totalSeconds: number): string {
  const sec = Math.max(0, Math.floor(totalSeconds));
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  const mm = String(m).padStart(2, '0');
  return h > 0 ? `${h}:${mm}:${String(s).padStart(2, '0')}` : `${mm}:${String(s).padStart(2, '0')}`;
}

/** Seconds elapsed since `startedAt`; null when the timestamp is unusable. */
export function elapsedSeconds(
  startedAt: string | number | null | undefined,
  now: number,
): number | null {
  if (startedAt === null || startedAt === undefined || startedAt === '') return null;
  const start = typeof startedAt === 'number' ? startedAt : Date.parse(startedAt);
  if (!Number.isFinite(start)) return null;
  return Math.max(0, (now - start) / 1000);
}

export function shortSha256(sha: string): string {
  return sha.length > 12 ? `${sha.slice(0, 12)}…` : sha;
}

export function formatSavedAt(ts: number | undefined): string {
  if (!ts) return '—';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleString();
}

export type PreviewKind = 'text' | 'image' | 'none';

// HTML is deliberately in the text bucket: the review page shows its source,
// never renders it (archived pages are untrusted and the app origin must stay
// clean).
export function previewKind(mime: string | undefined): PreviewKind {
  const m = (mime || '').toLowerCase();
  if (m.startsWith('image/')) return 'image';
  if (
    m.startsWith('text/') ||
    m === 'application/json' ||
    m === 'application/javascript' ||
    m === 'application/xml' ||
    m === 'application/x-javascript' ||
    m === 'application/manifest+json' ||
    m === 'application/x-www-form-urlencoded' ||
    m.endsWith('+json') ||
    m.endsWith('+xml')
  ) {
    return 'text';
  }
  return 'none';
}

export interface CaptureFile {
  sha256: string;
  filename: string;
  /** Readable archive path (`files/host/…`) of the first item with this sha256. */
  file?: string;
  mime: string;
  size: number;
  savedAt: number;
  /** Distinct URLs whose bytes hash to this sha256. */
  urls: string[];
  /** Position of this file in the URL's version history (1-based). */
  version: number;
  /** How many distinct versions the same URL produced (1 = no badge). */
  versionCount: number;
  /** All contributing items had state != captured. */
  state: string;
  downloadable: boolean;
}

function basename(url: string): string {
  try {
    const path = new URL(url).pathname;
    const last = path.split('/').filter(Boolean).pop();
    return last ? decodeURIComponent(last) : url;
  } catch {
    return url;
  }
}

function savedTime(item: CaptureItem): number {
  return item.ts_saved || item.ts_finished || item.ts_requested || 0;
}

/**
 * Collapse items into one row per sha256. Items without a sha256 (too_large,
 * missing_body, …) never produced a blob, so they are not file rows.
 */
export function groupCaptureFiles(items: CaptureItem[]): CaptureFile[] {
  // version history per URL, ordered by when each hash was first saved.
  const urlHashes = new Map<string, string[]>();
  const firstSeen = new Map<string, Map<string, number>>();
  for (const item of items) {
    if (!item.sha256) continue;
    const url = item.final_url || item.url;
    if (!url) continue;
    let seen = firstSeen.get(url);
    if (!seen) {
      seen = new Map();
      firstSeen.set(url, seen);
      urlHashes.set(url, []);
    }
    const ts = savedTime(item);
    const prev = seen.get(item.sha256);
    if (prev === undefined) {
      seen.set(item.sha256, ts);
      urlHashes.get(url)!.push(item.sha256);
    } else if (ts < prev) {
      seen.set(item.sha256, ts);
    }
  }
  for (const [url, hashes] of urlHashes) {
    const seen = firstSeen.get(url)!;
    hashes.sort((a, b) => (seen.get(a) || 0) - (seen.get(b) || 0));
  }

  const groups = new Map<
    string,
    { item: CaptureItem; file?: string; urls: Set<string>; captured: boolean }
  >();
  for (const item of items) {
    if (!item.sha256) continue;
    const url = item.final_url || item.url || '';
    const group = groups.get(item.sha256);
    if (!group) {
      groups.set(item.sha256, {
        item,
        file: item.file,
        urls: new Set(url ? [url] : []),
        captured: item.state === 'captured',
      });
      continue;
    }
    if (url) group.urls.add(url);
    if (item.state === 'captured') group.captured = true;
    // The group label keeps the FIRST item's readable path even though `item`
    // moves to the newest one for size/saved-at.
    if (savedTime(item) > savedTime(group.item)) group.item = item;
  }

  const files: CaptureFile[] = [];
  for (const [sha256, { item, file, urls, captured }] of groups) {
    let version = 1;
    let versionCount = 1;
    for (const url of urls) {
      const hashes = urlHashes.get(url) || [];
      const idx = hashes.indexOf(sha256);
      if (idx >= 0) version = Math.max(version, idx + 1);
      versionCount = Math.max(versionCount, hashes.length);
    }
    files.push({
      sha256,
      filename: item.filename || basename(item.final_url || item.url || '') || sha256.slice(0, 12),
      file,
      mime: item.mime || '',
      size: item.size_decoded || item.size_wire || 0,
      savedAt: savedTime(item),
      urls: Array.from(urls),
      version,
      versionCount,
      state: captured ? 'captured' : item.state,
      downloadable: captured,
    });
  }
  files.sort((a, b) => b.savedAt - a.savedAt);
  return files;
}

/**
 * Cut a long archive path from the left, landing on a segment boundary:
 * `files/example.com/wp-admin/w/load.php` → `…/w/load.php`. The tail is what
 * identifies the file; the host tree is what the eye can drop.
 */
export function truncatePathLeft(path: string, max = 48): string {
  if (path.length <= max) return path;
  const tail = path.slice(path.length - (max - 1));
  const cut = tail.indexOf('/');
  return `…${cut >= 0 ? tail.slice(cut) : tail}`;
}

/** Search over URL, filename, readable path and mime — what a user remembers. */
export function filterCaptureFiles(files: CaptureFile[], query: string): CaptureFile[] {
  const q = query.trim().toLowerCase();
  if (!q) return files;
  return files.filter(
    (f) =>
      f.filename.toLowerCase().includes(q) ||
      (f.file || '').toLowerCase().includes(q) ||
      f.mime.toLowerCase().includes(q) ||
      f.urls.some((u) => u.toLowerCase().includes(q)),
  );
}

/** Save a Blob under `filename` (download-all and per-file Download). */
export function saveBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // The click has already handed the URL to the download; revoke on the next
  // task so Firefox does not cancel the transfer.
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
