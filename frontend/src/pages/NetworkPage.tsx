import { useCallback, useEffect, useLayoutEffect, useMemo, useState, type ReactNode } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { ArrowLeft, RefreshCw } from 'lucide-react';

import { IsolaNetworkPanel, type PanelTab } from '../components/IsolaNetworkPanel';
import { useNetworkStream, type NetworkStreamStatus } from '../hooks/useNetworkStream';
import { api } from '../lib/api';
import { isIntelEnabled } from '../lib/config';
import { fetchSessionIntel, type IntelFetch } from '../lib/intel';
import { consumeSessionTokenFromUrl, getSessionToken } from '../lib/sessionToken';

interface SessionMeta {
  status: string;
  transport: string;
  targetUrl: string;
}

const STATUS_UI: Record<NetworkStreamStatus, { label: string; dot: string; text: string }> = {
  idle: { label: 'Idle', dot: 'bg-gray-500', text: 'text-zinc-500 dark:text-gray-400' },
  connecting: { label: 'Connecting', dot: 'bg-amber-400 animate-pulse', text: 'text-amber-600 dark:text-amber-300' },
  live: { label: 'Live', dot: 'bg-emerald-400', text: 'text-emerald-600 dark:text-emerald-300' },
  reconnecting: { label: 'Reconnecting', dot: 'bg-amber-400 animate-pulse', text: 'text-amber-600 dark:text-amber-300' },
  error: { label: 'Offline', dot: 'bg-red-400', text: 'text-red-600 dark:text-red-300' },
};

function StatusBadge({ status }: { status: NetworkStreamStatus }) {
  const ui = STATUS_UI[status];
  return (
    <span className={`flex items-center gap-1.5 text-[11px] ${ui.text}`}>
      <span className={`h-1.5 w-1.5 rounded-full ${ui.dot}`} />
      {ui.label}
    </span>
  );
}

function Header({
  sessionId,
  targetHost,
  status,
}: {
  sessionId: string;
  targetHost?: string;
  status?: NetworkStreamStatus;
}) {
  return (
    <header className="flex h-11 shrink-0 items-center gap-3 border-b border-zinc-200 bg-white px-3 dark:border-gray-800 dark:bg-gray-950">
      <Link
        to={`/session/${sessionId}`}
        className="flex items-center gap-1.5 rounded px-2 py-1 text-xs text-zinc-500 transition-colors hover:bg-zinc-200 hover:text-zinc-900 dark:text-gray-400 dark:hover:bg-gray-800 dark:hover:text-gray-100"
        title="Back to the session"
      >
        <ArrowLeft size={14} />
        <span>Session</span>
      </Link>
      <span className="text-sm font-semibold text-zinc-900 dark:text-gray-100">Network monitor</span>
      <code
        className="hidden max-w-[18rem] truncate rounded bg-zinc-100 px-1.5 py-0.5 text-[11px] text-zinc-500 sm:block dark:bg-gray-900 dark:text-gray-500"
        title={sessionId}
      >
        {sessionId}
      </code>
      {targetHost && (
        <span className="hidden truncate text-[11px] text-zinc-500 md:block dark:text-gray-500">· {targetHost}</span>
      )}
      <div className="flex-1" />
      {status && <StatusBadge status={status} />}
    </header>
  );
}

function Notice({ title, body, sessionId }: { title: string; body: string; sessionId: string }) {
  return (
    <div className="flex h-dvh flex-col bg-white text-zinc-800 dark:bg-gray-950 dark:text-gray-200">
      <Header sessionId={sessionId} />
      <div className="flex flex-1 items-center justify-center p-6">
        <div className="max-w-md text-center">
          <div className="text-sm font-semibold text-zinc-900 dark:text-gray-100">{title}</div>
          <p className="mt-2 text-xs leading-relaxed text-zinc-500 dark:text-gray-400">{body}</p>
        </div>
      </div>
    </div>
  );
}

/**
 * Standalone network monitor for a single session — the target of the viewer's
 * "open in new tab" action, served at /session/:id/network.
 *
 * Access is owner-only and enforced by the server: the page attaches to the
 * session's `channel=input` WebSocket, which the proxy rejects with 403 unless
 * the caller presents the session's control token (a share/view token grants
 * view-only and cannot open this channel). The client therefore holds no
 * traffic until the control token is presented; without one it shows a notice
 * and never renders captured data.
 */
