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

/**
 * Cut a long URL in the middle, keeping the scheme/host and the file name:
 * `https://example.com/…/load.php`. A single URL is shown inline, so both ends
 * must stay legible.
 */
export function truncateUrlMiddle(url: string, max = 64): string {
  if (url.length <= max) return url;
  const keep = Math.max(2, max - 1);
  const head = Math.ceil(keep * 0.6);
  const tail = keep - head;
  return `${url.slice(0, head)}…${url.slice(url.length - tail)}`;
}

/**
 * Path relative to its host section, for the compact panel row: the host is
 * already the section header, so repeating `files/host/` on every row wastes
 * the little width a docked panel has.
 */
export function hostRelativePath(file: CaptureFile, host: string): string {
  const prefix = `files/${host}/`;
  if (file.file && file.file.startsWith(prefix)) return file.file.slice(prefix.length);
  return file.file || file.filename;
}

/**
 * Keep the identity of a path when space runs out. The host is already the
 * section header, and the last two segments carry the content hash and the
 * file name; a middle truncation would hide the hash and make 21 sibling images
 * look identical. `get-inspire-x/3c1d…/grid.webp` → `…/3c1d…/grid.webp`.
 */
export function truncatePathTail(path: string, max = 48): string {
  if (path.length <= max) return path;
  const segments = path.split('/').filter(Boolean);
  return `…/${segments.slice(-2).join('/')}`;
}

/**
 * Host folder a file lives under (`files/<host>/…`). Falls back to the URL's
 * hostname for rows recorded before the readable tree existed, then to `local`
 * (the contract's bucket for host-less resources such as data: URLs).
 */
export function captureFileHost(file: CaptureFile): string {
  const match = /^files\/([^/]+)\//.exec(file.file || '');
  if (match) return match[1];
  for (const url of file.urls) {
    try {
      const host = new URL(url).host;
      if (host) return host;
    } catch {
      // not a parseable absolute URL; keep looking
    }
  }
  return 'local';
}

/** Case-insensitive natural compare: `x2` sorts before `x10`. */
export function compareNatural(a: string, b: string): number {
  const left = a.match(/\d+|\D+/g) || [a];
  const right = b.match(/\d+|\D+/g) || [b];
  const len = Math.min(left.length, right.length);
  for (let i = 0; i < len; i += 1) {
    const x = left[i];
    const y = right[i];
    const xNum = /^\d+$/.test(x);
    const yNum = /^\d+$/.test(y);
    if (xNum && yNum) {
      const diff = Number(x) - Number(y);
      if (diff !== 0) return diff;
      // `2` before `02` — shorter run first when numerically equal.
      if (x.length !== y.length) return x.length - y.length;
      continue;
    }
    const lx = x.toLowerCase();
    const ly = y.toLowerCase();
    if (lx !== ly) return lx < ly ? -1 : 1;
  }
  return left.length - right.length;
}

export type CaptureSort = 'path' | 'time' | 'size';
/** Segmented control: three host groupings plus MIME grouping. */
export type CaptureGroupMode = CaptureSort | 'type';

export interface CaptureGroup {
  /** Stable collapse key: host folder or normalized MIME label. */
  key: string;
  /** Header text (the host or the MIME label). */
  label: string;
  /** Decides whether a compact row must repeat the host in its path line. */
  kind: 'host' | 'mime';
  files: CaptureFile[];
  count: number;
  bytes: number;
}

function compareCaptureFiles(a: CaptureFile, b: CaptureFile, sort: CaptureSort): number {
  if (sort === 'time') {
    const diff = (b.savedAt || 0) - (a.savedAt || 0);
    if (diff !== 0) return diff;
  } else if (sort === 'size') {
    const diff = (b.size || 0) - (a.size || 0);
    if (diff !== 0) return diff;
  }
  // `path` is the default and the tie-breaker for every other order.
  return compareNatural(a.file || a.filename, b.file || b.filename);
}

/** Groups keep their file order and shared count/byte totals. */
function finalizeGroup(
  key: string,
  kind: CaptureGroup['kind'],
  bucket: CaptureFile[],
  sort: CaptureSort,
): CaptureGroup {
  const files = [...bucket].sort((a, b) => compareCaptureFiles(a, b, sort));
  return {
    key,
    label: key,
    kind,
    files,
    count: files.length,
    bytes: files.reduce((sum, file) => sum + (file.size || 0), 0),
  };
}

/**
 * Browser view of the deduped rows: one section per host folder, files sorted
 * inside it. Hosts stay in name order so a reload never reshuffles the tree.
 */
export function groupCaptureFilesByHost(
  files: CaptureFile[],
  sort: CaptureSort = 'path',
): CaptureGroup[] {
  const byHost = new Map<string, CaptureFile[]>();
  for (const file of files) {
    const host = captureFileHost(file);
    const bucket = byHost.get(host);
    if (bucket) bucket.push(file);
    else byHost.set(host, [file]);
  }
  const groups: CaptureGroup[] = [];
  for (const [host, bucket] of byHost) {
    groups.push(finalizeGroup(host, 'host', bucket, sort));
  }
  groups.sort((a, b) => compareNatural(a.label, b.label));
  return groups;
}

/**
 * Browser view grouped by response type: lowercased full MIME (`text/javascript`,
 * `image/webp`), with an `unknown` bucket for rows the streamer never labelled.
 * MIME labels sort alphabetically and files always sort by path inside.
 */
export function groupCaptureFilesByMime(files: CaptureFile[]): CaptureGroup[] {
  const byMime = new Map<string, CaptureFile[]>();
  for (const file of files) {
    const mime = (file.mime || '').trim().toLowerCase() || 'unknown';
    const bucket = byMime.get(mime);
    if (bucket) bucket.push(file);
    else byMime.set(mime, [file]);
  }
  const groups: CaptureGroup[] = [];
  for (const [mime, bucket] of byMime) {
    groups.push(finalizeGroup(mime, 'mime', bucket, 'path'));
  }
  groups.sort((a, b) => compareNatural(a.label, b.label));
  return groups;
}

/** Search over URL, host, readable path, filename, mime and sha256. */
export function filterCaptureFiles(files: CaptureFile[], query: string): CaptureFile[] {
  const q = query.trim().toLowerCase();
  if (!q) return files;
  return files.filter(
    (f) =>
      f.filename.toLowerCase().includes(q) ||
      (f.file || '').toLowerCase().includes(q) ||
      captureFileHost(f).toLowerCase().includes(q) ||
      f.mime.toLowerCase().includes(q) ||
      f.sha256.toLowerCase().includes(q) ||
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
