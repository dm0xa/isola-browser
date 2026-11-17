import { useCallback, useEffect, useRef, useState } from 'react';

import type { BrowserTab, ConsoleEntry, CookieEntry, NetEntry } from '../components/IsolaNetworkPanel';
import { mergeConsoleEntry } from '../lib/consoleEntries';
import { cookieFromWire, mergeCookieEntry } from '../lib/cookieEntries';
import type { DocumentHeaders } from '../lib/intel';
import { getNetworkLogMaxEntries } from '../lib/config';
import { mergeNetEntryInPlace } from '../lib/netEntries';

// Console and cookie caps mirror the in-viewer panel so a long-lived tab cannot
// grow unbounded. The network cap is configurable (session.network_log_max_entries)
// and shared with the streamer's capture window.
const MAX_CONSOLE = 300;
const MAX_COOKIE = 200;
// Give up after this many consecutive failed attaches and surface a manual
// retry instead of hammering a session that is gone or that rejects the token.
const MAX_FAILURES = 5;

export type NetworkStreamStatus = 'idle' | 'connecting' | 'live' | 'reconnecting' | 'error';

interface Snapshot {
  entries: NetEntry[];
  consoleEntries: ConsoleEntry[];
  cookieEntries: CookieEntry[];
  tabs: BrowserTab[];
  /** Document headers + posture, keyed by network entry id. */
  docHeaders: Record<string, DocumentHeaders>;
}

interface WireMessage {
  type?: string;
  e?: NetEntry;
  entries?: NetEntry[];
  tabs?: BrowserTab[];
  l?: string;
  t?: string;
  tb?: number;
  i?: number;
  d?: string;
  n?: string;
  h?: boolean;
  s?: boolean;
  ts?: number;
}

const EMPTY: Snapshot = { entries: [], consoleEntries: [], cookieEntries: [], tabs: [], docHeaders: {} };

/**
 * Attaches to a session's isola input WebSocket and collects the
 * network/console/cookie side-channel the streamer broadcasts there.
 *
 * The streamer replays its full capture window to every new attachment, so a
 * fresh viewer — or a reconnect — repopulates history without any client-side
 * persistence. `wsUrl` must already carry the control token; it is never
 * logged. Passing null keeps the stream disabled.
 */