export default function NetworkPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const [token, setToken] = useState('');
  const [meta, setMeta] = useState<SessionMeta | null>(null);
  const [metaError, setMetaError] = useState('');
  const [tab, setTab] = useState<PanelTab>('network');

  // One-shot: lift the token out of the URL (fragment preferred — it is never
  // sent to the server), persist it, and scrub it from the address bar before
  // any further navigation or subresource load. Runs as a layout effect so the
  // resolved token is in place before the first paint and the "control token
  // required" notice never flashes for a legitimate pop-out.
  useLayoutEffect(() => {
    if (!id) return;
    consumeSessionTokenFromUrl(id);
    setToken(getSessionToken(id));
  }, [id]);

  // Identify the session (transport, target) so we can tell whether the traffic
  // side-channel exists at all. Stops once the session is running.
  useEffect(() => {
    if (!id) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    const poll = async () => {
      try {
        const res = await api.get(
          `/api/sessions/${id}`,
          token ? { headers: { 'X-Session-Token': token } } : undefined,
        );
        if (cancelled) return;
        const data = res.data;
        setMeta({
          status: data.status || '',
          transport: data.transport || '',
          targetUrl: data.target_url || '',
        });
        setMetaError('');
        if (data.session_token && data.session_token !== token) {
          setToken(data.session_token);
        }
        if (data.status === 'running') return;
      } catch (err) {
        if (cancelled) return;
        const status = (err as { response?: { status?: number } })?.response?.status;
        setMetaError(
          status === 404
            ? 'Session not found or already closed.'
            : 'Could not reach the session service.',
        );
      }
      if (!cancelled) timer = setTimeout(poll, 3000);
    };

    void poll();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [id, token]);

  const targetHost = useMemo(() => {
    if (!meta?.targetUrl) return '';
    try {
      return new URL(meta.targetUrl).host;
    } catch {
      return meta.targetUrl;
    }
  }, [meta?.targetUrl]);

  const isIsola = meta?.transport === 'isola';
  const wrongTransport = !!meta && !!meta.transport && meta.transport !== 'isola';
  const awaitingMeta = !!token && !meta && !metaError;

  const wsUrl = useMemo(() => {
    if (!id || !token || !isIsola) return null;
    const protocol = window.location.protocol === 'https:' ? 'wss' : 'ws';
    const qs = new URLSearchParams({ session_token: token, host: '1', channel: 'input' });
    return `${protocol}://${window.location.host}/ws/${id}?${qs.toString()}`;
  }, [id, token, isIsola]);

  const { entries, consoleEntries, cookieEntries, docHeaders, tabs, status, retry } = useNetworkStream(wsUrl);

  const onIntel = useCallback<IntelFetch>((query) => {
    if (!id) return Promise.reject(new Error('No session'));
    return fetchSessionIntel(id, token, query);
  }, [id, token]);

  const backToSession = useCallback(() => navigate(`/session/${id}`), [navigate, id]);

  if (!id) {
    return (
      <div className="flex h-dvh items-center justify-center bg-white text-sm text-zinc-500 dark:bg-gray-950 dark:text-gray-400">
        Invalid session link.
      </div>
    );
  }

  if (!token) {
    return (
      <Notice
        sessionId={id}
        title="Control token required"
        body="Opening this page outside the session is not allowed. This monitor needs the session's control token, which is never exposed by the URL alone. Open it from the session viewer: ⋮ menu → “Show network panel” → ↗ (new tab)."
      />
    );
  }

  if (metaError) {
    return <Notice sessionId={id} title={metaError} body="The session may have ended. Return to the session to start a new one." />;
  }

  if (wrongTransport) {
    return (
      <Notice
        sessionId={id}
        title="Network monitor unavailable"
        body={`This session streams over “${meta?.transport}”. The traffic monitor is only available for sessions using the Isola transport.`}
      />
    );
  }

  if (awaitingMeta) {
    return (
      <div className="flex h-dvh flex-col bg-white text-zinc-800 dark:bg-gray-950 dark:text-gray-200">
        <Header sessionId={id} status="connecting" />
        <div className="flex flex-1 items-center justify-center text-xs text-zinc-500 dark:text-gray-500">
          Contacting session…
        </div>
      </div>
    );
  }

  const overlay =
    status === 'live' ? null : status === 'error' ? (
      <Overlay>
        <div className="text-sm font-medium text-zinc-900 dark:text-gray-100">Could not attach to session traffic</div>
        <p className="mt-2 text-xs leading-relaxed text-zinc-500 dark:text-gray-400">
          The session may have ended, or the control token is no longer valid. Reopen this monitor
          from the session viewer.
        </p>
        <div className="mt-4 flex items-center justify-center gap-2">
          <button
            onClick={retry}
            className="flex items-center gap-1.5 rounded bg-cyan-500/15 px-3 py-1.5 text-xs text-cyan-700 transition-colors hover:bg-cyan-500/25 dark:text-cyan-300"
          >
            <RefreshCw size={13} /> Retry
          </button>
          <button
            onClick={backToSession}
            className="rounded px-3 py-1.5 text-xs text-zinc-600 transition-colors hover:bg-zinc-200 hover:text-zinc-900 dark:text-gray-300 dark:hover:bg-gray-800 dark:hover:text-gray-100"
          >
            Back to session
          </button>
        </div>
      </Overlay>
    ) : (
      <Overlay>
        <div className="flex items-center gap-2 text-xs text-zinc-500 dark:text-gray-400">
          <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-amber-400" />
          {status === 'reconnecting' ? 'Reconnecting to session traffic…' : 'Attaching to session traffic…'}
        </div>
      </Overlay>
    );

  return (
    <div className="flex h-dvh flex-col bg-white text-zinc-800 dark:bg-gray-950 dark:text-gray-200">
      <Header sessionId={id} targetHost={targetHost} status={status} />
      <div className="relative min-h-0 flex-1">
        <IsolaNetworkPanel
          full
          entries={entries}
          consoleEntries={consoleEntries}
          cookieEntries={cookieEntries}
          docHeaders={docHeaders}
          onIntel={onIntel}
          intelDisabled={!isIntelEnabled()}
          layout="side"
          tab={tab}
          onTab={setTab}
          onLayout={() => {}}
          onClose={backToSession}
          closeTitle="Back to the session"
          tabs={tabs}
        />
        {overlay}
      </div>
    </div>
  );
}

function Overlay({ children }: { children: ReactNode }) {
  return (
    <div className="absolute inset-0 z-40 flex items-center justify-center bg-white/75 backdrop-blur-sm dark:bg-gray-950/75">
      <div className="max-w-sm px-6 text-center">{children}</div>
    </div>
  );
}
