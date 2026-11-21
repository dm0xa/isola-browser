// Saved-resources review surface for one capture archive.
//
// Two entry points share it:
//   /session/:id/captures  — owner page, opened from the viewer (the session
//                            control token may ride in the URL fragment, which
//                            is consumed and scrubbed here).
//   /captures/:id          — archive-only link; the archive key from
//                            localStorage is the only credential needed.
//
// Nothing archived is ever injected into this origin as markup: text (HTML
// included) is shown in a <pre>, images through an object URL, everything else
// is download-only.
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { Link, useLocation, useParams } from 'react-router-dom';
import { AlertTriangle, ArrowLeft, Copy, Download, FileImage, RefreshCw, X } from 'lucide-react';

import { copyToClipboard } from '../lib/clipboard';
import { isCaptureEnabled } from '../lib/config';
import {
  captureErrorText,
  fetchArchiveBlob,
  fetchArchiveZip,
  getArchive,
  getArchiveKey,
  type CaptureArchive,
} from '../lib/captureApi';
import {
  filterCaptureFiles,
  formatBytes,
  formatSavedAt,
  groupCaptureFiles,
  previewKind,
  saveBlob,
  shortSha256,
  type CaptureFile,
} from '../lib/captureFormat';
import { consumeSessionTokenFromUrl } from '../lib/sessionToken';

type OpenPreview = {
  sha256: string;
  filename: string;
  kind: 'text' | 'image';
  text?: string;
  url?: string;
};

