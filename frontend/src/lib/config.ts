// App config — loaded from GET /api/config at bootstrap and stored as
// module-level state.

let resolutions: { value: string; label: string }[] = [];
let browsers: { value: string; label: string }[] = [];
let transports: { value: string; label: string }[] = [];
let defaultTransport = 'isola';
let isolaTabs = true;
// Session timer (session.timer). When false, sessions are unlimited and the
// viewer must terminate the session when the owner closes the page.
let timerEnabled = true;
// Concurrent-session cap (ISOLA_MAX_SESSIONS). 0 = unlimited. Used to explain a
// 429 from POST /api/sessions in human terms.
let maxSessions = 0;
// Network capture window (session.network_log_max_entries). The streamer owns
// the buffer; the viewer trims to the same count so a replay never sends more
// history than the client is willing to hold.
const DEFAULT_NETWORK_LOG_MAX_ENTRIES = 1000;
const MAX_NETWORK_LOG_MAX_ENTRIES = 10000;
let networkLogMaxEntries = DEFAULT_NETWORK_LOG_MAX_ENTRIES;
let version = 'dev';
let commit = 'unknown';
let backendReachable = false;
type BackendStatusListener = (ok: boolean) => void;
const backendStatusListeners = new Set<BackendStatusListener>();

function resolutionArea(value: string): number {
  const [w, h] = value.split('x').map(Number);
  return (w || 0) * (h || 0);
}

function normalizeNetworkLogMaxEntries(value: unknown): number {
  const n = Number(value);
  if (!Number.isFinite(n) || n <= 0) return DEFAULT_NETWORK_LOG_MAX_ENTRIES;
  return Math.min(Math.floor(n), MAX_NETWORK_LOG_MAX_ENTRIES);
}

export function initAppConfig(config: {
  resolutions?: { value: string; label: string }[];
  browsers?: { value: string; label: string }[];
  transports?: { value: string; label: string }[];
  default_transport?: string;
  isola_tabs?: boolean;
  timer?: boolean;
  max_sessions?: number;
  network_log_max_entries?: number;
  version?: string;
  commit?: string;
}) {
  // Picker order is ours, not the config's: highest resolution first.
  resolutions = [...(config.resolutions || [])].sort(
    (a, b) => resolutionArea(b.value) - resolutionArea(a.value),
  );
  browsers = config.browsers || [];
  transports = config.transports || [];
  defaultTransport = config.default_transport || 'isola';
  isolaTabs = config.isola_tabs !== false;
  timerEnabled = config.timer !== false;
  maxSessions = Number(config.max_sessions) > 0 ? Number(config.max_sessions) : 0;
  networkLogMaxEntries = normalizeNetworkLogMaxEntries(config.network_log_max_entries);
  version = config.version || 'dev';
  commit = config.commit || 'unknown';
}

export function getIsolaTabs(): boolean {
  return isolaTabs;
}

export function isTimerEnabled(): boolean {
  return timerEnabled;
}

/** Concurrent-session cap; 0 means unlimited. */
export function getMaxSessions(): number {
  return maxSessions;
}

/** Network requests retained by the capture window (streamer + viewer). */
export function getNetworkLogMaxEntries(): number {
  return networkLogMaxEntries;
}

export function getResolutions(): { value: string; label: string }[] {
  return resolutions;
}

export function getBrowsers(): { value: string; label: string }[] {
  return browsers;
}

export function getTransports(): { value: string; label: string }[] {
  return transports;
}

export function getDefaultTransport(): string {
  return defaultTransport;
}

export function getVersion(): string {
  return version;
}

export function getCommit(): string {
  return commit;
}

export function setBackendReachable(ok: boolean) {
  if (backendReachable === ok) return;
  backendReachable = ok;
  for (const l of backendStatusListeners) {
    try {
      l(ok);
    } catch {
      // ignore listener errors
    }
  }
}

export function isBackendReachable(): boolean {
  return backendReachable;
}

export function onBackendStatusChange(listener: BackendStatusListener): () => void {
  backendStatusListeners.add(listener);
  listener(backendReachable);
  return () => {
    backendStatusListeners.delete(listener);
  };
}
