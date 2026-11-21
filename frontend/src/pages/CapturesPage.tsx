// Saved-resources review page for one capture archive.
//
// Two entry points share it:
//   /session/:id/captures  — owner page, opened from the viewer (the session
//                            control token may ride in the URL fragment, which
//                            is consumed and scrubbed here).
//   /captures/:id          — archive-only link; the archive key from
//                            localStorage is the only credential needed.
//
// The page owns the route, chrome and token handoff; the archive itself is
// rendered by CaptureArchiveView, the same component the viewer's docked
// Resources panel uses, so both surfaces cannot drift apart.
import { useCallback, useLayoutEffect, useState } from 'react';
import { Link, useLocation, useParams } from 'react-router-dom';
import { ArrowLeft } from 'lucide-react';

import { CaptureArchiveView } from '../components/CaptureArchiveView';
import type { CaptureArchive } from '../lib/captureApi';
import { consumeSessionTokenFromUrl } from '../lib/sessionToken';

export default function CapturesPage() {
  const { id } = useParams<{ id: string }>();
  const location = useLocation();
  const inSession = location.pathname.startsWith('/session/');
  const [archive, setArchive] = useState<CaptureArchive | null>(null);
  const [fileCount, setFileCount] = useState(0);

  // The owner page may arrive with `#token=…`: store it and scrub it before any
  // further navigation, exactly like the network pop-out.
  useLayoutEffect(() => {
    if (!inSession || !id) return;
    consumeSessionTokenFromUrl(id);
  }, [inSession, id]);

  const onArchive = useCallback((next: CaptureArchive | null, count: number) => {
    setArchive(next);
    setFileCount(count);
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
            {fileCount} {fileCount === 1 ? 'file' : 'files'}
            {archive?.meta?.complete === false ? ' · capture incomplete' : ''}
          </p>
        </div>
      </header>

      {/* The layout effect above stores the fragment token and the archive key
          before this child's effects run, and the view re-reads the key per
          request, so no extra gating is needed here. */}
      <CaptureArchiveView archiveId={id} layout="full" onArchive={onArchive} />
    </div>
  );
}
