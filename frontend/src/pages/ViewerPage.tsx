import React, { useEffect, useRef, useState, useCallback } from 'react';
import { useParams, useNavigate, useSearchParams, Link } from 'react-router-dom';
import { useIsolaStream, type IsolaQuality } from '../hooks/useIsolaStream';
import type { NetEntry, ConsoleEntry, CookieEntry, PanelLayout, PanelTab } from '../components/IsolaNetworkPanel';
import { LoadingOverlay } from '../components/LoadingOverlay';
import { getToolbarButtons, getPluginProviders, usePlugins } from '../plugins/registry';
import { useTheme } from '../lib/ThemeContext';
import { api } from '../lib/api';
import { getCommit, getVersion, isTimerEnabled } from '../lib/config';
import { clearSessionToken, getSessionToken, storeSessionToken } from '../lib/sessionToken';
import { endLaunch, setLaunchStatus, useLaunchProgress } from '../lib/launchProgress';
import { useSessionTimer } from './viewer/useSessionTimer';
import { useIsolaMonitorEvents } from './viewer/useIsolaMonitorEvents';
import { ViewerToolbar } from './viewer/ViewerToolbar';
import { ViewerStreamShell } from './viewer/ViewerStreamShell';
import { ViewerDebugOverlay } from './viewer/ViewerDebugOverlay';
import { buildDebugInfo, copyTextToClipboard, formatDebugInfo } from './viewer/debugInfo';
import { captureResolutionFor, parseResolution } from '../lib/resolution';

