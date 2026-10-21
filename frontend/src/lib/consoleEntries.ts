import type { ConsoleEntry } from '../components/IsolaNetworkPanel';

// Console lines carry a streamer-assigned id that survives re-publishing. It
// re-sends a line when it later learns which browser tab the line belongs to
// (traffic captured before the runner reported its tab strip is folded into the
// tab that turns out to be active). Fold on that id instead of appending: a
// second append would render the corrected line twice.
//
// Older streamers omit the id. Those fall back to (ts, level, text), which is
// what the panel has always used; the id is what keeps two genuinely distinct
// lines in two tabs from collapsing when they share a millisecond.
export function mergeConsoleEntry(list: ConsoleEntry[], entry: ConsoleEntry): ConsoleEntry[] {
  const idx = entry.i !== undefined
    ? list.findIndex((e) => e.i === entry.i)
    : entry.ts === undefined
      ? -1
      : list.findIndex(
        (e) => e.ts === entry.ts && e.level === entry.level && e.text === entry.text,
      );
  if (idx >= 0) {
    const next = [...list];
    next[idx] = { ...next[idx], ...entry };
    return next;
  }
  if (entry.ts === undefined) return [...list, entry];
  // A re-published line whose id is no longer buffered was trimmed out of the
  // capped list. Appending it would resurrect an old row that the user has
  // already scrolled past.
  if (entry.i !== undefined && list.length > 0 && list[0].ts !== undefined && entry.ts < list[0].ts) {
    return list;
  }
  return [...list, entry];
}