export function useNetworkStream(wsUrl: string | null) {
  const [snapshot, setSnapshot] = useState<Snapshot>(EMPTY);
  const [status, setStatus] = useState<NetworkStreamStatus>('idle');
  const [nonce, setNonce] = useState(0);

  const netRef = useRef<NetEntry[]>([]);
  const consoleRef = useRef<ConsoleEntry[]>([]);
  const cookieRef = useRef<CookieEntry[]>([]);
  const docHeadersRef = useRef<Record<string, DocumentHeaders>>({});
  const wsRef = useRef<WebSocket | null>(null);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const aliveRef = useRef(false);
  const failuresRef = useRef(0);

  const retry = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    if (!wsUrl) {
      setStatus('idle');
      return;
    }
    const url: string = wsUrl;

    aliveRef.current = true;
    failuresRef.current = 0;

    // One publish per burst instead of one per wire message: the streamer
    // republishes each request up to four times as status/size/hash arrive, and
    // every publish re-renders the whole capture (measured 2.3 ms per update at
    // the 1000-entry cap). Refs stay authoritative; the snapshot is published
    // at most every 50 ms.
    let publishTimer: number | null = null;
    const dirty = { entries: false, console: false, cookie: false };
    const publish = () => {
      publishTimer = null;
      if (!aliveRef.current) return;
      const d = { ...dirty };
      dirty.entries = dirty.console = dirty.cookie = false;
      if (!d.entries && !d.console && !d.cookie) return;
      setSnapshot((p) => ({
        ...p,
        entries: d.entries ? netRef.current.slice() : p.entries,
        consoleEntries: d.console ? consoleRef.current.slice() : p.consoleEntries,
        cookieEntries: d.cookie ? cookieRef.current.slice() : p.cookieEntries,
      }));
    };
    const schedulePublish = (kind: 'entries' | 'console' | 'cookie') => {
      dirty[kind] = true;
      if (publishTimer === null) publishTimer = window.setTimeout(publish, 50);
    };

    const clearBuffers = () => {
      netRef.current = [];
      consoleRef.current = [];
      cookieRef.current = [];
      docHeadersRef.current = {};
      dirty.entries = dirty.console = dirty.cookie = false;
      if (publishTimer !== null) {
        window.clearTimeout(publishTimer);
        publishTimer = null;
      }
      setSnapshot(EMPTY);
    };

    function schedule() {
      if (!aliveRef.current) return;
      failuresRef.current += 1;
      if (failuresRef.current > MAX_FAILURES) {
        setStatus('error');
        return;
      }
      setStatus('reconnecting');
      const delay = Math.min(1000 * 2 ** (failuresRef.current - 1), 15000);
      timerRef.current = setTimeout(open, delay);
    }

    function open() {
      if (!aliveRef.current) return;
      setStatus(failuresRef.current > 0 ? 'reconnecting' : 'connecting');

      let ws: WebSocket;
      try {
        ws = new WebSocket(url);
      } catch {
        schedule();
        return;
      }
      wsRef.current = ws;

      ws.onopen = () => {
        failuresRef.current = 0;
        setStatus('live');
        // The server replays the whole capture to a new attachment, so start
        // from empty buffers to avoid duplicating pre-reconnect history.
        clearBuffers();
      };

      ws.onmessage = (ev) => {
        if (typeof ev.data !== 'string') return;
        let msg: WireMessage;
        try {
          msg = JSON.parse(ev.data) as WireMessage;
        } catch {
          return;
        }
        switch (msg.type) {
          case 'net': {
            if (!msg.e) return;
            const max = getNetworkLogMaxEntries();
            const list = netRef.current;
            mergeNetEntryInPlace(list, msg.e);
            if (list.length > max) list.splice(0, list.length - max);
            schedulePublish('entries');
            break;
          }
          case 'net_init': {
            if (!Array.isArray(msg.entries) || msg.entries.length === 0) return;
            const max = getNetworkLogMaxEntries();
            const entries =
              msg.entries.length > max ? msg.entries.slice(msg.entries.length - max) : msg.entries;
            netRef.current = entries;
            // The replay is a bulk replace; publish it now rather than behind a
            // 50 ms timer so the first paint of a pop-out is not blank.
            dirty.entries = false;
            setSnapshot((p) => ({ ...p, entries }));
            break;
          }
          case 'console': {
            if (typeof msg.l !== 'string' || typeof msg.t !== 'string') return;
            const entry: ConsoleEntry = {
              i: typeof msg.i === 'number' ? msg.i : undefined,
              level: msg.l,
              text: msg.t,
              tb: typeof msg.tb === 'number' ? msg.tb : undefined,
              ts: typeof msg.ts === 'number' ? msg.ts : undefined,
            };
            const next = mergeConsoleEntry(consoleRef.current, entry);
            if (next.length > MAX_CONSOLE) next.splice(0, next.length - MAX_CONSOLE);
            consoleRef.current = next;
            schedulePublish('console');
            break;
          }
          case 'cookie': {
            const entry = cookieFromWire(msg as Record<string, unknown>);
            if (!entry) return;
            let next = mergeCookieEntry(cookieRef.current, entry);
            if (next.length > MAX_COOKIE) next = next.slice(next.length - MAX_COOKIE);
            cookieRef.current = next;
            schedulePublish('cookie');
            break;
          }
          case 'headers': {
            // Document headers arrive separately from the compact entry; merge by
            // entry id, newest response wins.
            const doc = (msg as unknown as { e?: DocumentHeaders }).e;
            if (!doc || !doc.id || !doc.hd) return;
            const next = { ...docHeadersRef.current, [doc.id]: doc };
            docHeadersRef.current = next;
            setSnapshot((p) => ({ ...p, docHeaders: next }));
            break;
          }
          case 'tabs': {
            // Browser tab strip state — lets the panel name traffic by tab
            // (domain) even for tabs that produced no captured entries yet.
            if (!Array.isArray(msg.tabs)) return;
            setSnapshot((p) => ({ ...p, tabs: msg.tabs as BrowserTab[] }));
            break;
          }
          default:
            // input echo, tabs, cursor, keepalive, selection — not our concern.
            break;
        }
      };

      ws.onclose = () => {
        if (wsRef.current === ws) wsRef.current = null;
        schedule();
      };

      ws.onerror = () => {
        try {
          ws.close();
        } catch {
          /* ignore */
        }
      };
    }

    open();

    return () => {
      aliveRef.current = false;
      if (timerRef.current) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
      const ws = wsRef.current;
      wsRef.current = null;
      if (ws) {
        ws.onclose = null;
        ws.onerror = null;
        ws.onmessage = null;
        ws.close();
      }
    };
  }, [wsUrl, nonce]);

  return { ...snapshot, status, retry };
}
