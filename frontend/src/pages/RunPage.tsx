import { useEffect, useRef, useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { useTheme } from '../lib/ThemeContext';
import { IsolaLogo } from '../components/IsolaLogo';
import { beginLaunch, endLaunch, setLaunchStatus } from '../lib/launchProgress';
import { apiErrorMessage } from '../lib/apiError';
import { storeSessionToken } from '../lib/sessionToken';
import { getIsolaTabs, getMaxSessions } from '../lib/config';
import { MAX_CAPACITY_RETRIES, capacityRetryDelayMs, isCapacityLimit } from '../lib/launchRetry';
import { sessionCreateAuthHeaders } from '../lib/createAuth';
import { fitResolutionToAspect } from '../lib/resolution';

type LaunchConfig = {
  url?: string;
  resolution?: string;
  browser_type?: string;
  browser_theme?: string;
  tor_enabled?: boolean;
  transport?: string;
  network_monitor?: boolean;
};

// The viewer's capture box is the window minus the app's own chrome: a 48px
// toolbar, plus the 36px isola tab strip when it is enabled. The isola
// transport draws the capture into #vnc-container with object-fit: contain, so
// any leftover space between that box and the sandbox screen shows up as dark
// letterbox bars on the sides. Sizing the sandbox to the box's aspect ratio
// keeps it filled edge-to-edge without cropping.
const VIEWER_TOOLBAR_PX = 48;
const ISOLA_TABBAR_PX = 36;

function viewerAspect(): number {
  const width = document.documentElement.clientWidth;
  const chrome = VIEWER_TOOLBAR_PX + (getIsolaTabs() ? ISOLA_TABBAR_PX : 0);
  const height = window.innerHeight - chrome;
  return width > 0 && height > 0 ? width / height : 0;
}

export default function RunPage() {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const { theme } = useTheme();
  const [error, setError] = useState<string>('');
  // True once this page has handed the session over to the viewer. The overlay
  // lives above the router (.lib/launchProgress), so it must NOT be ended by
  // this page's unmount at that hand-off.
  const handedOffRef = useRef(false);

  useEffect(() => {
    let cancelled = false;
    beginLaunch();

    const body: LaunchConfig = {};
    try {
      const raw = sessionStorage.getItem('isola_launch_config');
      if (raw) {
        const parsed = JSON.parse(raw) as LaunchConfig;
        Object.assign(body, parsed);
      }
    } catch {
      /* sessionStorage unavailable or corrupt — fall back to ?url= only */
    }
    // Minimal backward-compat fallback: deep link like /run?url=...
    let url = body.url || searchParams.get('url') || '';
    if (!url) {
      endLaunch();
      navigate('/', { replace: true });
      return;
    }
    if (!/^https?:\/\//i.test(url)) {
      url = 'https://' + url;
    }
    body.url = url;
    if (!body.browser_theme) {
      body.browser_theme = theme;
    }
    body.transport = 'isola';
    if (body.resolution) {
      body.resolution = fitResolutionToAspect(body.resolution, viewerAspect());
    }

    // Launch admission can refuse with 429 while the instance is at its session
    // cap (ISOLA_MAX_SESSIONS). That is transient — sessions expire on a timer —
    // so retry with backoff and show a waiting state instead of a hard error.
    let controller: AbortController | null = null;
    let retryTimer: ReturnType<typeof setTimeout> | null = null;
    let resolveRetry: (() => void) | null = null;

    const sleep = (ms: number) =>
      new Promise<void>((resolve) => {
        resolveRetry = resolve;
        retryTimer = setTimeout(() => {
          retryTimer = null;
          resolveRetry = null;
          resolve();
        }, ms);
      });

    const launch = async () => {
      for (let attempt = 0; ; attempt++) {
        setLaunchStatus('provisioning');
        controller = new AbortController();
        let res: Response;
        try {
          // Authenticated session creation: attach the bearer when signed in so
          // the session is owned by the caller. With no credential the session
          // is anonymous, as before.
          res = await fetch('/api/sessions', {
            method: 'POST',
            headers: {
              'Content-Type': 'application/json',
              ...sessionCreateAuthHeaders(),
            },
            body: JSON.stringify(body),
            signal: controller.signal,
          });
        } catch (err) {
          if (cancelled || (err as { name?: string })?.name === 'AbortError') return;
          throw err;
        }

        if (res.ok) {
          const data = await res.json();
          if (cancelled) return;
          if (!data || typeof data.id !== 'string') {
            throw new Error('Backend did not return a session id');
          }
          if (data.session_token) {
            storeSessionToken(data.id, data.session_token);
          }
          // Consume the launch config only now that a session actually exists.
          // Removing it where it is read (the earlier behaviour) broke the dev
          // flow: under React.StrictMode effects run twice, and the first run's
          // request is aborted by the cleanup below — so the second run found the
          // key already gone and the second run would post without launch config.
          try {
            sessionStorage.removeItem('isola_launch_config');
          } catch {
            /* sessionStorage unavailable — nothing to consume */
          }
          handedOffRef.current = true;
          // Backend status stays 'provisioning' until the container and
          // streamer are ready; ViewerPage polls it from here.
          setLaunchStatus('provisioning');
          const tokenSuffix = data.session_token ? `?token=${encodeURIComponent(data.session_token)}` : '';
          navigate(`/session/${data.id}${tokenSuffix}`, { replace: true });
          return;
        }

        let data: unknown;
        try {
          data = await res.json();
        } catch {
          data = null;
        }

        if (isCapacityLimit(res.status, data)) {
          if (attempt < MAX_CAPACITY_RETRIES) {
            const retryAfter = Number(res.headers.get('Retry-After'));
            setLaunchStatus('queued', 'AT CAPACITY — WAITING FOR A FREE SESSION');
            await sleep(capacityRetryDelayMs(attempt + 1, Number.isFinite(retryAfter) ? retryAfter : null));
            if (cancelled) return;
            continue;
          }
          const limit = getMaxSessions();
          throw new Error(
            limit > 0
              ? `All ${limit} browser slots are in use — try again in a moment.`
              : 'All browser slots are in use — try again in a moment.',
          );
        }

        throw new Error(apiErrorMessage(data, res.status));
      }
    };

    void launch().catch((err: unknown) => {
      if (cancelled) return;
      if ((err as { name?: string })?.name === 'AbortError') return;
      endLaunch();
      setError((err as Error)?.message || 'Failed to launch. Is the backend running?');
    });

    return () => {
      cancelled = true;
      controller?.abort();
      if (retryTimer) clearTimeout(retryTimer);
      // Release a pending retry wait so the loop can observe `cancelled` and exit.
      if (resolveRetry) {
        const resolve = resolveRetry;
        resolveRetry = null;
        resolve();
      }
    };
  }, [navigate, searchParams, theme]);

  // Unmount-only: left the launch page without a session (back button, etc.),
  // so don't leave the app-level overlay covering whatever page comes next.
  // Kept out of the launch effect's cleanup so re-running it (theme change)
  // cannot reset the overlay mid-launch.
  useEffect(() => () => {
    if (!handedOffRef.current) endLaunch();
  }, []);

  return (
    <div className="relative min-h-screen bg-zinc-200 dark:bg-gray-950">
      {error && (
        <div className="absolute inset-0 z-50 flex flex-col items-center justify-center bg-zinc-100 dark:bg-[#030712] px-6">
          <div className="mb-8 opacity-60">
            <IsolaLogo className="w-20 h-20" glow={false} />
          </div>
          <p className="text-sm font-mono text-red-600 dark:text-[#f87171] mb-2 text-center">{error}</p>
          <p className="text-xs font-mono text-zinc-500 dark:text-[#6b7280] mb-10 max-w-sm text-center">
            The session was not created.
          </p>
          <button
            type="button"
            onClick={() => navigate('/', { replace: true })}
            className="text-xs font-mono text-cyan-700 dark:text-[#00e5ff] hover:text-cyan-500 dark:hover:text-cyan-300 hover:underline underline-offset-4 decoration-cyan-700/40 dark:decoration-[#00e5ff]/40 transition-colors duration-150 focus-visible:underline focus-visible:outline-none"
          >
            Back to main →
          </button>
        </div>
      )}
    </div>
  );
}
