import { useCallback, useEffect, useMemo, useRef, useState, type CSSProperties } from 'react';
import { cookieDomainLabel, cookieExpiryLabel } from '../lib/cookieEntries';
import { CLASS_ACCENT, CLASS_ACCENT_LIGHT, CLASS_LABEL, CLASS_NAME, CLASS_ORDER, classifyResource, type ResourceClass } from '../lib/resourceType';
import { useTheme } from '../lib/ThemeContext';
import { ResourceGraph } from './ResourceGraph';

export interface NetEntry {
  id: string;
  url: string;
  m: string;
  s: number;
  t: string;
  ip?: string;
  b?: number;
  f?: boolean;
  e?: string;
  tb?: number; // browser tab the request belongs to (tab strip id)
  ts?: number; // capture time in Unix ms (streamer CDP wallTime)
  /** Hex SHA-1 of the response body, once the streamer has hashed the file. */
  h?: string;
}

export interface ConsoleEntry {
  level: string;
  text: string;
  tb?: number; // browser tab the message was logged in (tab strip id)
  ts?: number; // capture time in Unix ms
}

export interface CookieEntry {
  domain: string;
  name: string;
  value: string;
  path: string;
  sameSite: string;
  expires: string;
  maxAge?: number;
  httpOnly: boolean;
  secure: boolean;
}

export type PanelTab = 'network' | 'graph' | 'console' | 'cookies' | 'domains';
export type PanelLayout = 'bottom' | 'side';

// One entry of the in-container browser tab strip (broadcast by the streamer
// as a `tabs` message).
export interface BrowserTab {
  id: number;
  url: string;
  title: string;
  active: boolean;
}

export interface PanelSize {
  bottomHeightPx: number;
  sideWidthPx: number;
}

const RISKY_TLDS = ['tk', 'xyz', 'top', 'gq', 'cf', 'ml', 'pw', 'cc', 'su'];

export interface NetFlags {
  ip: boolean;
  insecure: boolean;
  riskyTld: boolean;
  randomHost: boolean;
  oddPort: boolean;
}

function shannon(s: string): number {
  if (!s.length) return 0;
  const freq = new Map<string, number>();
  for (const c of s) freq.set(c, (freq.get(c) || 0) + 1);
  let h = 0;
  for (const v of freq.values()) {
    const p = v / s.length;
    h -= p * Math.log2(p);
  }
  return h;
}

export function analyze(url: string): { host: string; path: string; flags: NetFlags } {
  let host = '';
  let path = '';
  let scheme = 'https:';
  let port = '';
  try {
    const u = new URL(url);
    host = u.hostname;
    path = u.pathname + u.search;
    scheme = u.protocol;
    port = u.port;
  } catch {
    host = url.slice(0, 40);
    path = '';
  }
  const flags: NetFlags = {
    ip: /^(\d{1,3}\.){3}\d{1,3}$/.test(host),
    insecure: scheme === 'http:',
    riskyTld: RISKY_TLDS.includes(host.split('.').pop() || ''),
    randomHost: (() => {
      const labels = host.split('.').filter((l) => l.length >= 12);
      return labels.some((l) => shannon(l) > 3.8);
    })(),
    oddPort: port !== '' && port !== '80' && port !== '443',
  };
  return { host, path, flags };
}

function fmtSize(b?: number): string {
  if (!b || b <= 0) return '—';
  if (b < 1024) return `${b} B`;
  if (b < 1048576) return `${(b / 1024).toFixed(1)} KB`;
  return `${(b / 1048576).toFixed(1)} MB`;
}

// The row shows a truncated digest — enough leading hex to compare files by eye
// — with the full value on the row's tooltip, in the detail panel, and behind
// the cell's click-to-copy.
function shortHash(h: string): string {
  return h.length > 12 ? `${h.slice(0, 12)}…` : h;
}

