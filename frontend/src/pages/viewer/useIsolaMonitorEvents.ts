import { useEffect, type Dispatch, type MutableRefObject, type SetStateAction } from 'react';
import type { ConsoleEntry, CookieEntry, NetEntry } from '../../components/IsolaNetworkPanel';
import { mergeConsoleEntry } from '../../lib/consoleEntries';
import { cookieFromWire, mergeCookieEntry } from '../../lib/cookieEntries';
import { getNetworkLogMaxEntries } from '../../lib/config';
import type { DocumentHeaders } from '../../lib/intel';
import { mergeNetEntryInPlace } from '../../lib/netEntries';

export function useIsolaMonitorEvents(
  netEntriesRef: MutableRefObject<NetEntry[]>,
  setNetEntries: Dispatch<SetStateAction<NetEntry[]>>,
  consoleRef: MutableRefObject<ConsoleEntry[]>,
  setConsoleEntries: Dispatch<SetStateAction<ConsoleEntry[]>>,
  cookieRef: MutableRefObject<CookieEntry[]>,
  setCookieEntries: Dispatch<SetStateAction<CookieEntry[]>>,
  setDocHeaders: Dispatch<SetStateAction<Record<string, DocumentHeaders>>>,
) {
  useEffect(() => {
    // One render per burst, not per event. A page load emits ~4 snapshots per
    // request (requestWillBeSent, responseReceived, finished, hash), and each
    // one used to copy the whole list and repaint the panel — several hundred
    // state updates in a few seconds while the H.264 stream is also decoding.
    // The refs stay authoritative; this publishes them at most every 50 ms.
    const pending = { net: false, console: false, cookie: false };
    let flushTimer: number | null = null;
    const flush = () => {
      flushTimer = null;
      if (pending.net) {
        pending.net = false;
        setNetEntries(netEntriesRef.current.slice());
      }
      if (pending.console) {
        pending.console = false;
        setConsoleEntries(consoleRef.current.slice());
      }
      if (pending.cookie) {
        pending.cookie = false;
        setCookieEntries(cookieRef.current.slice());
      }
    };
    const schedule = (kind: 'net' | 'console' | 'cookie') => {
      pending[kind] = true;
      if (flushTimer === null) flushTimer = window.setTimeout(flush, 50);
    };
    const flushNow = () => {
      if (flushTimer !== null) window.clearTimeout(flushTimer);
      flush();
    };

    const onNet = (e: Event) => {
      const entry = (e as CustomEvent<NetEntry>).detail;
      if (!entry) return;
      const max = getNetworkLogMaxEntries();
      const list = netEntriesRef.current;
      mergeNetEntryInPlace(list, entry);
      if (list.length > max) list.splice(0, list.length - max);
      schedule('net');
    };
    const onConsole = (e: Event) => {
      const d = (e as CustomEvent).detail;
      if (!d) return;
      const entry: ConsoleEntry = { i: typeof d.i === 'number' ? d.i : undefined, level: d.level || 'log', text: d.text || '', tb: typeof d.tb === 'number' ? d.tb : undefined, ts: typeof d.ts === 'number' ? d.ts : undefined };
      const next = mergeConsoleEntry(consoleRef.current, entry);
      if (next.length > 300) next.splice(0, next.length - 300);
      consoleRef.current = next;
      schedule('console');
    };
    const onCookie = (e: Event) => {
      const d = (e as CustomEvent).detail;
      if (!d || typeof d !== 'object') return;
      const entry = cookieFromWire(d as Record<string, unknown>);
      if (!entry) return;
      let next = mergeCookieEntry(cookieRef.current, entry);
      if (next.length > 200) next = next.slice(next.length - 200);
      cookieRef.current = next;
      schedule('cookie');
    };
    const onNetInit = (e: Event) => {
      const entries = (e as CustomEvent<NetEntry[]>).detail;
      if (!Array.isArray(entries) || entries.length === 0) return;
      const max = getNetworkLogMaxEntries();
      const trimmed = entries.length > max ? entries.slice(entries.length - max) : entries;
      netEntriesRef.current = trimmed;
      pending.net = false;
      if (flushTimer !== null) {
        window.clearTimeout(flushTimer);
        flushTimer = null;
      }
      setNetEntries(trimmed);
    };
    const onHeaders = (e: Event) => {
      const doc = (e as CustomEvent<DocumentHeaders>).detail;
      if (!doc || !doc.id || !doc.hd) return;
      setDocHeaders((prev) => ({ ...prev, [doc.id]: doc }));
    };
    const onNetClear = () => {
      netEntriesRef.current = [];
      consoleRef.current = [];
      cookieRef.current = [];
      pending.net = pending.console = pending.cookie = false;
      if (flushTimer !== null) {
        window.clearTimeout(flushTimer);
        flushTimer = null;
      }
      setNetEntries([]);
      setConsoleEntries([]);
      setCookieEntries([]);
      setDocHeaders({});
    };
    window.addEventListener('isola-net', onNet);
    window.addEventListener('isola-net-init', onNetInit);
    window.addEventListener('isola-net-clear', onNetClear);
    window.addEventListener('isola-console', onConsole);
    window.addEventListener('isola-cookie', onCookie);
    window.addEventListener('isola-headers', onHeaders);
    return () => {
      // Publish whatever the last burst left pending, so a panel remount does
      // not lose the tail of a capture.
      flushNow();
      window.removeEventListener('isola-net', onNet);
      window.removeEventListener('isola-net-init', onNetInit);
      window.removeEventListener('isola-net-clear', onNetClear);
      window.removeEventListener('isola-console', onConsole);
      window.removeEventListener('isola-cookie', onCookie);
      window.removeEventListener('isola-headers', onHeaders);
    };
  }, [netEntriesRef, setNetEntries, consoleRef, setConsoleEntries, cookieRef, setCookieEntries, setDocHeaders]);
}
