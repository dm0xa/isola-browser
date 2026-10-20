import { describe, expect, it } from 'vitest';

import type { NetEntry } from '../components/IsolaNetworkPanel';
import { mergeNetEntry } from './netEntries';

const entry = (over: Partial<NetEntry> = {}): NetEntry => ({
  id: 'target:1',
  url: 'https://example.com/app.js',
  m: 'GET',
  s: 0,
  t: 'Script',
  ...over,
});

describe('mergeNetEntry', () => {
  it('appends the first snapshot of a request', () => {
    const list = mergeNetEntry([], entry());
    expect(list).toHaveLength(1);
    expect(list[0].id).toBe('target:1');
  });

  it('updates the existing row instead of appending a duplicate', () => {
    const requestTime = mergeNetEntry([], entry({ url: 'https://a/b.js', t: 'Fetch' }));
    const received = mergeNetEntry(requestTime, entry({ s: 200, ip: '203.0.113.7' }));
    const finished = mergeNetEntry(received, entry({ s: 200, ip: '203.0.113.7', b: 1234 }));

    expect(finished).toHaveLength(1);
    expect(finished[0]).toMatchObject({ s: 200, ip: '203.0.113.7', b: 1234 });
  });

  it('fills in the SHA-1 on the row captured before the body was hashed', () => {
    const requestTime = mergeNetEntry([], entry({ t: 'Fetch', b: 0 }));
    const hashed = mergeNetEntry(requestTime, entry({ t: 'Fetch', s: 200, b: 1234, h: 'abcdef0123456789abcdef0123456789abcdef01' }));

    expect(hashed).toHaveLength(1);
    expect(hashed[0].s).toBe(200);
    expect(hashed[0].b).toBe(1234);
    expect(hashed[0].h).toBe('abcdef0123456789abcdef0123456789abcdef01');
  });

  it('keeps a known hash when a later snapshot omits it', () => {
    const hashed = mergeNetEntry([], entry({ h: 'deadbeef' }));
    const replay = mergeNetEntry(hashed, entry({ s: 200, b: 10 }));
    expect(replay[0].h).toBe('deadbeef');
  });

  it('leaves other requests in place and in order', () => {
    let list = mergeNetEntry([], entry({ id: 'a' }));
    list = mergeNetEntry(list, entry({ id: 'b' }));
    list = mergeNetEntry(list, entry({ id: 'a', s: 304 }));

    expect(list.map((e) => e.id)).toEqual(['a', 'b']);
    expect(list[0].s).toBe(304);
  });
});
