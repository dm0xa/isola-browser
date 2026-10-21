import { describe, expect, it } from 'vitest';

import type { ConsoleEntry } from '../components/IsolaNetworkPanel';
import { mergeConsoleEntry } from './consoleEntries';

const line = (over: Partial<ConsoleEntry> = {}): ConsoleEntry => ({
  level: 'log',
  text: 'hello',
  tb: 1,
  ts: 1_700_000_000_000,
  ...over,
});

describe('mergeConsoleEntry', () => {
  it('appends a line it has not seen', () => {
    const list = mergeConsoleEntry([], line());
    expect(list).toHaveLength(1);
  });

  it('replaces the tab attribution of a re-published line instead of duplicating it', () => {
    const list = mergeConsoleEntry([line({ tb: -1 })], line({ tb: 1 }));

    expect(list).toHaveLength(1);
    expect(list[0].tb).toBe(1);
  });

  it('keeps lines that differ in text or level', () => {
    const list = mergeConsoleEntry([line()], line({ text: 'other' }));
    expect(list).toHaveLength(2);

    const withError = mergeConsoleEntry([line()], line({ level: 'error' }));
    expect(withError).toHaveLength(2);
  });

  it('appends when the wire carries no timestamp to fold on', () => {
    const list = mergeConsoleEntry([line({ ts: undefined })], line({ ts: undefined }));
    expect(list).toHaveLength(2);
  });
});