export default function CapturesPage() {
  const { id } = useParams<{ id: string }>();
  const location = useLocation();
  const inSession = location.pathname.startsWith('/session/');

  const [key, setKey] = useState(() => (id ? getArchiveKey(id) : ''));
  const [archive, setArchive] = useState<CaptureArchive | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState('');
  const [actionError, setActionError] = useState('');
  const [query, setQuery] = useState('');
  const [copied, setCopied] = useState('');
  const [preview, setPreview] = useState<OpenPreview | null>(null);
  const [zipping, setZipping] = useState(false);
  const mountedRef = useRef(true);
  const objectUrlRef = useRef('');
  // Only the newest Open request may land: a slow first fetch must not replace
  // the preview the user asked for second.
  const previewSeqRef = useRef(0);

  // The owner page may arrive with `#token=…`: store it and scrub it before any
  // further navigation, exactly like the network pop-out.
  useLayoutEffect(() => {
    if (!inSession || !id) return;
    consumeSessionTokenFromUrl(id);
    setKey(getArchiveKey(id));
  }, [inSession, id]);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      if (objectUrlRef.current) URL.revokeObjectURL(objectUrlRef.current);
    };
  }, []);

  const load = useCallback(async () => {
    if (!id) return;
    if (!isCaptureEnabled()) {
      setLoading(false);
      setLoadError('Resource capture is disabled on this deployment.');
      return;
    }
    if (!key) {
      setLoading(false);
      setLoadError(
        'This browser does not hold the archive key for this capture. Open the review page from the session that recorded it.',
      );
      return;
    }
    setLoading(true);
    setLoadError('');
    try {
      const next = await getArchive(id, key);
      if (!mountedRef.current) return;
      setArchive(next);
    } catch (err) {
      if (!mountedRef.current) return;
      setLoadError(captureErrorText(err));
    } finally {
      if (mountedRef.current) setLoading(false);
    }
  }, [id, key]);

  useEffect(() => {
    void load();
  }, [load]);

  const files = useMemo(() => groupCaptureFiles(archive?.items ?? []), [archive]);
  const visible = useMemo(() => filterCaptureFiles(files, query), [files, query]);

  const closePreview = useCallback(() => {
    previewSeqRef.current += 1;
    setPreview(null);
    if (objectUrlRef.current) {
      URL.revokeObjectURL(objectUrlRef.current);
      objectUrlRef.current = '';
    }
  }, []);

  const openFile = useCallback(
    async (file: CaptureFile) => {
      if (!id) return;
      const kind = previewKind(file.mime);
      if (kind === 'none') return;
      const seq = previewSeqRef.current + 1;
      previewSeqRef.current = seq;
      setActionError('');
      setPreview({ sha256: file.sha256, filename: file.filename, kind });
      try {
        const blob = await fetchArchiveBlob(id, file.sha256, key);
        if (!mountedRef.current || previewSeqRef.current !== seq) return;
        if (kind === 'text') {
          const text = await blob.text();
          if (!mountedRef.current || previewSeqRef.current !== seq) return;
          setPreview({ sha256: file.sha256, filename: file.filename, kind, text });
        } else {
          if (objectUrlRef.current) URL.revokeObjectURL(objectUrlRef.current);
          const url = URL.createObjectURL(blob);
          objectUrlRef.current = url;
          setPreview({ sha256: file.sha256, filename: file.filename, kind, url });
        }
      } catch (err) {
        if (!mountedRef.current || previewSeqRef.current !== seq) return;
        setPreview(null);
        setActionError(captureErrorText(err));
      }
    },
    [id, key],
  );

  const downloadFile = useCallback(
    async (file: CaptureFile) => {
      if (!id) return;
      setActionError('');
      try {
        const blob = await fetchArchiveBlob(id, file.sha256, key);
        saveBlob(blob, file.filename);
      } catch (err) {
        setActionError(captureErrorText(err));
      }
    },
    [id, key],
  );

  const downloadAll = useCallback(async () => {
    if (!id) return;
    setZipping(true);
    setActionError('');
    try {
      const blob = await fetchArchiveZip(id, key);
      saveBlob(blob, `isola-capture-${id}.zip`);
    } catch (err) {
      setActionError(captureErrorText(err));
    } finally {
      setZipping(false);
    }
  }, [id, key]);

  const copySha = useCallback(async (sha256: string) => {
    const ok = await copyToClipboard(sha256);
    if (!ok) return;
    setCopied(sha256);
    window.setTimeout(() => setCopied((cur) => (cur === sha256 ? '' : cur)), 1500);
  }, []);

  const backTo = inSession && id ? `/session/${id}` : '/';

  if (!id) {
    return (
      <div className="flex h-dvh items-center justify-center bg-white text-sm text-zinc-500 dark:bg-gray-950 dark:text-gray-400">
        Invalid capture link.
      </div>
    );
  }

  return (
    <div className="flex h-dvh flex-col bg-white text-zinc-900 dark:bg-gray-950 dark:text-gray-100">
      <header className="flex shrink-0 items-center gap-3 border-b border-zinc-200 px-4 py-2 dark:border-gray-800">
        <Link
          to={backTo}
          className="flex items-center gap-1 text-xs text-zinc-500 hover:text-zinc-900 dark:text-gray-400 dark:hover:text-white"
          title={inSession ? 'Back to the session' : 'Home'}
        >
          <ArrowLeft size={14} aria-hidden />
          {inSession ? 'Session' : 'Home'}
        </Link>
        <div className="min-w-0 flex-1">
          <h1 className="truncate text-sm font-medium">Saved resources</h1>
          <p className="truncate text-[11px] text-zinc-500 dark:text-gray-500">
            {archive?.meta?.target_url ? `${archive.meta.target_url} · ` : ''}
            {archive?.meta?.created_at ? `recorded ${new Date(archive.meta.created_at).toLocaleString()} · ` : ''}
            {files.length} {files.length === 1 ? 'file' : 'files'}
            {archive?.meta?.complete === false ? ' · capture incomplete' : ''}
          </p>
        </div>
        <input
          type="search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Filter URL, file name, or mime"
          className="w-64 rounded border border-zinc-300 bg-white px-2 py-1 text-xs outline-none focus:border-cyan-500 dark:border-gray-700 dark:bg-gray-900 dark:text-gray-100"
          aria-label="Filter saved resources"
        />
        <button
          type="button"
          onClick={() => { void load(); }}
          className="flex items-center gap-1 rounded border border-zinc-300 px-2 py-1 text-xs text-zinc-600 hover:bg-zinc-100 dark:border-gray-700 dark:text-gray-300 dark:hover:bg-gray-800"
          title="Reload the archive"
        >
          <RefreshCw size={13} aria-hidden />
          Reload
        </button>
        <button
          type="button"
          onClick={() => { void downloadAll(); }}
          disabled={zipping || files.length === 0}
          className="flex items-center gap-1 rounded bg-cyan-600 px-2 py-1 text-xs font-medium text-white hover:bg-cyan-500 disabled:cursor-not-allowed disabled:opacity-40"
          title="Download every saved file as a zip"
        >
          <Download size={13} aria-hidden />
          {zipping ? 'Zipping…' : 'Download all'}
        </button>
      </header>

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
            <span className="min-w-0 flex-1 truncate font-mono" title={preview.filename}>{preview.filename}</span>
            <button
              type="button"
              onClick={closePreview}
              className="flex items-center gap-1 rounded px-1.5 py-0.5 text-zinc-500 hover:bg-zinc-200 dark:text-gray-400 dark:hover:bg-gray-800"
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
              onClick={() => { void load(); }}
              className="mt-2 rounded border border-red-300 px-2 py-0.5 hover:bg-red-100 dark:border-red-800 dark:hover:bg-red-900/40"
            >
              Retry
            </button>
          </div>
        )}

        {!loading && !loadError && files.length === 0 && (
          <div className="px-4 py-8 text-center text-xs text-zinc-500 dark:text-gray-400">
            <p className="text-sm text-zinc-700 dark:text-gray-200">No resources saved yet.</p>
            <p className="mt-1">
              Record from the session's menu while browsing; downloaded files appear here when the
              capture has pulled them from the sandbox.
            </p>
          </div>
        )}

        {!loading && !loadError && files.length > 0 && (
          <table className="w-full table-fixed border-collapse text-left text-xs">
            <thead className="sticky top-0 z-10 bg-zinc-100 text-[10px] uppercase tracking-wide text-zinc-500 dark:bg-gray-900 dark:text-gray-500">
              <tr>
                <th className="px-3 py-1.5 font-normal">file / url</th>
                <th className="w-28 px-2 py-1.5 font-normal">mime</th>
                <th className="w-20 px-2 py-1.5 text-right font-normal">size</th>
                <th className="w-40 px-2 py-1.5 font-normal">saved</th>
                <th className="w-32 px-2 py-1.5 font-normal">sha256</th>
                <th className="w-36 px-2 py-1.5 font-normal">actions</th>
              </tr>
            </thead>
            <tbody>
              {visible.length === 0 && (
                <tr>
                  <td colSpan={6} className="px-3 py-6 text-center text-zinc-500 dark:text-gray-400">
                    No saved files match “{query}”.
                  </td>
                </tr>
              )}
              {visible.map((file) => {
                const kind = previewKind(file.mime);
                return (
                  <tr
                    key={file.sha256}
                    className="border-b border-zinc-100 hover:bg-zinc-50 dark:border-gray-800/60 dark:hover:bg-gray-900/60"
                  >
                    <td className="px-3 py-1.5">
                      <div className="flex items-center gap-1.5">
                        <span className="truncate font-medium" title={file.filename}>{file.filename}</span>
                        {file.versionCount > 1 && (
                          <span
                            className="shrink-0 rounded bg-amber-500/15 px-1 text-[9px] uppercase tracking-wide text-amber-700 dark:text-amber-300"
                            title={`Version ${file.version} of ${file.versionCount} saved for this URL`}
                          >
                            v{file.version}
                          </span>
                        )}
                      </div>
                      <div className="truncate text-[10px] text-zinc-500 dark:text-gray-500" title={file.urls.join('\n')}>
                        {file.urls[0] || '—'}
                        {file.urls.length > 1 && (
                          <span className="ml-1.5 text-zinc-400 dark:text-gray-600">shared by {file.urls.length} URLs</span>
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
                      <div className="flex items-center gap-1">
                        <button
                          type="button"
                          onClick={() => { void downloadFile(file); }}
                          className="flex items-center gap-1 rounded border border-zinc-300 px-1.5 py-0.5 text-[10px] text-zinc-600 hover:bg-zinc-100 dark:border-gray-700 dark:text-gray-300 dark:hover:bg-gray-800"
                          title="Save this file"
                        >
                          <Download size={11} aria-hidden />
                          Download
                        </button>
                        {kind !== 'none' && (
                          <button
                            type="button"
                            onClick={() => { void openFile(file); }}
                            className="rounded border border-zinc-300 px-1.5 py-0.5 text-[10px] text-zinc-600 hover:bg-zinc-100 dark:border-gray-700 dark:text-gray-300 dark:hover:bg-gray-800"
                            title={kind === 'text' ? 'Show the response body as source text' : 'Show the image'}
                          >
                            Open
                          </button>
                        )}
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
