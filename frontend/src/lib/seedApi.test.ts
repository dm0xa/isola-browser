import { afterEach, describe, expect, it, vi } from 'vitest';

vi.mock('./api', () => ({
  api: { post: vi.fn() },
}));

import { api } from './api';
import { applySeed, buildSeedPayload, parseCookiePairs, seedErrorText } from './seedApi';

afterEach(() => {
  vi.clearAllMocks();
});

describe('parseCookiePairs', () => {
  it('parses a pasted Cookie header value', () => {
    expect(parseCookiePairs('Cookie: sid=abc; theme=dark')).toEqual([
      { name: 'sid', value: 'abc' },
      { name: 'theme', value: 'dark' },
    ]);
  });

  it('keeps "=" inside a value, respects quotes, and strips them', () => {
    expect(parseCookiePairs('token=a=b==; quoted="x;y"')).toEqual([
      { name: 'token', value: 'a=b==' },
      { name: 'quoted', value: 'x;y' },
    ]);
  });

  it('accepts newlines as separators', () => {
    expect(parseCookiePairs('a=1\nb=2')).toEqual([
      { name: 'a', value: '1' },
      { name: 'b', value: '2' },
    ]);
  });

  it('keeps only the last value for a repeated name', () => {
    expect(parseCookiePairs('a=1; a=2')).toEqual([{ name: 'a', value: '2' }]);
  });

  it('rejects a pair without a name', () => {
    expect(() => parseCookiePairs('=oops')).toThrow(/not name=value/);
    expect(() => parseCookiePairs('justtext')).toThrow(/not name=value/);
  });

  it('rejects Set-Cookie attribute pairs instead of seeding them', () => {
    expect(() => parseCookiePairs('a=1; Path=/')).toThrow(/name=value pairs/);
    expect(() => parseCookiePairs('a=1; expires=Wed, 21 Oct 2026 07:28:00 GMT')).toThrow(/name=value pairs/);
  });

  it('returns nothing for empty input', () => {
    expect(parseCookiePairs('   ')).toEqual([]);
  });
});

describe('buildSeedPayload', () => {
  it('wraps cookie pairs, forcing reload only when asked', () => {
    expect(buildSeedPayload('a=1; b=2', false)).toEqual({ cookies: [{ name: 'a', value: '1' }, { name: 'b', value: '2' }] });
    expect(buildSeedPayload('a=1', true)).toEqual({ cookies: [{ name: 'a', value: '1' }], reload: true });
  });

  it('passes a JSON payload through untouched (it owns its reload field)', () => {
    const body = '{"headers":[{"name":"X-Api-Key","value":"k"}],"reload":false}';
    expect(buildSeedPayload(body, true)).toEqual({ headers: [{ name: 'X-Api-Key', value: 'k' }], reload: false });
    expect(buildSeedPayload('{"reload":true}', false)).toEqual({ reload: true });
  });

  it('surfaces JSON syntax errors as input errors', () => {
    expect(() => buildSeedPayload('{"cookies":[}', false)).toThrow(/Invalid JSON/);
    expect(() => buildSeedPayload('[1,2]', false)).toThrow(/must be an object/);
    expect(() => buildSeedPayload('   ', false)).toThrow(/Nothing to send/);
  });
});

describe('applySeed', () => {
  it('posts the payload with the owner session token', async () => {
    const post = vi.mocked(api.post);
    post.mockResolvedValue({ data: { cookies: 1, headers: 0, storage: 0, reloaded: true } });

    const res = await applySeed('sid-1', { cookies: [{ name: 'a', value: 'b' }] }, 'tok-1');

    expect(res.reloaded).toBe(true);
    expect(post).toHaveBeenCalledWith(
      '/api/sessions/sid-1/seed',
      { cookies: [{ name: 'a', value: 'b' }] },
      { headers: { 'X-Session-Token': 'tok-1' } },
    );
  });

  it('omits the header when there is no token', async () => {
    const post = vi.mocked(api.post);
    post.mockResolvedValue({ data: { cookies: 1, headers: 0, storage: 0, reloaded: false } });
    await applySeed('sid-1', { cookies: [{ name: 'a', value: 'b' }] }, '');
    expect(post).toHaveBeenCalledWith('/api/sessions/sid-1/seed', expect.anything(), undefined);
  });
});

describe('seedErrorText', () => {
  it('prefers the API message, then the status, then the local message', () => {
    expect(seedErrorText({ response: { status: 409, data: { error: { code: 'seed_origin_mismatch', message: 'Page is on https://x' } } } })).toBe('Page is on https://x');
    expect(seedErrorText({ response: { status: 502, data: null } })).toBe('Request failed (502)');
    expect(seedErrorText(new Error('boom'))).toBe('boom');
    expect(seedErrorText(null)).toBe('Could not apply custom data');
  });
});
