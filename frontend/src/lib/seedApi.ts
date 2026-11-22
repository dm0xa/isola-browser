// Live custom data injection into a running session's page.
//
// Owner credential only: `/api/sessions/{id}/seed` requires the same
// `X-Session-Token` the viewer uses for session polls. A share/view-only
// viewer has no token and therefore cannot inject.
//
// The payload mirrors the streamer's /seed contract; values are secrets and
// must never be put in a URL, a log line, or local storage.
import { api } from './api';
import { apiErrorText } from './apiError';

const SESSION_TOKEN_HEADER = 'X-Session-Token';

export interface SeedCookie {
  name: string;
  value: string;
  /** Host the cookie belongs to. Omit to use the session target's host. */
  domain?: string;
  /** Full URL alternative to domain; the streamer derives the host from it. */
  url?: string;
  path?: string;
  same_site?: 'strict' | 'lax' | 'none';
  secure?: boolean;
  http_only?: boolean;
  /** Seconds since epoch; omit for a session cookie. */
  expires?: number;
}

export interface SeedHeader {
  name: string;
  value: string;
  /** Optional host allowlist; omit to apply on whichever host the page is on. */
  domains?: string[];
}

export interface SeedStorage {
  kind?: 'local' | 'session';
  /** Omit to use the origin the page is on right now. */
  origin?: string;
  items: Record<string, string>;
}

export interface SessionSeed {
  cookies?: SeedCookie[];
  headers?: SeedHeader[];
  storage?: SeedStorage[];
  /** Reload after applying so the change takes effect on the current page. */
  reload?: boolean;
}

export interface SeedResult {
  cookies: number;
  headers: number;
  storage: number;
  reloaded: boolean;
}

export async function applySeed(
  sessionId: string,
  seed: SessionSeed,
  token: string,
): Promise<SeedResult> {
  const res = await api.post(
    `/api/sessions/${sessionId}/seed`,
    seed,
    token ? { headers: { [SESSION_TOKEN_HEADER]: token } } : undefined,
  );
  return res.data as SeedResult;
}

/** Human-readable error for a failed seed call (inline in the dialog). */
export function seedErrorText(err: unknown): string {
  return apiErrorText(err, 'Could not apply custom data');
}

/**
 * parseCookiePairs turns a pasted `Cookie:` header value — `a=1; b=2` — into
 * cookie entries. Values may contain `=` (base64), so only the first one
 * splits. A leading `Cookie:` prefix is accepted because that is what people
 * copy out of devtools.
 */
export function parseCookiePairs(input: string): SeedCookie[] {
  const text = input.trim().replace(/^cookie:\s*/i, '');
  if (!text) return [];
  const out: SeedCookie[] = [];
  for (const raw of splitCookiePairs(text)) {
    const pair = raw.trim();
    if (!pair) continue;
    const eq = pair.indexOf('=');
    if (eq <= 0) {
      throw new Error(`"${pair.slice(0, 40)}" is not name=value`);
    }
    const name = pair.slice(0, eq).trim();
    let value = pair.slice(eq + 1).trim();
    if (!name) throw new Error(`"${pair.slice(0, 40)}" has no cookie name`);
    // A pasted Set-Cookie carries attributes (`Path=/`, `expires=…`) that look
    // like pairs. Silently seeding them would put junk in the jar; the JSON
    // form is the way to set attributes.
    if (COOKIE_ATTRIBUTES.test(name)) {
      throw new Error('Paste only name=value pairs; use the JSON form for cookie attributes');
    }
    if (value.length >= 2 && value.startsWith('"') && value.endsWith('"')) {
      value = value.slice(1, -1);
    }
    // A repeated name is meaningless in a Cookie header; keep the last value
    // instead of sending Chromium a batch whose duplicate it may reject whole.
    const existing = out.findIndex((c) => c.name === name);
    if (existing >= 0) {
      out[existing] = { name, value };
    } else {
      out.push({ name, value });
    }
  }
  return out;
}

const COOKIE_ATTRIBUTES = /^(path|domain|expires|max-age|samesite|secure|httponly|priority|partitioned)$/i;

/** Splits on `;` and newlines, but never inside a quoted value. */
function splitCookiePairs(text: string): string[] {
  const parts: string[] = [];
  let current = '';
  let quoted = false;
  for (const ch of text) {
    if (ch === '"') {
      quoted = !quoted;
      current += ch;
      continue;
    }
    if (!quoted && (ch === ';' || ch === '\n')) {
      parts.push(current);
      current = '';
      continue;
    }
    current += ch;
  }
  parts.push(current);
  return parts;
}

/**
 * buildSeedPayload accepts either a JSON object (the full request body) or a
 * pasted cookie header value. `reload` is only forced on for the cookie form;
 * an explicit JSON payload owns its own `reload` field.
 */
export function buildSeedPayload(text: string, reload: boolean): SessionSeed {
  const trimmed = text.trim();
  if (!trimmed) throw new Error('Nothing to send');
  if (trimmed.startsWith('{') || trimmed.startsWith('[')) {
    let parsed: unknown;
    try {
      parsed = JSON.parse(trimmed);
    } catch (err) {
      throw new Error(`Invalid JSON: ${err instanceof Error ? err.message : 'parse error'}`, { cause: err });
    }
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      throw new Error('The JSON payload must be an object');
    }
    // A JSON body owns its own `reload`: the dialog disables the checkbox for
    // this form, so overriding it here would make what the user sees disagree
    // with what is sent.
    return parsed as SessionSeed;
  }
  const cookies = parseCookiePairs(trimmed);
  if (cookies.length === 0) throw new Error('No cookies found');
  return reload ? { cookies, reload: true } : { cookies };
}
