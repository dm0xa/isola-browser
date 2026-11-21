import { describe, expect, it } from 'vitest';

import type { CaptureItem } from './captureApi';
import {
  captureFileHost,
  filterCaptureFiles,
  groupCaptureFiles,
  groupCaptureFilesByHost,
  hostRelativePath,
  truncatePathLeft,
  truncatePathTail,
  truncateUrlMiddle,
} from './captureFormat';

function item(overrides: Partial<CaptureItem>): CaptureItem {
  return {
    id: 'r1',
    url: 'https://a.example/app.js',
    mime: 'text/javascript',
    size_decoded: 10,
    sha256: 'a'.repeat(64),
    state: 'captured',
    ...overrides,
  };
}

describe('truncatePathLeft', () => {
  it('leaves a path that fits untouched', () => {
    expect(truncatePathLeft('files/a.example/app.js', 48)).toBe('files/a.example/app.js');
  });

  it('cuts from the left on a segment boundary', () => {
    // 26 chars, max 12 → keep the last 11 (`/w/load.php`).
    expect(truncatePathLeft('files/a.example/w/load.php', 12)).toBe('…/w/load.php');
  });

  it('keeps the tail when the cut window holds no separator', () => {
    expect(truncatePathLeft('files/a.example/abcdefghijklmnop.js', 12)).toBe('…ijklmnop.js');
  });
});

describe('groupCaptureFiles', () => {
  it('labels the group with the first item\'s readable path and keeps versions', () => {
    const shared = 'a'.repeat(64);
    const changed = 'b'.repeat(64);
    const files = groupCaptureFiles([
      item({ id: 'r1', sha256: shared, file: 'files/a.example/app.js', ts_saved: 100 }),
      item({
        id: 'r2',
        sha256: shared,
        url: 'https://cdn.example/app.js',
        file: 'files/cdn.example/app.js',
        ts_saved: 200,
      }),
      item({
        id: 'r3',
        sha256: changed,
        url: 'https://a.example/app.js',
        file: 'files/a.example/app~bbbbbbbbbbbb.js',
        ts_saved: 300,
      }),
    ]);

    const sharedRow = files.find((f) => f.sha256 === shared)!;
    // Same bytes, two names: one row, first path wins as the label.
    expect(sharedRow.file).toBe('files/a.example/app.js');
    expect(sharedRow.urls).toHaveLength(2);
    const changedRow = files.find((f) => f.sha256 === changed)!;
    expect(changedRow.version).toBe(2);
    expect(changedRow.versionCount).toBe(2);
  });

  it('filters on the readable path', () => {
    const files = groupCaptureFiles([
      item({ sha256: 'c'.repeat(64), file: 'files/a.example/w/load.php', url: 'https://a.example/w/load.php' }),
    ]);
    expect(filterCaptureFiles(files, 'files/a.example')).toHaveLength(1);
    expect(filterCaptureFiles(files, 'nothing-here')).toHaveLength(0);
  });
});

describe('truncateUrlMiddle', () => {
  it('leaves a short URL untouched', () => {
    expect(truncateUrlMiddle('https://a.example/app.js', 64)).toBe('https://a.example/app.js');
  });

  it('keeps both ends when it cuts', () => {
    const url = `https://a.example/${'deep/'.repeat(30)}load.php?ver=1234567890`;
    const cut = truncateUrlMiddle(url, 40);
    expect(cut.length).toBe(40);
    expect(cut.startsWith('https://a.example/')).toBe(true);
    expect(cut.endsWith('ver=1234567890')).toBe(true);
    expect(cut).toContain('…');
  });
});

