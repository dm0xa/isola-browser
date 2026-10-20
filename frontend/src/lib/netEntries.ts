import type { NetEntry } from '../components/IsolaNetworkPanel';

// The streamer broadcasts one request several times as it progresses:
// Network.requestWillBeSent (status and size still unknown, no hash),
// responseReceived, then the finished entry — and for a hashable body one more
// time once the body read settles and the SHA-1 exists. Each broadcast is a
// snapshot of the same row, never a new one, so fold them by id. Appending
// instead leaves the first snapshot in the list with status 0, size 0 and an
// empty sha1, which is what made hashes look "missing" for every file.
export function mergeNetEntry(list: NetEntry[], entry: NetEntry): NetEntry[] {
  const idx = list.findIndex((e) => e.id === entry.id);
  if (idx < 0) return [...list, entry];
  const next = [...list];
  // Later snapshots carry what earlier ones did not; a field the wire omits
  // (the hash until it is computed) keeps its previous value.
  next[idx] = { ...next[idx], ...entry };
  return next;
}
