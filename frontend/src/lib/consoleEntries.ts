import type { ConsoleEntry } from '../components/IsolaNetworkPanel';

// Console lines carry no id on the wire, but the streamer stamps every one with
// a capture timestamp that survives re-publishing. It re-sends a line when it
// later learns which browser tab the line belongs to — traffic captured before
// the runner reported its tab strip is folded into the tab that turns out to be
// active (see reassignUnknownTab in the streamer). Fold on that identity
// instead of appending: a second append would render the corrected line twice.
export function mergeConsoleEntry(list: ConsoleEntry[], entry: ConsoleEntry): ConsoleEntry[] {
  if (entry.ts === undefined) return [...list, entry];
  const idx = list.findIndex(
    (e) => e.ts === entry.ts && e.level === entry.level && e.text === entry.text,
  );
  if (idx < 0) return [...list, entry];
  const next = [...list];
  next[idx] = { ...next[idx], ...entry };
  return next;
}
