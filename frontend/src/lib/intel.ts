import { api } from './api';

// Wire types for the analysis the streamer produces. The viewer renders these
// as-is: posture, tracker labels and probe results are computed in Go so the
// panel and the MCP tools can never disagree.

export interface PostureCheck {
  name: string;
  present: boolean;
  value?: string;
  note?: string;
}

export interface PostureReport {
  score: number;
  total: number;
  grade: string;
  /** CDP securityState: secure | info | neutral | insecure | insecure-broken. */
  tls_state?: string;
  /** Explains a grade cap or a transport warning. */
  note?: string;
  checks: PostureCheck[];
}

export interface DocTLS {
  protocol?: string;
  cipher?: string;
  issuer?: string;
  subject?: string;
  valid_from?: number;
  valid_to?: number;
  sans?: string[];
  /** CDP verdict on the connection; "insecure" means Chromium ignored a cert error. */
  security_state?: string;
}

/** One document response's headers, as broadcast on the `headers` message. */
export interface DocumentHeaders {
  id: string;
  url: string;
  s: number;
  tb?: number;
  hd: Record<string, string>;
  pst?: PostureReport;
  tls?: DocTLS;
  ts?: number;
}

export interface AsnInfo {
  ip: string;
  asn?: number;
  prefix?: string;
  country?: string;
  registry?: string;
  org?: string;
}

export interface IntelHost {
  host?: string;
  ips?: string[];
  cname?: string;
  ptr?: Record<string, string>;
  asn?: AsnInfo[];
  resolve_error?: string;
  private?: boolean;
}

export interface ProbeResult {
  check: string;
  url?: string;
  status: 'ok' | 'empty' | 'unreachable' | 'error' | 'skipped';
  http_status?: number;
  error?: string;
  data?: unknown;
  cached?: boolean;
}

export interface IntelResponse {
  host?: string;
  dns?: Record<string, IntelHost>;
  probes?: Record<string, ProbeResult>;
  generated_at?: number;
}

export interface IntelQuery {
  host?: string;
  ips?: string;
  checks?: string;
  mode?: 'dns' | 'probe' | 'all';
}

/**
 * Fetches recon for a session. The proxy validates every parameter, so this is
 * only ever called from an explicit user action.
 */
export async function fetchSessionIntel(
  sessionId: string,
  token: string,
  query: IntelQuery,
): Promise<IntelResponse> {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value) params.set(key, value);
  }
  const res = await api.get<IntelResponse>(
    `/api/sessions/${sessionId}/intel?${params.toString()}`,
    { headers: { 'X-Session-Token': token } },
  );
  return res.data;
}

export type IntelFetch = (query: IntelQuery) => Promise<IntelResponse>;

/** Tailwind classes for a posture grade. */
export function postureGradeClass(grade: string): string {
  switch (grade) {
    case 'A':
    case 'B':
      return 'bg-emerald-500/15 text-emerald-700 dark:text-emerald-300';
    case 'C':
      return 'bg-amber-500/15 text-amber-700 dark:text-amber-300';
    default:
      return 'bg-red-500/15 text-red-700 dark:text-red-300';
  }
}

/** Tailwind classes for a probe result status. */
export function probeStatusClass(status: string): string {
  switch (status) {
    case 'ok':
      return 'text-emerald-700 dark:text-emerald-300';
    case 'empty':
      return 'text-zinc-500 dark:text-gray-500';
    case 'unreachable':
      return 'text-red-600 dark:text-red-400';
    default:
      return 'text-amber-700 dark:text-amber-300';
  }
}

/** Compact, copyable rendering of a probe's payload. */
export function formatProbeData(data: unknown): string {
  if (data === undefined || data === null) return '—';
  if (typeof data === 'string') return data;
  try {
    return JSON.stringify(data, null, 2);
  } catch {
    return String(data);
  }
}

/** The certificates' validity window as a short label. */
export function tlsValidity(tls: DocTLS): string {
  const fmt = (v?: number) => (v ? new Date(v * 1000).toISOString().slice(0, 10) : '?');
  if (!tls.valid_from && !tls.valid_to) return '';
  return `${fmt(tls.valid_from)} → ${fmt(tls.valid_to)}`;
}

/**
 * What Chromium concluded about the connection. "insecure" is the case the
 * certificate fields alone hide: the certificate was presented, but the chain
 * was not trusted (the sandbox runs with certificate errors ignored).
 */
export function tlsStateLabel(state?: string): string {
  switch (state) {
    case 'secure':
      return 'trusted';
    case 'info':
      return 'trusted · warnings';
    case 'neutral':
      return 'no TLS';
    case 'insecure':
      return 'untrusted cert';
    case 'insecure-broken':
      return 'broken TLS';
    default:
      return '';
  }
}

/** Tailwind classes for a transport state; empty for anything healthy. */
export function tlsStateClass(state?: string): string {
  switch (state) {
    case 'insecure':
    case 'insecure-broken':
      return 'bg-red-500/15 text-red-700 dark:text-red-300';
    case 'neutral':
    case 'info':
      return 'bg-amber-500/15 text-amber-700 dark:text-amber-300';
    default:
      return '';
  }
}
