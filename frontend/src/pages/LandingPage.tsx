import { useCallback, useState, useEffect, useRef } from 'react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { MonitorDown, X } from 'lucide-react';
import { useConfig } from '../hooks/useConfig';
import { useInstallPrompt } from '../hooks/useInstallPrompt';
import { useTheme } from '../lib/ThemeContext';
import {
  getCommit,
  getVersion,
  isCaptureEnabled,
  onBackendStatusChange,
  setBackendReachable,
} from '../lib/config';
import { IsolaLogo, ISOLA_LOGO_KEYFRAMES } from '../components/IsolaLogo';
import {
  LOCALE_OPTIONS,
  ZONE_OPTIONS,
  identitySummary,
  resolveIdentity,
  type IdentityOption,
} from '../lib/identityProfiles';

function normalizeUrl(input: string): string {
  const trimmed = input.trim();
  if (!trimmed) return trimmed;
  if (/^https?:\/\//i.test(trimmed)) return trimmed;

  if (/\s/.test(trimmed)) {
    return `https://duckduckgo.com/?q=${encodeURIComponent(trimmed)}`;
  }

  const hostname = trimmed.split('/')[0].split('?')[0];

  if (/^localhost(:\d+)?$/i.test(hostname)) {
    return `https://${trimmed}`;
  }

  if (/^\d{1,3}(\.\d{1,3}){3}(:\d+)?$/.test(hostname)) {
    return `https://${trimmed}`;
  }

  if (/^\[[0-9a-fA-F:]+\]?$/.test(hostname)) {
    return `https://${trimmed}`;
  }

  const lastDot = hostname.lastIndexOf('.');
  if (lastDot > 0 && lastDot < hostname.length - 2) {
    const tld = hostname.slice(lastDot + 1);
    if (/^[a-zA-Z]{2,}$/.test(tld)) {
      return `https://${trimmed}`;
    }
  }

  return `https://duckduckgo.com/?q=${encodeURIComponent(trimmed)}`;
}

const GITHUB_URL = 'https://github.com/dm0xa/isola-browser';
const DOCS_URL = `${GITHUB_URL}/blob/main/docs`;

// Release notes under the address box. Each entry links the documentation that
// ships with the feature, because the control is not on this page: Record is an
// opt-in viewer action, Custom data lives in the session menu, and locale/zone
// sit behind Advanced. `capture` entries are withheld while the deployment has
// capture disabled — the page must not advertise a control the backend turned
// off (isCaptureEnabled reads the same flag the viewer gates on).
const WHATS_NEW: { title: string; detail: string; href: string; capture?: boolean }[] = [
  {
    title: 'Record',
    detail: 'Saves every resource a page loads into a browsable, host-grouped archive.',
    href: `${DOCS_URL}/configuration.md#capture`,
    capture: true,
  },
  {
    title: 'Custom data',
    detail: 'Pushes cookies, headers and storage into an already-running page.',
    href: `${DOCS_URL}/api.md#send-custom-data-to-the-page`,
  },
  {
    title: 'Locale & zone',
    detail: 'A coherent per-session identity, random per launch by default.',
    href: `${DOCS_URL}/configuration.md#session`,
  },
];
// Dismissal is remembered per version: a returning operator reads the notes
// once, and the next release announces itself again.
const WHATS_NEW_KEY = 'isola_whats_new_version';

// withCurrent keeps a stored value selectable after an upgrade: the fields used
// to be free text, and silently dropping a value would change the session's
// identity on the first launch.
function withCurrent(options: readonly IdentityOption[], value: string): IdentityOption[] {
  const trimmed = value.trim();
  if (!trimmed || options.some((o) => o.value === trimmed)) return [...options];
  return [{ value: trimmed, label: `${trimmed} (custom)` }, ...options];
}

const identitySelectClass =
  'w-full min-w-0 rounded border border-zinc-400 bg-white px-1.5 py-0.5 text-xs text-zinc-800 focus:border-zinc-600 focus:outline-hidden dark:border-gray-700 dark:bg-gray-900 dark:text-[#aea2a2] dark:focus:border-gray-400';

const textStyle = {
  fontFamily: '"Bitcount Prop Single", monospace',
  fontVariationSettings: '"wght" 400, "slnt" 0, "CRSV" 0.5, "ELSH" 0, "ELXP" 0',
} as const;

// Me is the authenticated-identity payload from GET /api/me.
// Null = anonymous (not signed in): the page renders exactly
// as before — no tiles, address box visible.
interface Me {
  user_id: string;
  email: string;
  hide_address_bar?: boolean;
  apps: { id: number; name: string; url: string; icon?: string }[];
}

function useMe(): Me | null {
  const [me, setMe] = useState<Me | null>(null);
  useEffect(() => {
    const token = localStorage.getItem('access_token');
    if (!token) return;
    fetch('/api/me', { headers: { Authorization: `Bearer ${token}` } })
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(String(r.status)))))
      .then(setMe)
      .catch(() => setMe(null));
  }, []);
  return me;
}

