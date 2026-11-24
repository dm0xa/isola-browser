// Review body for one capture archive, shared by two surfaces:
//   * CapturesPage (`/session/:id/captures`, `/captures/:id`) in `full` mode —
//     the page owns the route, chrome and token handoff;
//   * CapturePanel (the viewer's docked Resources overlay) in `panel` mode.
//
// Everything that touches the API lives here: the load, the 5 s refresh while a
// recording is live, filtering, previewing, per-file and zip downloads. Nothing
// archived is ever injected as markup: text (HTML included) is shown in a
// <pre>, images through an object URL, everything else is download-only.
import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { AlertTriangle, ChevronDown, ChevronRight, Copy, Download, Eye, FileImage, RefreshCw, X } from 'lucide-react';

import { copyToClipboard } from '../lib/clipboard';
import { isCaptureEnabled } from '../lib/config';
import {
  captureErrorText,
  fetchArchiveBlob,
  fetchArchiveFile,
  fetchArchiveZip,
  getArchive,
  getArchiveKey,
  type CaptureArchive,
} from '../lib/captureApi';
import {
  captureFileHost,
  filterCaptureFiles,
  formatBytes,
  formatSavedAt,
  groupCaptureFiles,
  groupCaptureFilesByHost,
  groupCaptureFilesByMime,
  hostRelativePath,
  previewKind,
  saveBlob,
  shortSha256,
  truncatePathLeft,
  truncatePathTail,
  truncateUrlMiddle,
  type CaptureFile,
  type CaptureGroupMode,
  type PreviewKind,
} from '../lib/captureFormat';

// A recording is still growing; the archive endpoint is cheap (meta + items),
// so one poll per 5 s is enough to make new files appear without a button.
const RECORDING_POLL_MS = 5000;

const NO_KEY_MESSAGE =
  'This browser does not hold the archive key for this capture. Open the review page from the session that recorded it.';

type OpenPreview = {
  sha256: string;
  filename: string;
  /** Readable archive path shown in the detail header, when the item has one. */
  file?: string;
  kind: 'text' | 'image';
  text?: string;
  url?: string;
};

export type CaptureArchiveViewProps = {
  /** Archive id (the session id on this deployment). */
  archiveId: string;
  /** `panel` = compact chrome for the docked overlay, `full` = review page body. */
  layout?: 'panel' | 'full';
  /** True while a recording is live: poll for new files and explain an empty archive. */
  recording?: boolean;
  /** Called after every successful load with the archive and its deduped row count. */
  onArchive?: (archive: CaptureArchive | null, fileCount: number) => void;
  className?: string;
};

