import { describe, expect, it } from 'vitest';

import type { NetEntry } from '../components/IsolaNetworkPanel';
import { mergeNetEntryInPlace } from './netEntries';

const entry = (over: Partial<NetEntry> = {}): NetEntry => ({
  id: 'target:1',
  url: 'https://example.com/app.js',
  m: 'GET',
  s: 0,
  t: 'Script',
  ...over,
});

const merge = (list: NetEntry[], e: NetEntry): NetEntry[] => {
  mergeNetEntryInPlace(list, e);
  return list;
};

describe('mergeNetEntryInPlace', () => {
  it('appends the first snapshot of a request', () => {
    const list = merge([], entry());
    expect(list).toHaveLength(1);
    expect(list[0].id).toBe('target:1');
  });

  it('updates the existing row instead of appending a duplicate', () => {
    const requestTime = merge([], entry({ url: 'https://a/b.js', t: 'Fetch' }));
    const received = merge(requestTime, entry({ s: 200, ip: '203.0.113.7' }));
    const finished = merge(received, entry({ s: 200, ip: '203.0.113.7', b: 1234 }));

    expect(finished).toHaveLength(1);
    expect(finished[0]).toMatchObject({ s: 200, ip: '203.0.113.7', b: 1234 });
  });

  it('fills in the SHA-1 on the row captured before the body was hashed', () => {
    const requestTime = merge([], entry({ t: 'Fetch', b: 0 }));
    const hashed = merge(requestTime, entry({ t: 'Fetch', s: 200, b: 1234, h: 'abcdef0123456789abcdef0123456789abcdef01' }));

    expect(hashed).toHaveLength(1);
    expect(hashed[0].s).toBe(200);
    expect(hashed[0].b).toBe(1234);
    expect(hashed[0].h).toBe('abcdef0123456789abcdef0123456789abcdef01');
  });

  it('keeps a known hash when a later snapshot omits it', () => {
    const hashed = merge([], entry({ h: 'deadbeef' }));
    const replay = merge(hashed, entry({ s: 200, b: 10 }));
    expect(replay[0].h).toBe('deadbeef');
  });

  it('leaves other requests in place and in order', () => {
    const list = merge([], entry({ id: 'a' }));
    merge(list, entry({ id: 'b' }));
    merge(list, entry({ id: 'a', s: 304 }));

    expect(list.map((e) => e.id)).toEqual(['a', 'b']);
    expect(list[0].s).toBe(304);
  });
});