// Wall-clock time a request was captured (Unix ms from the streamer), shown as
// HH:MM:SS.mmm in the viewer's local zone. Old streamers omit the timestamp.
function fmtTime(ts?: number): string {
  if (!ts) return '—';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return '—';
  const p = (n: number, w = 2) => String(n).padStart(w, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}

// Clipboard output is a data format, not a screenshot of the table: ISO 8601
// times (unambiguous and sortable), raw numbers, and one escaped cell per
// column so a value can never break a record across lines.
function isoTime(ts?: number): string {
  if (!ts) return '';
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? '' : d.toISOString();
}

function tsvCell(v: string | number | boolean | undefined | null): string {
  return String(v ?? '').replace(/\t/g, '\\t').replace(/\r?\n/g, '\\n');
}

function tsvRow(cells: Array<string | number | boolean | undefined | null>): string {
  return cells.map(tsvCell).join('\t');
}

// Domain shown for a browser tab in the per-tab filter. Internal pages and
// empty/placeholder URLs carry no domain, so the caller falls back to the
// hosts seen in that tab's captured requests.
// Host the resource graph should hang off: the page open in the selected tab,
// or the active tab when the panel is showing every tab. Placeholder URLs
// (about:blank, new tab) are not a page, so the caller falls back to the
// first captured document.
export function graphOriginHost(tabs: BrowserTab[] | undefined, tabFilter: number | ''): string {
  const list = tabs ?? [];
  const chosen = tabFilter === ''
    ? list.find((t) => t.active) ?? list[0]
    : list.find((t) => t.id === tabFilter);
  const url = (chosen?.url || '').trim();
  // Same host string the graph uses for request nodes (no www strip). A
  // placeholder tab is not a page the user opened.
  if (!url || /^(about|chrome|edge|devtools|data|view-source):/i.test(url)) return '';
  try {
    return new URL(url).hostname;
  } catch {
    return '';
  }
}

function domainFromUrl(raw: string): string {
  const url = (raw || '').trim();
  if (!url || /^(about|chrome|edge|devtools|data|view-source):/i.test(url)) return '';
  try {
    return new URL(url).hostname.replace(/^www\./, '');
  } catch {
    return '';
  }
}

export interface TabOption {
  id: number;
  label: string;
  title: string;
}

// Builds the per-tab filter options, named by domain. Sources are merged so a
// tab shows up even when none of its traffic was captured (the streamer's CDP
// client attaches to the active target only, so a freshly opened tab can
// briefly produce nothing): the browser tab strip first, then any tab id seen
// in captured events. Tabs sharing a domain are numbered so the options stay
// distinct ("example.com (1)", "example.com (2)").
export function buildTabOptions(
  tabs: BrowserTab[] | undefined,
  entries: NetEntry[],
  consoleEntries: ConsoleEntry[],
): TabOption[] {
  const domainById = new Map<number, string>();
  for (const t of tabs ?? []) {
    if (typeof t.id === 'number') domainById.set(t.id, domainFromUrl(t.url));
  }
  const observed = new Set<number>();
  for (const e of entries) if (typeof e.tb === 'number') observed.add(e.tb);
  for (const c of consoleEntries) if (typeof c.tb === 'number') observed.add(c.tb);
  for (const id of observed) if (!domainById.has(id)) domainById.set(id, '');

  // Placeholder URLs (about:blank, new-tab pages) carry no domain: fall back to
  // the host seen most often in that tab's captured requests.
  const hostCounts = new Map<number, Map<string, number>>();
  for (const e of entries) {
    if (typeof e.tb !== 'number' || domainById.get(e.tb)) continue;
    const host = analyze(e.url).host.replace(/^www\./, '');
    if (!host) continue;
    let counts = hostCounts.get(e.tb);
    if (!counts) {
      counts = new Map();
      hostCounts.set(e.tb, counts);
    }
    counts.set(host, (counts.get(host) || 0) + 1);
  }
  for (const [id, counts] of hostCounts) {
    if (domainById.get(id)) continue;
    let best = '';
    let bestN = -1;
    for (const [host, n] of counts) {
      if (n > bestN) {
        best = host;
        bestN = n;
      }
    }
    if (best) domainById.set(id, best);
  }

  const labelOf = (id: number) => domainById.get(id) || (id < 0 ? 'unattributed' : `tab ${id}`);
  const ids = Array.from(domainById.keys()).sort((a, b) => a - b);
  const totals = new Map<string, number>();
  for (const id of ids) totals.set(labelOf(id), (totals.get(labelOf(id)) || 0) + 1);
  const seen = new Map<string, number>();
  return ids.map((id) => {
    const base = labelOf(id);
    const ordinal = (seen.get(base) || 0) + 1;
    seen.set(base, ordinal);
    const url = (tabs ?? []).find((t) => t.id === id)?.url || '';
    return {
      id,
      label: (totals.get(base) || 1) > 1 ? `${base} (${ordinal})` : base,
      title: url ? `Tab ${id} — ${url}` : `Tab ${id}`,
    };
  });
}

function statusStyle(s: number, failed: boolean): string {
  if (failed || s === 0) return 'text-red-600 dark:text-red-400';
  if (s >= 500) return 'text-red-600 dark:text-red-300';
  if (s >= 400) return 'text-red-600 dark:text-red-400';
  if (s >= 300) return 'text-amber-600 dark:text-yellow-300';
  if (s >= 200) return 'text-emerald-600 dark:text-emerald-400';
  return 'text-zinc-500 dark:text-gray-500';
}

// HTTP method a request is filed under, defaulting to GET when the streamer did
// not report one (matches how rows render).
export function methodOf(e: NetEntry): string {
  return (e.m || 'GET').toUpperCase();
}

export type StatusGroup = '2xx' | '3xx' | '4xx' | '5xx' | 'ERR' | 'other';

// Buckets a request into the coarse status family shown in the filter chips.
// Transport failures (status 0 / flagged failed) are their own group; responses
// that never arrived fall into `other`.
export function statusGroupOf(e: NetEntry): StatusGroup {
  if (e.f) return 'ERR';
  if (!e.s) return 'other';
  if (e.s >= 500) return '5xx';
  if (e.s >= 400) return '4xx';
  if (e.s >= 300) return '3xx';
  if (e.s >= 200) return '2xx';
  return 'other';
}

// Display order for status chips: families low→high, then failures, then the
// unclassified bucket.
const STATUS_ORDER: StatusGroup[] = ['2xx', '3xx', '4xx', '5xx', 'ERR', 'other'];

const METHOD_ACCENT: Record<string, string> = {
  GET: '#34d399',
  POST: '#60a5fa',
  PUT: '#fbbf24',
  PATCH: '#a78bfa',
  DELETE: '#f87171',
  HEAD: '#22d3ee',
  OPTIONS: '#94a3b8',
};
const ACCENT_FALLBACK = '#94a3b8';

const STATUS_ACCENT: Record<StatusGroup, string> = {
  '2xx': '#34d399',
  '3xx': '#fbbf24',
  '4xx': '#fb923c',
  '5xx': '#ef4444',
  ERR: '#f472b6',
  other: '#94a3b8',
};

function methodAccent(m: string): string {
  return METHOD_ACCENT[m] || ACCENT_FALLBACK;
}

const FLAG_LABEL: Record<keyof NetFlags, string> = {
  ip: 'IP host',
  insecure: 'insecure',
  riskyTld: 'risky TLD',
  randomHost: 'random domain',
  oddPort: 'odd port',
};

const TABS: PanelTab[] = ['network', 'graph', 'domains', 'console', 'cookies'];

const LS_BOTTOM_HEIGHT = 'isola-panel-bottom-h';
const LS_SIDE_WIDTH = 'isola-panel-side-w';
const DEFAULT_BOTTOM_HEIGHT = () => clampBottom(Math.round(window.innerHeight * 0.45));
const DEFAULT_SIDE_WIDTH = 480;
const MIN_BOTTOM_HEIGHT = 120;
const MIN_SIDE_WIDTH = 260;

// Keep the panel from swallowing the toolbar/viewer area.
const MAX_BOTTOM_HEIGHT = () => Math.max(MIN_BOTTOM_HEIGHT + 40, window.innerHeight - 140);
const MAX_SIDE_WIDTH = () => Math.max(MIN_SIDE_WIDTH + 40, window.innerWidth - 220);
const clampBottom = (v: number) => Math.min(Math.max(v, MIN_BOTTOM_HEIGHT), MAX_BOTTOM_HEIGHT());
const clampSide = (v: number) => Math.min(Math.max(v, MIN_SIDE_WIDTH), MAX_SIDE_WIDTH());

interface Props {
  entries: NetEntry[];
  consoleEntries: ConsoleEntry[];
  cookieEntries: CookieEntry[];
  layout: PanelLayout;
  tab: PanelTab;
  onTab: (t: PanelTab) => void;
  onLayout: (l: PanelLayout) => void;
  onClose: () => void;
  onSizeChange?: (size: PanelSize) => void;
  // Browser tab strip state, used to name the per-tab filter by domain.
  tabs?: BrowserTab[];
  // Standalone mode (the /session/:id/network tab): fill the parent instead of
  // docking to an edge, and drop the resize/dock controls that only make sense
  // beside the live view.
  full?: boolean;
  // When provided, offer a "new tab" action in the header.
  onPopOut?: () => void;
  closeTitle?: string;
}

export function IsolaNetworkPanel({ entries, consoleEntries, cookieEntries, layout, tab, onTab, onLayout, onClose, onSizeChange, tabs, full, onPopOut, closeTitle }: Props) {
  const { theme } = useTheme();
  const [filter, setFilter] = useState('');
  const [onlyFlagged, setOnlyFlagged] = useState(false);
  const [tabFilter, setTabFilter] = useState<number | ''>('');
  const [methodFilter, setMethodFilter] = useState<ReadonlySet<string>>(new Set());
  const [statusFilter, setStatusFilter] = useState<ReadonlySet<StatusGroup>>(new Set());
  const [typeFilter, setTypeFilter] = useState<ReadonlySet<ResourceClass>>(new Set());
  const [hot, setHot] = useState(false);
  const [openKey, setOpenKey] = useState<string | null>(null);
  const [copiedTag, setCopiedTag] = useState('');
  const listRef = useRef<HTMLDivElement>(null);
  const stickRef = useRef(true);
  const draggingRef = useRef(false);
  const startRectRef = useRef<{ width: number; height: number; clientY: number; clientX: number } | null>(null);

  const [bottomHeight, setBottomHeight] = useState<number>(() => {
    try {
      const v = localStorage.getItem(LS_BOTTOM_HEIGHT);
      // Clamp restored value: guards against oversized values saved earlier.
      return v ? clampBottom(parseInt(v, 10)) : DEFAULT_BOTTOM_HEIGHT();
    } catch { return DEFAULT_BOTTOM_HEIGHT(); }
  });
  const [sideWidth, setSideWidth] = useState<number>(() => {
    try {
      const v = localStorage.getItem(LS_SIDE_WIDTH);
      return v ? clampSide(parseInt(v, 10)) : DEFAULT_SIDE_WIDTH;
    } catch { return DEFAULT_SIDE_WIDTH; }
  });

  // Re-clamp when the window itself shrinks (orientation change, browser resize).
  useEffect(() => {
    const onWinResize = () => {
      setBottomHeight((h) => clampBottom(h));
      setSideWidth((w) => clampSide(w));
    };
    window.addEventListener('resize', onWinResize);
    return () => window.removeEventListener('resize', onWinResize);
  }, []);

  useEffect(() => {
    try { localStorage.setItem(LS_BOTTOM_HEIGHT, String(bottomHeight)); } catch {}
  }, [bottomHeight]);
  useEffect(() => {
    try { localStorage.setItem(LS_SIDE_WIDTH, String(sideWidth)); } catch {}
  }, [sideWidth]);

  useEffect(() => {
    onSizeChange?.({ bottomHeightPx: bottomHeight, sideWidthPx: sideWidth });
  }, [bottomHeight, sideWidth, onSizeChange]);

  // Per-tab filter options, named by domain (see buildTabOptions).
  const tabOptions = useMemo(
    () => buildTabOptions(tabs, entries, consoleEntries),
    [tabs, entries, consoleEntries],
  );

  const tabFiltered = useMemo(
    () => (tabFilter === '' ? entries : entries.filter((e) => e.tb === tabFilter)),
    [entries, tabFilter],
  );
  const consoleFiltered = useMemo(
    () => (tabFilter === '' ? consoleEntries : consoleEntries.filter((c) => c.tb === tabFilter)),
    [consoleEntries, tabFilter],
  );

  const analyzed = useMemo(
    () => tabFiltered.map((e) => ({ e, ...analyze(e.url) })),
    [tabFiltered],
  );
  const visible = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return analyzed.filter(({ e, host, flags }) => {
      if (onlyFlagged && !Object.values(flags).some(Boolean)) return false;
      if (methodFilter.size > 0 && !methodFilter.has(methodOf(e))) return false;
      if (statusFilter.size > 0 && !statusFilter.has(statusGroupOf(e))) return false;
      if (typeFilter.size > 0 && !typeFilter.has(classifyResource(e.t || '', e.url))) return false;
      if (!q) return true;
      return e.url.toLowerCase().includes(q) || host.includes(q);
    });
  }, [analyzed, filter, onlyFlagged, methodFilter, statusFilter, typeFilter]);

  // The graph honours the panel's text / flagged / method / status filters but
  // keeps its own resource-type facet: the legend in the graph toolbar is the
  // authoritative type filter there, and applying both would make one of the
  // two chip rows silently invisible.
  const graphEntries = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return analyzed
      .filter(({ e, host, flags }) => {
        if (onlyFlagged && !Object.values(flags).some(Boolean)) return false;
        if (methodFilter.size > 0 && !methodFilter.has(methodOf(e))) return false;
        if (statusFilter.size > 0 && !statusFilter.has(statusGroupOf(e))) return false;
        if (!q) return true;
        return e.url.toLowerCase().includes(q) || host.includes(q);
      })
      .map(({ e }) => e);
  }, [analyzed, filter, onlyFlagged, methodFilter, statusFilter]);

  // Method / status chips are derived from the tab-filtered set (before the text
  // and flagged filters), so their counts and presence stay stable as the user
  // narrows the list.
  const methodLegend = useMemo(() => {
    const counts = new Map<string, number>();
    for (const { e } of analyzed) {
      const m = methodOf(e);
      counts.set(m, (counts.get(m) || 0) + 1);
    }
    return Array.from(counts.entries()).sort((a, b) => b[1] - a[1]);
  }, [analyzed]);

  const statusLegend = useMemo(() => {
    const counts = new Map<StatusGroup, number>();
    for (const { e } of analyzed) {
      const g = statusGroupOf(e);
      counts.set(g, (counts.get(g) || 0) + 1);
    }
    return STATUS_ORDER.filter((g) => counts.has(g)).map((g) => [g, counts.get(g) as number] as const);
  }, [analyzed]);

  // Resource-type facet, classified with the same helper the graph colours
  // with so a row's type and its node agree.
  const typeLegend = useMemo(() => {
    const counts = new Map<ResourceClass, number>();
    for (const { e } of analyzed) {
      const c = classifyResource(e.t || '', e.url);
      counts.set(c, (counts.get(c) || 0) + 1);
    }
    return CLASS_ORDER.filter((c) => counts.has(c)).map((c) => [c, counts.get(c) as number] as const);
  }, [analyzed]);

  const toggleMethod = useCallback((m: string) => {
    setMethodFilter((prev) => {
      const next = new Set(prev);
      if (next.has(m)) next.delete(m);
      else next.add(m);
      return next;
    });
  }, []);

  const toggleStatus = useCallback((g: StatusGroup) => {
    setStatusFilter((prev) => {
      const next = new Set(prev);
      if (next.has(g)) next.delete(g);
      else next.add(g);
      return next;
    });
  }, []);

  const toggleType = useCallback((c: ResourceClass) => {
    setTypeFilter((prev) => {
      const next = new Set(prev);
      if (next.has(c)) next.delete(c);
      else next.add(c);
      return next;
    });
  }, []);

  const clearFacets = useCallback(() => {
    setMethodFilter(new Set());
    setStatusFilter(new Set());
    setTypeFilter(new Set());
  }, []);

  const flaggedCount = useMemo(
    () => analyzed.filter(({ flags }) => Object.values(flags).some(Boolean)).length,
    [analyzed],
  );

  // A single request streams several updates (request → response → finished)
  // that share the same id and url, so id+url is not a unique row identity.
  // Rows are keyed by position instead; `openId` recovers the request id of the
  // open row so its siblings can be highlighted without expanding them.
  const openId = useMemo(() => {
    if (!openKey) return null;
    const i = Number.parseInt(openKey.slice(openKey.lastIndexOf('#') + 1), 10);
    return Number.isNaN(i) ? null : visible[i]?.e.id ?? null;
  }, [openKey, visible]);

  interface DomainRow {
    host: string;
    ips: Set<string>;
    count: number;
    bytes: number;
    flagged: boolean;
    flags: NetFlags;
    entries: Array<{ e: NetEntry; host: string; path: string; flags: NetFlags }>;
  }

  const domainRows = useMemo<DomainRow[]>(() => {
    const map = new Map<string, DomainRow>();
    const q = filter.trim().toLowerCase();
    for (const row of analyzed) {
      const { e, host, flags } = row;
      if (onlyFlagged && !Object.values(flags).some(Boolean)) continue;
      if (q && !e.url.toLowerCase().includes(q) && !host.includes(q)) continue;
      let agg = map.get(host);
      if (!agg) {
        agg = {
          host,
          ips: new Set(),
          count: 0,
          bytes: 0,
          flagged: false,
          flags,
          entries: [],
        };
        map.set(host, agg);
      }
      agg.count++;
      agg.bytes += e.b || 0;
      if (e.ip) agg.ips.add(e.ip);
      if (Object.values(flags).some(Boolean)) agg.flagged = true;
      agg.entries.push(row);
    }
    return Array.from(map.values()).sort((a, b) => {
      if (a.flagged !== b.flagged) return a.flagged ? -1 : 1;
      return b.count - a.count;
    });
  }, [analyzed, filter, onlyFlagged]);

  const [domainOpenKey, setDomainOpenKey] = useState<string | null>(null);

  useEffect(() => {
    const el = listRef.current;
    if (el && stickRef.current) el.scrollTop = el.scrollHeight;
  }, [visible]);

  const copyText = useCallback((text: string, tag: string) => {
    navigator.clipboard.writeText(text).then(() => {
      setCopiedTag(tag);
      window.setTimeout(() => setCopiedTag((c) => (c === tag ? '' : c)), 1500);
    }).catch(() => {});
  }, []);

  const copyAllVisible = useCallback(() => {
    const lines = [
      tsvRow(['time', 'method', 'status', 'bytes', 'type', 'ip', 'sha1', 'error', 'url']),
      ...visible.map(({ e }) =>
        tsvRow([isoTime(e.ts), e.m || 'GET', e.f ? 'ERR' : e.s || '', e.b || 0, e.t, e.ip, e.h, e.e, e.url]),
      ),
    ];
    copyText(lines.join('\n'), 'all');
  }, [visible, copyText]);

  const copyDomains = useCallback(() => {
    const lines = [
      tsvRow(['domain', 'ips', 'requests', 'bytes']),
      ...domainRows.map((d) => tsvRow([d.host, Array.from(d.ips).join('|'), d.count, d.bytes])),
    ];
    copyText(lines.join('\n'), 'domains');
  }, [domainRows, copyText]);

  const copyConsole = useCallback(() => {
    const lines = [
      tsvRow(['time', 'level', 'text']),
      ...consoleFiltered.map((c) => tsvRow([isoTime(c.ts), c.level, c.text])),
    ];
    copyText(lines.join('\n'), 'console');
  }, [consoleFiltered, copyText]);

  const copyCookies = useCallback(() => {
    const lines = [
      tsvRow(['name', 'value', 'domain', 'path', 'expires', 'samesite', 'httponly', 'secure']),
      ...cookieEntries.map((c) =>
        tsvRow([
          c.name,
          c.value,
          cookieDomainLabel(c.domain),
          c.path || '/',
          cookieExpiryLabel(c),
          c.sameSite,
          c.httpOnly,
          c.secure,
        ]),
      ),
    ];
    copyText(lines.join('\n'), 'cookies');
  }, [cookieEntries, copyText]);

  // Copy follows the active tab: each tab renders its own kind of row, so
  // copying network requests while the user is looking at cookies/console is
  // never what the button appears to promise.
  const copyAction =
    tab === 'domains' ? copyDomains : tab === 'console' ? copyConsole : tab === 'cookies' ? copyCookies : copyAllVisible;
  const copyTag = tab === 'domains' ? 'domains' : tab === 'console' ? 'console' : tab === 'cookies' ? 'cookies' : 'all';
  const copyCount =
    tab === 'domains' ? domainRows.length : tab === 'console' ? consoleFiltered.length : tab === 'cookies' ? cookieEntries.length : visible.length;
  const copyTitle =
    tab === 'domains' ? 'Copy domains as TSV to clipboard'
      : tab === 'console' ? 'Copy visible console lines as TSV to clipboard'
        : tab === 'cookies' ? 'Copy all cookies as TSV to clipboard'
          : 'Copy visible rows as TSV to clipboard';

  const exportHAR = useCallback(() => {
    const har = {
      log: {
        version: '1.2',
        creator: { name: 'isola network monitor', version: '1.0', _comment: 'metadata-only capture (no bodies/headers)' },
        entries: tabFiltered.map((e) => ({
          startedDateTime: e.ts ? new Date(e.ts).toISOString() : new Date().toISOString(),
          time: 0,
          request: { method: e.m || 'GET', url: e.url, httpVersion: 'HTTP/1.1', headers: [], queryString: [], cookies: [], headersSize: -1, bodySize: 0 },
          response: { status: e.s || 0, statusText: e.f ? 'FAILED' : '', httpVersion: 'HTTP/1.1', headers: [], cookies: [], content: { size: e.b || 0, mimeType: e.t || '' }, redirectURL: '', headersSize: -1, bodySize: e.b || 0 },
          cache: {},
          timings: { send: 0, wait: 0, receive: 0 },
          _resourceType: e.t || '',
          _sourceIP: e.ip || '',
          _sha1: e.h || '',
          _failed: e.f || false,
          _error: e.e || '',
        })),
      },
    };
    const blob = new Blob([JSON.stringify(har, null, 2)], { type: 'application/json' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = `isola-network-${Date.now()}.har`;
    a.click();
    URL.revokeObjectURL(a.href);
  }, [tabFiltered]);

  const onResizeStart = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    e.stopPropagation();
    draggingRef.current = true;
    startRectRef.current = {
      width: sideWidth,
      height: bottomHeight,
      clientY: e.clientY,
      clientX: e.clientX,
    };
    document.body.style.userSelect = 'none';
    document.body.style.cursor = layout === 'bottom' ? 'ns-resize' : 'ew-resize';
  }, [sideWidth, bottomHeight, layout]);

  useEffect(() => {
    const onMove = (e: MouseEvent) => {
      if (!draggingRef.current || !startRectRef.current) return;
      const start = startRectRef.current;
      if (layout === 'bottom') {
        const delta = start.clientY - e.clientY;
        setBottomHeight(clampBottom(start.height + delta));
      } else {
        const delta = start.clientX - e.clientX;
        setSideWidth(clampSide(start.width + delta));
      }
    };
    const onUp = () => {
      if (draggingRef.current) {
        draggingRef.current = false;
        startRectRef.current = null;
        document.body.style.userSelect = '';
        document.body.style.cursor = '';
      }
    };
    window.addEventListener('mousemove', onMove, true);
    window.addEventListener('mouseup', onUp, true);
    return () => {
      window.removeEventListener('mousemove', onMove, true);
      window.removeEventListener('mouseup', onUp, true);
    };
  }, [layout]);

  // The standalone page fills its parent with an opaque panel. The in-viewer
  // panel instead floats over the stream like the debug overlay: a translucent,
  // blurred card inset from the edges that does not reserve canvas space.
  const panelBg = theme === 'light' ? 'rgba(255, 255, 255, 0.97)' : 'rgba(17, 24, 39, 0.97)';
  const FLOAT_INSET = 12;
  const panelStyle: CSSProperties = full
    ? { inset: 0, backgroundColor: panelBg }
    : layout === 'bottom'
      ? { left: FLOAT_INSET, right: FLOAT_INSET, bottom: FLOAT_INSET, height: `${bottomHeight}px` }
      // Clear the 36px isola browser chrome (tab strip) plus the float margin.
      : { top: FLOAT_INSET + 32, right: FLOAT_INSET, bottom: FLOAT_INSET, width: `${sideWidth}px` };

  return (
    <div
      onMouseEnter={() => setHot(true)}
      onMouseLeave={() => setHot(false)}
      className={
        full
          ? 'absolute inset-0 z-30 flex flex-col font-mono text-xs text-zinc-800 dark:text-gray-200'
          : `absolute z-30 flex flex-col overflow-hidden rounded-xl border border-cyan-500/30 bg-white/80 font-mono text-xs text-zinc-800 shadow-2xl backdrop-blur transition-opacity duration-200 dark:border-cyan-500/20 dark:bg-slate-950/70 dark:text-gray-200 ${hot ? 'opacity-100' : 'opacity-60'}`
      }
      style={panelStyle}
    >
      {!full && layout === 'bottom' && (
        <div
          onMouseDown={onResizeStart}
          onDoubleClick={() => setBottomHeight(DEFAULT_BOTTOM_HEIGHT())}
          className="absolute top-0 left-0 right-0 z-40 h-1.5 cursor-ns-resize hover:bg-cyan-500/20 transition-colors"
          title="Drag to resize · double-click to reset"
        >
          <div className="pointer-events-none mx-auto mt-px h-0.5 w-10 rounded-full bg-zinc-400 dark:bg-gray-600" />
        </div>
      )}
      {!full && layout === 'side' && (
        <div
          onMouseDown={onResizeStart}
          onDoubleClick={() => setSideWidth(DEFAULT_SIDE_WIDTH)}
          className="absolute left-0 top-0 bottom-0 z-40 w-1.5 cursor-ew-resize hover:bg-cyan-500/20 transition-colors"
          title="Drag to resize · double-click to reset"
        >
          <div className="pointer-events-none my-auto ml-px h-10 w-0.5 rounded-full bg-zinc-400 dark:bg-gray-600" />
        </div>
      )}

      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 border-b border-zinc-300 px-2 py-1.5 dark:border-gray-700">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-bold text-zinc-900 dark:text-gray-100">Analysis</span>
          <span className={`whitespace-nowrap ${flaggedCount ? 'rounded bg-amber-500/20 px-1.5 py-0.5 text-amber-700 dark:text-amber-300' : 'text-zinc-500 dark:text-gray-500'}`}>
            {tabFilter === '' ? entries.length : `${tabFiltered.length}/${entries.length}`} req · {flaggedCount} flagged
          </span>
          <input type="text" value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="filter…" className="w-40 rounded border border-zinc-300 bg-white px-2 py-0.5 text-zinc-800 outline-none focus:border-cyan-500 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-200" />
          <label className="flex cursor-pointer items-center gap-1 select-none">
            <input type="checkbox" checked={onlyFlagged} onChange={(e) => setOnlyFlagged(e.target.checked)} className="accent-amber-400" />
            flagged
          </label>
          {tabOptions.length >= 2 && (
            <select
              value={tabFilter}
              onChange={(e) => setTabFilter(e.target.value === '' ? '' : Number(e.target.value))}
              className="rounded border border-zinc-300 bg-white px-1 py-0.5 text-zinc-800 outline-none focus:border-cyan-500 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-200"
              title="Show only traffic from one browser tab"
            >
              <option value="">all tabs</option>
              {tabOptions.map((t) => (
                <option key={t.id} value={t.id} title={t.title}>{t.label}</option>
              ))}
            </select>
          )}
        </div>
        <div className="ml-auto flex items-center gap-2">
          <button onClick={copyAction} disabled={copyCount === 0} className="whitespace-nowrap rounded px-1.5 py-0.5 text-cyan-600 hover:bg-zinc-200 hover:text-cyan-700 disabled:opacity-40 dark:text-cyan-400 dark:hover:bg-gray-700 dark:hover:text-cyan-200" title={copyTitle}>
            {copiedTag === copyTag ? '✓ copied' : '⧉ copy'}
          </button>
          <button onClick={exportHAR} className="whitespace-nowrap rounded px-1.5 py-0.5 text-cyan-600 hover:bg-zinc-200 hover:text-cyan-700 dark:text-cyan-400 dark:hover:bg-gray-700 dark:hover:text-cyan-200" title="Export captured requests as HAR 1.2">⇩ HAR</button>
          {onPopOut && (
            <button onClick={onPopOut} className="whitespace-nowrap rounded px-1.5 py-0.5 text-cyan-600 hover:bg-zinc-200 hover:text-cyan-700 dark:text-cyan-400 dark:hover:bg-gray-700 dark:hover:text-cyan-200" title="Open this monitor in a new browser tab">↗ new tab</button>
          )}
          {!full && (
            <button onClick={() => onLayout(layout === 'bottom' ? 'side' : 'bottom')} className="whitespace-nowrap rounded px-1.5 py-0.5 text-zinc-500 hover:bg-zinc-200 hover:text-zinc-900 dark:text-gray-400 dark:hover:bg-gray-700 dark:hover:text-gray-100" title={layout === 'bottom' ? 'Move panel to the side' : 'Move panel to the bottom'}>
              {layout === 'bottom' ? '⇥ side' : '⤓ bottom'}
            </button>
          )}
          <button onClick={onClose} className="whitespace-nowrap rounded px-1.5 py-0.5 text-zinc-500 hover:bg-zinc-200 hover:text-zinc-900 dark:text-gray-400 dark:hover:bg-gray-700 dark:hover:text-gray-100" title={closeTitle || 'Close panel'}>✕</button>
        </div>
      </div>
      <div className="flex flex-wrap items-center gap-0 border-b border-zinc-300 px-1 dark:border-gray-700">
        {TABS.map((t) => (
          <button key={t} onClick={() => onTab(t)}
            className={`whitespace-nowrap px-3 py-1 text-[11px] capitalize transition-colors ${tab === t ? 'border-b-2 border-cyan-500 text-cyan-600 dark:border-cyan-400 dark:text-cyan-300' : 'text-zinc-500 hover:text-zinc-700 dark:text-gray-500 dark:hover:text-gray-300'}`}>
            {t}
            {t === 'network' && ` (${tabFiltered.length})`}
            {t === 'graph' && ` (${tabFiltered.length})`}
            {t === 'domains' && ` (${domainRows.length})`}
            {t === 'console' && ` (${consoleFiltered.length})`}
            {t === 'cookies' && ` (${cookieEntries.length})`}
          </button>
        ))}
      </div>
      {tab === 'network' && (
        <>
          {(methodLegend.length > 0 || statusLegend.length > 0 || typeLegend.length > 0) && (
            <div className="flex flex-wrap items-center gap-x-2 gap-y-1 border-b border-zinc-200 px-2 py-1 text-[10px] dark:border-gray-800">
              <div className="flex flex-wrap items-center gap-1">
                {methodLegend.map(([m, n]) => {
                  const accent = methodAccent(m);
                  const active = methodFilter.has(m);
                  return (
                    <button
                      key={m}
                      onClick={() => toggleMethod(m)}
                      title={`Filter by ${m}`}
                      className="inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 transition-all"
                      style={{
                        backgroundColor: active ? `${accent}22` : 'transparent',
                        boxShadow: active ? `inset 0 0 0 1px ${accent}88` : 'none',
                        opacity: methodFilter.size > 0 && !active ? 0.4 : 1,
                      }}
                    >
                      <span
                        className="inline-flex h-4 items-center justify-center rounded text-[8.5px] font-bold tracking-wide"
                        style={{ backgroundColor: `${accent}18`, color: accent, boxShadow: `inset 0 0 0 1px ${accent}44`, padding: '0 4px' }}
                      >
                        {m}
                      </span>
                      <span className="text-zinc-500 dark:text-gray-500">{n}</span>
                    </button>
                  );
                })}
              </div>
              {statusLegend.length > 0 && (
                <div className="flex flex-wrap items-center gap-1">
                  {statusLegend.map(([g, n]) => {
                    const accent = STATUS_ACCENT[g];
                    const active = statusFilter.has(g);
                    return (
                      <button
                        key={g}
                        onClick={() => toggleStatus(g)}
                        title={`Filter by ${g}`}
                        className="inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 transition-all"
                        style={{
                          backgroundColor: active ? `${accent}22` : 'transparent',
                          boxShadow: active ? `inset 0 0 0 1px ${accent}88` : 'none',
                          opacity: statusFilter.size > 0 && !active ? 0.4 : 1,
                        }}
                      >
                        <span
                          className="inline-flex h-4 items-center justify-center rounded text-[8.5px] font-bold tracking-wide"
                          style={{ backgroundColor: `${accent}18`, color: accent, boxShadow: `inset 0 0 0 1px ${accent}44`, padding: '0 4px' }}
                        >
                          {g}
                        </span>
                        <span className="text-zinc-500 dark:text-gray-500">{n}</span>
                      </button>
                    );
                  })}
                </div>
              )}
              {typeLegend.length > 0 && (
                <div className="flex flex-wrap items-center gap-1">
                  {typeLegend.map(([c, n]) => {
                    const accent = (theme === 'light' ? CLASS_ACCENT_LIGHT : CLASS_ACCENT)[c];
                    const active = typeFilter.has(c);
                    return (
                      <button
                        key={c}
                        onClick={() => toggleType(c)}
                        title={`Filter by ${CLASS_NAME[c]}`}
                        className="inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 transition-all"
                        style={{
                          backgroundColor: active ? `${accent}22` : 'transparent',
                          boxShadow: active ? `inset 0 0 0 1px ${accent}88` : 'none',
                          opacity: typeFilter.size > 0 && !active ? 0.4 : 1,
                        }}
                      >
                        <span
                          className="inline-flex h-4 items-center justify-center rounded text-[8.5px] font-bold tracking-wide"
                          style={{ backgroundColor: `${accent}18`, color: accent, boxShadow: `inset 0 0 0 1px ${accent}44`, padding: '0 4px' }}
                        >
                          {CLASS_LABEL[c]}
                        </span>
                        <span className="text-zinc-500 dark:text-gray-500">{n}</span>
                      </button>
                    );
                  })}
                </div>
              )}
              {(methodFilter.size > 0 || statusFilter.size > 0 || typeFilter.size > 0) && (
                <button onClick={clearFacets} className="whitespace-nowrap rounded px-1.5 py-0.5 text-cyan-600 hover:bg-zinc-200 hover:text-cyan-700 dark:text-cyan-400 dark:hover:bg-gray-700 dark:hover:text-cyan-200" title="Clear method, status and type filters">
                  ✕ clear
                </button>
              )}
            </div>
          )}
          <div className="flex items-center gap-2 border-b border-zinc-200 px-2 py-0.5 text-[10px] uppercase tracking-wide text-zinc-500 dark:border-gray-800 dark:text-gray-500">
            <span className="w-[92px] shrink-0">time</span>
            <span className="w-10 shrink-0">method</span><span className="w-10 shrink-0">status</span>
            <span className="min-w-0 flex-1">host + path</span><span className="w-16 shrink-0">type</span>
            <span className="w-14 shrink-0 text-right">size</span><span className="w-28 shrink-0">sha1</span>
            <span className="w-24 shrink-0">source IP</span>
          </div>
          <div ref={listRef} onScroll={(e) => { const el = e.currentTarget; stickRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24; }} className="flex-1 overflow-y-auto">
            {visible.length === 0 && <div className="px-2 py-3 text-zinc-500 dark:text-gray-500">{entries.length === 0 ? 'No requests captured yet — browse the site.' : 'No rows match the filter.'}</div>}
            {visible.map(({ e, host, path, flags }, i) => {
              const flagged = Object.entries(flags).filter(([, v]) => v).map(([k]) => FLAG_LABEL[k as keyof NetFlags]);
              const rowKey = `${e.id}#${i}`;
              const open = openKey === rowKey;
              const similar = !open && openId !== null && e.id === openId;
              return (
                <div key={rowKey}>
                  <div
                    onClick={() => setOpenKey(open ? null : rowKey)}
                    className={`group flex cursor-pointer items-center gap-2 px-2 py-0.5 hover:bg-zinc-200/70 dark:hover:bg-gray-800/70 ${open ? 'bg-zinc-200/90 dark:bg-gray-800/90' : similar ? 'bg-cyan-500/10' : flagged.length ? 'bg-amber-500/5' : ''}`}
                    title={e.url + (e.e ? ` — ${e.e}` : '') + (similar ? ' — same request as the open row' : '')}
                  >
                    <span className="w-[92px] shrink-0 text-zinc-500 dark:text-gray-500">{fmtTime(e.ts)}</span>
                    <span className="w-10 shrink-0 text-blue-600 dark:text-blue-400">{e.m || 'GET'}</span>
                    <span className={`w-10 shrink-0 ${statusStyle(e.s, !!e.f)}`}>{e.f ? 'ERR' : e.s || '…'}</span>
                    <span className="min-w-0 flex-1 truncate select-text">
                      <span className="text-zinc-800 dark:text-gray-300">{host}</span><span className="text-zinc-500 dark:text-gray-500">{path}</span>
                      {flagged.length > 0 && <span className="ml-2 text-amber-700 dark:text-amber-300">⚠ {flagged.join(' · ')}</span>}
                    </span>
                    <span className="w-16 shrink-0 truncate text-zinc-400 dark:text-gray-500">{e.t || '—'}</span>
                    <span className="w-14 shrink-0 text-right text-zinc-600 dark:text-gray-400">{fmtSize(e.b)}</span>
                    {e.h ? (
                      <button
                        onClick={(ev) => { ev.stopPropagation(); copyText(e.h as string, `${rowKey}:sha1`); }}
                        className="w-28 shrink-0 truncate text-left font-mono text-[10px] text-zinc-500 hover:text-cyan-700 dark:text-gray-500 dark:hover:text-cyan-200"
                        title={`sha1 ${e.h} — click to copy`}
                      >
                        {copiedTag === `${rowKey}:sha1` ? '✓ copied' : shortHash(e.h)}
                      </button>
                    ) : (
                      <span className="w-28 shrink-0 truncate font-mono text-[10px] text-zinc-300 dark:text-gray-600" title="no hash (not hashable, too large, still hashing, or capture off)">—</span>
                    )}
                    <span className="w-24 shrink-0 truncate text-zinc-400 dark:text-gray-500">{e.ip || '—'}</span>
                    <button
                      onClick={(ev) => { ev.stopPropagation(); copyText(e.url, rowKey); }}
                      className="hidden shrink-0 rounded px-1 text-zinc-400 hover:text-cyan-700 group-hover:block dark:text-gray-400 dark:hover:text-cyan-200"
                      title="Copy URL"
                    >
                      {copiedTag === rowKey ? '✓' : '⧉'}
                    </button>
                  </div>
                  {open && (
                    <div className="border-b border-zinc-200 bg-zinc-100/90 px-3 py-2 dark:border-gray-800 dark:bg-gray-900/90">
                      <div className="break-all select-text text-zinc-900 dark:text-gray-200">{e.url}</div>
                      <div className="mt-1 flex flex-wrap gap-x-3 gap-y-0.5 text-[10px] text-zinc-500 dark:text-gray-500">
                        <span>time {fmtTime(e.ts)}</span>
                        <span>type {e.t || '—'}</span>
                        <span>size {fmtSize(e.b)}</span>
                        <span>ip {e.ip || '—'}</span>
                        {e.e && <span className="text-red-600 dark:text-red-400">error {e.e}</span>}
                      </div>
                      {e.h && (
                        <div className="mt-1 flex items-center gap-2 text-[10px]">
                          <span className="text-zinc-500 dark:text-gray-500">sha1</span>
                          <code className="select-text break-all font-mono text-zinc-700 dark:text-gray-300">{e.h}</code>
                        </div>
                      )}
                      <div className="mt-1.5 flex gap-2">
                        <button onClick={() => copyText(e.url, 'detail-url')} className="rounded border border-zinc-300 px-1.5 py-0.5 text-[10px] text-cyan-600 hover:bg-zinc-200 dark:border-gray-700 dark:text-cyan-400 dark:hover:bg-gray-800">
                          {copiedTag === 'detail-url' ? '✓ copied' : 'Copy URL'}
                        </button>
                        {e.h && (
                          <button onClick={() => copyText(e.h as string, 'detail-sha1')} className="rounded border border-zinc-300 px-1.5 py-0.5 text-[10px] text-cyan-600 hover:bg-zinc-200 dark:border-gray-700 dark:text-cyan-400 dark:hover:bg-gray-800">
                            {copiedTag === 'detail-sha1' ? '✓ copied' : 'Copy SHA-1'}
                          </button>
                        )}
                        <button
                          onClick={() => copyText(JSON.stringify({ time: e.ts ? new Date(e.ts).toISOString() : undefined, method: e.m || 'GET', url: e.url, status: e.s, type: e.t, size: e.b, ip: e.ip, sha1: e.h || '', failed: !!e.f, error: e.e || undefined }, null, 2), 'detail-json')}
                          className="rounded border border-zinc-300 px-1.5 py-0.5 text-[10px] text-cyan-600 hover:bg-zinc-200 dark:border-gray-700 dark:text-cyan-400 dark:hover:bg-gray-800"
                        >
                          {copiedTag === 'detail-json' ? '✓ copied' : 'Copy JSON'}
                        </button>
                      </div>
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        </>
      )}
      {tab === 'graph' && (
        <div className="flex-1 overflow-hidden">
          <ResourceGraph
            entries={graphEntries}
            originHost={graphOriginHost(tabs, tabFilter)}
            onSelect={(sel) => {
              if (sel) setFilter(sel);
            }}
          />
        </div>
      )}
      {tab === 'domains' && (
        <>
          <div className="flex items-center gap-2 border-b border-zinc-200 px-2 py-0.5 text-[10px] uppercase tracking-wide text-zinc-500 dark:border-gray-800 dark:text-gray-500">
            <span className="flex-1">domain</span>
            <span className="w-10 text-right">reqs</span>
            <span className="w-16 text-right">total</span>
            <span className="w-28">IP addresses</span>
          </div>
          <div className="flex-1 overflow-y-auto">
            {domainRows.length === 0 && <div className="px-2 py-3 text-zinc-500 dark:text-gray-500">No domains captured yet.</div>}
            {domainRows.map((d) => {
              const rowKey = 'domain-' + d.host;
              const open = domainOpenKey === rowKey;
              const flaggedLabels = Object.entries(d.flags).filter(([, v]) => v).map(([k]) => FLAG_LABEL[k as keyof NetFlags]);
              return (
                <div key={rowKey}>
                  <div
                    onClick={() => setDomainOpenKey(open ? null : rowKey)}
                    className={`group flex cursor-pointer items-center gap-2 px-2 py-0.5 hover:bg-zinc-200/70 dark:hover:bg-gray-800/70 ${open ? 'bg-zinc-200/90 dark:bg-gray-800/90' : ''} ${d.flagged ? 'bg-amber-500/5' : ''}`}
                  >
                    <span className="min-w-0 flex-1 truncate select-text">
                      <span className={`${d.flags.ip ? 'text-cyan-700 dark:text-cyan-300' : 'text-zinc-800 dark:text-gray-300'}`}>{d.host}</span>
                      {flaggedLabels.length > 0 && <span className="ml-2 text-amber-700 dark:text-amber-300">⚠ {flaggedLabels.join(' · ')}</span>}
                    </span>
                    <span className="w-10 shrink-0 text-right text-zinc-600 dark:text-gray-400">{d.count}</span>
                    <span className="w-16 shrink-0 text-right text-zinc-600 dark:text-gray-400">{fmtSize(d.bytes)}</span>
                    <span className="w-28 shrink-0 truncate text-zinc-500 dark:text-gray-500">{d.ips.size > 0 ? Array.from(d.ips).join(', ') : '—'}</span>
                    <button
                      onClick={(ev) => { ev.stopPropagation(); copyText(d.host + (d.ips.size > 0 ? '\t' + Array.from(d.ips).join(',') : ''), rowKey); }}
                      className="hidden shrink-0 rounded px-1 text-zinc-400 hover:text-cyan-700 group-hover:block dark:text-gray-400 dark:hover:text-cyan-200"
                      title="Copy domain + IPs"
                    >
                      {copiedTag === rowKey ? '✓' : '⧉'}
                    </button>
                  </div>
                  {open && (
                    <div className="border-b border-zinc-200 bg-zinc-100/90 px-2 py-1 dark:border-gray-800 dark:bg-gray-900/90">
                      {d.entries.slice(-30).reverse().map(({ e }) => (
                        <div key={e.id + e.url} className="flex items-center gap-2 py-0.5 text-[11px]">
                          <span className="w-[92px] shrink-0 text-zinc-500 dark:text-gray-500">{fmtTime(e.ts)}</span>
                          <span className="w-10 shrink-0 text-blue-600 dark:text-blue-400">{e.m || 'GET'}</span>
                          <span className={`w-10 shrink-0 ${statusStyle(e.s, !!e.f)}`}>{e.f ? 'ERR' : e.s || '…'}</span>
                          <span className="flex-1 truncate text-zinc-600 dark:text-gray-400">{e.url.replace(/^https?:\/\/[^/]+/, '')}</span>
                          <span className="w-14 shrink-0 text-right text-zinc-500 dark:text-gray-500">{fmtSize(e.b)}</span>
                        </div>
                      ))}
                      {d.entries.length > 30 && (
                        <div className="pt-0.5 text-[10px] text-zinc-400 dark:text-gray-600">… showing last 30 of {d.entries.length} requests</div>
                      )}
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        </>
      )}
      {tab === 'console' && (
        <div className="flex-1 overflow-y-auto p-1">
          {consoleFiltered.length === 0 && <div className="px-2 py-3 text-zinc-500 dark:text-gray-500">{consoleEntries.length === 0 ? 'No console output yet.' : 'No console output for this tab.'}</div>}
          {consoleFiltered.map((c, i) => (
            <div key={i} className={`border-b border-zinc-200/60 px-2 py-0.5 dark:border-gray-800/50 ${c.level === 'error' || c.level === 'exception' ? 'bg-red-500/5 text-red-600 dark:text-red-400' : c.level === 'warning' ? 'text-amber-600 dark:text-yellow-300' : 'text-zinc-700 dark:text-gray-300'}`}>
              <span className="mr-1.5 text-[10px] text-zinc-400 dark:text-gray-600">{fmtTime(c.ts)}</span>
              <span className="mr-1.5 text-[10px] text-zinc-400 dark:text-gray-600">{c.level}</span>
              <span className="break-all">{c.text}</span>
            </div>
          ))}
        </div>
      )}
      {tab === 'cookies' && (
        <div className="flex-1 overflow-auto">
          {cookieEntries.length === 0 && (
            <div className="px-2 py-3 text-zinc-500 dark:text-gray-500">No cookies captured yet.</div>
          )}
          {cookieEntries.length > 0 && (
            <table className="w-full min-w-[720px] border-collapse text-left text-[11px]">
              <thead className="sticky top-0 z-10 bg-zinc-100 text-[10px] uppercase tracking-wide text-zinc-500 dark:bg-gray-950 dark:text-gray-500">
                <tr className="border-b border-zinc-200 dark:border-gray-800">
                  <th className="px-2 py-1 font-medium">Name</th>
                  <th className="px-2 py-1 font-medium">Value</th>
                  <th className="px-2 py-1 font-medium">Domain</th>
                  <th className="px-2 py-1 font-medium">Path</th>
                  <th className="px-2 py-1 font-medium">Expires</th>
                  <th className="px-2 py-1 font-medium">SameSite</th>
                  <th className="px-2 py-1 font-medium">Flags</th>
                </tr>
              </thead>
              <tbody>
                {cookieEntries.map((c) => (
                  <tr key={`${c.domain}\0${c.name}`} className="border-b border-zinc-200/60 text-zinc-700 hover:bg-zinc-200/40 dark:border-gray-800/40 dark:text-gray-300 dark:hover:bg-gray-800/30">
                    <td className="max-w-[120px] truncate px-2 py-1 font-mono text-cyan-700 dark:text-cyan-200/90" title={c.name}>{c.name}</td>
                    <td className="max-w-[200px] truncate px-2 py-1 font-mono text-zinc-800 dark:text-gray-200" title={c.value || '—'}>
                      {c.value || <span className="text-zinc-400 dark:text-gray-600">—</span>}
                    </td>
                    <td className="max-w-[140px] truncate px-2 py-1 text-zinc-600 dark:text-gray-400" title={cookieDomainLabel(c.domain)}>
                      {cookieDomainLabel(c.domain)}
                    </td>
                    <td className="max-w-[80px] truncate px-2 py-1 font-mono text-zinc-600 dark:text-gray-400" title={c.path || '/'}>
                      {c.path || '/'}
                    </td>
                    <td className="max-w-[140px] truncate px-2 py-1 text-zinc-600 dark:text-gray-400" title={cookieExpiryLabel(c)}>
                      {cookieExpiryLabel(c)}
                    </td>
                    <td className="px-2 py-1 text-zinc-600 dark:text-gray-400">{c.sameSite || '—'}</td>
                    <td className="px-2 py-1">
                      <span className="flex flex-wrap gap-1">
                        {c.httpOnly && <span className="rounded bg-blue-500/15 px-1 text-[10px] text-blue-700 dark:text-blue-300">HttpOnly</span>}
                        {c.secure && <span className="rounded bg-emerald-500/15 px-1 text-[10px] text-emerald-700 dark:text-emerald-300">Secure</span>}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </div>
  );
}