export function CaptureArchiveView({
  archiveId,
  layout = 'full',
  recording = false,
  onArchive,
  className,
}: CaptureArchiveViewProps) {
  const [archive, setArchive] = useState<CaptureArchive | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState('');
  const [actionError, setActionError] = useState('');
  const [query, setQuery] = useState('');
  const [mode, setMode] = useState<CaptureGroupMode>('path');
  const [collapsedGroups, setCollapsedGroups] = useState<Set<string>>(() => new Set());
  const [expandedSha, setExpandedSha] = useState('');
  const [copied, setCopied] = useState('');
  const [preview, setPreview] = useState<OpenPreview | null>(null);
  const [zipping, setZipping] = useState(false);
  const mountedRef = useRef(true);
  const objectUrlRef = useRef('');
  const filterRef = useRef<HTMLInputElement>(null);
  // Only the newest Open request may land: a slow first fetch must not replace
  // the preview the user asked for second.
  const previewSeqRef = useRef(0);
  // Kept in a ref so a parent that forgets to memoise the callback cannot make
  // `load` unstable and turn the initial load into a fetch loop.
  const onArchiveRef = useRef(onArchive);
  useEffect(() => {
    onArchiveRef.current = onArchive;
  });

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      if (objectUrlRef.current) URL.revokeObjectURL(objectUrlRef.current);
    };
  }, []);

  const load = useCallback(
    async (silent: boolean) => {
      if (!archiveId) return;
      if (!isCaptureEnabled()) {
        if (silent) return;
        setLoading(false);
        setLoadError('Resource capture is disabled on this deployment.');
        return;
      }
      // Read the key per call: a recording started after this view mounted puts
      // it in localStorage, and the next poll must pick it up.
      const key = getArchiveKey(archiveId);
      if (!key) {
        if (silent) return;
        setLoading(false);
        setLoadError(NO_KEY_MESSAGE);
        return;
      }
      if (!silent) {
        setLoading(true);
        setLoadError('');
      }
      try {
        const next = await getArchive(archiveId, key);
        if (!mountedRef.current) return;
        setArchive(next);
        setLoadError('');
        onArchiveRef.current?.(next, groupCaptureFiles(next.items).length);
      } catch (err) {
        if (!mountedRef.current) return;
        if (!silent) setLoadError(captureErrorText(err));
      } finally {
        if (mountedRef.current && !silent) setLoading(false);
      }
    },
    [archiveId],
  );

  // Reload on mount, on archive change, and when a recording starts or ends
  // (the key may only have appeared with the start).
  useEffect(() => {
    void load(false);
  }, [load, recording]);

  useEffect(() => {
    if (!recording) return;
    const timer = window.setInterval(() => {
      void load(true);
    }, RECORDING_POLL_MS);
    return () => window.clearInterval(timer);
  }, [recording, load]);

  const compact = layout === 'panel';
  const files = useMemo(() => groupCaptureFiles(archive?.items ?? []), [archive]);
  const visible = useMemo(() => filterCaptureFiles(files, query), [files, query]);
  const groups = useMemo(
    () => (mode === 'type' ? groupCaptureFilesByMime(visible) : groupCaptureFilesByHost(visible, mode)),
    [visible, mode],
  );

  // The docked panel is opened by an explicit action, so the cursor belongs in
  // the filter: typing narrows immediately. The full page does not steal focus.
  useEffect(() => {
    if (compact) filterRef.current?.focus();
  }, [compact]);

  const toggleGroup = useCallback((key: string) => {
    setCollapsedGroups((cur) => {
      const next = new Set(cur);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }, []);

  const closePreview = useCallback(() => {
    previewSeqRef.current += 1;
    setPreview(null);
    if (objectUrlRef.current) {
      URL.revokeObjectURL(objectUrlRef.current);
      objectUrlRef.current = '';
    }
  }, []);

  // Esc dismisses the preview without reaching into row state; the detail row
  // stays open so the reviewer keeps their place.
  useEffect(() => {
    if (!preview) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') closePreview();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [preview, closePreview]);

  const openFile = useCallback(
    async (file: CaptureFile) => {
      if (!archiveId) return;
      const key = getArchiveKey(archiveId);
      if (!key) return;
      const kind = previewKind(file.mime);
      if (kind === 'none') return;
      const seq = previewSeqRef.current + 1;
      previewSeqRef.current = seq;
      setActionError('');
      setPreview({ sha256: file.sha256, filename: file.filename, file: file.file, kind });
      try {
        const blob = await fetchArchiveBlob(archiveId, file.sha256, key);
        if (!mountedRef.current || previewSeqRef.current !== seq) return;
        if (kind === 'text') {
          const text = await blob.text();
          if (!mountedRef.current || previewSeqRef.current !== seq) return;
          setPreview({ sha256: file.sha256, filename: file.filename, file: file.file, kind, text });
        } else {
          if (objectUrlRef.current) URL.revokeObjectURL(objectUrlRef.current);
          const url = URL.createObjectURL(blob);
          objectUrlRef.current = url;
          setPreview({ sha256: file.sha256, filename: file.filename, file: file.file, kind, url });
        }
      } catch (err) {
        if (!mountedRef.current || previewSeqRef.current !== seq) return;
        setPreview(null);
        setActionError(captureErrorText(err));
      }
    },
    [archiveId],
  );

  const downloadFile = useCallback(
    async (file: CaptureFile) => {
      if (!archiveId) return;
      const key = getArchiveKey(archiveId);
      if (!key) return;
      setActionError('');
      try {
        // Readable path first: it is the name the file has inside the archive.
        const blob = file.file
          ? await fetchArchiveFile(archiveId, file.file, key)
          : await fetchArchiveBlob(archiveId, file.sha256, key);
        saveBlob(blob, file.filename);
      } catch (err) {
        setActionError(captureErrorText(err));
      }
    },
    [archiveId],
  );

  const downloadAll = useCallback(async () => {
    if (!archiveId) return;
    const key = getArchiveKey(archiveId);
    if (!key) return;
    setZipping(true);
    setActionError('');
    try {
      const blob = await fetchArchiveZip(archiveId, key);
      saveBlob(blob, `isola-capture-${archiveId}.zip`);
    } catch (err) {
      setActionError(captureErrorText(err));
    } finally {
      if (mountedRef.current) setZipping(false);
    }
  }, [archiveId]);

  const copySha = useCallback(async (sha256: string) => {
    const ok = await copyToClipboard(sha256);
    if (!ok) return;
    setCopied(sha256);
    window.setTimeout(() => setCopied((cur) => (cur === sha256 ? '' : cur)), 1500);
  }, []);

  const copyFilePath = useCallback(async (path: string) => {
    const ok = await copyToClipboard(path);
    if (!ok) return;
    const token = `file:${path}`;
    setCopied(token);
    window.setTimeout(() => setCopied((cur) => (cur === token ? '' : cur)), 1500);
  }, []);

  const copyUrl = useCallback(async (url: string) => {
    const ok = await copyToClipboard(url);
    if (!ok) return;
    const token = `url:${url}`;
    setCopied(token);
    window.setTimeout(() => setCopied((cur) => (cur === token ? '' : cur)), 1500);
  }, []);

  // `captured` is the normal case and must not cost a badge on every row; only
  // an anomalous state earns attention.
  const renderStateBadge = (file: CaptureFile) => {
    if (file.state === 'captured') return null;
    return (
      <span
        data-testid="capture-state"
        className="shrink-0 rounded bg-amber-500/15 px-1 text-[9px] uppercase tracking-wide text-amber-800 dark:text-amber-300"
      >
        {file.state}
      </span>
    );
  };

  // Shared between the full-page table's detail row and the panel's expanded
  // block: full path, full sha and every URL, each with its own copy action.
  const renderDetailBody = (file: CaptureFile, pathLabel: string) => (
    <div className="flex flex-col gap-1.5 text-[10px] text-zinc-600 dark:text-gray-400">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <span className="shrink-0 text-zinc-500 dark:text-gray-500">path</span>
        <span className="min-w-0 flex-1 break-all font-mono text-zinc-800 dark:text-gray-200">
          {pathLabel}
        </span>
        <button
          type="button"
          onClick={() => { void copyFilePath(pathLabel); }}
          aria-label="Copy file path"
          className="flex shrink-0 items-center gap-1 rounded px-1.5 py-0.5 hover:bg-zinc-200 dark:hover:bg-gray-800"
          title={`Copy ${pathLabel}`}
        >
          <Copy size={10} aria-hidden />
          {copied === `file:${pathLabel}` ? 'copied' : 'Copy path'}
        </button>
      </div>
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <span className="shrink-0 text-zinc-500 dark:text-gray-500">sha256</span>
        <span className="min-w-0 flex-1 break-all font-mono">{file.sha256}</span>
        <button
          type="button"
          onClick={() => { void copySha(file.sha256); }}
          aria-label="Copy sha256"
          className="flex shrink-0 items-center gap-1 rounded px-1.5 py-0.5 hover:bg-zinc-200 dark:hover:bg-gray-800"
          title="Copy sha256"
        >
          <Copy size={10} aria-hidden />
          {copied === file.sha256 ? 'copied' : 'copy'}
        </button>
      </div>
      {file.urls.length > 0 && (
        <div>
          <div className="mb-0.5 text-zinc-500 dark:text-gray-500">URLs ({file.urls.length})</div>
          <ul className="space-y-0.5">
            {file.urls.map((url) => (
              <li key={url} className="flex items-start gap-1">
                <span className="min-w-0 flex-1 break-all font-mono text-zinc-600 dark:text-gray-400">
                  {url}
                </span>
                <button
                  type="button"
                  onClick={() => { void copyUrl(url); }}
                  aria-label={`Copy URL ${url}`}
                  className="flex shrink-0 items-center gap-1 rounded px-1.5 py-0.5 hover:bg-zinc-200 dark:hover:bg-gray-800"
                  title="Copy URL"
                >
                  <Copy size={10} aria-hidden />
                  {copied === `url:${url}` ? 'copied' : 'copy'}
                </button>
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );

  // Line one of a panel row is the file; the trailing actions are icon-only so
  // the row stays two lines. Non-captured rows have no actions at all.
  const renderActions = (file: CaptureFile, kind: PreviewKind) => {
    if (!file.downloadable) return null;
    return (
      <>
        <button
          type="button"
          onClick={() => { void downloadFile(file); }}
          aria-label={compact ? 'Download' : undefined}
          className={
            compact
              ? 'rounded p-0.5 text-zinc-500 hover:bg-zinc-200 hover:text-zinc-800 dark:text-gray-400 dark:hover:bg-gray-800 dark:hover:text-gray-100'
              : 'flex items-center gap-1 rounded border border-zinc-300 px-1.5 py-0.5 text-[10px] text-zinc-600 hover:bg-zinc-100 dark:border-gray-700 dark:text-gray-300 dark:hover:bg-gray-800'
          }
          title="Save this file"
        >
          <Download size={compact ? 12 : 11} aria-hidden />
          {!compact && 'Download'}
        </button>
        {kind !== 'none' && (
          <button
            type="button"
            onClick={() => { void openFile(file); }}
            aria-label={compact ? 'Open' : undefined}
            className={
              compact
                ? 'rounded p-0.5 text-zinc-500 hover:bg-zinc-200 hover:text-zinc-800 dark:text-gray-400 dark:hover:bg-gray-800 dark:hover:text-gray-100'
                : 'rounded border border-zinc-300 px-1.5 py-0.5 text-[10px] text-zinc-600 hover:bg-zinc-100 dark:border-gray-700 dark:text-gray-300 dark:hover:bg-gray-800'
            }
            title={kind === 'text' ? 'Show the response body as source text' : 'Show the image'}
          >
            {compact ? <Eye size={12} aria-hidden /> : 'Open'}
          </button>
        )}
      </>
    );
  };

  return (
    <div className={`flex min-h-0 flex-1 flex-col overflow-hidden ${className || ''}`}>
      <div
        className={`flex shrink-0 flex-wrap items-center gap-2 border-b border-zinc-200 px-3 py-1.5 dark:border-gray-800 ${
          compact ? 'text-[11px]' : 'text-xs'
        }`}
      >
        <input
          ref={filterRef}
          type="search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder={compact ? 'filter…' : 'Filter URL, host, file name, mime, or sha'}
          className={`rounded border border-zinc-300 bg-white px-2 py-1 outline-none focus:border-cyan-500 dark:border-gray-700 dark:bg-gray-900 dark:text-gray-100 ${
            compact ? 'w-40' : 'w-64'
          }`}
          aria-label="Filter saved resources"
        />
        {!compact && (
          <span className="whitespace-nowrap text-zinc-500 dark:text-gray-500">
            {visible.length === files.length
              ? `${files.length} ${files.length === 1 ? 'file' : 'files'}`
              : `${visible.length}/${files.length} files`}
          </span>
        )}
        <div
          className="flex items-center gap-0.5 rounded border border-zinc-300 p-0.5 dark:border-gray-700"
          role="group"
          aria-label="Group and sort saved files"
        >
          {(['path', 'time', 'size', 'type'] as const).map((key) => (
            <button
              key={key}
              type="button"
              onClick={() => setMode(key)}
              aria-pressed={mode === key}
              title={key === 'type' ? 'Group by MIME type' : `Group by host, sort by ${key}`}
              className={`rounded px-1.5 py-0.5 ${
                mode === key
                  ? 'bg-cyan-600 text-white'
                  : 'text-zinc-600 hover:bg-zinc-100 dark:text-gray-300 dark:hover:bg-gray-800'
              }`}
            >
              {key}
            </button>
          ))}
        </div>
        <div className="ml-auto flex items-center gap-2">
          <button
            type="button"
            onClick={() => { void load(false); }}
            className="flex items-center gap-1 rounded border border-zinc-300 px-2 py-1 text-zinc-600 hover:bg-zinc-100 dark:border-gray-700 dark:text-gray-300 dark:hover:bg-gray-800"
            title="Reload the archive"
          >
            <RefreshCw size={13} aria-hidden />
            <span className={compact ? 'sr-only' : ''}>Reload</span>
          </button>
          <button
            type="button"
            onClick={() => { void downloadAll(); }}
            disabled={zipping || files.length === 0}
            className="flex items-center gap-1 rounded bg-cyan-600 px-2 py-1 font-medium text-white hover:bg-cyan-500 disabled:cursor-not-allowed disabled:opacity-40"
            title="Download every saved file as a zip"
          >
            <Download size={13} aria-hidden />
            {zipping ? 'Zipping…' : compact ? 'Zip' : 'Download all'}
          </button>
        </div>
      </div>

      {actionError && (
        <div className="flex shrink-0 items-center gap-2 border-b border-red-200 bg-red-50 px-4 py-1.5 text-xs text-red-700 dark:border-red-900/60 dark:bg-red-950/40 dark:text-red-300">
          <AlertTriangle size={13} aria-hidden />
          <span className="min-w-0 flex-1 truncate" title={actionError}>{actionError}</span>
          <button type="button" onClick={() => setActionError('')} aria-label="Dismiss error">
            <X size={13} />
          </button>
        </div>
      )}

      {preview && (
        <section className="flex max-h-[55%] shrink-0 flex-col border-b border-zinc-200 bg-zinc-50 dark:border-gray-800 dark:bg-gray-900">
          <div className="flex items-center gap-2 px-4 py-1.5 text-xs">
            {preview.kind === 'image' ? <FileImage size={13} aria-hidden /> : <span className="font-mono">Aa</span>}
            <span
              className="min-w-0 flex-1 truncate font-mono"
              title={preview.file || preview.filename}
            >
              {preview.file || preview.filename}
            </span>
            {preview.file && (
              <button
                type="button"
                onClick={() => { void copyFilePath(preview.file!); }}
                className="flex shrink-0 items-center gap-1 rounded px-1.5 py-0.5 text-zinc-500 hover:bg-zinc-200 dark:text-gray-400 dark:hover:bg-gray-800"
                title={`Copy ${preview.file}`}
                aria-label="Copy file path"
              >
                <Copy size={11} aria-hidden />
                {copied === `file:${preview.file}` ? 'copied' : 'Copy path'}
              </button>
            )}
            <button
              type="button"
              onClick={closePreview}
              className="flex shrink-0 items-center gap-1 rounded px-1.5 py-0.5 text-zinc-500 hover:bg-zinc-200 dark:text-gray-400 dark:hover:bg-gray-800"
              aria-label="Close preview"
            >
              <X size={13} /> Close
            </button>
          </div>
          <div className="min-h-0 flex-1 overflow-auto px-4 pb-3">
            {preview.kind === 'text' ? (
              preview.text === undefined ? (
                <p className="py-2 text-xs text-zinc-500 dark:text-gray-500">Loading…</p>
              ) : (
                <pre className="whitespace-pre-wrap break-all rounded bg-white p-2 font-mono text-[11px] leading-relaxed text-zinc-800 dark:bg-gray-950 dark:text-gray-200">
                  {preview.text}
                </pre>
              )
            ) : preview.url ? (
              <img src={preview.url} alt={preview.filename} className="mx-auto max-h-[45vh] object-contain" />
            ) : (
              <p className="py-2 text-xs text-zinc-500 dark:text-gray-500">Loading…</p>
            )}
          </div>
        </section>
      )}

      <div className="min-h-0 flex-1 overflow-auto">
        {loading && (
          <p className="px-4 py-6 text-xs text-zinc-500 dark:text-gray-400">Loading archive…</p>
        )}

        {!loading && loadError && (
          <div className="mx-4 my-6 rounded border border-red-200 bg-red-50 p-3 text-xs text-red-700 dark:border-red-900/60 dark:bg-red-950/40 dark:text-red-300">
            <div className="flex items-center gap-2">
              <AlertTriangle size={14} aria-hidden />
              <span className="font-medium">Could not open this capture</span>
            </div>
            <p className="mt-1 break-words">{loadError}</p>
            <button
              type="button"
              onClick={() => { void load(false); }}
              className="mt-2 rounded border border-red-300 px-2 py-0.5 hover:bg-red-100 dark:border-red-800 dark:hover:bg-red-900/40"
            >
              Retry
            </button>
          </div>
        )}

        {!loading && !loadError && files.length === 0 && (
          <div className="px-4 py-8 text-center text-xs text-zinc-500 dark:text-gray-400">
            {recording ? (
              <>
                <p className="text-sm text-zinc-700 dark:text-gray-200">Recording — nothing captured yet</p>
                <p className="mt-1">
                  Downloaded files appear here as the capture pulls them from the sandbox.
                </p>
              </>
            ) : (
              <>
                <p className="text-sm text-zinc-700 dark:text-gray-200">No captured files</p>
                <p className="mt-1">
                  Record from the session's menu while browsing; downloaded files appear here when
                  the capture has pulled them from the sandbox. A page that finished loading before
                  Record started has no bodies to save — reload it while recording.
                </p>
              </>
            )}
          </div>
        )}

        {!loading && !loadError && files.length > 0 && (compact ? (
          <div className="text-xs">
            {visible.length === 0 && (
              <p className="px-3 py-6 text-center text-zinc-500 dark:text-gray-400">
                No saved files match “{query}”.
              </p>
            )}
            {groups.map((group) => {
              const isCollapsed = collapsedGroups.has(group.key);
              return (
                <section key={group.key} className="border-b border-zinc-200 last:border-b-0 dark:border-gray-800">
                  <button
                    type="button"
                    onClick={() => toggleGroup(group.key)}
                    aria-expanded={!isCollapsed}
                    className="sticky top-0 z-10 flex w-full items-center gap-1.5 border-b border-zinc-200 bg-zinc-100 px-2 py-1 text-left hover:bg-zinc-200 dark:border-gray-800 dark:bg-gray-900 dark:hover:bg-gray-800"
                  >
                    {isCollapsed ? <ChevronRight size={12} aria-hidden /> : <ChevronDown size={12} aria-hidden />}
                    <span className="min-w-0 flex-1 truncate font-mono font-semibold text-zinc-800 dark:text-gray-200">
                      {group.label}
                    </span>
                    <span className="shrink-0 whitespace-nowrap text-[10px] font-normal text-zinc-500 dark:text-gray-500">
                      {group.count} {group.count === 1 ? 'file' : 'files'} · {formatBytes(group.bytes)}
                    </span>
                  </button>
                  {!isCollapsed && group.files.map((file) => {
                    const kind = previewKind(file.mime);
                    const captured = file.downloadable;
                    const pathLabel = file.file || file.filename;
                    const detailOpen = expandedSha === file.sha256;
                    const singleUrl = file.urls.length === 1 ? file.urls[0] : '';
                    // In MIME mode the host is no longer the section header, so
                    // the row path has to carry it again.
                    const host = captureFileHost(file);
                    const rowPath = group.kind === 'mime'
                      ? `${host}/${hostRelativePath(file, host)}`
                      : hostRelativePath(file, group.key);
                    return (
                      <div key={file.sha256} className="border-b border-zinc-100 last:border-b-0 dark:border-gray-800/60">
                        <div
                          className={`flex items-start gap-1 px-2 py-1 ${
                            captured
                              ? 'hover:bg-zinc-50 dark:hover:bg-gray-900/60'
                              : 'bg-amber-50/40 dark:bg-amber-950/10'
                          }`}
                        >
                          <button
                            type="button"
                            onClick={() => setExpandedSha(detailOpen ? '' : file.sha256)}
                            aria-expanded={detailOpen}
                            aria-label={`File details for ${pathLabel}`}
                            title="Show details"
                            className="mt-0.5 shrink-0 rounded p-0.5 text-zinc-400 hover:bg-zinc-200 hover:text-zinc-700 dark:hover:bg-gray-800 dark:hover:text-gray-200"
                          >
                            <ChevronRight
                              size={11}
                              aria-hidden
                              className={`transition-transform ${detailOpen ? 'rotate-90' : ''}`}
                            />
                          </button>
                          <div className="min-w-0 flex-1">
                            <div className="flex items-center gap-1">
                              <span
                                data-testid="capture-file-path"
                                className="min-w-0 truncate font-mono font-medium"
                                title={pathLabel}
                                aria-label={pathLabel}
                              >
                                {truncatePathTail(rowPath)}
                              </span>
                              {file.versionCount > 1 && (
                                <span
                                  className="shrink-0 rounded bg-amber-500/15 px-1 text-[9px] uppercase tracking-wide text-amber-700 dark:text-amber-300"
                                  title={`Version ${file.version} of ${file.versionCount} saved for this URL`}
                                >
                                  v{file.version}
                                </span>
                              )}
                              {renderStateBadge(file)}
                            </div>
                            <div className="mt-0.5 flex items-center gap-1.5 text-[10px] text-zinc-500 dark:text-gray-500">
                              <div className="flex min-w-0 flex-1 items-center gap-1">
                                {singleUrl ? (
                                  <>
                                    <span className="min-w-0 truncate" title={singleUrl}>
                                      {truncateUrlMiddle(singleUrl, 56)}
                                    </span>
                                    <button
                                      type="button"
                                      onClick={() => { void copyUrl(singleUrl); }}
                                      aria-label={`Copy URL ${singleUrl}`}
                                      title="Copy URL"
                                      className="shrink-0 rounded p-0.5 hover:bg-zinc-200 hover:text-zinc-700 dark:hover:bg-gray-800 dark:hover:text-gray-200"
                                    >
                                      <Copy size={10} aria-hidden />
                                    </button>
                                    {copied === `url:${singleUrl}` && <span>copied</span>}
                                  </>
                                ) : file.urls.length > 1 ? (
                                  <button
                                    type="button"
                                    onClick={() => setExpandedSha(detailOpen ? '' : file.sha256)}
                                    aria-expanded={detailOpen}
                                    className="truncate text-cyan-700 hover:underline dark:text-cyan-300"
                                  >
                                    shared by {file.urls.length} URLs
                                  </button>
                                ) : (
                                  <span>—</span>
                                )}
                              </div>
                              <span
                                className="max-w-[55%] shrink-0 truncate whitespace-nowrap"
                                title={file.mime || undefined}
                              >
                                {file.mime || '—'} · {formatBytes(file.size)} · {formatSavedAt(file.savedAt)}
                              </span>
                            </div>
                          </div>
                          <div className="flex shrink-0 items-center gap-0.5">
                            {renderActions(file, kind)}
                          </div>
                        </div>
                        {detailOpen && (
                          <div className="border-t border-zinc-200 bg-zinc-50/70 px-4 py-2 dark:border-gray-800 dark:bg-gray-950/40">
                            {renderDetailBody(file, pathLabel)}
                          </div>
                        )}
                      </div>
                    );
                  })}
                </section>
              );
            })}
          </div>
        ) : (
          <table className="w-full table-fixed border-collapse text-left text-xs">
            <thead className="sticky top-0 z-20 bg-zinc-100 text-[10px] uppercase tracking-wide text-zinc-500 dark:bg-gray-900 dark:text-gray-500">
              <tr>
                <th className="px-3 py-1.5 font-normal">file / url</th>
                <th className="w-28 px-2 py-1.5 font-normal">mime</th>
                <th className="w-20 px-2 py-1.5 text-right font-normal">size</th>
                <th className="w-40 px-2 py-1.5 font-normal">saved</th>
                <th className="w-32 px-2 py-1.5 font-normal">sha256</th>
                <th className="w-36 px-2 py-1.5 font-normal">actions</th>
              </tr>
            </thead>
            {visible.length === 0 && (
              <tbody>
                <tr>
                  <td colSpan={6} className="px-3 py-6 text-center text-zinc-500 dark:text-gray-400">
                    No saved files match “{query}”.
                  </td>
                </tr>
              </tbody>
            )}
            {groups.map((group) => {
              const isCollapsed = collapsedGroups.has(group.key);
              return (
                <tbody key={group.key}>
                  <tr className="border-b border-zinc-200 bg-zinc-50 dark:border-gray-800 dark:bg-gray-900/50">
                    <th
                      colSpan={6}
                      scope="colgroup"
                      className="sticky top-[25px] z-[5] bg-zinc-50 p-0 text-left font-normal dark:bg-gray-900"
                    >
                      <button
                        type="button"
                        onClick={() => toggleGroup(group.key)}
                        aria-expanded={!isCollapsed}
                        className="flex w-full items-center gap-1.5 px-2 py-1 text-left hover:bg-zinc-100 dark:hover:bg-gray-800/60"
                      >
                        {isCollapsed ? (
                          <ChevronRight size={12} aria-hidden />
                        ) : (
                          <ChevronDown size={12} aria-hidden />
                        )}
                        <span className="min-w-0 flex-1 truncate font-mono font-semibold text-zinc-800 dark:text-gray-200">
                          {group.label}
                        </span>
                        <span className="shrink-0 whitespace-nowrap text-[10px] font-normal text-zinc-500 dark:text-gray-500">
                          {group.count} {group.count === 1 ? 'file' : 'files'} · {formatBytes(group.bytes)}
                        </span>
                      </button>
                    </th>
                  </tr>
                  {!isCollapsed && group.files.map((file) => {
                    const kind = previewKind(file.mime);
                    const captured = file.downloadable;
                    const pathLabel = file.file || file.filename;
                    const detailOpen = expandedSha === file.sha256;
                    const singleUrl = file.urls.length === 1 ? file.urls[0] : '';
                    return (
                      <Fragment key={file.sha256}>
                        <tr
                          className={`border-b border-zinc-100 dark:border-gray-800/60 ${
                            captured
                              ? 'hover:bg-zinc-50 dark:hover:bg-gray-900/60'
                              : 'bg-amber-50/40 dark:bg-amber-950/10'
                          }`}
                        >
                          <td className="px-3 py-1.5">
                            <div className="flex items-center gap-1.5">
                              <button
                                type="button"
                                onClick={() => setExpandedSha(detailOpen ? '' : file.sha256)}
                                aria-expanded={detailOpen}
                                aria-label={`File details for ${pathLabel}`}
                                title="Show details"
                                className="shrink-0 rounded p-0.5 text-zinc-400 hover:bg-zinc-200 hover:text-zinc-700 dark:hover:bg-gray-800 dark:hover:text-gray-200"
                              >
                                <ChevronRight
                                  size={11}
                                  aria-hidden
                                  className={`transition-transform ${detailOpen ? 'rotate-90' : ''}`}
                                />
                              </button>
                              <span
                                data-testid="capture-file-path"
                                className="truncate font-mono font-medium"
                                title={pathLabel}
                              >
                                {truncatePathLeft(pathLabel)}
                              </span>
                              {file.versionCount > 1 && (
                                <span
                                  className="shrink-0 rounded bg-amber-500/15 px-1 text-[9px] uppercase tracking-wide text-amber-700 dark:text-amber-300"
                                  title={`Version ${file.version} of ${file.versionCount} saved for this URL`}
                                >
                                  v{file.version}
                                </span>
                              )}
                              {renderStateBadge(file)}
                            </div>
                            <div className="mt-0.5 flex items-center gap-1 pl-5 text-[10px] text-zinc-500 dark:text-gray-500">
                              {singleUrl ? (
                                <>
                                  <span className="min-w-0 truncate" title={singleUrl}>
                                    {truncateUrlMiddle(singleUrl)}
                                  </span>
                                  <button
                                    type="button"
                                    onClick={() => { void copyUrl(singleUrl); }}
                                    aria-label={`Copy URL ${singleUrl}`}
                                    title="Copy URL"
                                    className="shrink-0 rounded p-0.5 hover:bg-zinc-200 hover:text-zinc-700 dark:hover:bg-gray-800 dark:hover:text-gray-200"
                                  >
                                    <Copy size={10} aria-hidden />
                                  </button>
                                  {copied === `url:${singleUrl}` && <span>copied</span>}
                                </>
                              ) : file.urls.length > 1 ? (
                                <button
                                  type="button"
                                  onClick={() => setExpandedSha(detailOpen ? '' : file.sha256)}
                                  aria-expanded={detailOpen}
                                  className="text-cyan-700 hover:underline dark:text-cyan-300"
                                >
                                  shared by {file.urls.length} URLs
                                </button>
                              ) : (
                                <span>—</span>
                              )}
                            </div>
                          </td>
                          <td className="truncate px-2 py-1.5 text-zinc-600 dark:text-gray-400" title={file.mime}>
                            {file.mime || '—'}
                          </td>
                          <td className="px-2 py-1.5 text-right font-mono text-zinc-600 dark:text-gray-400">
                            {formatBytes(file.size)}
                          </td>
                          <td className="px-2 py-1.5 text-zinc-600 dark:text-gray-400">{formatSavedAt(file.savedAt)}</td>
                          <td className="px-2 py-1.5">
                            <button
                              type="button"
                              onClick={() => { void copySha(file.sha256); }}
                              className="flex items-center gap-1 font-mono text-[10px] text-zinc-500 hover:text-cyan-700 dark:text-gray-500 dark:hover:text-cyan-300"
                              title={`sha256 ${file.sha256} — click to copy`}
                            >
                              <Copy size={11} aria-hidden />
                              {copied === file.sha256 ? 'copied' : shortSha256(file.sha256)}
                            </button>
                          </td>
                          <td className="px-2 py-1.5">
                            <div className="flex items-center gap-1">{renderActions(file, kind)}</div>
                          </td>
                        </tr>
                        {detailOpen && (
                          <tr className="border-b border-zinc-100 bg-zinc-50/70 align-top dark:border-gray-800/60 dark:bg-gray-950/40">
                            <td colSpan={6} className="px-6 py-2">
                              {renderDetailBody(file, pathLabel)}
                            </td>
                          </tr>
                        )}
                      </Fragment>
                    );
                  })}
                </tbody>
              );
            })}
          </table>
        ))}
      </div>
    </div>
  );
}
