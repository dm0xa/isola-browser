import { describe, expect, it } from 'vitest';

import { cookieFromWire, cookieExpiryLabel, mergeCookieEntry } from './cookieEntries';

describe('cookieEntries', () => {
  it('parses wire messages and merges by domain+name', () => {
    const a = cookieFromWire({
      n: 'PHPSESSID',
      d: '.example.com',
      v: 'abc123',
      p: '/',
      ss: 'Lax',
      h: true,
      s: true,
    });
    expect(a?.value).toBe('abc123');
    const list = mergeCookieEntry([], a!);
    const b = cookieFromWire({ n: 'PHPSESSID', d: '.example.com', v: 'rotated' });
    const merged = mergeCookieEntry(list, b!);
    expect(merged).toHaveLength(1);
    expect(merged[0].value).toBe('rotated');
    expect(cookieExpiryLabel(merged[0])).toBe('Session');
    expect(cookieExpiryLabel({ ...merged[0], maxAge: 120 })).toBe('120s');
  });

  it('carries the product-family fingerprint when the streamer sent one', () => {
    const withFp = cookieFromWire({ n: '_ga', d: '.example.com', fp: 'Google Analytics' });
    expect(withFp?.fp).toBe('Google Analytics');
    const withoutFp = cookieFromWire({ n: 'custom', d: '.example.com' });
    expect(withoutFp?.fp).toBeUndefined();
  });
});