export default function ViewerPage() {
  const CONNECTING_STALL_TIMEOUT_MS = 12000;
  const RECOVERABLE_ERROR_RETRY_MS = 1000;

  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const { theme } = useTheme();
  const [searchParams, setSearchParams] = useSearchParams();
  const viewOnly = searchParams.get('viewonly') === 'true';
  // Admin shadow mode: ?shadow=true attaches as a server-enforced view-only
  // subscriber authenticated by the admin bearer token in the WS query (the
  // proxy drops input/clipboard/ABR; ping and request_keyframe still reach
  // the encoder so a late joiner can resync). No session control token is
  // needed or used.
  const shadow = searchParams.get('shadow') === 'true';
  // Capture the admin token once, then drop it from the URL (below) so it does
  // not linger in the address bar, history, or a copied debug report. Shadow
  // mode keeps working from the ref.
  const shadowTokenRef = useRef(searchParams.get('admin_token') || '');
  const shadowToken = searchParams.get('admin_token') || shadowTokenRef.current;
  const hideToolbar = searchParams.get('hide_toolbar') === '1';
  const debugEnabled = searchParams.get('debug') === '1';
  const [debugCopied, setDebugCopied] = useState(false);
  const [debugCopyFailed, setDebugCopyFailed] = useState(false);
  const toggleDebug = () => {
    const next = new URLSearchParams(searchParams);
    if (debugEnabled) next.delete('debug');
    else next.set('debug', '1');
    setSearchParams(next, { replace: true });
  };
  const [shareCopied, setShareCopied] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const [isFullscreen, setIsFullscreen] = useState(false);
  // Slot in the toolbar row that hosts the isola tab strip while fullscreen,
  // folding the browser chrome into a single line with the session controls.
  const tabBarSlotRef = useRef<HTMLDivElement>(null);

  const canvasRef = useRef<HTMLDivElement>(null);
  const shellRef = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<string>('provisioning');
  // The launch overlay lives above the router (see lib/launchProgress), so a
  // session started from RunPage keeps the same overlay and only advances its
  // phases from here. For direct deep links the flow is inactive and the
  // page-local overlay below handles connecting.
  const launch = useLaunchProgress();
  // Network monitor (isola): entries stream in as 'isola-net' DOM events
  // from the transport hook. Capped so long sessions don't grow unbounded.
  //
  // The container always streams network events for isola sessions (env is
  // set at creation time) so the panel can be toggled on later and still
  // show the full traffic history — we keep the DOM listeners attached
  // regardless of panel state and only toggle *visibility* here.
  const [netEntries, setNetEntries] = useState<NetEntry[]>([]);
  const [netPanel, setNetPanel] = useState<PanelLayout | 'off'>('off');
  const netEntriesRef = useRef<NetEntry[]>([]);
  const [netTab, setNetTab] = useState<PanelTab>('network');
  const [consoleEntries, setConsoleEntries] = useState<ConsoleEntry[]>([]);
  const [cookieEntries, setCookieEntries] = useState<CookieEntry[]>([]);
  const consoleRef = useRef<ConsoleEntry[]>([]);
  const cookieRef = useRef<CookieEntry[]>([]);
  // True when the session was provisioned with CDP network capture enabled
  // (currently always for isola sessions; reflects the new network_monitor
  // field from /api/sessions/{id}). The menu toggle is only shown when this
  // is true.
  const [networkCaptureEnabled, setNetworkCaptureEnabled] = useState(false);
  const [sessionError, setSessionError] = useState<string>('');
  const connectedRef = useRef(false);
  const connectingRef = useRef(false);
  const connectStartedAtRef = useRef(0);
  const recoveryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const sessionTokenRef = useRef('');
  // Captured session config (from /api/sessions/{id}) so the toolbar's
  // "restart" action can launch a fresh session with the same URL, transport,
  // resolution, browser, and theme.
  const sessionConfigRef = useRef<{
    targetUrl: string;
    transport: string;
    resolution: string;
    browserType: string;
    torEnabled: boolean;
  }>({ targetUrl: '', transport: 'isola', resolution: '', browserType: '', torEnabled: false });
  // The landing-page URL shown in the isola tab strip before the runner has
  // emitted any tabs (or while the active tab is still on about:blank).
  const [seedTargetUrl, setSeedTargetUrl] = useState('');
  const [restarting, setRestarting] = useState(false);
  const isola = useIsolaStream(seedTargetUrl);
  const { metrics: isolaMetrics } = isola;
  const transport = 'isola' as const;
  const [connectionGeneration, setConnectionGeneration] = useState(0);

  const isolaRef = useRef(isola);
  isolaRef.current = isola;
  const [clipboardSynced, setClipboardSynced] = useState(false);
  // Mirror of `restarting` for read access inside the polling callback,
  // which closes over an older render and would otherwise see stale state.
  const restartingRef = useRef(false);
  restartingRef.current = restarting;
  const { remainingSec, extendAvailable, extend, sessionStatus, viewerCount, handlePollData, isUnlimited, expiresAt } = useSessionTimer(id);
  // Dynamic plugins (usePlugins re-renders when module chunks register)
  const pluginVersion = usePlugins();
  const toolbarButtons = React.useMemo(() => {
    void pluginVersion;
    return getToolbarButtons();
  }, [pluginVersion]);
  const providers = React.useMemo(() => {
    void pluginVersion;
    return getPluginProviders();
  }, [pluginVersion]);

  const resetTransport = useCallback(() => {
    if (recoveryTimerRef.current) {
      clearTimeout(recoveryTimerRef.current);
      recoveryTimerRef.current = null;
    }
    connectStartedAtRef.current = 0;
    connectedRef.current = false;
    connectingRef.current = false;
    isolaRef.current.disconnect();
    setConnectionGeneration((prev) => prev + 1);
  }, []);

  // Pop the network monitor into its own browser tab. The control token rides
  // in the URL fragment — which browsers never send to the server — so the new
  // tab can open the owner-only input channel without depending on
  // sessionStorage being inherited (and without a token in the query string,
  // where it would leak via the request line and Referer headers).
  const openNetworkPopOut = useCallback(() => {
    if (!id) return;
    const token = sessionTokenRef.current;
    const url = `/session/${id}/network${token ? `#token=${encodeURIComponent(token)}` : ''}`;
    window.open(url, '_blank', 'noopener,noreferrer');
  }, [id]);

  useEffect(() => {
    // Priority: URL ?token= param > sessionStorage > empty.
    // This lets the session owner share a full URL (including token) and
    // guarantees the WebSocket handshake won't 401 on a fresh browser tab.
    const urlToken = searchParams.get('token');
    if (urlToken && id) {
      storeSessionToken(id, urlToken);
      // Clean the token from the URL so it doesn't appear in browser
      // history or get leaked via referrer headers.
      const cleaned = new URLSearchParams(searchParams);
      cleaned.delete('token');
      setSearchParams(cleaned, { replace: true });
    }
    sessionTokenRef.current = id ? getSessionToken(id) : '';
  }, [id, searchParams, setSearchParams]);

  useEffect(() => {
    // The admin bearer token was captured into shadowTokenRef above; remove it
    // from the URL so it is not left in the address bar, history, screenshots,
    // or a copied debug report.
    if (!searchParams.has('admin_token')) return;
    const cleaned = new URLSearchParams(searchParams);
    cleaned.delete('admin_token');
    setSearchParams(cleaned, { replace: true });
  }, [searchParams, setSearchParams]);

  const isConnected = isola.connected;
  const isFirstFrame = isola.firstFrame;
  const streamError = isola.error;
  const isReconnecting = isola.reconnecting;
  const effectiveRemoteClipboard = isola.remoteClipboard;
  const audioMuted = isola.audioMuted;
  const setAudioMuted = isola.setAudioMuted;
  const audioActive = isola.audioActive;

  // Stream quality (isola ABR): manual override Auto/High/Medium/Low,
  // persisted in localStorage by the hook. Seed from the same key so the
  // menu checkmark matches what a reconnect will apply.
  const [quality] = useState<IsolaQuality>(() => {
    try {
      const v = localStorage.getItem('isola_quality');
      if (v === 'high' || v === 'medium' || v === 'low' || v === 'auto') return v;
    } catch { /* ignore */ }
    return 'auto';
  });
  // Mark poll loop complete only after the transport WS is actually open.
  useEffect(() => {
    if (isConnected) {
      connectedRef.current = true;
      connectingRef.current = false;
    }
  }, [isConnected]);

  // Reset and re-poll when stream auth/connect fails (e.g. expired session token).
  useEffect(() => {
    if (!streamError || !id) return;
    if (connectStartedAtRef.current && !isConnected && !isFirstFrame) {
      connectingRef.current = false;
      connectedRef.current = false;
      clearSessionToken(id);
      sessionTokenRef.current = '';
      setConnectionGeneration((g) => g + 1);
    }
  }, [streamError, isConnected, isFirstFrame, id]);

  const toggleFullscreen = useCallback(() => {
    if (document.fullscreenElement) {
      void document.exitFullscreen();
    } else {
      // Refused in iframes without allow="fullscreen" and on browsers with no
      // element fullscreen (iOS Safari); the button then simply does nothing
      // instead of leaving an unhandled rejection in the console.
      void document.documentElement.requestFullscreen().catch(() => {});
    }
  }, []);

  // Track fullscreen so the toolbar can add an exit control and fold the
  // browser chrome (tabs + address bar) into its own row.
  useEffect(() => {
    const onChange = () => setIsFullscreen(!!document.fullscreenElement);
    document.addEventListener('fullscreenchange', onChange);
    onChange();
    return () => document.removeEventListener('fullscreenchange', onChange);
  }, []);

  // In fullscreen, host the isola tab strip in the toolbar slot so the session
  // id, tabs, address bar and action buttons all sit on one line. Otherwise
  // hand it back to the stream container (null = default placement).
  useEffect(() => {
    isolaRef.current.setTabBarHost(isFullscreen ? tabBarSlotRef.current : null);
  }, [isFullscreen]);

  // ── Match the sandbox viewport to the capture box ──────────────────────
  // The capture draws with object-fit: contain, so a box whose aspect ratio
  // differs from the sandbox's — fullscreen always does, and so does a window
  // drag — is letterboxed. Ask the backend to reshape the sandbox instead, so
  // the page reflows like a real maximised browser. Debounced, because every
  // accepted resize replaces the encoder, and skipped while the network panel
  // is docked: its insets shrink the capture element without changing the
  // user's window.
  const resizeBudgetRef = useRef('');
  // The geometry the sandbox is believed to have, so a measurement that would
  // not change it (a few pixels of drag, or the launch size itself) costs
  // nothing.
  const sandboxGeometryRef = useRef<{ width: number; height: number } | null>(null);
  const resizeSessionRef = useRef('');
  useEffect(() => {
    if (!id || viewOnly || shadow) return;
    if (resizeSessionRef.current !== id) {
      // A restart navigates to a new session id with a possibly different
      // launch resolution: the budget and the baseline belong to that session.
      resizeSessionRef.current = id;
      resizeBudgetRef.current = '';
      sandboxGeometryRef.current = null;
    }
    let timer: ReturnType<typeof setTimeout> | null = null;

    const request = () => {
      if (netPanel !== 'off') return;
      const launch = sessionConfigRef.current.resolution;
      const box = document.querySelector<HTMLVideoElement | HTMLCanvasElement>(
        '#vnc-container video, #vnc-container canvas',
      );
      if (!launch || !box) return;
      if (!resizeBudgetRef.current) {
        // The launch resolution is the pixel budget; it must never shrink from
        // our own resizes, or toggling fullscreen back and forth would degrade
        // the picture a step at a time.
        resizeBudgetRef.current = launch;
        sandboxGeometryRef.current = parseResolution(launch);
      }
      const current = sandboxGeometryRef.current;
      if (!current) return;
      const target = captureResolutionFor(
        resizeBudgetRef.current,
        current,
        box.clientWidth,
        box.clientHeight,
      );
      if (!target) return;
      sandboxGeometryRef.current = target;
      const token = sessionTokenRef.current;
      void api
        .post(
          `/api/sessions/${id}/resize`,
          target,
          token ? { headers: { 'X-Session-Token': token } } : undefined,
        )
        .catch(() => {
          // A refused resize keeps the geometry the sandbox already has; forget
          // the request so the next measurement can retry it.
          if (sandboxGeometryRef.current === target) sandboxGeometryRef.current = current;
        });
    };

    const schedule = () => {
      if (timer) clearTimeout(timer);
      timer = setTimeout(request, 600);
    };

    schedule();
    window.addEventListener('resize', schedule);
    return () => {
      if (timer) clearTimeout(timer);
      window.removeEventListener('resize', schedule);
    };
  }, [id, viewOnly, shadow, isFullscreen, isConnected, netPanel]);

  const toggleAudio = useCallback(async () => {
    if (!id) return;
    try {
      const sessionToken = getSessionToken(id);
      if (!audioActive) {
        // Enable capture (owner) and unmute local playback. View-only and
        // shadow tabs only unmute — they cannot POST /audio.
        setAudioMuted(false);
        if (!viewOnly && !shadow) {
          await api.post(
            `/api/sessions/${id}/audio`,
            { enabled: true },
            sessionToken ? { headers: { 'X-Session-Token': sessionToken } } : undefined,
          );
        }
      } else {
        setAudioMuted(!audioMuted);
      }
    } catch (err) {
      console.error('Audio toggle failed', err);
    }
  }, [id, audioActive, audioMuted, setAudioMuted, viewOnly, shadow]);

  // Unified poll: polls the session endpoint until status=running, then
  // connects the selected transport. Keep this interval tight because it sits
  // directly on the first-frame path.
  // Poll until the session is running, then connect the Isola stream.
  useEffect(() => {
    if (!id) return;

    let cancelled = false;
    let timerId: ReturnType<typeof setTimeout>;

    const poll = async () => {
      if (cancelled || connectedRef.current || connectingRef.current) return;

      try {
        const sessionToken = sessionTokenRef.current;
        const res = await api.get(
          `/api/sessions/${id}`,
          sessionToken ? { headers: { 'X-Session-Token': sessionToken } } : undefined,
        );
        const data = res.data;
        setStatus(data.status);
        setLaunchStatus(data.status);
        handlePollData(data);

        if (data.session_token) {
          sessionTokenRef.current = data.session_token;
          storeSessionToken(id, data.session_token);
        } else if (data.share_url && data.status === 'running' && !viewOnly) {
          window.location.replace(data.share_url);
          return;
        } else if (data.status === 'running' && sessionTokenRef.current) {
          // Stale token in sessionStorage — clear so next poll can fetch a fresh one.
          clearSessionToken(id);
          sessionTokenRef.current = '';
          connectingRef.current = false;
        }

        if (data.target_url) {
          sessionConfigRef.current.targetUrl = data.target_url;
          setSeedTargetUrl(data.target_url);
        }
        if (typeof data.network_monitor === 'boolean') {
          setNetworkCaptureEnabled(data.network_monitor);
        }
        sessionConfigRef.current.transport = 'isola';
        // Capture the launch config so a restart re-creates the *same*
        // session. Reading these from localStorage instead changed the
        // resolution/browser on restart (localStorage can be empty on a
        // share-link viewer, or hold a different preference than the
        // session was launched with).
        if (data.resolution) {
          sessionConfigRef.current.resolution = data.resolution;
        }
        if (data.browser_type) {
          sessionConfigRef.current.browserType = data.browser_type;
        }
        if (typeof data.tor_enabled === 'boolean') {
          sessionConfigRef.current.torEnabled = data.tor_enabled;
        }

        if (data.status === 'running') {
          const protocol = window.location.protocol === 'https:' ? 'wss' : 'ws';
          const baseUrl = `${protocol}://${window.location.host}/ws/${id}`;
          if (!sessionTokenRef.current && !shadow) {
            // Wait for ensureControlToken on next poll.
            if (!cancelled) timerId = setTimeout(poll, 200);
            return;
          }
          const tokenQuery = shadow
            ? `admin_token=${encodeURIComponent(shadowToken)}&`
            : `session_token=${encodeURIComponent(sessionTokenRef.current)}&`;

          const streamWsUrl = `${baseUrl}?${tokenQuery}host=1&channel=isola`;
          const inputWsUrl = `${baseUrl}?${tokenQuery}host=1&channel=input`;
          const el = canvasRef.current;
          if (el && !connectedRef.current && !connectingRef.current) {
            connectingRef.current = true;
            connectStartedAtRef.current = Date.now();
            isolaRef.current.connect(streamWsUrl, inputWsUrl, el, viewOnly, data.bandwidth_kbps, id);
          }
          if (connectedRef.current) {
            return; // stop polling — stream is up
          }
          if (!cancelled) {
            timerId = setTimeout(poll, connectingRef.current ? 500 : 200);
          }
          return;
        }

        if (data.status === 'error' || data.status === 'terminated') {
          endLaunch();
          setStatus(data.status);
          // Don't flash the error page if we're in the middle of a restart —
          // the old session is being torn down on purpose; navigation to the
          // new session will replace this view momentarily.
          if (!restartingRef.current) {
            setSessionError(data.status === 'terminated' ? 'Session has ended' : 'Session encountered an error');
          }
          return; // stop polling — terminal
        }
      } catch (err: any) {
        if (err.response?.status === 404) {
          endLaunch();
          setStatus('error');
          if (!restartingRef.current) {
            setSessionError('Session not found');
          }
          return;
        }
        console.warn('[viewer] poll error', err);
      }

      // Tight poll while provisioning/running transition is pending. The backend
      // has already done the expensive work, so long intervals just add visible delay.
      if (!cancelled && !connectedRef.current) {
        timerId = setTimeout(poll, 200);
      }
    };

    // Start first poll immediately
    poll();

    return () => {
      cancelled = true;
      clearTimeout(timerId);
      if (connectedRef.current) {
        isolaRef.current.disconnect();
        connectedRef.current = false;
        connectingRef.current = false;
      }
    };
  }, [id, navigate, viewOnly, handlePollData, connectionGeneration, shadow, shadowToken]);

  useEffect(() => {
    if (recoveryTimerRef.current) {
      clearTimeout(recoveryTimerRef.current);
      recoveryTimerRef.current = null;
    }

    if (status !== 'running') {
      connectedRef.current = false;
      connectStartedAtRef.current = 0;
      return;
    }

    // Session time ran out — don't try to reconnect, the redirect effect
    // will navigate away and the container will be killed on the backend.
    // Use wall-clock `Date.now()` rather than the state `remainingSec` to
    // avoid races during React state batching.
    if (
      expiresAt !== null &&
      !isUnlimited &&
      Math.floor((expiresAt - Date.now()) / 1000) <= 0 &&
      sessionStatus !== 'terminated'
    ) {
      connectedRef.current = false;
      connectStartedAtRef.current = 0;
      return;
    }

    if (isConnected) {
      connectedRef.current = true;
      connectStartedAtRef.current = 0;
      return;
    }

    if (!connectedRef.current || isReconnecting || connectingRef.current) {
      return;
    }

    const unrecoverableError =
      streamError === 'Authentication failed' ||
      streamError === 'Session is no longer available';

    if (unrecoverableError) {
      return;
    }

    const retryDelay = streamError
      ? RECOVERABLE_ERROR_RETRY_MS
      : Math.max(CONNECTING_STALL_TIMEOUT_MS - (Date.now() - connectStartedAtRef.current), 0);

    recoveryTimerRef.current = setTimeout(() => {
      resetTransport();
    }, retryDelay);

    return () => {
      if (recoveryTimerRef.current) {
        clearTimeout(recoveryTimerRef.current);
        recoveryTimerRef.current = null;
      }
    };
  }, [status, isConnected, isReconnecting, streamError, resetTransport, isUnlimited, remainingSec, sessionStatus, expiresAt]);

  // timer: false — the sandbox ends the moment the owner leaves the page
  // (tab/window close, reload, hard navigation). fetch with keepalive lets
  // the DELETE survive the unload where XHR/regular fetch would be cancelled.
  // Share viewers have no session token and never send this; the backend
  // rejects non-owner calls anyway. SPA navigation (End button, redirects)
  // does not fire pagehide, and the End button clears the token first.
  useEffect(() => {
    if (!id || viewOnly || isTimerEnabled()) return;
    const sendClose = () => {
      const sessionToken = getSessionToken(id);
      if (!sessionToken) return;
      try {
        void fetch(`/api/sessions/${id}`, {
          method: 'DELETE',
          keepalive: true,
          headers: { 'X-Session-Token': sessionToken },
        });
      } catch {
        // ignore — nothing can be done while the page is unloading
      }
    };
    window.addEventListener('pagehide', sendClose);
    return () => window.removeEventListener('pagehide', sendClose);
  }, [id, viewOnly]);

  // Flash "Synced" when remote clipboard updates
  useEffect(() => {
    if (effectiveRemoteClipboard) {
      setClipboardSynced(true);
      const t = setTimeout(() => setClipboardSynced(false), 1500);
      return () => clearTimeout(t);
    }
  }, [effectiveRemoteClipboard]);

  // Show error page when session is terminated or in error state.
  // Skip during a restart — the old session's status poll will return
  // 'terminated' as the backend tears it down, and we don't want to flash
  // the "Session ended" UI before navigation lands on the new session.
  useEffect(() => {
    if (restarting) return;
    if (sessionStatus === 'terminated') {
      setSessionError('Session has ended');
    } else if (sessionStatus === 'error') {
      setSessionError('Session encountered an error');
    }
  }, [sessionStatus, restarting]);

  // Session expired — redirect to home instead of attempting reconnects.
  // The container will eventually be killed on the backend anyway, so the
  // stream sockets will die; proactively leaving here avoids a pointless
  // reconnection loop that would just bounce against an unavailable target.
  useEffect(() => {
    if (isUnlimited) return;
    if (restarting) return;
    if (expiresAt === null) return;
    if (sessionStatus === 'terminated') return;
    // Wall-clock check — never trust state timing alone. React batches
    // handlePollData → setExpiresAt → setRemainingSec across separate
    // effects; depending on scheduling order we can briefly observe
    // stale `remainingSec=0` with a fresh `expiresAt`. Computing inline
    // here is authoritative.
    const sec = Math.max(0, Math.floor((expiresAt - Date.now()) / 1000));
    if (sec > 0) return;
    // Final sanity gate — only redirect once the session has actually
    // progressed past provisioning/starting (these states legitimately
    // happen on every fresh session before the stream goes live, even
    // if the backend somehow delivered an expired session row to us).
    if (status === 'provisioning' || status === 'starting') return;

    // Tear down active transports so close handlers don't re-trigger the
    // recovery timer during navigation.
    if (recoveryTimerRef.current) {
      clearTimeout(recoveryTimerRef.current);
      recoveryTimerRef.current = null;
    }
    isolaRef.current.disconnect();
    connectedRef.current = false;
    connectingRef.current = false;
    connectStartedAtRef.current = 0;

    navigate('/', { replace: true });
  }, [remainingSec, isUnlimited, restarting, expiresAt, sessionStatus, status, navigate]);

  // Reset the restart-in-progress flag when the route param changes.
  // React Router reuses the same component instance across navigation,
  // so `restarting` would otherwise stay `true` and keep the toolbar's
  // restart icon spinning even after we land on the new session.
  useEffect(() => {
    setRestarting(false);
  }, [id]);

  const handleEndSession = () => {
    if (recoveryTimerRef.current) {
      clearTimeout(recoveryTimerRef.current);
      recoveryTimerRef.current = null;
    }
    isolaRef.current.disconnect();
    connectStartedAtRef.current = 0;
    connectedRef.current = false;
    if (id) {
      const sessionToken = getSessionToken(id);
      api.delete(
        `/api/sessions/${id}`,
        sessionToken ? { headers: { 'X-Session-Token': sessionToken } } : undefined,
      ).catch(() => {});
      clearSessionToken(id);
    }
    navigate('/');
  };

  // End the current session and open a fresh one with the same URL,
  // transport, resolution, browser, and Tor setting. Values are captured from
  // the running session (poll); localStorage is only a fallback for the
  // legacy case where the server did not report them.
  const handleRestart = async () => {
    if (restarting) return;
    if (!sessionConfigRef.current.targetUrl) return;

    setRestarting(true);
    try {
      const resolution =
        sessionConfigRef.current.resolution ||
        localStorage.getItem('isola_resolution') ||
        '1920x1080';
      const browserType =
        sessionConfigRef.current.browserType ||
        localStorage.getItem('isola_browser') ||
        'chromium';
      const res = await api.post('/api/sessions', {
        url: sessionConfigRef.current.targetUrl,
        resolution,
        browser_type: browserType,
        browser_theme: theme,
        tor_enabled: sessionConfigRef.current.torEnabled,
        transport: 'isola',
      });

      const newId: string | undefined = res.data?.id;
      if (!newId) {
        throw new Error('No session id returned');
      }
      if (res.data.session_token) {
        storeSessionToken(newId, res.data.session_token);
      }

      // Capture old id/token BEFORE navigation so we can clean it up.
      const oldId = id;
      const oldToken = oldId ? getSessionToken(oldId) : null;

      // Navigate FIRST. React Router will change the URL, useParams returns
      // the new id on the next render, and the polling/streaming effects
      // will clean themselves up via their cleanup functions. Doing the
      // navigate before the delete avoids the old session's 'terminated'
      // poll racing with the new session's poll and flashing the error page.
      navigate(`/session/${newId}`, { replace: true });

      // Tear down the current stream — cleanup also runs in the polling
      // effect, but doing it explicitly here releases sockets immediately
      // instead of waiting for React's next render.
      if (recoveryTimerRef.current) {
        clearTimeout(recoveryTimerRef.current);
        recoveryTimerRef.current = null;
      }
      isolaRef.current.disconnect();
      connectedRef.current = false;

      // Fire-and-forget cleanup of the old session.
      if (oldId) {
        api.delete(
          `/api/sessions/${oldId}`,
          oldToken ? { headers: { 'X-Session-Token': oldToken } } : undefined,
        ).catch(() => {});
        clearSessionToken(oldId);
      }
    } catch (err) {
      console.error('[restart] failed', err);
      setRestarting(false);
    }
  };

  const handleShare = async () => {
    try {
      const sessionToken = id ? getSessionToken(id) : '';
      const res = await api.post(
        `/api/sessions/${id}/share`,
        undefined,
        sessionToken ? { headers: { 'X-Session-Token': sessionToken } } : undefined,
      );
      const data = res.data;
      if (data.share_url) {
        navigator.clipboard.writeText(data.share_url).then(() => {
          setShareCopied(true);
          setTimeout(() => setShareCopied(false), 2000);
        });
      }
    } catch { /* ignore */ }
  };

  const statusText =
    status === 'provisioning' ? 'Starting remote browser...' :
    isReconnecting ? 'Reconnecting...' :
    status === 'running' && !isConnected && !streamError ? 'Connecting...' :
    '';
  const errorText = sessionError || streamError || (status === 'error' ? 'Failed to start session' : '');

  // Hold the loading overlay up for a short window after the first frame
  // arrives so the video element has time to paint real pixels (instead of
  // the often-white initial H.264 keyframe) before the canvas is exposed.
  // Without this brief hold, the transition from loader to canvas flashes
  // for ~1s on dark themes.
  const [loaderHeld, setLoaderHeld] = useState(false);
  useEffect(() => {
    if (isFirstFrame && isConnected && !streamError) {
      const t = setTimeout(() => setLoaderHeld(true), 350);
      return () => clearTimeout(t);
    }
    setLoaderHeld(false);
    return undefined;
  }, [isFirstFrame, isConnected, streamError]);

  // Hand the launch overlay over to the live canvas at the same moment the
  // page-local loading overlay would have disappeared (after the paint hold).
  useEffect(() => {
    if (launch.active && loaderHeld) endLaunch();
  }, [launch.active, loaderHeld]);

  // Terminal stream errors must uncover the error screen instead of leaving
  // the launch overlay up forever.
  useEffect(() => {
    if (streamError) endLaunch();
  }, [streamError]);

  // Keep the loading overlay visible until the first frame is actually on
  // screen — not just until ICE connects.
  const showOverlay =
    (!isConnected || !isFirstFrame || !loaderHeld) &&
    !streamError &&
    status !== 'error' &&
    !isReconnecting;
  const blankCover = isConnected && !isFirstFrame && !streamError;
  const showError = !!errorText && !isReconnecting;
  // Native CSS cursor is shaped by the streamer's cursor broadcast.
  // A wedged page renderer reads as an ordinary low fps, so it needs its own
  // signal. Two sources: the runner's verdict after it replaced a renderer
  // (authoritative, arrives ~8s into a stall), and this viewer's own CDP probe
  // failures (the streamer's cursor-shape probes stop being answered — the
  // earlier warning, ~3s).
  const rendererStalledSec = Math.round((isola.metrics.rendererStalledMs || 0) / 1000);
  const rendererStallVisible = isola.metrics.rendererStalled
    || (isola.metrics.cdpProbeFailStreak >= 3 && isola.metrics.cdpProbeFailAgoMs >= 0 && isola.metrics.cdpProbeFailAgoMs < 15000);
  // The second freeze shape: renderer healthy, compositor handing back empty
  // paint images. Shown while empties are still arriving, or while the sandbox
  // has fallen back to screenshot-driven frames. A total that stops growing is
  // a recovered window — including one the runner forgot to age out — so the
  // badge clears instead of staying up for the rest of the session.
  const captureEmptyTotal = isola.metrics.captureEmptyFrames;
  const [captureEmptiesFresh, setCaptureEmptiesFresh] = useState(false);
  const captureEmptyTotalRef = useRef(0);
  useEffect(() => {
    if (captureEmptyTotal === captureEmptyTotalRef.current) return;
    captureEmptyTotalRef.current = captureEmptyTotal;
    if (captureEmptyTotal <= 0) {
      setCaptureEmptiesFresh(false);
      return;
    }
    setCaptureEmptiesFresh(true);
    const t = setTimeout(() => setCaptureEmptiesFresh(false), 10_000);
    return () => clearTimeout(t);
  }, [captureEmptyTotal]);
  const captureStallVisible = isola.metrics.captureFallback
    || (isola.metrics.captureHealthOk && isola.metrics.captureEmptyWindow >= 5 && captureEmptiesFresh);
  const containerStyle = { width: '100%', height: '100%' };

  // Total load time — from page mount to first decoded frame. Captured
  // exactly once per session (the first time isFirstFrame becomes true)
  // so subsequent reconnect/recovery frames don't overwrite it.
  const mountTimeRef = useRef(performance.now());
  const [loadMs, setLoadMs] = useState<number | null>(null);
  useEffect(() => {
    if (isFirstFrame && loadMs === null && !streamError) {
      setLoadMs(Math.round(performance.now() - mountTimeRef.current));
    }
  }, [isFirstFrame, loadMs, streamError]);
  const loadSec = loadMs !== null ? (loadMs / 1000).toFixed(2) : null;

  const gapMs = isolaMetrics.playoutLagMs;
  const fps = isolaMetrics.recentFps > 0 ? isolaMetrics.recentFps : 0;
  const fpsDisplay = fps > 0 && fps <= 120 ? fps : (fps > 120 ? 0 : fps);
  const latencyText = gapMs > 0 ? `${Math.round(gapMs)}ms` : fpsDisplay > 0 ? '…' : '—';
  const fpsText = fpsDisplay > 0 ? fpsDisplay.toFixed(1) : (isConnected ? '…' : '—');
  const latencyColor =
    gapMs <= 0 ? 'text-zinc-400 dark:text-gray-600' :
    gapMs <= 50 ? 'text-emerald-600 dark:text-emerald-400' :
    gapMs <= 150 ? 'text-yellow-600 dark:text-yellow-400' :
    'text-red-600 dark:text-red-400';
  const fpsColor =
    fpsDisplay <= 0 ? 'text-zinc-400 dark:text-gray-600' :
    fpsDisplay >= 25 ? 'text-emerald-600 dark:text-emerald-400' :
    fpsDisplay >= 15 ? 'text-yellow-600 dark:text-yellow-400' :
    'text-red-600 dark:text-red-400';

  // Collected in one place (./viewer/debugInfo) so the copied text and the
  // window.__isolaViewerDebug hook always report the same thing. Build identity
  // is read once here (it is fixed for the page life) to keep the memo deps
  // simple and exhaustive.
  const appVersion = getVersion();
  const appCommit = getCommit();
  const debugInfo = React.useMemo(() => buildDebugInfo({
    transport,
    sessionId: id,
    targetUrl: sessionConfigRef.current.targetUrl || seedTargetUrl,
    resolution: sessionConfigRef.current.resolution,
    status,
    viewOnly,
    shadow,
    viewerCount,
    remainingSec,
    isUnlimited,
    quality,
    loadMs,
    networkCaptureEnabled,
    browserTabCount: isola.browserTabs.length,
    isConnected,
    isFirstFrame,
    streamError,
    isReconnecting,
    clipboardSynced,
    showOverlay,
    blankCover,
    showError,
    errorText,
    sessionError,
    audioActive,
    audioMuted,
    restarting,
    reconnects: connectionGeneration,
    version: appVersion,
    commit: appCommit,
    latencyText,
    fpsText,
    metrics: isolaMetrics,
  }), [
    transport, id, seedTargetUrl, status, viewOnly, shadow, viewerCount,
    remainingSec, isUnlimited, quality, loadMs, networkCaptureEnabled,
    isola.browserTabs.length, isConnected, isFirstFrame, streamError,
    isReconnecting, clipboardSynced, showOverlay, blankCover, showError,
    errorText, sessionError, audioActive, audioMuted, restarting,
    connectionGeneration, appVersion, appCommit,
    latencyText, fpsText, isolaMetrics,
  ]);

  useEffect(() => {
    const target = window as Window & { __isolaViewerDebug?: unknown };
    target.__isolaViewerDebug = debugInfo;
  }, [debugInfo]);

  const copyDebugOverlay = useCallback(async () => {
    const ok = await copyTextToClipboard(formatDebugInfo(debugInfo));
    if (ok) {
      setDebugCopied(true);
      window.setTimeout(() => setDebugCopied(false), 2000);
    } else {
      setDebugCopyFailed(true);
      window.setTimeout(() => setDebugCopyFailed(false), 2000);
    }
  }, [debugInfo]);

  useIsolaMonitorEvents(netEntriesRef, setNetEntries, consoleRef, setConsoleEntries, cookieRef, setCookieEntries);

    // Wrap toolbar + canvas in plugin providers
  // Toolbar is hidden when ?hide_toolbar=1 is present
  const toolbarAndCanvas = (
    <>
      <ViewerToolbar
        hideToolbar={hideToolbar}
        id={id}
        showOverlay={showOverlay}
        statusText={statusText}
        isReconnecting={isReconnecting}
        isConnected={isConnected}
        isola={isola}
        viewerCount={viewerCount}
        remainingSec={remainingSec}
        isUnlimited={isUnlimited}
        status={status}
        extendAvailable={extendAvailable}
        viewOnly={viewOnly}
        onExtend={extend}
        isFullscreen={isFullscreen}
        tabBarSlotRef={tabBarSlotRef}
        rendererStallVisible={rendererStallVisible}
        rendererStalledSec={rendererStalledSec}
        captureStallVisible={captureStallVisible}
        clipboardSynced={clipboardSynced}
        menuOpen={menuOpen}
        setMenuOpen={setMenuOpen}
        audioMuted={audioMuted}
        toggleAudio={toggleAudio}
        toggleFullscreen={toggleFullscreen}
        debugEnabled={debugEnabled}
        toggleDebug={toggleDebug}
        networkCaptureEnabled={networkCaptureEnabled}
        netPanel={netPanel}
        setNetPanel={setNetPanel}
        openNetworkPopOut={openNetworkPopOut}
        shareCopied={shareCopied}
        handleShare={handleShare}
        toolbarButtons={toolbarButtons}
        restarting={restarting}
        sessionTargetUrl={sessionConfigRef.current.targetUrl}
        handleRestart={handleRestart}
        handleEndSession={handleEndSession}
        onRecoverRenderer={() => isola.recoverRenderer()}
      />
      <ViewerStreamShell
        shellRef={shellRef}
        canvasRef={canvasRef}
        containerStyle={containerStyle}
        networkCaptureEnabled={networkCaptureEnabled}
        netPanel={netPanel}
        netEntries={netEntries}
        consoleEntries={consoleEntries}
        cookieEntries={cookieEntries}
        netTab={netTab}
        setNetTab={setNetTab}
        setNetPanel={setNetPanel}
        openNetworkPopOut={openNetworkPopOut}
        browserTabs={isola.browserTabs}
        theme={theme}
      />
      <ViewerDebugOverlay
        enabled={debugEnabled}
        transport={transport}
        id={id}
        debugCopied={debugCopied}
        debugCopyFailed={debugCopyFailed}
        copyDebugOverlay={copyDebugOverlay}
        toggleDebug={toggleDebug}
        latencyColor={latencyColor}
        latencyText={latencyText}
        fpsColor={fpsColor}
        fpsText={fpsText}
        isolaMetrics={isolaMetrics}
        loadMs={loadMs}
        loadSec={loadSec}
        quality={quality}
        rendererStallVisible={rendererStallVisible}
        rendererStalledSec={rendererStalledSec}
      />

      {/* Status overlay — restart wins over the regular overlay so any
          error flash from the previous session is hidden behind the
          "Restarting session…" UI. */}
      {restarting && (
        <LoadingOverlay status="restarting" isReconnecting={false} isRestarting={true} />
      )}

      {!launch.active && !restarting && showOverlay && (
        <LoadingOverlay status={status} isReconnecting={false} />
      )}

      {!launch.active && !restarting && isReconnecting && (
        <LoadingOverlay status="reconnecting" isReconnecting={true} />
      )}

      {blankCover && (
        <div className="absolute inset-0 bg-zinc-200 dark:bg-gray-950" />
      )}

      {showError && (
        <div className="absolute inset-0 z-50 flex flex-col items-center justify-center bg-zinc-100 dark:bg-[#030712]">
          <div className="mb-8 opacity-60">
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" className="w-20 h-20" style={{ overflow: 'visible' }}>
              <g transform="translate(256, 270)">
                <path d="M 0 50 L 110 0 L 0 -50 L -110 0 Z" className="fill-zinc-300 dark:fill-[#1a2024]" />
                <path d="M -110 0 L 0 50 L 0 85 L -110 35 Z" className="fill-zinc-200 dark:fill-[#0f1418]" />
                <path d="M 110 0 L 0 50 L 0 85 L 110 35 Z" className="fill-zinc-100 dark:fill-[#0a0e10]" />
                <path d="M 0 -20 L 100 -65 L 0 -110 L -100 -65 Z" fill="none" className="stroke-cyan-600 dark:stroke-[#00e5ff]" strokeWidth="10" strokeLinejoin="miter" opacity="0.6" />
                <path d="M -100 -65 L 0 -20 L 0 5 L -100 -40 Z" fill="none" className="stroke-cyan-600 dark:stroke-[#00e5ff]" strokeWidth="10" strokeLinejoin="miter" opacity="0.6" />
                <path d="M 100 -65 L 0 -20 L 0 5 L 100 -40 Z" fill="none" className="stroke-cyan-600 dark:stroke-[#00e5ff]" strokeWidth="10" strokeLinejoin="miter" opacity="0.6" />
                <path d="M 0 -85 L 85 -125 L 0 -165 L -85 -125 Z" className="fill-zinc-700 dark:fill-white" opacity="0.7" />
                <path d="M -85 -125 L 0 -85 L 0 -65 L -85 -105 Z" className="fill-zinc-600 dark:fill-[#e0e0e0]" opacity="0.6" />
                <path d="M 85 -125 L 0 -85 L 0 -65 L 85 -105 Z" className="fill-zinc-500 dark:fill-[#cccccc]" opacity="0.6" />
              </g>
            </svg>
          </div>
          <p className="text-xs font-mono text-zinc-500 dark:text-[#4b5563] mb-4 tracking-wider uppercase">
            Session {id?.slice(0, 8)}
          </p>
          <p className="text-sm font-mono text-red-600 dark:text-[#f87171] mb-2">{errorText}</p>
          <p className="text-xs font-mono text-zinc-500 dark:text-[#6b7280] mb-10 max-w-sm text-center">
            {sessionError ? 'The browser session you are looking for does not exist or has expired.' : 'The browser encountered an unexpected problem.'}
          </p>
          <div className="flex items-center">
            <Link
              to="/"
              className="text-xs font-mono text-cyan-700 dark:text-[#00e5ff] hover:text-cyan-500 dark:hover:text-cyan-300 hover:underline underline-offset-4 decoration-cyan-700/40 dark:decoration-[#00e5ff]/40 transition-colors duration-150"
            >
              New session →
            </Link>
          </div>
        </div>
      )}
    </>
  );

  // Nest plugin providers
  const wrapped = providers.reduceRight(
    (children, Provider) => (
      <Provider sessionId={id!} viewOnly={viewOnly}>{children}</Provider>
    ),
    toolbarAndCanvas
  );

  // h-full, not h-screen: in fullscreen the app must fill the top-layer
  // element's height, and 100vh can keep the windowed height (leaving black
  // bands above and below the stream). The 100% chain is set in index.css.
  return <div className="flex flex-col h-full bg-zinc-200 dark:bg-gray-950 relative">{wrapped}</div>;
}
