import { describe, expect, it } from 'vitest';

import type { CaptureItem } from './captureApi';
import { filterCaptureFiles, groupCaptureFiles, truncatePathLeft } from './captureFormat';

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
