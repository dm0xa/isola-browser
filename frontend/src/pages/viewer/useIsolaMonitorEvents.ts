import { useEffect, type Dispatch, type MutableRefObject, type SetStateAction } from 'react';
import type { ConsoleEntry, CookieEntry, NetEntry } from '../../components/IsolaNetworkPanel';
import { mergeConsoleEntry } from '../../lib/consoleEntries';
import { cookieFromWire, mergeCookieEntry } from '../../lib/cookieEntries';
import { getNetworkLogMaxEntries } from '../../lib/config';
import { mergeNetEntry } from '../../lib/netEntries';

export function useIsolaMonitorEvents(
  netEntriesRef: MutableRefObject<NetEntry[]>,
  setNetEntries: Dispatch<SetStateAction<NetEntry[]>>,
  consoleRef: MutableRefObject<ConsoleEntry[]>,
  setConsoleEntries: Dispatch<SetStateAction<ConsoleEntry[]>>,
  cookieRef: MutableRefObject<CookieEntry[]>,
  setCookieEntries: Dispatch<SetStateAction<CookieEntry[]>>,
) {
  useEffect(() => {
    const onNet = (e: Event) => {
      const entry = (e as CustomEvent<NetEntry>).detail;
      if (!entry) return;
      const max = getNetworkLogMaxEntries();
      const next = mergeNetEntry(netEntriesRef.current, entry);
      if (next.length > max) next.splice(0, next.length - max);
      netEntriesRef.current = next;
      setNetEntries(next);
    };
    const onConsole = (e: Event) => {
      const d = (e as CustomEvent).detail;
      if (!d) return;
      const entry: ConsoleEntry = { level: d.level || 'log', text: d.text || '', tb: typeof d.tb === 'number' ? d.tb : undefined, ts: typeof d.ts === 'number' ? d.ts : undefined };
      const next = mergeConsoleEntry(consoleRef.current, entry);
      if (next.length > 300) next.splice(0, next.length - 300);
      consoleRef.current = next;
      setConsoleEntries(next);
    };
    const onCookie = (e: Event) => {
      const d = (e as CustomEvent).detail;
      if (!d || typeof d !== 'object') return;
      const entry = cookieFromWire(d as Record<string, unknown>);
      if (!entry) return;
      let next = mergeCookieEntry(cookieRef.current, entry);
      if (next.length > 200) next = next.slice(next.length - 200);
      cookieRef.current = next;
      setCookieEntries(next);
    };
    const onNetInit = (e: Event) => {
      const entries = (e as CustomEvent<NetEntry[]>).detail;
      if (!Array.isArray(entries) || entries.length === 0) return;
      const max = getNetworkLogMaxEntries();
      const trimmed = entries.length > max ? entries.slice(entries.length - max) : entries;
      netEntriesRef.current = trimmed;
      setNetEntries(trimmed);
    };
    const onNetClear = () => {
      netEntriesRef.current = [];
      setNetEntries([]);
      consoleRef.current = [];
      setConsoleEntries([]);
      cookieRef.current = [];
      setCookieEntries([]);
    };
    window.addEventListener('isola-net', onNet);
    window.addEventListener('isola-net-init', onNetInit);
    window.addEventListener('isola-net-clear', onNetClear);
    window.addEventListener('isola-console', onConsole);
    window.addEventListener('isola-cookie', onCookie);
    return () => {
      window.removeEventListener('isola-net', onNet);
      window.removeEventListener('isola-net-init', onNetInit);
      window.removeEventListener('isola-net-clear', onNetClear);
      window.removeEventListener('isola-console', onConsole);
      window.removeEventListener('isola-cookie', onCookie);
    };
  }, [netEntriesRef, setNetEntries, consoleRef, setConsoleEntries, cookieRef, setCookieEntries]);
}
