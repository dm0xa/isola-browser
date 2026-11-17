import type { NetEntry } from '../components/IsolaNetworkPanel';

// mergeNetEntryInPlace folds one wire snapshot into the working list, keyed by
// id. The streamer broadcasts one request several times as it progresses:
// Network.requestWillBeSent (status and size still unknown, no hash),
// responseReceived, then the finished entry — and for a hashable body one more
// time once the body read settles and the SHA-1 exists. Each broadcast is a
// snapshot of the same row, never a new one, so appending instead leaves the
// first snapshot in the list with status 0, size 0 and an empty sha1, which is
// what made hashes look "missing" for every file.
//
// In place rather than copy-on-write: the viewer receives one snapshot per
// network event (a page load of 150 requests emits ~600) and at the capture cap
// each copy is a thousand elements. The caller publishes a new array on its own
// render cadence, so the render still sees a fresh identity.
export function mergeNetEntryInPlace(list: NetEntry[], entry: NetEntry): void {
  const idx = list.findIndex((e) => e.id === entry.id);
  if (idx < 0) list.push(entry);
  // Later snapshots carry what earlier ones did not; a field the wire omits
  // (the hash until it is computed) keeps its previous value.
  else list[idx] = { ...list[idx], ...entry };
}
