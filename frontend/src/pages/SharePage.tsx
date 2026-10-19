import { useCallback, useEffect, useRef, useState } from 'react';
import { useParams, useNavigate, Link } from 'react-router-dom';
import { useIsolaStream } from '../hooks/useIsolaStream';
import { AudioToggleButton } from '../components/AudioToggleButton';
import { IsolaLogo, ISOLA_LOGO_KEYFRAMES } from '../components/IsolaLogo';
import { LoadingOverlay } from '../components/LoadingOverlay';

function formatTime(totalSec: number): string {
  if (totalSec <= 0) return '0:00';
  const m = Math.floor(totalSec / 60);
  const s = totalSec % 60;
  return `${m}:${s.toString().padStart(2, '0')}`;
}

export default function SharePage() {
  const { token } = useParams<{ token: string }>();
  const navigate = useNavigate();
  const canvasRef = useRef<HTMLDivElement>(null);
  const shellRef = useRef<HTMLDivElement>(null);

  const [status, setStatus] = useState<string>('loading');
  const [expiresAt, setExpiresAt] = useState<number | null>(null);
  const [remainingSec, setRemainingSec] = useState(0);
  const connectedRef = useRef(false);

  const isola = useIsolaStream();
  const isolaRef = useRef(isola);
  isolaRef.current = isola;

  const sessionIdRef = useRef<string | null>(null);

  useEffect(() => {
    if (!token) return;
    fetch(`/api/share/${token}`)
      .then((r) => {
        if (!r.ok) throw new Error('invalid');
        return r.json();
      })
      .then((data) => {
        sessionIdRef.current = data.id;
        if (data.expires_at) {
          setExpiresAt(new Date(data.expires_at).getTime());
        }
        // GetShare only succeeds for a running session, so the extra
        // provisioning poll was a wasted round-trip on the first-frame path.
        setStatus(data.status === 'running' ? 'running' : 'error');
      })
      .catch(() => navigate('/?error=share_invalid', { replace: true }));
  }, [token, navigate]);

  useEffect(() => {
    if (status !== 'running' || !token || !sessionIdRef.current) return;
    const el = canvasRef.current;
    if (!el || connectedRef.current) return;

    connectedRef.current = true;
    const protocol = window.location.protocol === 'https:' ? 'wss' : 'ws';
    const baseUrl = `${protocol}://${window.location.host}/ws/${sessionIdRef.current}`;
    const streamWsUrl = `${baseUrl}?share_token=${encodeURIComponent(token)}&channel=isola`;
    const inputWsUrl = `${baseUrl}?share_token=${encodeURIComponent(token)}&channel=input`;
    isolaRef.current.connect(streamWsUrl, inputWsUrl, el, true);

    return () => {
      if (connectedRef.current) {
        isolaRef.current.disconnect();
        connectedRef.current = false;
      }
    };
  }, [status, token]);

  useEffect(() => {
    if (!token) return;
    let cancelled = false;

    const syncExpiry = async () => {
      if (cancelled) return;
      try {
        const res = await fetch(`/api/share/${token}`);
        if (!res.ok) return;
        const data = await res.json();
        if (data.expires_at && !cancelled) {
          setExpiresAt(new Date(data.expires_at).getTime());
        }
        if ((data.status === 'error' || data.status === 'terminated') && !cancelled) {
          setStatus('error');
        }
      } catch {
        // retry next interval
      }
    };

    syncExpiry();
    const interval = setInterval(syncExpiry, 10_000);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [token]);

  useEffect(() => {
    if (!expiresAt) return;
    const tick = () => {
      const sec = Math.max(0, Math.floor((expiresAt - Date.now()) / 1000));
      setRemainingSec(sec);
    };
    tick();
    const interval = setInterval(tick, 1000);
    return () => clearInterval(interval);
  }, [expiresAt]);

  const toggleAudio = useCallback(() => {
    isola.setAudioMuted(!isola.audioMuted);
  }, [isola]);

  const timerColor =
    remainingSec <= 60 ? 'text-red-600 dark:text-red-400' :
    remainingSec <= 180 ? 'text-yellow-600 dark:text-yellow-400' :
    'text-zinc-600 dark:text-gray-400';

  if (status === 'loading') {
    return (
      <div className="flex items-center justify-center h-screen bg-zinc-200 dark:bg-gray-950">
        <div className="w-8 h-8 border-2 border-zinc-400 dark:border-gray-600 border-t-zinc-900 dark:border-t-white rounded-full animate-spin" />
      </div>
    );
  }

  if (status === 'error') {
    return (
      <div className="flex flex-col items-center justify-center h-screen bg-zinc-200 dark:bg-gray-950 gap-4">
        <p className="text-red-600 dark:text-red-400 text-sm">Share link expired or invalid</p>
        <button
          onClick={() => navigate('/')}
          className="px-4 py-2 bg-zinc-300 hover:bg-zinc-400 dark:bg-gray-800 dark:hover:bg-gray-700 rounded-lg text-sm transition-colors"
        >
          Go home
        </button>
      </div>
    );
  }

  return (
    <div className="flex flex-col h-screen bg-zinc-200 dark:bg-gray-950 relative">
      <div className="flex items-center justify-between px-4 py-1 bg-zinc-100 dark:bg-gray-900 shrink-0 relative z-50">
        <Link to="/" className="text-zinc-500 dark:text-gray-400 hover:text-zinc-900 dark:hover:text-white transition-colors shrink-0" title="Home">
          <IsolaLogo className="w-9 h-9 mt-1" mini />
        </Link>
        <div className="flex items-center gap-4">
          <span className={`text-xs font-mono ${timerColor}`}>
            {remainingSec > 0 ? formatTime(remainingSec) : '...'}
          </span>
          {isola.reconnecting && (
            <span className="text-xs text-yellow-600 dark:text-yellow-400 font-mono animate-pulse">Reconnecting...</span>
          )}
          <span className={`w-1.5 h-1.5 rounded-full ${isola.connected ? 'bg-green-500' : isola.reconnecting ? 'bg-yellow-500 animate-pulse' : 'bg-yellow-500'}`} />
        </div>
        <div className="flex items-center gap-1">
          <AudioToggleButton
            muted={isola.audioMuted}
            onToggle={toggleAudio}
            disabled={!isola.connected}
          />
          <button
            onClick={() => navigate('/')}
            className="px-3 py-1 text-zinc-500 dark:text-gray-400 hover:text-zinc-900 dark:hover:text-white rounded text-xs transition-colors"
          >
            Close
          </button>
        </div>
      </div>

      <div ref={shellRef} className="flex-1 bg-zinc-100 dark:bg-gray-900 min-h-0 overflow-hidden flex items-center justify-center">
        <div id="vnc-container" ref={canvasRef} className="relative overflow-hidden bg-zinc-100 dark:bg-gray-900 shrink-0 w-full h-full" />
      </div>
      <style>{ISOLA_LOGO_KEYFRAMES + `
        #vnc-container > div {
          width: 100% !important;
          height: 100% !important;
          display: flex !important;
          align-items: center !important;
          justify-content: center !important;
          background: var(--vnc-container-bg) !important;
          overflow: hidden !important;
        }
        #vnc-container > div > canvas {
          margin: 0 auto !important;
          max-width: 100% !important;
          max-height: 100% !important;
          pointer-events: none !important;
          user-select: none !important;
          -webkit-user-select: none !important;
        }
      `}</style>

      {!isola.firstFrame && !isola.error && (
        <LoadingOverlay status="running" isReconnecting={false} phases="connect" />
      )}
    </div>
  );
}
