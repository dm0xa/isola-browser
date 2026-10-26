import type { CookieEntry } from '../components/IsolaNetworkPanel';

export function cookieKey(c: Pick<CookieEntry, 'domain' | 'name'>): string {
  return `${c.domain}\0${c.name}`;
}

export function cookieFromWire(msg: Record<string, unknown>): CookieEntry | null {
  if (typeof msg.n !== 'string' || msg.n === '') return null;
  return {
    domain: typeof msg.d === 'string' ? msg.d : '',
    name: msg.n,
    value: typeof msg.v === 'string' ? msg.v : '',
    path: typeof msg.p === 'string' ? msg.p : '',
    sameSite: typeof msg.ss === 'string' ? msg.ss : '',
    expires: typeof msg.exp === 'string' ? msg.exp : '',
    maxAge: typeof msg.ma === 'number' ? msg.ma : undefined,
    httpOnly: !!msg.h,
    secure: !!msg.s,
    fp: typeof msg.fp === 'string' && msg.fp !== '' ? msg.fp : undefined,
  };
}

export function mergeCookieEntry(list: CookieEntry[], entry: CookieEntry): CookieEntry[] {
  const key = cookieKey(entry);
  const idx = list.findIndex((c) => cookieKey(c) === key);
  if (idx >= 0) {
    const next = [...list];
    next[idx] = { ...next[idx], ...entry };
    return next;
  }
  return [...list, entry];
}

export function cookieExpiryLabel(c: CookieEntry): string {
  if (c.expires) return c.expires;
  if (c.maxAge != null && c.maxAge > 0) return `${c.maxAge}s`;
  return 'Session';
}

export function cookieDomainLabel(domain: string): string {
  return domain || '(host-only)';
}