describe('truncatePathTail', () => {
  it('leaves a short path untouched', () => {
    expect(truncatePathTail('app.js', 48)).toBe('app.js');
  });

  it('keeps the last two segments behind an ellipsis prefix', () => {
    const path = 'get-inspire-xyz/3c1d19c74beb41e2bc872f156bd30c54/grid.webp';
    expect(truncatePathTail(path, 44)).toBe('…/3c1d19c74beb41e2bc872f156bd30c54/grid.webp');
  });

  it('drops many leading segments when the path is long', () => {
    expect(truncatePathTail('a/b/c/d/grid.webp', 10)).toBe('…/d/grid.webp');
  });
});

describe('hostRelativePath', () => {
  it('strips the files/<host>/ prefix', () => {
    const [file] = groupCaptureFiles([
      item({ sha256: 'a'.repeat(64), file: 'files/a.example/w/grid.webp' }),
    ]);
    expect(hostRelativePath(file, 'a.example')).toBe('w/grid.webp');
  });

  it('keeps the full path when the host does not prefix it', () => {
    const [file] = groupCaptureFiles([
      item({ sha256: 'b'.repeat(64), file: 'files/other.example/x.js' }),
    ]);
    expect(hostRelativePath(file, 'a.example')).toBe('files/other.example/x.js');
  });
});

describe('captureFileHost', () => {
  it('reads the host folder from the readable path', () => {
    const [file] = groupCaptureFiles([
      item({ sha256: 'a'.repeat(64), file: 'files/cdn.example__8443/lib/x.js' }),
    ]);
    expect(captureFileHost(file)).toBe('cdn.example__8443');
  });

  it('falls back to the URL host, then to local', () => {
    const [legacy] = groupCaptureFiles([
      item({ sha256: 'b'.repeat(64), url: 'https://legacy.example/x.js' }),
    ]);
    expect(captureFileHost(legacy)).toBe('legacy.example');

    const [local] = groupCaptureFiles([
      item({ sha256: 'c'.repeat(64), url: 'data:text/css,body{}', mime: 'text/css' }),
    ]);
    expect(captureFileHost(local)).toBe('local');
  });
});

describe('groupCaptureFilesByHost', () => {
  const mixed = groupCaptureFiles([
    item({ id: 'r1', sha256: 'a'.repeat(64), file: 'files/b.example/a10.js', url: 'https://b.example/a10.js', size_decoded: 100, ts_saved: 300 }),
    item({ id: 'r2', sha256: 'b'.repeat(64), file: 'files/b.example/a2.js', url: 'https://b.example/a2.js', size_decoded: 300, ts_saved: 100 }),
    item({ id: 'r3', sha256: 'c'.repeat(64), file: 'files/a2.example/index.html', url: 'https://a2.example/', size_decoded: 200, ts_saved: 200 }),
  ]);

  it('splits by host with counts and unique bytes, hosts in natural order', () => {
    const groups = groupCaptureFilesByHost(mixed);
    expect(groups.map((g) => g.host)).toEqual(['a2.example', 'b.example']);
    expect(groups[1].count).toBe(2);
    expect(groups[1].bytes).toBe(400);
  });

  it('sorts naturally by path inside a host', () => {
    const [, b] = groupCaptureFilesByHost(mixed);
    expect(b.files.map((f) => f.file)).toEqual(['files/b.example/a2.js', 'files/b.example/a10.js']);
  });

  it('sorts by newest save and by size on request', () => {
    const byTime = groupCaptureFilesByHost(mixed, 'time')[1];
    expect(byTime.files.map((f) => f.file)).toEqual(['files/b.example/a10.js', 'files/b.example/a2.js']);
    const bySize = groupCaptureFilesByHost(mixed, 'size')[1];
    expect(bySize.files.map((f) => f.file)).toEqual(['files/b.example/a2.js', 'files/b.example/a10.js']);
  });

  it('filters on host and sha', () => {
    expect(filterCaptureFiles(mixed, 'a2.example')).toHaveLength(1);
    expect(filterCaptureFiles(mixed, 'b'.repeat(64))).toHaveLength(1);
    expect(filterCaptureFiles(mixed, 'b.example')).toHaveLength(2);
  });
});