export default function LandingPage() {
  const me = useMe();
  const [url, setUrl] = useState('');
  const [error, setError] = useState('');
  const [displayedText, setDisplayedText] = useState('');
  const [showCursor, setShowCursor] = useState(true);
  const [menuOpen, setMenuOpen] = useState(false);
  const [backendOk, setBackendOk] = useState(true);
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const { resolutionVisible, resolutions, browsers } = useConfig();
  const { available: installAvailable, install: promptInstall, dismiss: dismissInstall } = useInstallPrompt();
  const { theme, toggle: toggleTheme } = useTheme();
  const menuRef = useRef<HTMLDivElement>(null);
  const searchError = searchParams.get('error');
  useEffect(() => {
    // Do NOT keep stale launch-paste errors across page reloads / navigations.
    // Only the explicit error= query param (deep-linking session_not_found /
    // share_invalid) is preserved; anything else is ephemeral to one launch.
    if (!searchError) setError('');
  }, [searchError]);

  // Keep 1080p as the no-preference default (bandwidth sweet spot) even
  // though the picker is sorted highest-first.
  const defaultResolution =
    resolutions.find((r) => r.value === '1920x1080')?.value ??
    resolutions[0]?.value ??
    '1920x1080';
  // "My screen": launch the remote browser at the viewer's physical display
  // size (CSS px × DPR) so the stream maps ~1:1 to their monitor. The
  // backend accepts any WxH launch resolution — no whitelist to update.
  const myScreenValue = `${Math.round(window.screen.width * window.devicePixelRatio)}x${Math.round(window.screen.height * window.devicePixelRatio)}`;
  const defaultBrowser = browsers.length > 0 ? browsers[0].value : 'chromium';
  const [resolution, setResolution] = useState(() => localStorage.getItem('isola_resolution') || defaultResolution);
  const [browserType, setBrowserType] = useState(() => localStorage.getItem('isola_browser') || defaultBrowser);
  const [torEnabled, setTorEnabled] = useState(() => localStorage.getItem('isola_tor') === 'true');
  // Page-visible identity. Empty = let the deployment default (or the image's
  // UTC/en-US) apply; these are sent only when the user typed something.
  const [timezone, setTimezone] = useState(() => localStorage.getItem('isola_timezone') || '');
  const [locale, setLocale] = useState(() => localStorage.getItem('isola_locale') || '');
  // Advanced (timezone/locale) is collapsed by default: it is tuning, not a
  // launch decision like resolution or Tor.
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const [whatsNewSeen, setWhatsNewSeen] = useState(() => {
    try {
      return localStorage.getItem(WHATS_NEW_KEY) || '';
    } catch {
      return '';
    }
  });
  const whatsNewItems = WHATS_NEW.filter((item) => !item.capture || isCaptureEnabled());
  const whatsNewVisible = whatsNewItems.length > 0 && whatsNewSeen !== getVersion();
  const transport = 'isola';

  // Every launch path funnels through here — the address box, the app tiles and
  // the ?q= deep link — so the stored launch config and the /run navigation
  // cannot drift apart.
  const launchTarget = useCallback(
    (target: string, tor = torEnabled) => {
      const launchConfig = {
        url: target,
        resolution,
        browser_type: browserType,
        browser_theme: theme,
        tor_enabled: tor,
        transport,
        // Per-tab CDP capture (network/console/cookies) only exists on the
        // isola transport; the viewer's network panel is gated on this flag.
        network_monitor: true,
        // "Random" rolls one coherent locale/zone pair per launch. Sending the
        // resolved pair (never a blank) means the page always has a declared
        // identity; the backend applies it to a warm container over CDP.
        ...(() => {
          const identity = resolveIdentity(locale, timezone);
          return { timezone: identity.timezone, locale: identity.locale };
        })(),
      };
      try {
        sessionStorage.setItem('isola_launch_config', JSON.stringify(launchConfig));
      } catch {
        /* sessionStorage unavailable — fall through with URL query only */
      }
      navigate(`/run?url=${encodeURIComponent(target)}`, { replace: true });
    },
    [navigate, resolution, browserType, theme, torEnabled, transport, timezone, locale],
  );

  useEffect(() => {
    localStorage.setItem('isola_resolution', resolution);
  }, [resolution]);

  useEffect(() => {
    localStorage.setItem('isola_browser', browserType);
  }, [browserType]);

  useEffect(() => {
    if (browsers.length === 0) return;
    if (!browsers.some((b) => b.value === browserType)) {
      setBrowserType(browsers[0].value);
    }
  }, [browserType, browsers]);

  useEffect(() => {
    localStorage.setItem('isola_tor', torEnabled.toString());
  }, [torEnabled]);

  useEffect(() => {
    localStorage.setItem('isola_timezone', timezone);
  }, [timezone]);

  useEffect(() => {
    localStorage.setItem('isola_locale', locale);
  }, [locale]);

  useEffect(() => {
    if (resolutions.length === 0) return;
    if (resolution === myScreenValue) return;
    if (!resolutions.some((r) => r.value === resolution)) {
      setResolution(defaultResolution);
    }
  }, [resolution, resolutions, defaultResolution, myScreenValue]);

  // isola always runs the Electron image regardless of the browser pick.
  const browserShown = false;

  useEffect(() => {
    const text = 'isola';
    let i = 0;
    const interval = setInterval(() => {
      if (i <= text.length) {
        setDisplayedText(text.slice(0, i));
        i++;
      } else {
        clearInterval(interval);
      }
    }, 180);
    return () => clearInterval(interval);
  }, []);

  useEffect(() => {
    const interval = setInterval(() => setShowCursor((c) => !c), 530);
    return () => clearInterval(interval);
  }, []);

  useEffect(() => {
    if (!menuOpen) return;
    const handler = (e: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) {
        setMenuOpen(false);
      }
    };
    document.addEventListener('mousedown', handler);
    return () => document.removeEventListener('mousedown', handler);
  }, [menuOpen]);

  useEffect(() => {
    const off = onBackendStatusChange(setBackendOk);
    const probe = async () => {
      try {
        const r = await fetch('/api/config', { cache: 'no-store' });
        if (r.ok) setBackendReachable(true);
        else throw new Error(`http ${r.status}`);
      } catch {
        setBackendReachable(false);
      }
    };
    probe();
    const interval = setInterval(probe, 4000);
    return () => {
      off();
      clearInterval(interval);
    };
  }, []);

  useEffect(() => {
    const err = searchParams.get('error');
    if (err === 'session_not_found') {
      setError('Session not found');
    } else if (err === 'share_invalid') {
      setError('Share link expired or invalid');
    }
  }, [searchParams]);

  // `?q=` makes isola usable as a browser search engine:
  //   https://isola.example/?q=%s
  // The text lands in the address box and is normalised on submit exactly like
  // typed input, so a pasted URL opens as a site and anything else becomes a
  // search. `&autorun=1` skips the keystroke and launches immediately. Both
  // params are consumed from the address bar so a reload (or a copied URL)
  // cannot relaunch or re-prefill.
  const q = searchParams.get('q');
  const autorun = searchParams.get('autorun') === '1';
  const consumedQ = useRef(false);
  useEffect(() => {
    if (q === null || consumedQ.current) return;
    consumedQ.current = true;
    const next = new URLSearchParams(searchParams);
    next.delete('q');
    next.delete('autorun');
    setSearchParams(next, { replace: true });
    const target = q.trim();
    if (!target) return;
    if (autorun) launchTarget(normalizeUrl(target));
    else setUrl(target);
  }, [q, autorun, searchParams, setSearchParams, launchTarget]);

  const handleLaunch = (e: React.FormEvent) => {
    e.preventDefault();
    const target = normalizeUrl(url);
    if (!target) return;
    launchTarget(target);
  };

  // Persist the version that was read, not a boolean: the notes are per
  // release, so storing "seen" would hide every future release too.
  const dismissWhatsNew = () => {
    const seen = getVersion();
    setWhatsNewSeen(seen);
    try {
      localStorage.setItem(WHATS_NEW_KEY, seen);
    } catch {
      /* storage unavailable — the notes reappear on the next load */
    }
  };

  const menuItemClass = (active: boolean) =>
    `block w-full text-left px-3 py-1.5 text-xs transition-colors ${
      active
        ? 'text-zinc-800 bg-zinc-300 dark:text-white dark:bg-gray-700'
        : 'text-zinc-600 hover:text-zinc-800 hover:bg-zinc-300 dark:text-gray-500 dark:hover:text-white dark:hover:bg-gray-700/50'
    }`;

  return (
    <div className="flex flex-col items-center justify-center min-h-screen px-6 bg-zinc-200 dark:bg-gray-950 transition-colors">
      {/* Top-right: theme toggle */}
      <div className="absolute top-6 right-6 flex gap-4 text-sm">
        <button
          type="button"
          onClick={toggleTheme}
          className="text-zinc-500 hover:text-zinc-700 dark:text-gray-600 dark:hover:text-gray-300 transition-colors"
          title={theme === 'dark' ? 'Switch to light' : 'Switch to dark'}
        >
          {theme === 'dark' ? (
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" className="w-5 h-5">
              <circle cx="12" cy="12" r="5" />
              <path d="M12 1v2M12 21v2M4.22 4.22l1.42 1.42M18.36 18.36l1.42 1.42M1 12h2M21 12h2M4.22 19.78l1.42-1.42M18.36 5.64l1.42-1.42" />
            </svg>
          ) : (
            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" className="w-5 h-5">
              <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z" />
            </svg>
          )}
        </button>
      </div>
      <div className="text-center mb-2">
        <div className="flex items-center select-none" style={{ width: '320px', margin: '0 auto' }}>
          <div className="shrink-0 pointer-events-none" style={{ width: '96px' }}>
            <style>{ISOLA_LOGO_KEYFRAMES}</style>
            <div className="relative">
              <span
                aria-hidden
                className={`absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 w-16 h-16 rounded-full bg-purple-500/20 dark:bg-purple-500/30 blur-lg pointer-events-none transition-opacity duration-700 ease-out motion-reduce:transition-none ${
                  torEnabled ? 'opacity-100' : 'opacity-0'
                }`}
              />
              <IsolaLogo className="w-24 h-24 mt-3" float draggable={false} />
            </div>
          </div>
          <h1
            className="text-5xl font-extralight tracking-tight text-zinc-900 dark:text-white whitespace-nowrap pl-[25px] pr-[25px] pointer-events-none"
            style={textStyle}
            draggable={false}
          >
            {displayedText}
            <span className={`inline-block w-[3px] h-[0.8em] bg-gray-900 dark:bg-white ml-0.5 align-middle ${showCursor ? 'opacity-100' : 'opacity-0'} transition-opacity duration-75`} />
          </h1>
        </div>
        {/* <p className="text-xs tracking-[0.2em] text-zinc-500 dark:text-gray-500 mt-1" style={textStyle}>
        </p> */}
      </div>

      <form onSubmit={handleLaunch} className="w-full max-w-lg mt-12">
        {!me?.hide_address_bar && (
        <div className="flex items-end gap-3 border-b border-zinc-400 dark:border-gray-700 focus-within:border-zinc-600 dark:focus-within:border-gray-400 transition-colors pb-2">
          <input
            type="text"
            placeholder="Enter URL..."
            value={url}
            onChange={(e) => { setUrl(e.target.value); setError(''); if (searchParams.has('error')) setSearchParams({}); }}
            className="flex-1 bg-transparent text-zinc-800 dark:text-[#aea2a2] text-sm placeholder-zinc-500 dark:placeholder-white/50 focus:outline-hidden py-1"
            autoFocus={!(me && me.apps.length > 0)}
          />
          <div className="relative" ref={menuRef}>
            <button
              type="button"
              onClick={() => setMenuOpen(!menuOpen)}
              className="text-gray-400 hover:text-gray-700 dark:text-gray-500 dark:hover:text-white transition-colors pb-0.5"
              title="Settings"
            >
              <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" className="w-4 h-4">
                <circle cx="12" cy="12" r="3" />
                <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
              </svg>
            </button>
            {menuOpen && (
              <div className="absolute bottom-full left-0 mb-1 bg-zinc-200 dark:bg-gray-800 border border-zinc-400 dark:border-gray-700 rounded-lg py-1 shadow-xl z-50 min-w-[240px]">
                {resolutionVisible && (
                  <>
                    <div className="px-3 py-1 text-[10px] text-gray-500 dark:text-gray-600 uppercase tracking-wider">Resolution</div>
                    {resolutions.map((r) => (
                      <button
                        key={r.value}
                        type="button"
                        onClick={() => { setResolution(r.value); }}
                        className={menuItemClass(resolution === r.value)}
                      >
                        {r.label}
                      </button>
                    ))}
                    <button
                      type="button"
                      onClick={() => { setResolution(myScreenValue); }}
                      className={menuItemClass(resolution === myScreenValue) + ' flex items-center justify-between gap-3'}
                      title="Match your display size"
                    >
                      <span>Current</span>
                      <span className="tabular-nums opacity-60">{myScreenValue}</span>
                    </button>
                  </>
                )}
                {resolutionVisible && browserShown && (
                  <div className="mx-2 my-1 border-t border-zinc-400 dark:border-gray-700" />
                )}
                {browserShown && (
                  <>
                    <div className="px-3 py-1 text-[10px] text-zinc-600 dark:text-gray-600 uppercase tracking-wider">Browser</div>
                    {browsers.map((b) => (
                      <button
                        key={b.value}
                        type="button"
                        onClick={() => { setBrowserType(b.value); }}
                        className={menuItemClass(browserType === b.value)}
                      >
                        <span className="flex items-center gap-2">
                          {b.value === 'chromium' && (
                            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" className="w-3.5 h-3.5 stroke-current fill-none" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
                              <circle cx="12" cy="12" r="10" />
                              <path d="M12 2a10 10 0 0 0-5 18.7L17 8h-5l5 12.7A10 10 0 0 0 12 2z" />
                              <path d="M17 20.7A10 10 0 0 1 7 8h5l5 12.7z" />
                              <circle cx="12" cy="12" r="4" />
                              <circle cx="12" cy="12" r="2.5" />
                            </svg>
                          )}
                          {b.value === 'tor' && (
                            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" className="w-3.5 h-3.5 stroke-current fill-none" strokeWidth="1.5">
                              <circle cx="12" cy="12" r="10" />
                              <circle cx="12" cy="12" r="6" />
                              <circle cx="12" cy="12" r="3" />
                            </svg>
                          )}
                          {b.label}
                        </span>
                      </button>
                    ))}
                  </>
                )}
                <div className="mx-2 my-1 border-t border-zinc-400 dark:border-gray-700" />
                <div className="px-3 py-1 text-[10px] text-zinc-600 dark:text-gray-600 uppercase tracking-wider">Tor</div>
                <button
                  type="button"
                  onClick={() => setTorEnabled(!torEnabled)}
                  className={menuItemClass(torEnabled)}
                >
                  <span className="flex items-center gap-2">
                    <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" className="w-4 h-4">
                      {torEnabled ? (
                        <>
                          <circle cx="12" cy="12" r="9" fill="currentColor" opacity="0.15" />
                          <circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" strokeWidth="1.5" />
                          <circle cx="12" cy="12" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.2" />
                          <circle cx="12" cy="12" r="2.5" fill="currentColor" opacity="0.6" />
                        </>
                      ) : (
                        <>
                          <circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" strokeWidth="1.2" />
                          <circle cx="12" cy="12" r="5.5" fill="none" stroke="currentColor" strokeWidth="1" />
                          <circle cx="12" cy="12" r="2.5" fill="none" stroke="currentColor" strokeWidth="0.8" />
                        </>
                      )}
                    </svg>
                    <span className={torEnabled ? 'text-purple-500' : ''}>
                      {torEnabled ? 'Enabled' : 'Disabled'}
                    </span>
                  </span>
                </button>
                <div className="mx-2 my-1 border-t border-zinc-400 dark:border-gray-700" />
                <button
                  type="button"
                  onClick={() => setAdvancedOpen((open) => !open)}
                  aria-expanded={advancedOpen}
                  aria-controls="isola-advanced"
                  className={menuItemClass(advancedOpen) + ' flex items-center gap-2'}
                >
                  <span className="flex-1 text-left">Advanced</span>
                  <span className={`truncate text-[10px] ${advancedOpen ? 'opacity-70' : 'opacity-50'}`}>
                    {identitySummary(locale, timezone)}
                  </span>
                  <svg
                    xmlns="http://www.w3.org/2000/svg"
                    viewBox="0 0 24 24"
                    className={`h-3 w-3 shrink-0 transition-transform ${advancedOpen ? 'rotate-180' : ''}`}
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="2"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    aria-hidden
                  >
                    <path d="m6 9 6 6 6-6" />
                  </svg>
                </button>
                {advancedOpen && (
                  <div id="isola-advanced" className="flex flex-col gap-1.5 px-3 pt-1 pb-2">
                    <label className="flex items-center gap-2 text-[11px] text-zinc-600 dark:text-gray-500">
                      <span className="w-12 shrink-0">Locale</span>
                      <select
                        value={locale}
                        onChange={(e) => setLocale(e.target.value)}
                        className={identitySelectClass}
                      >
                        <option value="">Random each launch</option>
                        {withCurrent(LOCALE_OPTIONS, locale).map((option) => (
                          <option key={option.value} value={option.value}>{option.label}</option>
                        ))}
                      </select>
                    </label>
                    <label className="flex items-center gap-2 text-[11px] text-zinc-600 dark:text-gray-500">
                      <span className="w-12 shrink-0">Zone</span>
                      <select
                        value={timezone}
                        onChange={(e) => setTimezone(e.target.value)}
                        className={identitySelectClass}
                      >
                        <option value="">Random each launch</option>
                        {withCurrent(ZONE_OPTIONS, timezone).map((option) => (
                          <option key={option.value} value={option.value}>{option.label}</option>
                        ))}
                      </select>
                    </label>
                    <p className="pl-14 text-[10px] leading-snug text-zinc-500 dark:text-gray-600">
                      Sets the page's Date, Intl and navigator.languages. Random picks one coherent pair per launch.
                    </p>
                  </div>
                )}
              </div>
            )}
          </div>
          <span className="relative group">
            <button
              type="submit"
              disabled={!url.trim() || !backendOk}
              className="text-sm text-zinc-700 hover:text-zinc-900 dark:text-[#aea2a2] dark:hover:text-white disabled:opacity-40 transition-colors pb-1"
              title={backendOk ? '' : 'Backend unreachable — start the server or check your connection'}
            >
              Open →
            </button>
            {!backendOk && (
              <span
                className="pointer-events-none absolute bottom-full right-0 mb-2 px-2.5 py-1.5 rounded-md text-[11px] leading-snug w-56 text-right
                           text-white/90 bg-gray-900/90 dark:bg-black/80
                           border border-zinc-700/60 dark:border-white/10
                           shadow-xl backdrop-blur-xs
                           opacity-0 group-hover:opacity-100 transition-opacity duration-150
                           origin-bottom-right"
                role="tooltip"
              >
                <span className="block font-medium text-red-400 mb-0.5">Backend unreachable</span>
                <span className="block text-white/60">Start the server or check your connection</span>
              </span>
            )}
          </span>
        </div>
        )}
        {error && (
          <p className="mt-3 text-red-400 text-sm text-center">{error}</p>
        )}

        {me && me.apps.length > 0 && (
          <div className={`grid gap-3 ${me.hide_address_bar ? 'grid-cols-3 mt-4' : 'grid-cols-3 mt-8'}`}>
            {me.apps.map((app) => {
              const launch = (e: React.MouseEvent) => {
                e.preventDefault();
                launchTarget(normalizeUrl(app.url), false);
              };
              return (
                <a
                  key={app.id}
                  href={app.url}
                  onClick={launch}
                  className="flex flex-col items-center gap-2 px-3 py-4 rounded-xl border border-zinc-300 dark:border-gray-800
                             bg-zinc-100/60 dark:bg-gray-900/60 hover:border-zinc-500 dark:hover:border-gray-600
                             transition-colors"
                  title={app.url}
                >
                  <span className="text-xl" aria-hidden>{app.icon || '🌐'}</span>
                  <span className="text-xs text-zinc-700 dark:text-[#aea2a2] truncate max-w-full">{app.name}</span>
                </a>
              );
            })}
          </div>
        )}
      </form>

      {whatsNewVisible && (
        <div className="mt-10 w-full max-w-lg">
          <div className="flex items-baseline justify-between gap-3">
            <span className="text-[10px] uppercase tracking-[0.2em] text-zinc-500 dark:text-gray-600">
              What's new
            </span>
            <button
              type="button"
              onClick={dismissWhatsNew}
              className="-mr-1 p-1 text-zinc-400 hover:text-zinc-700 dark:text-gray-600 dark:hover:text-gray-300 transition-colors"
              title="Hide until the next release"
              aria-label="Dismiss what's new"
            >
              <X className="w-3 h-3" aria-hidden />
            </button>
          </div>
          <ul className="mt-1.5 border-t border-zinc-300/70 dark:border-gray-800">
            {whatsNewItems.map((item) => (
              <li key={item.title} className="border-b border-zinc-300/70 dark:border-gray-800">
                <a
                  href={item.href}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="group flex items-baseline gap-3 py-2"
                >
                  <span className="w-24 shrink-0 text-xs text-zinc-800 dark:text-[#aea2a2] transition-colors group-hover:text-zinc-950 dark:group-hover:text-white">
                    {item.title}
                  </span>
                  <span className="flex-1 text-[11px] leading-snug text-zinc-500 dark:text-gray-500 transition-colors group-hover:text-zinc-700 dark:group-hover:text-gray-400">
                    {item.detail}
                  </span>
                </a>
              </li>
            ))}
          </ul>
        </div>
      )}

      <div className="absolute bottom-4 left-0 right-0 px-6 flex items-center justify-between text-[10px] tracking-widest text-zinc-400 dark:text-gray-700">
        <span className="relative flex items-center gap-2 mr-auto">
          <span
            className={`inline-block w-1.5 h-1.5 rounded-full transition-colors ${
              backendOk ? 'bg-emerald-500' : 'bg-red-500 animate-pulse'
            }`}
            title={backendOk ? 'Backend reachable' : 'Backend unreachable — start the server or check your connection'}
          />
          <span className={backendOk ? '' : 'text-red-400/80 dark:text-red-500/70'}>
            {backendOk ? '' : ''}
          </span>
          {/* Absolutely positioned on purpose: the chip must not take part in
              the footer's flex layout, or the centred version line would slide
              sideways as it fades in. */}
          <span
            aria-hidden={!torEnabled}
            className={`absolute left-full top-1/2 ml-2 -translate-y-1/2 flex items-center gap-1.5 whitespace-nowrap normal-case tracking-normal text-purple-500/90 dark:text-purple-400/80 transition-opacity duration-700 ease-out motion-reduce:transition-none ${
              torEnabled ? 'opacity-100' : 'opacity-0'
            }`}
            title="New sessions route through Tor"
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              viewBox="0 0 24 24"
              className="w-2.5 h-2.5"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              aria-hidden
            >
              <circle cx="12" cy="12" r="10" />
              <circle cx="12" cy="12" r="6" />
              <circle cx="12" cy="12" r="2" />
            </svg>
            Tor
          </span>
        </span>
        <span className="flex-1 text-center">
          <a
            href={`${GITHUB_URL}/releases`}
            target="_blank"
            rel="noopener noreferrer"
            className="hover:text-zinc-600 dark:hover:text-gray-500 transition-colors"
            title="Releases on GitHub"
          >
            v{getVersion()}
          </a>
          {' · '}
          {getCommit() && getCommit() !== 'unknown' ? (
            <a
              href={`${GITHUB_URL}/commit/${getCommit()}`}
              target="_blank"
              rel="noopener noreferrer"
              className="hover:text-zinc-600 dark:hover:text-gray-500 transition-colors"
              title="Commit on GitHub"
            >
              {getCommit()}
            </a>
          ) : (
            <span>{getCommit()}</span>
          )}
        </span>
        <a
          href={GITHUB_URL}
          target="_blank"
          rel="noopener noreferrer"
          className="ml-4 text-zinc-500 hover:text-zinc-700 dark:text-gray-600 dark:hover:text-gray-300 transition-colors"
          title="View source on GitHub"
          aria-label="View source on GitHub"
        >
          <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="currentColor" className="w-4 h-4">
            <path d="M12 .5C5.37.5 0 5.87 0 12.5c0 5.3 3.44 9.8 8.21 11.39.6.11.82-.26.82-.58 0-.29-.01-1.24-.02-2.25-3.34.73-4.04-1.42-4.04-1.42-.55-1.39-1.34-1.76-1.34-1.76-1.09-.75.08-.73.08-.73 1.2.08 1.84 1.24 1.84 1.24 1.07 1.83 2.81 1.3 3.5.99.11-.78.42-1.3.76-1.6-2.67-.3-5.47-1.34-5.47-5.95 0-1.31.47-2.38 1.24-3.22-.12-.31-.54-1.53.12-3.18 0 0 1.01-.32 3.3 1.23a11.5 11.5 0 0 1 3-.4c1.02 0 2.05.14 3 .4 2.29-1.55 3.3-1.23 3.3-1.23.66 1.65.24 2.87.12 3.18.77.84 1.24 1.91 1.24 3.22 0 4.62-2.8 5.65-5.48 5.94.43.37.81 1.1.81 2.22 0 1.6-.01 2.9-.01 3.29 0 .32.22.7.82.58A12.01 12.01 0 0 0 24 12.5C24 5.87 18.63.5 12 .5z" />
          </svg>
        </a>
      </div>

      {/* Install affordance: shown only while Chromium holds an unspent prompt,
          so a click always opens its install dialog. It is not rendered when
          Chromium withholds the event — the address-bar install entry covers
          that case. The × is a separate control (a button inside a button is
          invalid HTML) and its choice is stored in localStorage. */}
      {installAvailable && (
        <div className="fixed bottom-16 right-6 flex items-center gap-1 pl-3.5 pr-1.5 py-1.5 rounded-full text-xs border border-zinc-300 dark:border-gray-800 bg-white/85 dark:bg-gray-900/85 backdrop-blur text-zinc-700 dark:text-[#aea2a2] shadow-lg">
          <button
            type="button"
            onClick={() => { void promptInstall(); }}
            className="flex items-center gap-2 text-left hover:text-zinc-900 dark:hover:text-white transition-colors"
            title="Install isola as an app"
          >
            <MonitorDown className="w-4 h-4 shrink-0" aria-hidden />
            <span>Install app</span>
          </button>
          <button
            type="button"
            onClick={dismissInstall}
            className="p-1 rounded-full text-zinc-400 hover:text-zinc-700 dark:text-gray-500 dark:hover:text-gray-300 transition-colors"
            title="Don't show this again"
            aria-label="Dismiss install prompt"
          >
            <X className="w-3 h-3" aria-hidden />
          </button>
        </div>
      )}
    </div>
  );
}
