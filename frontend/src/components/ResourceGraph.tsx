import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react';
import type { NetEntry } from './IsolaNetworkPanel';
import { analyze } from './IsolaNetworkPanel';
import { copyToClipboard } from '../lib/clipboard';
import {
  CLASS_ACCENT,
  CLASS_ACCENT_LIGHT,
  CLASS_BG,
  CLASS_BG_LIGHT,
  CLASS_LABEL,
  CLASS_NAME,
  CLASS_ORDER,
  classifyResource,
  type ResourceClass,
} from '../lib/resourceType';
import {
  ISOLA_LOGO_CENTER,
  ISOLA_LOGO_FACE_PATHS,
  ISOLA_LOGO_RING_PATHS,
  ISOLA_LOGO_TOP_PATHS,
  ISOLA_LOGO_VIEWBOX,
} from './IsolaLogo';

export type { ResourceClass };

export type GraphNodeKind = 'root' | 'host' | 'group' | 'resource' | 'more';

export interface GraphNode {
  id: string;
  kind: GraphNodeKind;
  label: string;
  host: string;
  resourceClass?: ResourceClass;
  fullUrl?: string;
  count: number;
  /** Distinct resources under a host (or inside a type group). */
  resourceCount?: number;
  /** Distinct failed / flagged resources under a host or group. */
  failedCount?: number;
  flaggedCount?: number;
  flagged: boolean;
  failed: boolean;
  /** The action a `more` row performs. */
  moreAction?: 'expand' | 'collapse';
  /** Latest capture timestamp for a resource; orders the preview by recency. */
  lastTs?: number;
  /** Hex SHA-1 of the file's content, once the streamer has hashed it. */
  sha1?: string;
  /** The streamer clipped this URL to its wire budget; copy is incomplete. */
  ut?: boolean;
}

export interface GraphEdge {
  id: string;
  from: string;
  to: string;
  direct: boolean;
}

export interface GraphData {
  nodes: GraphNode[];
  edges: GraphEdge[];
  /** Synthetic `[request]` entry node every chain hangs off. */
  rootId: string | null;
  /** The origin host (the site the user actually asked for). */
  originId: string | null;
}

/** Synthetic entry node — the single "where it all started" block. */
const ROOT_NODE_ID = 'root:request';

/**
 * Whether the page is currently painted light.
 *
 * The graph is drawn on its own canvas, but everything around it — the panel,
 * the tab strip, the toolbar — is styled by Tailwind's `dark:` variants, which
 * key off the `dark` class on <html>. Reading that same signal (rather than the
 * React theme context) means the canvas can never disagree with its
 * surroundings, which is exactly the bug this guards against: a graph that
 * stayed dark inside an otherwise light panel.
 */
function useDocumentLight(): boolean {
  const read = () => typeof document === 'undefined' || !document.documentElement.classList.contains('dark');
  const [docLight, setDocLight] = useState(read);
  useEffect(() => {
    const el = document.documentElement;
    const sync = () => setDocLight(!el.classList.contains('dark'));
    sync();
    const obs = new MutationObserver(sync);
    obs.observe(el, { attributes: true, attributeFilter: ['class'] });
    return () => obs.disconnect();
  }, []);
  return docLight;
}

interface GraphPalette {
  canvas: string;
  vignetteFrom: string;
  vignetteTo: string;
  gridMinor: string;
  gridMajor: string;
  columnLine: string;
  columnLabel: string;
  cardRoot: string;
  cardHost: string;
  cardHostFailed: string;
  cardHostFlagged: string;
  labelRoot: string;
  labelHostOrigin: string;
  labelHost: string;
  labelRes: string;
  badgeBg: string;
  badgeText: string;
  edgeDirect: string;
  edgeCross: string;
  edgeEntry: string;
  warn: string;
  error: string;
  neutral: string;
}

// The graph is drawn on its own canvas rather than inheriting the panel's
// background, so every structural colour comes from here and follows the theme.
const GRAPH_PALETTE: Record<'dark' | 'light', GraphPalette> = {
  dark: {
    canvas: '#05070d',
    vignetteFrom: '#0a0f1c',
    vignetteTo: '#05070d',
    gridMinor: 'rgba(148, 163, 184, 0.04)',
    gridMajor: 'rgba(148, 163, 184, 0.06)',
    columnLine: 'rgba(148, 163, 184, 0.08)',
    columnLabel: 'rgba(148, 163, 184, 0.3)',
    cardRoot: 'rgba(14, 26, 42, 0.96)',
    cardHost: 'rgba(24, 33, 48, 0.92)',
    cardHostFailed: 'rgba(60, 16, 20, 0.92)',
    cardHostFlagged: 'rgba(56, 38, 8, 0.92)',
    labelRoot: '#a5f3fc',
    labelHostOrigin: '#e0f2fe',
    labelHost: '#dbe4f0',
    labelRes: '#c3cedd',
    badgeBg: 'rgba(148, 163, 184, 0.09)',
    badgeText: '#8fa0b5',
    edgeDirect: '#5b6b81',
    edgeCross: '#a78bfa',
    edgeEntry: '#22d3ee',
    warn: '#f59e0b',
    error: '#ef4444',
    neutral: '#64748b',
  },
  light: {
    canvas: '#f4f4f5',
    vignetteFrom: '#ffffff',
    vignetteTo: '#e9ebee',
    gridMinor: 'rgba(15, 23, 42, 0.045)',
    gridMajor: 'rgba(15, 23, 42, 0.07)',
    columnLine: 'rgba(15, 23, 42, 0.1)',
    columnLabel: 'rgba(71, 85, 105, 0.5)',
    cardRoot: 'rgba(255, 255, 255, 0.97)',
    cardHost: 'rgba(255, 255, 255, 0.97)',
    cardHostFailed: 'rgba(254, 242, 242, 0.97)',
    cardHostFlagged: 'rgba(255, 251, 235, 0.97)',
    labelRoot: '#0e7490',
    labelHostOrigin: '#0f172a',
    labelHost: '#1e293b',
    labelRes: '#334155',
    badgeBg: 'rgba(15, 23, 42, 0.06)',
    badgeText: '#64748b',
    edgeDirect: '#7f8ea3',
    edgeCross: '#8b5cf6',
    edgeEntry: '#0891b2',
    warn: '#b45309',
    error: '#dc2626',
    neutral: '#64748b',
  },
};

export function buildGraph(
  entries: NetEntry[],
  pinnedHost?: string,
  typeFilter?: ReadonlySet<ResourceClass>,
): GraphData {
  const nodes = new Map<string, GraphNode>();
  const edges = new Map<string, GraphEdge>();
  const hostId = (h: string) => `host:${h}`;
  const resourceId = (url: string) => `res:${url}`;

  if (entries.length === 0) return { nodes: [], edges: [], rootId: null, originId: null };

  // One parse per entry. The trigger scan below looks back up to 40 entries per
  // row, so calling analyze()/classifyResource() inside it was ~40k URL parses
  // per build — and the memo runs twice and on every network event.
  const facts = entries.map((e) => {
    const { host, flags } = analyze(e.url);
    return {
      host,
      cls: classifyResource(e.t || '', e.url),
      flagged: Object.values(flags).some(Boolean),
    };
  });

  // The origin is decided over the whole capture, never the filtered slice:
  // narrowing to one type must not make a CDN host look like the opened page.
  let rootHost: string | null = null;
  let rootDocHost: string | null = null;
  for (let i = 0; i < entries.length; i++) {
    const { host, cls } = facts[i];
    if (!rootDocHost && (cls === 'document' || (entries[i].t || '').toLowerCase().includes('document'))) {
      rootDocHost = host;
    }
    if (!rootHost) rootHost = host;
  }

  // A non-empty type filter drops everything else, so the graph shows exactly
  // the kinds the user kept (a filter, not a spotlight). Indices are kept so a
  // precomputed fact can be indexed directly.
  const included = typeFilter && typeFilter.size > 0
    ? entries.map((_, i) => i).filter((i) => typeFilter.has(facts[i].cls))
    : entries.map((_, i) => i);

  for (const idx of included) {
    const e = entries[idx];
    const { host, cls: rClass, flagged: entryFlagged } = facts[idx];
    const entryFailed = !!e.f;

    const hid = hostId(host);
    if (!nodes.has(hid)) {
      nodes.set(hid, {
        id: hid, kind: 'host', label: host, host,
        count: 0, resourceCount: 0, failedCount: 0, flaggedCount: 0,
        flagged: false, failed: false,
      });
    }
    const hNode = nodes.get(hid)!;
    hNode.count++;
    if (entryFlagged) hNode.flagged = true;
    if (entryFailed) hNode.failed = true;

    const rid = resourceId(e.url);
    if (!nodes.has(rid)) {
      const path = (() => { try { const u = new URL(e.url); const parts = u.pathname.split('/').filter(Boolean); return parts.pop() || u.hostname; } catch { return e.url.slice(0, 60); } })();
      const resource: GraphNode = {
        id: rid, kind: 'resource', label: path, host, fullUrl: e.url, resourceClass: rClass,
        count: 1, flagged: entryFlagged, failed: entryFailed,
        lastTs: e.ts,
        sha1: e.h,
        ut: e.ut,
      };
      nodes.set(rid, resource);
      hNode.resourceCount = (hNode.resourceCount || 0) + 1;
      if (resource.flagged) hNode.flaggedCount = (hNode.flaggedCount || 0) + 1;
      if (resource.failed) hNode.failedCount = (hNode.failedCount || 0) + 1;
    } else {
      const r = nodes.get(rid)!;
      r.count++;
      if (entryFailed && !r.failed) {
        r.failed = true;
        hNode.failed = true;
        hNode.failedCount = (hNode.failedCount || 0) + 1;
      }
      if (entryFlagged && !r.flagged) {
        r.flagged = true;
        hNode.flagged = true;
        hNode.flaggedCount = (hNode.flaggedCount || 0) + 1;
      }
      if (!r.sha1 && e.h) r.sha1 = e.h;
      if (e.ts && (!r.lastTs || e.ts > r.lastTs)) r.lastTs = e.ts;
    }

    const edgeIdH = `${hid}->${rid}`;
    if (!edges.has(edgeIdH)) {
      edges.set(edgeIdH, { id: edgeIdH, from: hid, to: rid, direct: true });
    }
  }

  // Pinned host is the page the user opened (the tab URL). The first document
  // in `entries` is not that page once the capture window has dropped it:
  // a polling host then becomes the origin and the node swaps while idle.
  const finalRootHost = (pinnedHost || '').trim() || rootDocHost || rootHost;

  // Synthetic entry node: a single `[request]` block that every chain hangs
  // off, so the graph reads "this is where it all started" at a glance.
  const rootId = finalRootHost ? ROOT_NODE_ID : null;
  const originId = finalRootHost ? hostId(finalRootHost) : null;
  if (finalRootHost && !nodes.has(hostId(finalRootHost))) {
    nodes.set(hostId(finalRootHost), {
      id: hostId(finalRootHost), kind: 'host', label: finalRootHost, host: finalRootHost,
      count: 0, resourceCount: 0, failedCount: 0, flaggedCount: 0, flagged: false, failed: false,
    });
  }

  if (finalRootHost) {
    const resources = [...nodes.values()].filter((n) => n.kind === 'resource');
    const failedCount = resources.filter((n) => n.failed).length;
    const flaggedCount = resources.filter((n) => n.flagged).length;
    nodes.set(ROOT_NODE_ID, {
      id: ROOT_NODE_ID, kind: 'root', label: '[request]', host: '',
      count: included.length, resourceCount: resources.length, failedCount, flaggedCount,
      flagged: flaggedCount > 0, failed: failedCount > 0,
    });
    const rootEdgeId = `${ROOT_NODE_ID}->${hostId(finalRootHost)}`;
    edges.set(rootEdgeId, { id: rootEdgeId, from: ROOT_NODE_ID, to: hostId(finalRootHost), direct: false });
  }

  const lastTriggerByHost = new Map<string, string>();
  for (let i = 0; i < included.length; i++) {
    const host = facts[included[i]].host;

    for (let j = i - 1; j >= 0 && j >= i - 40; j--) {
      const prev = facts[included[j]];
      if (prev.host === host) continue;
      if (prev.cls !== 'script' && prev.cls !== 'stylesheet' && prev.cls !== 'document') continue;
      const edgeId = `${hostId(prev.host)}->${hostId(host)}`;
      if (!edges.has(edgeId)) {
        edges.set(edgeId, { id: edgeId, from: hostId(prev.host), to: hostId(host), direct: false });
      }
      lastTriggerByHost.set(host, hostId(prev.host));
      break;
    }

    if (host !== finalRootHost && !lastTriggerByHost.has(host) && finalRootHost) {
      const edgeId = `${hostId(finalRootHost)}->${hostId(host)}`;
      if (!edges.has(edgeId)) {
        edges.set(edgeId, { id: edgeId, from: hostId(finalRootHost), to: hostId(host), direct: false });
      }
      lastTriggerByHost.set(host, hostId(finalRootHost));
    }
  }

  return {
    nodes: Array.from(nodes.values()),
    edges: Array.from(edges.values()),
    rootId,
    originId,
  };
}

/** Resource rows shown before a type group offers “show all”. */
export const GROUP_PREVIEW = 12;

export interface GraphExpansion {
  /** Host node ids expanded into type groups. */
  hosts: ReadonlySet<string>;
  /** Group node ids expanded into resource rows. */
  groups: ReadonlySet<string>;
  /** Group node ids whose full resource list is shown instead of the preview. */
  all: ReadonlySet<string>;
}

export const EMPTY_EXPANSION: GraphExpansion = {
  hosts: new Set(),
  groups: new Set(),
  all: new Set(),
};

const groupNodeId = (hostId: string, cls: ResourceClass) => `grp:${hostId}:${cls}`;
const moreNodeId = (hostId: string, cls: ResourceClass) => `more:${hostId}:${cls}`;

function compareResources(a: GraphNode, b: GraphNode): number {
  if (a.failed !== b.failed) return a.failed ? -1 : 1;
  if (a.flagged !== b.flagged) return a.flagged ? -1 : 1;
  if (a.count !== b.count) return b.count - a.count;
  if ((a.lastTs || 0) !== (b.lastTs || 0)) return (b.lastTs || 0) - (a.lastTs || 0);
  return a.label.localeCompare(b.label);
}

/**
 * Summary-first view of the full graph.
 *
 * Every host is reduced to its card until the user opens it; an open host
 * reveals one row per resource class, and an open class reveals a bounded
 * preview of its resources plus an explicit “more” row. This keeps the
 * cross-axis of the layered layout proportional to what the user asked to
 * see, not to the request count of a polling site.
 */
export function applyExpansion(full: GraphData, expansion: GraphExpansion): GraphData {
  if (full.nodes.length === 0) return full;

  const hosts = full.nodes.filter((n) => n.kind === 'host');
  const resourcesByHost = new Map<string, GraphNode[]>();
  for (const n of full.nodes) {
    if (n.kind !== 'resource') continue;
    const list = resourcesByHost.get(n.host);
    if (list) list.push(n);
    else resourcesByHost.set(n.host, [n]);
  }

  const nodes = new Map<string, GraphNode>();
  const edges = new Map<string, GraphEdge>();

  // Root and host cards are always present. Host→host trigger edges survive
  // because they describe the chain, not the leaves.
  for (const n of full.nodes) {
    if (n.kind === 'root' || n.kind === 'host') nodes.set(n.id, n);
  }
  for (const e of full.edges) {
    if (nodes.has(e.from) && nodes.has(e.to)) edges.set(e.id, e);
  }

  for (const host of hosts) {
    if (!expansion.hosts.has(host.id)) continue;
    const resources = resourcesByHost.get(host.host) || [];
    if (resources.length === 0) continue;

    const byClass = new Map<ResourceClass, GraphNode[]>();
    for (const r of resources) {
      const cls = r.resourceClass || 'other';
      const list = byClass.get(cls);
      if (list) list.push(r);
      else byClass.set(cls, [r]);
    }

    for (const cls of CLASS_ORDER) {
      const list = byClass.get(cls);
      if (!list || list.length === 0) continue;
      const sorted = [...list].sort(compareResources);
      const gid = groupNodeId(host.id, cls);
      const open = expansion.groups.has(gid);
      const showAll = expansion.all.has(gid);
      const failedCount = sorted.filter((r) => r.failed).length;
      const flaggedCount = sorted.filter((r) => r.flagged).length;

      nodes.set(gid, {
        id: gid,
        kind: 'group',
        label: CLASS_NAME[cls],
        host: host.host,
        resourceClass: cls,
        count: sorted.length,
        resourceCount: sorted.length,
        failedCount,
        flaggedCount,
        flagged: flaggedCount > 0,
        failed: failedCount > 0,
      });
      const groupEdgeId = `${host.id}->${gid}`;
      edges.set(groupEdgeId, { id: groupEdgeId, from: host.id, to: gid, direct: true });
      if (!open) continue;

      const shown = showAll ? sorted : sorted.slice(0, GROUP_PREVIEW);
      for (const r of shown) {
        nodes.set(r.id, r);
        const edgeId = `${gid}->${r.id}`;
        edges.set(edgeId, { id: edgeId, from: gid, to: r.id, direct: true });
      }

      if (sorted.length > GROUP_PREVIEW) {
        const mid = moreNodeId(host.id, cls);
        const rest = sorted.slice(GROUP_PREVIEW);
        nodes.set(mid, showAll
          ? {
            id: mid, kind: 'more', label: `show top ${GROUP_PREVIEW}`, host: host.host,
            resourceClass: cls, count: GROUP_PREVIEW, moreAction: 'collapse',
            failedCount: 0, flaggedCount: 0, flagged: false, failed: false,
          }
          : {
            id: mid, kind: 'more', label: `+${rest.length} more`, host: host.host,
            resourceClass: cls, count: rest.length, moreAction: 'expand',
            failedCount: rest.filter((r) => r.failed).length,
            flaggedCount: rest.filter((r) => r.flagged).length,
            flagged: rest.some((r) => r.flagged),
            failed: rest.some((r) => r.failed),
          });
        const moreEdgeId = `${gid}->${mid}`;
        edges.set(moreEdgeId, { id: moreEdgeId, from: gid, to: mid, direct: false });
      }
    }
  }

  return {
    nodes: [...nodes.values()],
    edges: [...edges.values()],
    rootId: full.rootId,
    originId: full.originId,
  };
}

/** Every host opened, every type group open and fully expanded. */
function expandAllState(data: GraphData): GraphExpansion {
  const hosts = new Set<string>();
  const groups = new Set<string>();
  const all = new Set<string>();
  const hostIdByHostname = new Map<string, string>();
  for (const n of data.nodes) {
    if (n.kind === 'host') {
      hosts.add(n.id);
      hostIdByHostname.set(n.host, n.id);
    }
  }

  const classesByHost = new Map<string, Set<ResourceClass>>();
  for (const n of data.nodes) {
    if (n.kind !== 'resource') continue;
    const hostId = hostIdByHostname.get(n.host);
    if (!hostId) continue;
    const classes = classesByHost.get(hostId) ?? new Set<ResourceClass>();
    classes.add(n.resourceClass || 'other');
    classesByHost.set(hostId, classes);
  }

  for (const [hostId, classes] of classesByHost) {
    hosts.add(hostId);
    for (const cls of classes) {
      const gid = groupNodeId(hostId, cls);
      groups.add(gid);
      all.add(gid);
    }
  }
  return { hosts, groups, all };
}

// Nodes and edges that stay lit while `focusId` is hovered or pinned.
// Immediate neighbours in both directions, plus every ancestor back to the
// entry. Stopping at one hop left the path from `[request]` faded whenever
// the pointer sat on a resource further down the chain.
export function focusEmphasis(
  focusId: string | null,
  edges: Array<{ id: string; from: string; to: string }>,
): { nodes: Set<string>; edges: Set<string> } {
  const nodes = new Set<string>();
  const edgeIds = new Set<string>();
  if (!focusId) return { nodes, edges: edgeIds };

  nodes.add(focusId);
  const incoming = new Map<string, Array<{ id: string; from: string }>>();
  for (const e of edges) {
    if (e.from === focusId || e.to === focusId) {
      edgeIds.add(e.id);
      nodes.add(e.from);
      nodes.add(e.to);
    }
    const list = incoming.get(e.to);
    if (list) list.push({ id: e.id, from: e.from });
    else incoming.set(e.to, [{ id: e.id, from: e.from }]);
  }

  const stack = [focusId];
  const seen = new Set<string>([focusId]);
  while (stack.length) {
    const id = stack.pop()!;
    for (const e of incoming.get(id) || []) {
      edgeIds.add(e.id);
      nodes.add(e.from);
      if (!seen.has(e.from)) {
        seen.add(e.from);
        stack.push(e.from);
      }
    }
  }
  return { nodes, edges: edgeIds };
}

export interface LayoutNode {
  id: string;
  x: number;
  y: number;
  w: number;
  h: number;
  node: GraphNode;
  depth: number;
}

export interface LayoutEdge {
  id: string;
  from: LayoutNode;
  to: LayoutNode;
  direct: boolean;
}

export interface LayoutColumn {
  depth: number;
  x: number;
  w: number;
  top: number;
  bottom: number;
  /** Only the first sub-column of a wrapped band carries the depth label. */
  label: boolean;
}

const HOST_H = 40;
const RES_H = 30;
const GROUP_H = 30;
const MORE_H = 26;
const HOST_BASE_W = 180;
const RES_BASE_W = 200;
const GROUP_BASE_W = 160;
const ROOT_W = 148;
const COL_GAP = 92;
const SUB_COL_GAP = 30;
const ROW_GAP = 10;
const CH_PX = 6.8;
const PAD_X = 28;

function estimateHostWidth(n: GraphNode): number {
  // Label starts at x=45. From the right edge: count badge 38, chevron 16,
  // failure / flag badges 22 each, plus breathing room.
  const controls = 38
    + (n.resourceCount ? 16 : 0)
    + (n.failedCount ? 22 : 0)
    + (n.flaggedCount ? 22 : 0)
    + 8;
  const textW = 45 + Math.min(30, n.label.length) * CH_PX + controls;
  return Math.max(HOST_BASE_W, Math.round(textW));
}

function estimateResWidth(n: GraphNode): number {
  const badge = 40;
  const cnt = (n.count > 1 ? 28 : 0) + (n.flagged ? 16 : 0) + (n.failed ? 16 : 0);
  const textW = Math.min(26, n.label.length) * CH_PX + PAD_X + badge + cnt;
  return Math.max(RES_BASE_W, Math.round(textW));
}

function estimateGroupWidth(n: GraphNode): number {
  // Label starts at x=29. The right side holds the count, chevron and badges.
  const controls = 38 + 16 + (n.failedCount ? 25 : 0) + (n.flaggedCount ? 25 : 0) + 8;
  const textW = 29 + Math.min(18, n.label.length) * CH_PX + controls;
  return Math.max(GROUP_BASE_W, Math.round(textW));
}

function estimateMoreWidth(n: GraphNode): number {
  const textW = 12 + Math.min(24, n.label.length) * CH_PX + 20 + (n.failed ? 16 : 0) + 8;
  return Math.max(132, Math.round(textW));
}

/** Box size for a node — the synthetic root uses its own fixed chip size. */
function nodeSize(n: GraphNode): { w: number; h: number } {
  if (n.kind === 'root') return { w: ROOT_W, h: HOST_H };
  if (n.kind === 'host') return { w: estimateHostWidth(n), h: HOST_H };
  if (n.kind === 'group') return { w: estimateGroupWidth(n), h: GROUP_H };
  if (n.kind === 'more') return { w: estimateMoreWidth(n), h: MORE_H };
  return { w: estimateResWidth(n), h: RES_H };
}

/** Column order within a depth: root, hosts, type groups, resources, “more”. */
const KIND_RANK: Record<GraphNodeKind, number> = { root: 0, host: 1, group: 2, resource: 3, more: 4 };

function computeDepths(data: GraphData): Map<string, number> {
  const depths = new Map<string, number>();
  const incoming = new Map<string, string[]>();
  for (const n of data.nodes) {
    depths.set(n.id, 0);
    incoming.set(n.id, []);
  }
  for (const e of data.edges) {
    incoming.get(e.to)?.push(e.from);
  }

  // Longest-path layering, tolerant of cycles: real trigger chains loop in the
  // wild (host A's script loads host B, whose script loads host A back), and a
  // BFS-relaxation ping-pongs on those — inflating depths until the iteration
  // cap and leaving depth values with no nodes between them, which rendered
  // column guides with ±Infinity coordinates. DFS memoisation simply skips
  // back edges (an ancestor still on the stack), so depth(v) settles at
  // 1 + max(depth of acyclic parents) and stays finite.
  const WHITE = 0;
  const GRAY = 1;
  const BLACK = 2;
  const state = new Map<string, number>();
  const visit = (id: string) => {
    const st = state.get(id) ?? WHITE;
    if (st !== WHITE) return;
    state.set(id, GRAY);
    let best = 0;
    for (const p of incoming.get(id) || []) {
      if ((state.get(p) ?? WHITE) === GRAY) continue;
      visit(p);
      best = Math.max(best, (depths.get(p) || 0) + 1);
    }
    depths.set(id, Math.min(best, data.nodes.length));
    state.set(id, BLACK);
  };
  for (const n of data.nodes) visit(n.id);
  return depths;
}

/** Depth axis: horizontal flows left→right, vertical flows top→bottom. */
export type LayoutDirection = 'horizontal' | 'vertical';

export function layoutGraph(
  data: GraphData,
  width: number,
  height: number,
  direction: LayoutDirection = 'horizontal',
): { nodes: LayoutNode[]; edges: LayoutEdge[]; columns: LayoutColumn[]; totalW: number; totalH: number } {
  const layoutNodes: LayoutNode[] = [];
  const layoutEdges: LayoutEdge[] = [];

  if (data.nodes.length === 0) return { nodes: [], edges: [], columns: [], totalW: 0, totalH: 0 };

  // The two modes are transposes of each other: `depth` always advances along
  // the flow axis, and a depth's siblings always spread across the other one.
  const vertical = direction === 'vertical';

  // A depth's siblings used to form one unbounded column. A host with 165
  // resources produced a 6 600 px rail that no zoom level could fit, so long
  // sibling runs now wrap into sub-columns at about two viewports of cross-axis
  // length. Both axes carry the graph and “fit” becomes meaningful again.
  const crossLimit = Math.max(300, (vertical ? width * 1.35 : height * 1.7));

  const depths = computeDepths(data);
  const byDepth = new Map<number, GraphNode[]>();
  for (const n of data.nodes) {
    const d = depths.get(n.id) ?? 0;
    if (!byDepth.has(d)) byDepth.set(d, []);
    byDepth.get(d)!.push(n);
  }
  // Only lay out depths that actually hold nodes — a sparse depth map must
  // never produce an empty band (its guide coordinates would be ±Infinity).
  const depthKeys = [...byDepth.keys()].sort((a, b) => a - b);

  const nodeMap = new Map<string, LayoutNode>();

  // Each depth is a band. `thickness` is its biggest node along the flow axis;
  // `chunks` hold the siblings that fit across the cross axis.
  const bands = depthKeys.map((d) => {
    const list = byDepth.get(d) || [];
    list.sort((a, b) => {
      if (KIND_RANK[a.kind] !== KIND_RANK[b.kind]) return KIND_RANK[a.kind] - KIND_RANK[b.kind];
      if (a.host !== b.host) return a.host.localeCompare(b.host);
      return b.count - a.count;
    });
    const measured = list.map((n) => ({ n, ...nodeSize(n) }));
    const thickness = Math.max(0, ...measured.map((m) => (vertical ? m.h : m.w)));

    const chunks: Array<typeof measured> = [];
    let current: typeof measured = [];
    let currentCross = -ROW_GAP;
    for (const m of measured) {
      const crossSize = vertical ? m.w : m.h;
      if (current.length > 0 && currentCross + ROW_GAP + crossSize > crossLimit) {
        chunks.push(current);
        current = [];
        currentCross = -ROW_GAP;
      }
      current.push(m);
      currentCross += ROW_GAP + crossSize;
    }
    if (current.length > 0) chunks.push(current);
    const chunkCross = chunks.map((chunk) => chunk.reduce(
      (sum, m, i) => sum + (vertical ? m.w : m.h) + (i > 0 ? ROW_GAP : 0),
      0,
    ));

    return { depth: d, chunks, chunkCross, thickness };
  });

  // Bands are anchored to the top/left, not centred on the longest band: the
  // entry point must stay in view when one band is thousands of pixels long.
  const maxNodeCross = Math.max(60, ...bands.flatMap((b) => b.chunkCross));
  const crossTotal = maxNodeCross + 60;

  let cursor = 24;
  const offsets: number[] = [];
  for (const b of bands) {
    offsets.push(cursor);
    const subColumns = Math.max(1, b.chunks.length);
    cursor += b.thickness * subColumns + SUB_COL_GAP * (subColumns - 1) + COL_GAP;
  }
  const spanTotal = cursor - COL_GAP + 24;

  const columns: LayoutColumn[] = [];

  bands.forEach((b, di) => {
    b.chunks.forEach((chunk, ci) => {
      const chunkOffset = offsets[di] + ci * (b.thickness + SUB_COL_GAP);
      let crossPos = 30;
      let left = Infinity;
      let top = Infinity;
      let right = -Infinity;
      let bottom = -Infinity;
      for (const m of chunk) {
        const along = chunkOffset + (b.thickness - (vertical ? m.h : m.w)) / 2;
        const x = vertical ? crossPos : along;
        const y = vertical ? along : crossPos;
        const ln: LayoutNode = { id: m.n.id, x, y, w: m.w, h: m.h, node: m.n, depth: b.depth };
        nodeMap.set(m.n.id, ln);
        layoutNodes.push(ln);
        left = Math.min(left, x);
        top = Math.min(top, y);
        right = Math.max(right, x + m.w);
        bottom = Math.max(bottom, y + m.h);
        crossPos += (vertical ? m.w : m.h) + ROW_GAP;
      }
      columns.push(
        vertical
          ? { depth: b.depth, x: left, w: right - left, top: chunkOffset, bottom: chunkOffset + b.thickness, label: ci === 0 }
          : { depth: b.depth, x: chunkOffset, w: b.thickness, top, bottom, label: ci === 0 },
      );
    });
  });

  for (const e of data.edges) {
    const from = nodeMap.get(e.from);
    const to = nodeMap.get(e.to);
    if (!from || !to) continue;
    layoutEdges.push({ id: e.id, from, to, direct: e.direct });
  }

  return vertical
    ? { nodes: layoutNodes, edges: layoutEdges, columns, totalW: crossTotal, totalH: spanTotal }
    : { nodes: layoutNodes, edges: layoutEdges, columns, totalW: spanTotal, totalH: crossTotal };
}

const Z_MIN = 0.25;
const Z_MAX = 4;
const clampZ = (z: number) => Math.max(Z_MIN, Math.min(Z_MAX, z));

const LS_GRAPH_DIRECTION = 'isola-graph-direction';
const LOD_ZOOM = 0.5;

function toggled(set: ReadonlySet<string>, id: string): ReadonlySet<string> {
  const next = new Set(set);
  if (next.has(id)) next.delete(id);
  else next.add(id);
  return next;
}

function removed(set: ReadonlySet<string>, id: string): ReadonlySet<string> {
  if (!set.has(id)) return set;
  const next = new Set(set);
  next.delete(id);
  return next;
}

const SVG_NS = 'http://www.w3.org/2000/svg';

/** Gap kept around the graph, and the room the watermark needs inside it. */
const EXPORT_PAD = 72;
const LOGO_SIZE = 56;
const LOGO_MARGIN = 12;
/** Advance of one character at the caption's 11px monospace size. */
const CAPTION_CH = 7.4;

/**
 * One-line provenance for the exported image: what was captured, how much of
 * it, which filters were on, and when. A PNG outlives the session it came from.
 */
export function exportCaption(input: {
  origin: string;
  requests: number;
  hosts: number;
  resources: number;
  failed: number;
  types: readonly string[];
  failedOnly: boolean;
  at: Date;
}): string {
  const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;
  const parts: string[] = [];
  if (input.origin) parts.push(input.origin);
  parts.push(plural(input.requests, 'request', 'requests'));
  parts.push(plural(input.hosts, 'host', 'hosts'));
  parts.push(plural(input.resources, 'resource', 'resources'));
  if (input.failed > 0) parts.push(`${input.failed} failed`);
  if (input.types.length > 0) parts.push(input.types.join(' + '));
  if (input.failedOnly) parts.push('failed only');
  parts.push(`${input.at.toISOString().slice(0, 16).replace('T', ' ')}Z`);
  return parts.join(' · ');
}

/**
 * Frame geometry for the PNG export: the content box that is actually painted,
 * padded. Card widths are estimates and labels/rings can spill past them, so a
 * frame derived from the layout's nominal totals cropped the right and bottom
 * edges of the image.
 */
export function exportFrame(
  bbox: { x: number; y: number; width: number; height: number } | null,
  fallback: { w: number; h: number },
  pad = EXPORT_PAD,
): { w: number; h: number; dx: number; dy: number } {
  const measured = bbox
    && Number.isFinite(bbox.x) && Number.isFinite(bbox.y)
    && Number.isFinite(bbox.width) && Number.isFinite(bbox.height)
    && (bbox.width > 0 || bbox.height > 0)
    ? bbox
    : null;
  const x = measured?.x ?? 0;
  const y = measured?.y ?? 0;
  const contentW = measured?.width ?? fallback.w;
  const contentH = measured?.height ?? fallback.h;
  return {
    w: Math.max(1, Math.round(contentW + pad * 2)),
    h: Math.max(1, Math.round(contentH + pad * 2)),
    dx: pad - x,
    dy: pad - y,
  };
}

/** Nested <svg> watermark with the Isola logo, stamped into the export's padding. */
function buildLogoStamp(frameW: number, frameH: number): SVGSVGElement {
  const size = LOGO_SIZE; // logical px in export space (scaled ×eff at raster time)
  // `LOGO_SIZE + LOGO_MARGIN` stays inside EXPORT_PAD, so the mark sits in the
  // padding band instead of covering the graph the way a corner stamp did.
  const inset = size + LOGO_MARGIN;
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('x', String(Math.max(LOGO_MARGIN, frameW - inset)));
  svg.setAttribute('y', String(Math.max(LOGO_MARGIN, frameH - inset)));
  svg.setAttribute('width', String(size));
  svg.setAttribute('height', String(size));
  svg.setAttribute('viewBox', ISOLA_LOGO_VIEWBOX);
  svg.setAttribute('overflow', 'visible');
  const g = document.createElementNS(SVG_NS, 'g');
  g.setAttribute('transform', ISOLA_LOGO_CENTER);
  const append = (p: { d: string; fill?: string; stroke?: string; strokeWidth?: number }) => {
    const el = document.createElementNS(SVG_NS, 'path');
    el.setAttribute('d', p.d);
    if (p.fill) el.setAttribute('fill', p.fill);
    if (p.stroke) {
      el.setAttribute('fill', 'none');
      el.setAttribute('stroke', p.stroke);
      el.setAttribute('stroke-width', String(p.strokeWidth));
      el.setAttribute('stroke-linejoin', 'miter');
    }
    g.appendChild(el);
  };
  ISOLA_LOGO_FACE_PATHS.forEach(append);
  ISOLA_LOGO_RING_PATHS.forEach(append);
  ISOLA_LOGO_TOP_PATHS.forEach(append);
  svg.appendChild(g);
  return svg;
}

/** Caption + watermark, drawn in the export's bottom padding. */
function buildExportFooter(
  frameW: number,
  frameH: number,
  caption: string,
  color: string,
): SVGGElement {
  const footer = document.createElementNS(SVG_NS, 'g');
  if (caption) {
    const text = document.createElementNS(SVG_NS, 'text');
    text.setAttribute('x', String(EXPORT_PAD));
    text.setAttribute('y', String(frameH - 30));
    text.setAttribute('font-size', '12');
    text.setAttribute('font-family', 'ui-monospace, SFMono-Regular, Menlo, monospace');
    text.setAttribute('fill', color);
    text.textContent = caption;
    footer.appendChild(text);
  }
  footer.appendChild(buildLogoStamp(frameW, frameH));
  return footer;
}

interface View {
  z: number;
  x: number;
  y: number;
}

interface Props {
  entries: NetEntry[];
  /** Host of the page the user opened. Stays the origin after its request leaves the capture window. */
  originHost?: string;
  onSelect?: (url: string | null) => void;
}

export function ResourceGraph({ entries, originHost, onSelect }: Props) {
  const light = useDocumentLight();
  const pal = GRAPH_PALETTE[light ? 'light' : 'dark'];
  const accentOf = useCallback(
    (cls: ResourceClass) => (light ? CLASS_ACCENT_LIGHT : CLASS_ACCENT)[cls],
    [light],
  );
  const classBgOf = useCallback(
    (cls: ResourceClass) => (light ? CLASS_BG_LIGHT : CLASS_BG)[cls],
    [light],
  );
  const containerRef = useRef<HTMLDivElement>(null);
  const svgRef = useRef<SVGSVGElement>(null);
  const [size, setSize] = useState({ w: 800, h: 500 });
  const [hoverId, setHoverId] = useState<string | null>(null);
  const [focusedId, setFocusedId] = useState<string | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  // A hover tooltip must outlive the pointer leaving the node by enough to
  // cross the gap into the card, or its copy buttons vanish before the
  // pointer can reach them.
  const hoverClearRef = useRef<number | null>(null);
  const cancelHoverClear = useCallback(() => {
    if (hoverClearRef.current !== null) window.clearTimeout(hoverClearRef.current);
    hoverClearRef.current = null;
  }, []);
  const scheduleHoverClear = useCallback((id: string) => {
    cancelHoverClear();
    hoverClearRef.current = window.setTimeout(() => {
      hoverClearRef.current = null;
      setHoverId((h) => (h === id ? null : h));
    }, 200);
  }, [cancelHoverClear]);
  useEffect(() => () => cancelHoverClear(), [cancelHoverClear]);
  const [typeFilter, setTypeFilter] = useState<ReadonlySet<ResourceClass>>(() => new Set());
  // Copy confirmation is scoped to the node that was copied: a global flag
  // showed ✓ on the next node's button too.
  const [copied, setCopied] = useState<string | null>(null);
  const copiedResetRef = useRef<number | null>(null);
  const markCopied = useCallback((key: string) => {
    setCopied(key);
    if (copiedResetRef.current !== null) window.clearTimeout(copiedResetRef.current);
    copiedResetRef.current = window.setTimeout(() => {
      copiedResetRef.current = null;
      setCopied(null);
    }, 1500);
  }, []);
  useEffect(() => () => {
    if (copiedResetRef.current !== null) window.clearTimeout(copiedResetRef.current);
  }, []);
  const [view, setView] = useState<View>({ z: 1, x: 0, y: 0 });
  const [panning, setPanning] = useState(false);
  const [exporting, setExporting] = useState(false);
  const [direction, setDirection] = useState<LayoutDirection>(() => {
    try { return localStorage.getItem(LS_GRAPH_DIRECTION) === 'vertical' ? 'vertical' : 'horizontal'; } catch { return 'horizontal'; }
  });
  const [expansion, setExpansion] = useState<GraphExpansion>(() => ({
    hosts: new Set(),
    groups: new Set(),
    all: new Set(),
  }));
  const [onlyFailed, setOnlyFailed] = useState(false);
  const [notice, setNotice] = useState('');
  const autoExpandedOriginRef = useRef<string | null>(null);
  const minimapRef = useRef<HTMLDivElement>(null);
  const minimapDragRef = useRef(false);

  const viewRef = useRef<View>(view);
  const setV = useCallback((v: View) => {
    viewRef.current = v;
    setView(v);
  }, []);

  const pointersRef = useRef<Map<number, { x: number; y: number }>>(new Map());
  const panRef = useRef<{ sx: number; sy: number; px: number; py: number } | null>(null);
  const clickStartRef = useRef<{ x: number; y: number } | null>(null);
  const pinchRef = useRef<{ dist: number; mx: number; my: number; px: number; py: number; z: number } | null>(null);
  const userAdjustedRef = useRef(false);
  const lastFitKeyRef = useRef('');

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const ro = new ResizeObserver((entries) => {
      for (const e of entries) {
        const cr = e.contentRect;
        setSize({ w: cr.width, h: cr.height });
      }
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  useEffect(() => {
    try { localStorage.setItem(LS_GRAPH_DIRECTION, direction); } catch {}
  }, [direction]);

  const groupByType = !onlyFailed && typeFilter.size !== 1;

  const { layout, graph, viewGraph, fullGraph } = useMemo(() => {
    // `fullGraph` keeps every captured type so the legend's counts stay stable
    // while `graph` is what the current filter / failed spotlight draws.
    const full = buildGraph(entries, originHost);
    const source = onlyFailed ? entries.filter((e) => e.f) : entries;
    const g = buildGraph(source, originHost, typeFilter);
    // A single-type facet is already an explicit “show only this kind” drill
    // down, and a failed spotlight is a filtered leaf view: both bypass the
    // summary grouping. Every other view is summary-first.
    const shaped = !onlyFailed && typeFilter.size !== 1 ? applyExpansion(g, expansion) : g;
    return {
      graph: g,
      viewGraph: shaped,
      fullGraph: full,
      layout: layoutGraph(shaped, size.w, size.h, direction),
    };
  }, [entries, originHost, size.w, size.h, direction, typeFilter, onlyFailed, expansion]);

  // Opening a page (or navigating to another origin) starts from the same
  // summary: the opened page's host is expanded, every type group is closed.
  useEffect(() => {
    const originId = fullGraph.originId;
    if (!originId || autoExpandedOriginRef.current === originId) return;
    autoExpandedOriginRef.current = originId;
    setExpansion({ hosts: new Set([originId]), groups: new Set(), all: new Set() });
  }, [fullGraph.originId]);

  const failedAvailable = useMemo(
    () => fullGraph.nodes.some((n) => n.kind === 'resource' && n.failed),
    [fullGraph],
  );
  useEffect(() => {
    if (onlyFailed && !failedAvailable) setOnlyFailed(false);
  }, [onlyFailed, failedAvailable]);

  const vertical = direction === 'vertical';

  // Toggling a type is a filter, not a spotlight: the graph redraws without the
  // other kinds, so re-frame it (a stale manual zoom would crop the new set).
  const toggleType = useCallback((cls: ResourceClass) => {
    setTypeFilter((prev) => {
      const next = new Set(prev);
      if (next.has(cls)) next.delete(cls);
      else next.add(cls);
      return next;
    });
    setSelectedId(null);
    userAdjustedRef.current = true;
  }, []);

  const clearTypes = useCallback(() => {
    setTypeFilter(new Set());
    setSelectedId(null);
    userAdjustedRef.current = true;
  }, []);

  const toggleHost = useCallback((id: string) => {
    setExpansion((prev) => ({ ...prev, hosts: toggled(prev.hosts, id) }));
    setSelectedId(null);
    userAdjustedRef.current = true;
  }, []);

  const toggleGroup = useCallback((id: string) => {
    const open = expansion.groups.has(id);
    setExpansion((prev) => {
      if (prev.groups.has(id)) {
        return { ...prev, groups: removed(prev.groups, id), all: removed(prev.all, id) };
      }
      return { ...prev, groups: new Set(prev.groups).add(id) };
    });
    // Opening a group keeps its ancestors and revealed files lit while the
    // rest fades; closing it removes the fade.
    setSelectedId(open ? null : id);
    userAdjustedRef.current = true;
  }, [expansion.groups]);

  const expandGroup = useCallback((id: string) => {
    setExpansion((prev) => ({
      ...prev,
      groups: new Set(prev.groups).add(id),
      all: new Set(prev.all).add(id),
    }));
    setSelectedId(id);
    userAdjustedRef.current = true;
  }, []);

  const previewGroup = useCallback((id: string) => {
    setExpansion((prev) => ({ ...prev, all: removed(prev.all, id) }));
    setSelectedId(id);
    userAdjustedRef.current = true;
  }, []);

  const collapseAll = useCallback(() => {
    setExpansion({ hosts: new Set(), groups: new Set(), all: new Set() });
    setSelectedId(null);
    userAdjustedRef.current = false;
    lastFitKeyRef.current = '';
  }, []);

  const expandAll = useCallback(() => {
    setExpansion(expandAllState(graph));
    setSelectedId(null);
    userAdjustedRef.current = false;
    lastFitKeyRef.current = '';
  }, [graph]);

  const toggleFailed = useCallback(() => {
    setOnlyFailed((v) => !v);
    setSelectedId(null);
    userAdjustedRef.current = true;
  }, []);

  const noticeResetRef = useRef<number | null>(null);
  const flashNotice = useCallback((message: string) => {
    setNotice(message);
    if (noticeResetRef.current !== null) window.clearTimeout(noticeResetRef.current);
    noticeResetRef.current = window.setTimeout(() => {
      noticeResetRef.current = null;
      setNotice((n) => (n === message ? '' : n));
    }, 3200);
  }, []);

  // Switching axis re-frames the graph even if the user had zoomed manually.
  const chooseDirection = useCallback((dir: LayoutDirection) => {
    userAdjustedRef.current = false;
    lastFitKeyRef.current = '';
    setDirection(dir);
  }, []);

  const contentW = layout.totalW + 64;
  const contentH = layout.totalH + 64;

  const fit = useCallback((collapseToFit = false) => {
    if (contentW <= 0 || contentH <= 0 || size.w <= 0 || size.h <= 0) return;
    const raw = Math.min(size.w / contentW, size.h / contentH, 1.15);
    const canCollapse = groupByType && (expansion.hosts.size > 0 || expansion.groups.size > 0);
    // Fit is the “make this readable” affordance: if the only way to fit is an
    // unreadable overview, shed the disclosure state first and say so.
    if (collapseToFit && raw < 0.5 && canCollapse) {
      setExpansion({ hosts: new Set(), groups: new Set(), all: new Set() });
      flashNotice('Collapsed groups to fit');
      return;
    }
    const z = clampZ(raw);
    setV({
      z,
      x: Math.max(12, (size.w - contentW * z) / 2),
      y: Math.max(12, (size.h - contentH * z) / 2),
    });
  }, [contentW, contentH, size.w, size.h, setV, expansion.hosts, expansion.groups, groupByType, flashNotice]);

  // Fit only until the user sets a zoom. New requests change the graph size
  // constantly; refitting on that was snapping the percentage back.
  useEffect(() => {
    const key = `${layout.nodes.length}:${layout.totalW}x${layout.totalH}:${size.w}x${size.h}`;
    if (key === lastFitKeyRef.current) return;
    lastFitKeyRef.current = key;
    if (userAdjustedRef.current) return;
    fit();
  }, [fit, layout.nodes.length, layout.totalW, layout.totalH, size.w, size.h]);

  const setZoom = useCallback((z: number) => {
    const v = viewRef.current;
    const nz = clampZ(z);
    userAdjustedRef.current = true;
    if (nz === v.z || v.z === 0) return;
    const ax = size.w / 2;
    const ay = size.h / 2;
    setV({
      z: nz,
      x: ax - (ax - v.x) * (nz / v.z),
      y: ay - (ay - v.y) * (nz / v.z),
    });
  }, [setV, size.w, size.h]);

  const zoomAt = useCallback((factor: number, ax: number, ay: number) => {
    const v = viewRef.current;
    const nz = clampZ(v.z * factor);
    if (nz === v.z) return;
    userAdjustedRef.current = true;
    setV({
      z: nz,
      x: ax - (ax - v.x) * (nz / v.z),
      y: ay - (ay - v.y) * (nz / v.z),
    });
  }, [setV]);

  // Numbers for the exported image's caption. They describe the graph the user
  // is looking at (filters included), not the raw capture.
  const exportStats = useMemo(() => {
    const resources = graph.nodes.filter((n) => n.kind === 'resource');
    return {
      origin: originHost || (graph.originId ? graph.originId.replace(/^host:/, '') : ''),
      requests: graph.nodes.find((n) => n.kind === 'root')?.count ?? 0,
      hosts: graph.nodes.filter((n) => n.kind === 'host').length,
      resources: resources.length,
      failed: resources.filter((n) => n.failed).length,
    };
  }, [graph, originHost]);

  // Export the WHOLE graph (not just the visible viewport) as a hi-res PNG.
  // The live <svg> already contains every node/edge, so we clone it, neutralise
  // the pan/zoom transform, resize the frame to the painted content box, and
  // rasterise it at a multiple of the logical size.
  const downloadPng = useCallback(async (scale = 3) => {
    if (layout.nodes.length === 0 || exporting) return;
    setExporting(true);
    try {
      // Overview zoom hides resource labels; force one detailed frame before
      // cloning so an export never inherits the semantic-zoom shorthand.
      await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));
      const live = svgRef.current;
      if (!live) return;
      // Clone the live SVG as it is, including a pinned node's fade. Clearing
      // the selection first exported every node at full strength.
      //
      // The frame comes from the content group's real bounding box, not from
      // layout.totalW/totalH: card widths are estimates, labels and selection
      // rings can spill past them, and the graph is pannable, so the nominal
      // totals could leave the right/bottom edge (or a panned view) cropped.
      const content = live.querySelector<SVGGElement>('g');
      let bbox: { x: number; y: number; width: number; height: number } | null = null;
      try { bbox = content ? content.getBBox() : null; } catch { bbox = null; }
      const frame = exportFrame(bbox, { w: layout.totalW, h: layout.totalH });
      const caption = exportCaption({
        ...exportStats,
        types: [...typeFilter].map((cls) => CLASS_NAME[cls]),
        failedOnly: onlyFailed,
        at: new Date(),
      });
      // The caption runs along the bottom edge and must end before the mark, so
      // a narrow graph widens to fit rather than clipping its own provenance.
      const captionW = Math.ceil(caption.length * CAPTION_CH);
      const w = Math.max(frame.w, EXPORT_PAD + captionW + 16 + LOGO_SIZE + LOGO_MARGIN);
      const h = frame.h;

      const clone = live.cloneNode(true) as SVGSVGElement;
      clone.setAttribute('xmlns', 'http://www.w3.org/2000/svg');
      clone.setAttribute('width', String(w));
      clone.setAttribute('height', String(h));
      clone.setAttribute('viewBox', `0 0 ${w} ${h}`);
      clone.style.overflow = 'hidden';

      // Deterministic export framing: drop the interactive pan/zoom transform
      // and shift the measured box into the padded frame.
      clone.querySelector('g')?.setAttribute('transform', `translate(${frame.dx}, ${frame.dy})`);
      // Background layers were sized to the viewport — stretch them to the frame.
      clone.querySelectorAll<SVGRectElement>('rect[data-export-bg]').forEach((r) => {
        r.setAttribute('width', String(w));
        r.setAttribute('height', String(h));
      });
      // Freeze any in-flight opacity/transform transitions before rasterising.
      clone.querySelectorAll<SVGElement>('[style]').forEach((el) => {
        el.style.transition = 'none';
      });
      // Watermark and caption live in the bottom padding (appended last → they
      // paint on top of the stretched background, never over a node).
      clone.appendChild(buildExportFooter(w, h, caption, pal.neutral));

      const xml = new XMLSerializer().serializeToString(clone);
      const svgUrl = URL.createObjectURL(new Blob([xml], { type: 'image/svg+xml;charset=utf-8' }));
      const img = new Image();
      const loaded = new Promise<void>((resolve, reject) => {
        img.onload = () => resolve();
        img.onerror = () => reject(new Error('failed to rasterise graph'));
      });
      img.src = svgUrl;
      await loaded;

      // Cap the long edge so a huge graph can't blow past canvas limits.
      const eff = Math.min(scale, 8192 / Math.max(w, h));
      const canvas = document.createElement('canvas');
      canvas.width = Math.max(1, Math.round(w * eff));
      canvas.height = Math.max(1, Math.round(h * eff));
      const ctx = canvas.getContext('2d');
      if (ctx) {
        ctx.fillStyle = pal.canvas;
        ctx.fillRect(0, 0, canvas.width, canvas.height);
        ctx.drawImage(img, 0, 0, canvas.width, canvas.height);
      }
      URL.revokeObjectURL(svgUrl);

      const blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, 'image/png'));
      if (!blob) return;
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = `isola-resource-graph-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-')}.png`;
      document.body.appendChild(a);
      a.click();
      a.remove();
      window.setTimeout(() => URL.revokeObjectURL(a.href), 10_000);
    } finally {
      setExporting(false);
    }
  }, [layout.nodes.length, layout.totalW, layout.totalH, exporting, pal.canvas, pal.neutral, exportStats, typeFilter, onlyFailed]);

  // Non-passive wheel: zoom to cursor.
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      e.preventDefault();
      const rect = el.getBoundingClientRect();
      const factor = Math.exp(-e.deltaY * 0.0016);
      zoomAt(factor, e.clientX - rect.left, e.clientY - rect.top);
    };
    el.addEventListener('wheel', onWheel, { passive: false });
    return () => el.removeEventListener('wheel', onWheel);
  }, [zoomAt]);

  // Escape clears hover, pinned selection + filter.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      setHoverId(null);
      setSelectedId(null);
      setTypeFilter(new Set());
      setOnlyFailed(false);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  const localPos = (e: React.PointerEvent) => {
    const rect = containerRef.current!.getBoundingClientRect();
    return { x: e.clientX - rect.left, y: e.clientY - rect.top };
  };

  const panBy = useCallback((dx: number, dy: number) => {
    const v = viewRef.current;
    userAdjustedRef.current = true;
    setV({ z: v.z, x: v.x + dx, y: v.y + dy });
  }, [setV]);

  // Keyboard access to the canvas: arrows pan, f/0 fit, +/- zoom. The
  // container is focusable and gains focus on the first pointer press.
  const onCanvasKey = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const step = e.shiftKey ? 220 : 80;
    if (e.key === 'ArrowLeft') { e.preventDefault(); panBy(step, 0); }
    else if (e.key === 'ArrowRight') { e.preventDefault(); panBy(-step, 0); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); panBy(0, step); }
    else if (e.key === 'ArrowDown') { e.preventDefault(); panBy(0, -step); }
    else if (e.key === 'f' || e.key === 'F' || e.key === '0') {
      e.preventDefault();
      userAdjustedRef.current = false;
      fit();
    } else if (e.key === '+' || e.key === '=') {
      e.preventDefault();
      zoomAt(1.2, size.w / 2, size.h / 2);
    } else if (e.key === '-' || e.key === '_') {
      e.preventDefault();
      zoomAt(0.83, size.w / 2, size.h / 2);
    }
  };

  const onPointerDown = (e: React.PointerEvent) => {
    // The tooltip's own controls must not be swallowed by canvas panning:
    // capturing the pointer here would retarget the click away from them.
    if ((e.target as Element).closest('[data-graph-tooltip]')) return;
    if ((e.target as Element).closest('[data-node]') && e.pointerType === 'mouse') return;
    const el = containerRef.current!;
    el.focus({ preventScroll: true });
    el.setPointerCapture(e.pointerId);
    const p = localPos(e);
    pointersRef.current.set(e.pointerId, p);
    if (pointersRef.current.size === 1) {
      const v = viewRef.current;
      clickStartRef.current = p;
      panRef.current = { sx: p.x, sy: p.y, px: v.x, py: v.y };
      setPanning(true);
    } else if (pointersRef.current.size === 2) {
      const [a, b] = [...pointersRef.current.values()];
      const v = viewRef.current;
      pinchRef.current = {
        dist: Math.hypot(a.x - b.x, a.y - b.y) || 1,
        mx: (a.x + b.x) / 2,
        my: (a.y + b.y) / 2,
        px: v.x, py: v.y, z: v.z,
      };
      panRef.current = null;
    }
  };

  const onPointerMove = (e: React.PointerEvent) => {
    if (!pointersRef.current.has(e.pointerId)) return;
    const p = localPos(e);
    pointersRef.current.set(e.pointerId, p);
    if (pointersRef.current.size >= 2 && pinchRef.current) {
      const [a, b] = [...pointersRef.current.values()];
      const pin = pinchRef.current;
      const dist = Math.hypot(a.x - b.x, a.y - b.y) || 1;
      const nz = clampZ(pin.z * (dist / pin.dist));
      const wx = (pin.mx - pin.px) / pin.z;
      const wy = (pin.my - pin.py) / pin.z;
      const mx = (a.x + b.x) / 2;
      const my = (a.y + b.y) / 2;
      userAdjustedRef.current = true;
      setV({ z: nz, x: mx - wx * nz, y: my - wy * nz });
    } else if (panRef.current) {
      const d = panRef.current;
      userAdjustedRef.current = true;
      setV({ z: viewRef.current.z, x: d.px + (p.x - d.sx), y: d.py + (p.y - d.sy) });
    }
  };

  const endPointer = (e: React.PointerEvent) => {
    pointersRef.current.delete(e.pointerId);
    if (pointersRef.current.size === 0) {
      panRef.current = null;
      pinchRef.current = null;
      setPanning(false);
    } else if (pointersRef.current.size === 1) {
      pinchRef.current = null;
      const [p] = [...pointersRef.current.values()];
      const v = viewRef.current;
      panRef.current = { sx: p.x, sy: p.y, px: v.x, py: v.y };
    }
  };

  const stats = useMemo(() => {
    const hosts = graph.nodes.filter((n) => n.kind === 'host').length;
    const resources = graph.nodes.filter((n) => n.kind === 'resource').length;
    const flagged = graph.nodes.filter((n) => n.flagged).length;
    const failed = graph.nodes.filter((n) => n.failed).length;
    return { hosts, resources, flagged, failed };
  }, [graph]);

  // The legend reflects every captured type (not the filtered view) so a type
  // that was filtered out can always be turned back on.
  const classLegend = useMemo(() => {
    const counts = new Map<ResourceClass, number>();
    for (const n of fullGraph.nodes) {
      if (n.kind === 'resource' && n.resourceClass) {
        counts.set(n.resourceClass, (counts.get(n.resourceClass) || 0) + 1);
      }
    }
    return CLASS_ORDER.filter((c) => counts.has(c)).map((c) => [c, counts.get(c) as number] as const);
  }, [fullGraph]);

  // Hover takes precedence while active; a pinned (clicked) node keeps the
  // emphasis when the pointer moves away.
  const focusId = hoverId ?? selectedId;

  const emphasis = useMemo(
    () => focusEmphasis(
      focusId,
      layout.edges.map((e) => ({ id: e.id, from: e.from.id, to: e.to.id })),
    ),
    [focusId, layout.edges],
  );

  // Drop a pinned selection if its node disappeared from the graph.
  useEffect(() => {
    if (selectedId && !layout.nodes.some((n) => n.id === selectedId)) setSelectedId(null);
  }, [selectedId, layout.nodes]);

  useEffect(() => {
    if (focusedId && !layout.nodes.some((n) => n.id === focusedId)) setFocusedId(null);
  }, [focusedId, layout.nodes]);

  const compact = size.w < 640;
  const tiny = size.w < 430;
  const hasExpansion = expansion.hosts.size > 0 || expansion.groups.size > 0;
  const canGroup = groupByType && graph.nodes.some((n) => n.kind === 'resource');
  const focusNode = focusId ? layout.nodes.find((n) => n.id === focusId) : null;
  // Screen-only shorthand: below 50% resources collapse to empty class bars.
  // The PNG exporter sets `exporting`, which forces full cards for every node.
  const overview = view.z < LOD_ZOOM && !exporting;

  const nodeDim = (n: LayoutNode): number => {
    if (focusId && !emphasis.nodes.has(n.id)) return 0.14;
    return 1;
  };

  const edgeStyle = (e: LayoutEdge) => {
    const related = emphasis.edges.has(e.id);
    const failedTarget = e.to.node.kind === 'resource' && e.to.node.failed;
    // The `[request]` → origin link is the spine of the graph: solid cyan so
    // the eye follows it from the entry block into the site.
    const isEntry = e.from.node.kind === 'root';
    const isMoreEdge = e.to.node.kind === 'more';
    const color = failedTarget ? pal.error : isEntry ? pal.edgeEntry : isMoreEdge ? pal.neutral : e.direct ? pal.edgeDirect : pal.edgeCross;
    const dim = focusId && !related;
    const solid = isEntry || e.direct;
    const opacity = dim ? 0.05 : related ? 0.95 : isMoreEdge ? 0.3 : isEntry ? 0.75 : e.direct ? 0.5 : 0.28;
    return {
      color,
      opacity,
      width: (isEntry ? 1.8 : e.direct ? 1.2 : 1) + (related ? 0.7 : 0),
      dash: solid ? undefined : '6 5',
      // Trigger lines are structure, not activity: animate them only while the
      // hovered/focused path is actually being read.
      animate: !isMoreEdge && related && !solid,
      fast: related,
    };
  };

  const renderEdge = (e: LayoutEdge) => {
    const s = edgeStyle(e);
    // Edges leave the downstream face of the source and meet the upstream face
    // of the target — right→left when horizontal, bottom→top when vertical.
    let d: string;
    let head: string;
    if (vertical) {
      const x1 = e.from.x + e.from.w / 2;
      const y1 = e.from.y + e.from.h;
      const x2 = e.to.x + e.to.w / 2;
      const y2 = e.to.y - 6;
      const mid = y1 + (y2 - y1) / 2;
      d = `M ${x1} ${y1} C ${x1} ${mid}, ${x2} ${mid}, ${x2} ${y2}`;
      head = `${x2},${y2} ${x2 - 3.5},${y2 - 7} ${x2 + 3.5},${y2 - 7}`;
    } else {
      const x1 = e.from.x + e.from.w;
      const y1 = e.from.y + e.from.h / 2;
      const x2 = e.to.x - 6;
      const y2 = e.to.y + e.to.h / 2;
      const mid = x1 + (x2 - x1) / 2;
      d = `M ${x1} ${y1} C ${mid} ${y1}, ${mid} ${y2}, ${x2} ${y2}`;
      head = `${x2},${y2} ${x2 - 7},${y2 - 3.5} ${x2 - 7},${y2 + 3.5}`;
    }

    return (
      <g key={e.id}>
        <path
          d={d}
          stroke={s.color}
          strokeWidth={s.width}
          strokeDasharray={s.dash}
          fill="none"
          opacity={s.opacity}
          className={s.animate ? (s.fast ? 'rg-flow rg-flow-fast' : 'rg-flow') : undefined}
          strokeLinecap="round"
        />
        {!s.dash && (
          <polygon
            points={head}
            fill={s.color}
            opacity={s.opacity * 0.9}
          />
        )}
      </g>
    );
  };

  const renderRootNode = (n: LayoutNode) => {
    const { node } = n;
    const dim = nodeDim(n);
    const selected = selectedId === n.id;
    const accent = pal.edgeEntry;

    return (
      <g key={n.id} data-node transform={`translate(${n.x}, ${n.y})`} opacity={dim}
        style={{ cursor: 'pointer', transition: 'opacity 150ms ease' }}
        onMouseEnter={() => { cancelHoverClear(); setHoverId(n.id); }}
        onMouseLeave={() => scheduleHoverClear(n.id)}
        onClick={() => { setSelectedId((s) => (s === n.id ? null : n.id)); }}
      >
        <g className="rg-node" style={{ filter: selected ? 'drop-shadow(0 0 12px rgba(34, 211, 238, 0.5))' : 'drop-shadow(0 0 8px rgba(34, 211, 238, 0.3))' }}>
          {selected && (
            <rect x={-7.5} y={-7.5} width={n.w + 15} height={n.h + 15} rx={15} fill="none" stroke={accent} strokeOpacity={0.65} strokeWidth={1.2} strokeDasharray="5 4" />
          )}
          <rect x={-4} y={-4} width={n.w + 8} height={n.h + 8} rx={13} fill="rgba(34, 211, 238, 0.06)" stroke="rgba(34, 211, 238, 0.28)" strokeWidth={1} />
          <rect width={n.w} height={n.h} rx={10} fill={pal.cardRoot} stroke={accent} strokeOpacity={0.9} strokeWidth={1.6} />
          <rect x={0.5} y={6} width={3} height={n.h - 12} rx={1.5} fill={accent} opacity={0.85} />
          <g transform={`translate(13, ${n.h / 2 - 9})`}>
            <rect width={22} height={18} rx={5} fill={`${accent}16`} stroke={accent} strokeOpacity={0.5} strokeWidth={0.8} />
            <text x={11} y={12.5} textAnchor="middle" fontSize={10} fontWeight={700} fill={accent} fontFamily="ui-monospace, SFMono-Regular, Menlo, monospace">
              ▶
            </text>
          </g>
          <text x={45} y={n.h / 2 + 4} fontSize={12.5} fontWeight={700} fill={pal.labelRoot} fontFamily="ui-monospace, SFMono-Regular, Menlo, monospace" letterSpacing={0.3}>
            {node.label}
          </text>
          <g transform={`translate(${n.w - 38}, ${n.h / 2 - 9})`}>
            <rect width={26} height={18} rx={5} fill={pal.badgeBg} />
            <text x={13} y={12.5} textAnchor="middle" fontSize={10.5} fontWeight={600} fill={pal.badgeText} fontFamily="ui-monospace, monospace">
              {node.count}
            </text>
          </g>
        </g>
      </g>
    );
  };

  const renderHostNode = (n: LayoutNode) => {
    const { node } = n;
    const isOrigin = n.id === viewGraph.originId;
    const dim = nodeDim(n);
    const selected = selectedId === n.id;
    const focused = focusedId === n.id;
    const expandable = groupByType && (node.resourceCount ?? 0) > 0;
    const open = expansion.hosts.has(n.id);
    const accent = node.failed ? pal.error : node.flagged ? pal.warn : isOrigin ? pal.edgeEntry : pal.neutral;
    const bg = isOrigin
      ? pal.cardRoot
      : node.failed
        ? pal.cardHostFailed
        : node.flagged
          ? pal.cardHostFlagged
          : pal.cardHost;
    const label = node.label.length > 30 ? node.label.slice(0, 28) + '…' : node.label;
    const glow = selected
      ? `drop-shadow(0 0 12px ${node.failed ? 'rgba(239, 68, 68, 0.5)' : node.flagged ? 'rgba(245, 158, 11, 0.5)' : 'rgba(34, 211, 238, 0.45)'})`
      : node.failed
      ? 'drop-shadow(0 0 6px rgba(239, 68, 68, 0.35))'
      : isOrigin
        ? 'drop-shadow(0 0 7px rgba(34, 211, 238, 0.28))'
        : undefined;

    return (
      <g key={n.id} data-node transform={`translate(${n.x}, ${n.y})`} opacity={dim}
        style={{ cursor: 'pointer', transition: 'opacity 150ms ease' }}
        role={expandable ? 'button' : undefined}
        tabIndex={expandable ? 0 : undefined}
        aria-expanded={expandable ? open : undefined}
        aria-label={`${node.host}: ${node.count} requests, ${node.resourceCount || 0} resources, ${open ? 'expanded' : 'collapsed'}`}
        onMouseEnter={() => { cancelHoverClear(); setHoverId(n.id); }}
        onMouseLeave={() => scheduleHoverClear(n.id)}
        onFocus={() => { cancelHoverClear(); setFocusedId(n.id); setHoverId(n.id); }}
        onBlur={() => { setFocusedId(null); scheduleHoverClear(n.id); }}
        onKeyDown={expandable ? (ev) => {
          if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); toggleHost(n.id); }
        } : undefined}
        onClick={() => {
          if (expandable) toggleHost(n.id);
          else setSelectedId((s) => (s === n.id ? null : n.id));
        }}
      >
        <g className="rg-node" style={{ filter: glow }}>
          {(selected || focused) && (
            <rect x={-7.5} y={-7.5} width={n.w + 15} height={n.h + 15} rx={15} fill="none" stroke={accent} strokeOpacity={0.65} strokeWidth={1.2} strokeDasharray="5 4" />
          )}
          {isOrigin && (
            <rect x={-4} y={-4} width={n.w + 8} height={n.h + 8} rx={13} fill="rgba(34, 211, 238, 0.06)" stroke="rgba(34, 211, 238, 0.25)" strokeWidth={1} />
          )}
          <rect width={n.w} height={n.h} rx={10} fill={bg} stroke={accent} strokeOpacity={isOrigin ? 0.9 : 0.55} strokeWidth={isOrigin ? 1.6 : 1.1} />
          <rect x={0.5} y={6} width={3} height={n.h - 12} rx={1.5} fill={accent} opacity={0.85} />
          <g transform={`translate(13, ${n.h / 2 - 9})`}>
            <rect width={22} height={18} rx={5} fill={`${accent}16`} stroke={accent} strokeOpacity={0.5} strokeWidth={0.8} />
            <text x={11} y={12.5} textAnchor="middle" fontSize={10} fontWeight={700} fill={accent} fontFamily="ui-monospace, SFMono-Regular, Menlo, monospace">
              {isOrigin ? '◆' : 'H'}
            </text>
          </g>
          <text x={45} y={n.h / 2 + 4} fontSize={12.5} fontWeight={isOrigin ? 700 : 600} fill={isOrigin ? pal.labelHostOrigin : pal.labelHost} fontFamily="ui-sans-serif, system-ui, -apple-system">
            {label}
          </text>
          <g transform={`translate(${n.w - 38}, ${n.h / 2 - 9})`}>
            <rect width={26} height={18} rx={5} fill={pal.badgeBg} />
            <text x={13} y={12.5} textAnchor="middle" fontSize={10.5} fontWeight={600} fill={pal.badgeText} fontFamily="ui-monospace, monospace">
              {node.count}
            </text>
          </g>
          {expandable && (
            <g transform={`translate(${n.w - 58}, ${n.h / 2 - 5})`}>
              <path
                d={open ? 'M0 1 L5 5.5 L10 1' : 'M1 0 L6 5 L1 10'}
                fill="none" stroke={pal.labelHost} strokeOpacity={0.65} strokeWidth={1.5}
                strokeLinecap="round" strokeLinejoin="round"
              />
            </g>
          )}
          {node.flagged && (
            <g transform={`translate(${n.w - 104}, ${n.h / 2 - 9})`}>
              <rect width={20} height={18} rx={5} fill="rgba(245, 158, 11, 0.14)" />
              <text x={10} y={12.5} textAnchor="middle" fontSize={10} fill={pal.warn} fontFamily="ui-monospace, monospace">⚑</text>
            </g>
          )}
          {node.failed && (
            <g transform={`translate(${n.w - 82}, ${n.h / 2 - 9})`}>
              <rect width={20} height={18} rx={5} fill="rgba(239, 68, 68, 0.14)" />
              <text x={10} y={12.5} textAnchor="middle" fontSize={10} fill={pal.error} fontFamily="ui-monospace, monospace">✕</text>
            </g>
          )}
        </g>
      </g>
    );
  };

  const renderGroupNode = (n: LayoutNode) => {
    const { node } = n;
    const cls = node.resourceClass || 'other';
    const accent = accentOf(cls);
    const dim = nodeDim(n);
    const selected = selectedId === n.id;
    const open = expansion.groups.has(n.id);
    const focused = focusedId === n.id;
    const failedCount = node.failedCount || 0;
    const flaggedCount = node.flaggedCount || 0;
    return (
      <g
        key={n.id}
        data-node
        transform={`translate(${n.x}, ${n.y})`}
        opacity={dim}
        style={{ cursor: 'pointer', transition: 'opacity 150ms ease' }}
        role="button"
        tabIndex={0}
        aria-expanded={open}
        aria-label={`${node.label} on ${node.host}: ${node.count} resources, ${open ? 'expanded' : 'collapsed'}`}
        onMouseEnter={() => { cancelHoverClear(); setHoverId(n.id); }}
        onMouseLeave={() => scheduleHoverClear(n.id)}
        onFocus={() => { cancelHoverClear(); setFocusedId(n.id); setHoverId(n.id); }}
        onBlur={() => { setFocusedId(null); scheduleHoverClear(n.id); }}
        onKeyDown={(ev) => {
          if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); toggleGroup(n.id); }
        }}
        onClick={() => toggleGroup(n.id)}
      >
        <g className="rg-node" style={{ filter: node.failed ? 'drop-shadow(0 0 5px rgba(239, 68, 68, 0.25))' : undefined }}>
          {(selected || focused) && (
            <rect x={-6.5} y={-6.5} width={n.w + 13} height={n.h + 13} rx={11} fill="none" stroke={accent} strokeOpacity={0.65} strokeWidth={1.2} strokeDasharray="5 4" />
          )}
          <rect width={n.w} height={n.h} rx={8} fill={classBgOf(cls)} stroke={node.failed ? pal.error : accent} strokeOpacity={0.55} strokeWidth={1} />
          <rect x={0.5} y={5} width={3} height={n.h - 10} rx={1.5} fill={accent} opacity={0.9} />
          <circle cx={18} cy={n.h / 2} r={3.5} fill={accent} />
          <text x={29} y={n.h / 2 + 3.5} fontSize={11.5} fontWeight={600} fill={pal.labelRes} fontFamily="ui-sans-serif, system-ui">
            {node.label}
          </text>
          <g transform={`translate(${n.w - 38}, ${n.h / 2 - 8})`}>
            <rect width={26} height={16} rx={4} fill={pal.badgeBg} />
            <text x={13} y={11.5} textAnchor="middle" fontSize={10} fontWeight={600} fill={pal.badgeText} fontFamily="ui-monospace, monospace">
              {node.count}
            </text>
          </g>
          <g transform={`translate(${n.w - 56}, ${n.h / 2 - 5})`}>
            <path
              d={open ? 'M0 1 L5 5.5 L10 1' : 'M1 0 L6 5 L1 10'}
              fill="none" stroke={pal.neutral} strokeOpacity={0.7} strokeWidth={1.4}
              strokeLinecap="round" strokeLinejoin="round"
            />
          </g>
          {flaggedCount > 0 && (
            <g transform={`translate(${n.w - 106}, ${n.h / 2 - 8})`}>
              <rect width={22} height={16} rx={4} fill="rgba(245, 158, 11, 0.14)" />
              <text x={11} y={11.5} textAnchor="middle" fontSize={9} fill={pal.warn} fontFamily="ui-monospace, monospace">{flaggedCount}</text>
            </g>
          )}
          {failedCount > 0 && (
            <g transform={`translate(${n.w - 81}, ${n.h / 2 - 8})`}>
              <rect width={22} height={16} rx={4} fill="rgba(239, 68, 68, 0.14)" />
              <text x={11} y={11.5} textAnchor="middle" fontSize={9} fill={pal.error} fontFamily="ui-monospace, monospace">{failedCount}</text>
            </g>
          )}
        </g>
      </g>
    );
  };

  const renderMoreNode = (n: LayoutNode) => {
    const { node } = n;
    const cls = node.resourceClass || 'other';
    const accent = accentOf(cls);
    const dim = nodeDim(n);
    const expand = node.moreAction === 'expand';
    const focused = focusedId === n.id;
    const gid = n.id.replace(/^more:/, 'grp:');
    return (
      <g
        key={n.id}
        data-node
        transform={`translate(${n.x}, ${n.y})`}
        opacity={dim}
        style={{ cursor: 'pointer', transition: 'opacity 150ms ease' }}
        role="button"
        tabIndex={0}
        aria-label={expand ? `Show all ${node.count} more ${CLASS_NAME[cls]} resources` : `Show the ${GROUP_PREVIEW}-row preview`}
        onMouseEnter={() => { cancelHoverClear(); setHoverId(n.id); }}
        onMouseLeave={() => scheduleHoverClear(n.id)}
        onFocus={() => { cancelHoverClear(); setFocusedId(n.id); setHoverId(n.id); }}
        onBlur={() => { setFocusedId(null); scheduleHoverClear(n.id); }}
        onKeyDown={(ev) => {
          if (ev.key === 'Enter' || ev.key === ' ') {
            ev.preventDefault();
            if (expand) expandGroup(gid); else previewGroup(gid);
          }
        }}
        onClick={() => { if (expand) expandGroup(gid); else previewGroup(gid); }}
      >
        {focused && (
          <rect x={-6.5} y={-6.5} width={n.w + 13} height={n.h + 13} rx={11} fill="none" stroke={accent} strokeOpacity={0.7} strokeWidth={1.2} strokeDasharray="5 4" />
        )}
        <rect width={n.w} height={n.h} rx={8} fill="transparent" stroke={accent} strokeOpacity={0.4} strokeWidth={1} strokeDasharray="4 4" />
        <text x={12} y={n.h / 2 + 3.5} fontSize={10.5} fontWeight={500} fill={pal.neutral} fontFamily="ui-sans-serif, system-ui">
          {node.label}
        </text>
        {node.failed && <circle cx={n.w - 30} cy={n.h / 2} r={3} fill={pal.error} />}
        <text x={n.w - 12} y={n.h / 2 + 3.5} textAnchor="end" fontSize={9} fill={pal.neutral} fontFamily="ui-monospace, monospace">
          {expand ? '▾' : '▴'}
        </text>
      </g>
    );
  };

  const renderResNode = (n: LayoutNode) => {
    const { node } = n;
    const cls = node.resourceClass || 'other';
    const dim = nodeDim(n);
    const selected = selectedId === n.id;
    const accent = accentOf(cls);
    const stroke = node.failed ? pal.error : node.flagged ? pal.warn : selected ? accent : `${accent}44`;
    const label = node.label.length > 26 ? node.label.slice(0, 24) + '…' : node.label;
    const glow = node.failed ? 'drop-shadow(0 0 5px rgba(239, 68, 68, 0.3))' : undefined;
    // Overview zoom reduces resources to class-coloured bars; the tooltip and
    // a hover restore the full card, so labels are never gone, only deferred.
    const detailed = !overview || selected || hoverId === n.id;

    return (
      <g key={n.id} data-node transform={`translate(${n.x}, ${n.y})`} opacity={dim}
        style={{ cursor: 'pointer', transition: 'opacity 150ms ease' }}
        onMouseEnter={() => { cancelHoverClear(); setHoverId(n.id); }}
        onMouseLeave={() => scheduleHoverClear(n.id)}
        onClick={() => { setSelectedId((s) => (s === n.id ? null : n.id)); }}
      >
        {!detailed ? (
          <rect
            width={n.w} height={n.h} rx={8}
            fill={node.failed ? 'rgba(239, 68, 68, 0.34)' : `${accent}38`}
            stroke={node.failed ? pal.error : `${accent}99`}
            strokeWidth={1}
          />
        ) : (
          <g className="rg-node" style={{ filter: glow }}>
            {selected && (
              <rect x={-6.5} y={-6.5} width={n.w + 13} height={n.h + 13} rx={11} fill="none" stroke={accent} strokeOpacity={0.65} strokeWidth={1.2} strokeDasharray="5 4" />
            )}
            <rect width={n.w} height={n.h} rx={8} fill={classBgOf(cls)} stroke={stroke} strokeWidth={node.failed || node.flagged ? 1.3 : 1} />
            <g transform={`translate(9, ${n.h / 2 - 9})`}>
              <rect width={34} height={18} rx={5} fill={`${accent}1a`} stroke={accent} strokeOpacity={0.45} strokeWidth={0.8} />
              <text x={17} y={12.5} textAnchor="middle" fontSize={9} fontWeight={700} fill={accent} fontFamily="ui-monospace, monospace" letterSpacing={0.5}>
                {CLASS_LABEL[cls]}
              </text>
            </g>
            <text x={53} y={n.h / 2 + 3.5} fontSize={11.5} fontWeight={500} fill={pal.labelRes} fontFamily="ui-sans-serif, system-ui">
              {label}
            </text>
            {node.count > 1 && (
              <g transform={`translate(${n.w - 36}, ${n.h / 2 - 8})`}>
                <rect width={26} height={16} rx={4} fill={pal.badgeBg} />
                <text x={13} y={11.5} textAnchor="middle" fontSize={10} fontWeight={600} fill={pal.badgeText} fontFamily="ui-monospace, monospace">
                  ×{node.count}
                </text>
              </g>
            )}
          </g>
        )}
      </g>
    );
  };

  const tooltip = (() => {
    if (!focusNode) return null;
    // A pinned resource keeps its tooltip open for the copy buttons. A pinned
    // group only shows its card while hovered, so it cannot cover the rows it
    // just revealed.
    if (hoverId !== focusNode.id && focusNode.node.kind !== 'resource') return null;
    const { node } = focusNode;
    const isRootNode = node.kind === 'root';
    const isHost = node.kind === 'host';
    const isGroup = node.kind === 'group';
    const isMore = node.kind === 'more';
    const isResource = node.kind === 'resource';
    const cls = node.resourceClass;
    const accent = isRootNode || isHost ? pal.edgeEntry : cls ? accentOf(cls) : pal.neutral;
    const left = Math.min(size.w - 250, Math.max(8, focusNode.x * view.z + view.x + focusNode.w * view.z + 12));
    const top = Math.min(size.h - 90, Math.max(8, focusNode.y * view.z + view.y - 8));
    const url = isResource ? node.fullUrl || '' : isHost ? node.host : '';
    const shortUrl = url.length > 96 ? url.slice(0, 44) + ' … ' + url.slice(-48) : url;

    return (
      <div
        data-graph-tooltip=""
        onMouseEnter={cancelHoverClear}
        onMouseLeave={() => scheduleHoverClear(node.id)}
        className="pointer-events-auto absolute z-20 w-60 rounded-xl border border-zinc-200 bg-white/95 p-2.5 shadow-2xl shadow-black/20 backdrop-blur-sm dark:border-white/10 dark:bg-[#0c1220]/95 dark:shadow-black/60"
        style={{ left, top }}
      >
        <div className="flex items-center gap-1.5">
          {isRootNode ? (
            <span className="inline-flex h-4 items-center rounded bg-cyan-500/10 px-1.5 text-[9px] font-bold tracking-wider text-cyan-700 dark:bg-cyan-400/10 dark:text-cyan-300">REQUEST</span>
          ) : isHost ? (
            <span className="inline-flex h-4 items-center rounded bg-cyan-500/10 px-1.5 text-[9px] font-bold tracking-wider text-cyan-700 dark:bg-cyan-400/10 dark:text-cyan-300">HOST</span>
          ) : isGroup ? (
            <span className="inline-flex h-4 items-center rounded px-1.5 text-[9px] font-bold tracking-wider" style={{ backgroundColor: `${accent}1a`, color: accent }}>GROUP</span>
          ) : isMore ? (
            <span className="inline-flex h-4 items-center rounded bg-black/5 px-1.5 text-[9px] font-bold tracking-wider text-zinc-500 dark:bg-white/10 dark:text-slate-400">
              {node.moreAction === 'expand' ? 'SHOW ALL' : 'PREVIEW'}
            </span>
          ) : (
            <span className="inline-flex h-4 items-center rounded px-1.5 text-[9px] font-bold tracking-wider" style={{ backgroundColor: `${accent}1a`, color: accent }}>
              {cls ? CLASS_LABEL[cls] : 'RES'}
            </span>
          )}
          {isHost && node.id === viewGraph.originId && (
            <span className="text-[9px] font-semibold tracking-wider text-cyan-600/70 dark:text-cyan-400/70">ORIGIN</span>
          )}
          <span className="ml-auto flex items-center gap-1.5">
            {(isResource || isHost || isGroup) && node.host && (
              <button
                type="button"
                onClick={(ev) => {
                  ev.stopPropagation();
                  onSelect?.(node.host);
                }}
                title={`Show only ${node.host}`}
                aria-label={`Show only ${node.host}`}
                className="pointer-events-auto inline-flex h-4 w-4 items-center justify-center rounded border border-zinc-300 text-zinc-500 transition-colors hover:bg-zinc-200 hover:text-cyan-600 dark:border-white/10 dark:text-slate-400 dark:hover:bg-white/10 dark:hover:text-cyan-400"
              >
                <svg width={10} height={10} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2.2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
                  <path d="M22 3H2l8 9.46V19l4 2v-8.54L22 3Z" />
                </svg>
              </button>
            )}
            <span className="text-[10px] font-medium text-zinc-500 dark:text-slate-400">
              {isGroup ? `${node.count} res` : isMore ? (node.moreAction === 'expand' ? `+${node.count}` : `${node.count}`) : `×${node.count}`}
            </span>
          </span>
        </div>
        <div className="mt-1.5 break-words text-[11.5px] font-semibold leading-tight text-zinc-900 dark:text-slate-100">
          {isRootNode ? 'Entry point' : isHost ? node.host : node.label}
        </div>
        {isRootNode ? (
          <div className="mt-0.5 text-[10px] text-zinc-500 dark:text-slate-500">Everything below was triggered by this request.</div>
        ) : isGroup || isMore ? (
          <div className="mt-0.5 text-[10px] text-zinc-500 dark:text-slate-500">
            {isGroup ? `${node.count} resources on ${node.host}` : `on ${node.host}`}
          </div>
        ) : (
          !isHost && <div className="mt-0.5 text-[10px] text-zinc-500 dark:text-slate-500">{node.host}</div>
        )}
        {url && (
          <div className="mt-1 flex items-center gap-1.5">
            <code className="min-w-0 flex-1 truncate font-mono text-[9.5px] leading-relaxed text-zinc-500 dark:text-slate-500" title={url}>
              {shortUrl}
            </code>
            <button
              type="button"
              onClick={(ev) => {
                ev.stopPropagation();
                void copyToClipboard(url).then((ok) => {
                  if (!ok) return;
                  markCopied(`url:${node.id}`);
                  if (node.ut) flashNotice('Copied the truncated URL shown in this capture');
                });
              }}
              className="pointer-events-auto shrink-0 rounded border border-zinc-300 px-1 text-[9px] text-cyan-600 hover:bg-zinc-200 dark:border-white/10 dark:text-cyan-400 dark:hover:bg-white/10"
              title={node.ut ? 'Copy URL (truncated in this capture)' : 'Copy URL'}
            >
              {copied === `url:${node.id}` ? '✓' : 'copy'}
            </button>
          </div>
        )}
        {node.sha1 && (
          <div className="mt-1.5 flex items-center gap-1.5">
            <span className="shrink-0 text-[9px] uppercase tracking-wider text-zinc-400 dark:text-slate-500">sha1</span>
            <code className="min-w-0 flex-1 truncate font-mono text-[9.5px] text-zinc-600 dark:text-slate-400" title={node.sha1}>
              {node.sha1}
            </code>
            <button
              type="button"
              onClick={(ev) => {
                ev.stopPropagation();
                void copyToClipboard(node.sha1!).then((ok) => {
                  if (!ok) return;
                  markCopied(`sha1:${node.id}`);
                });
              }}
              className="pointer-events-auto shrink-0 rounded border border-zinc-300 px-1 text-[9px] text-cyan-600 hover:bg-zinc-200 dark:border-white/10 dark:text-cyan-400 dark:hover:bg-white/10"
              title="Copy SHA-1"
            >
              {copied === `sha1:${node.id}` ? '✓' : 'copy'}
            </button>
          </div>
        )}
        {(node.flagged || node.failed) && (
          <div className="mt-1.5 flex gap-1.5 text-[9.5px]">
            {node.flagged && <span className="rounded bg-amber-400/10 px-1.5 py-0.5 font-medium text-amber-600 dark:text-amber-400">⚑ {node.flaggedCount || 1} flagged</span>}
            {node.failed && <span className="rounded bg-red-400/10 px-1.5 py-0.5 font-medium text-red-600 dark:text-red-400">✕ {node.failedCount || 1} failed</span>}
          </div>
        )}
      </div>
    );
  })();

  // A minimap is the navigation aid a 2-D canvas deserves: it shows where the
  // viewport sits in the wrapped graph and can be dragged directly. It only
  // appears while the content overflows, so a fitted small graph stays clean.
  // Nodes are painted into one <canvas>: a <rect> per node added ~1000
  // elements to the DOM after "expand all", all re-created on every event.
  const minimapGeom = useMemo(() => {
    if (tiny || layout.nodes.length === 0) return null;
    const overflowX = layout.totalW * view.z > size.w * 1.02;
    const overflowY = layout.totalH * view.z > size.h * 1.02;
    if (!overflowX && !overflowY) return null;

    const mmW = 152;
    const mmH = 100;
    const pad = 6;
    const scale = Math.min(
      (mmW - pad * 2) / Math.max(1, layout.totalW),
      (mmH - pad * 2) / Math.max(1, layout.totalH),
    );
    const offX = (mmW - layout.totalW * scale) / 2;
    const offY = (mmH - layout.totalH * scale) / 2;
    return { mmW, mmH, scale, offX, offY };
  }, [tiny, layout, view.z, size.w, size.h]);

  const minimapCanvasRef = useRef<HTMLCanvasElement | null>(null);
  useEffect(() => {
    const g = minimapGeom;
    const canvas = minimapCanvasRef.current;
    if (!g || !canvas) return;
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    const { mmW, mmH, scale, offX, offY } = g;
    canvas.width = Math.round(mmW * dpr);
    canvas.height = Math.round(mmH * dpr);
    const ctx = canvas.getContext('2d');
    if (!ctx) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.fillStyle = pal.canvas;
    ctx.fillRect(0, 0, mmW, mmH);
    for (const n of layout.nodes) {
      const cls = n.node.resourceClass;
      const fill = n.node.failed
        ? pal.error
        : n.node.kind === 'host'
          ? (n.node.id === viewGraph.originId ? pal.edgeEntry : pal.neutral)
          : cls ? accentOf(cls) : pal.neutral;
      ctx.globalAlpha = n.node.kind === 'resource' || n.node.kind === 'more' ? 0.55 : 0.9;
      ctx.fillStyle = fill;
      ctx.fillRect(
        offX + n.x * scale,
        offY + n.y * scale,
        Math.max(1, n.w * scale),
        Math.max(1, n.h * scale),
      );
    }
    ctx.globalAlpha = 1;
  }, [minimapGeom, layout, viewGraph.originId, pal, accentOf]);

  const minimap = (() => {
    const g = minimapGeom;
    if (!g) return null;
    const { mmW, mmH, scale, offX, offY } = g;
    const viewX = offX + (-view.x / view.z) * scale;
    const viewY = offY + (-view.y / view.z) * scale;
    const viewW = (size.w / view.z) * scale;
    const viewH = (size.h / view.z) * scale;

    const jump = (e: React.PointerEvent) => {
      const el = minimapRef.current;
      if (!el) return;
      const rect = el.getBoundingClientRect();
      const wx = (e.clientX - rect.left - offX) / scale;
      const wy = (e.clientY - rect.top - offY) / scale;
      userAdjustedRef.current = true;
      setV({
        z: viewRef.current.z,
        x: size.w / 2 - wx * viewRef.current.z,
        y: size.h / 2 - wy * viewRef.current.z,
      });
    };

    return (
      <div
        ref={minimapRef}
        className="absolute bottom-2 right-2 z-10 cursor-crosshair touch-none overflow-hidden rounded-lg border border-zinc-300/80 bg-white/85 shadow-lg shadow-black/10 backdrop-blur-sm dark:border-white/10 dark:bg-slate-950/85"
        style={{ width: mmW, height: mmH }}
        title="Minimap — drag to move the view"
        onClick={(e) => e.stopPropagation()}
        onDoubleClick={(e) => e.stopPropagation()}
        onPointerDown={(e) => {
          e.stopPropagation();
          (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
          minimapDragRef.current = true;
          jump(e);
        }}
        onPointerMove={(e) => {
          if (!minimapDragRef.current) return;
          e.stopPropagation();
          jump(e);
        }}
        onPointerUp={(e) => {
          e.stopPropagation();
          minimapDragRef.current = false;
          (e.currentTarget as HTMLElement).releasePointerCapture(e.pointerId);
        }}
        onPointerCancel={() => { minimapDragRef.current = false; }}
      >
        <canvas
          ref={minimapCanvasRef}
          width={mmW}
          height={mmH}
          style={{ width: mmW, height: mmH, display: 'block' }}
          aria-hidden
        />
        <svg className="absolute inset-0" width={mmW} height={mmH} aria-hidden>
          <rect
            x={viewX}
            y={viewY}
            width={Math.max(2, viewW)}
            height={Math.max(2, viewH)}
            rx={2}
            fill="rgba(34, 211, 238, 0.10)"
            stroke={pal.edgeEntry}
            strokeWidth={1}
          />
        </svg>
      </div>
    );
  })();

  const chip = 'inline-flex items-center gap-1 rounded-full bg-black/5 px-2 py-0.5 text-[10px] font-medium dark:bg-white/10';

  return (
    <div className="flex h-full flex-col font-sans">
      <style>{`
        @keyframes rg-flow { to { stroke-dashoffset: -22; } }
        .rg-flow { animation: rg-flow 1.5s linear infinite; }
        .rg-flow-fast { animation-duration: 0.55s; }
        .rg-node { transition: transform 160ms cubic-bezier(0.2, 0.8, 0.3, 1); transform-box: fill-box; transform-origin: center; }
        [data-node]:hover .rg-node { transform: scale(1.025); }
        @keyframes rg-pulse { 0%, 100% { transform: scale(1); opacity: 0.5; } 50% { transform: scale(1.5); opacity: 0; } }
      `}</style>

      <div className={`flex flex-wrap items-center border-b border-zinc-200 dark:border-white/5 ${compact ? 'gap-x-2 gap-y-1 px-2 py-1' : 'gap-x-3 gap-y-1 px-2.5 py-1.5'} text-[10px] text-zinc-500 dark:text-slate-400`}>
        <span className={chip}>
          <span className="h-1.5 w-1.5 rounded-full bg-slate-400" />
          {stats.hosts} host{stats.hosts !== 1 ? 's' : ''}
        </span>
        <span className={chip}>
          <span className="h-1.5 w-1.5 rounded-full" style={{ backgroundColor: pal.edgeEntry }} />
          {stats.resources} res
        </span>
        {stats.failed > 0 && (
          <button
            onClick={toggleFailed}
            aria-pressed={onlyFailed}
            title={onlyFailed ? 'Stop showing only failed resources' : 'Show only failed resources'}
            className={`${chip} transition-all ${onlyFailed
              ? 'bg-red-500/20 text-red-700 ring-1 ring-red-400/60 dark:bg-red-400/20 dark:text-red-300'
              : 'bg-red-500/10 text-red-600 dark:bg-red-400/10 dark:text-red-400'}`}
          >
            <span className="h-1.5 w-1.5 rounded-full bg-red-400" />
            {stats.failed} failed{onlyFailed ? ' · only' : ''}
          </button>
        )}
        {stats.flagged > 0 && (
          <span className={`${chip} bg-amber-500/10 text-amber-600 dark:bg-amber-400/10 dark:text-amber-400`}>
            <span className="h-1.5 w-1.5 rounded-full bg-amber-400" />
            {stats.flagged} flagged
          </span>
        )}

        {!tiny && (
          <div className="flex flex-wrap items-center gap-x-1.5 gap-y-1">
            {classLegend.map(([cls, n]) => {
              const active = typeFilter.has(cls);
              const accent = accentOf(cls);
              return (
                <button
                  key={cls}
                  onClick={() => toggleType(cls)}
                  aria-pressed={active}
                  title={`${active ? 'Stop filtering' : 'Show only'} ${CLASS_NAME[cls]}`}
                  className="inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 transition-all"
                  style={{
                    backgroundColor: active ? `${accent}22` : 'transparent',
                    boxShadow: active ? `inset 0 0 0 1px ${accent}88` : 'none',
                    opacity: typeFilter.size > 0 && !active ? 0.45 : 1,
                  }}
                >
                  <span
                    className="inline-flex h-4 items-center justify-center rounded text-[8.5px] font-bold tracking-wide"
                    style={{
                      backgroundColor: `${accent}18`,
                      color: accent,
                      boxShadow: `inset 0 0 0 1px ${accent}44`,
                      padding: '0 4px',
                    }}
                  >
                    {CLASS_LABEL[cls]}
                  </span>
                  {!compact && <span className="text-zinc-500 dark:text-slate-500">{n}</span>}
                </button>
              );
            })}
            {typeFilter.size > 0 && (
              <button
                onClick={clearTypes}
                title="Clear the type filter"
                className="inline-flex items-center gap-1 rounded-full px-1.5 py-0.5 text-zinc-500 transition-colors hover:bg-black/5 hover:text-zinc-900 dark:text-slate-400 dark:hover:bg-white/10 dark:hover:text-slate-100"
              >
                ✕ type filter
              </button>
            )}
          </div>
        )}

        {notice && (
          <span role="status" className={`${chip} bg-cyan-500/10 text-cyan-700 dark:bg-cyan-400/10 dark:text-cyan-300`}>
            {notice}
          </span>
        )}

        <div className="ml-auto flex items-center gap-0.5">
          {canGroup && (
            <div className="mr-1 inline-flex overflow-hidden rounded-md border border-zinc-300 dark:border-white/10" role="group" aria-label="Graph expansion">
              <button
                onClick={collapseAll}
                disabled={!hasExpansion}
                aria-label="Collapse all hosts and groups"
                title="Collapse all hosts and groups"
                className="px-1.5 py-0.5 text-zinc-500 transition-colors hover:bg-black/5 hover:text-zinc-900 disabled:cursor-default disabled:opacity-35 disabled:hover:bg-transparent dark:text-slate-400 dark:hover:bg-white/10 dark:hover:text-slate-100 dark:disabled:hover:text-slate-400"
              >
                <svg width={12} height={12} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2.2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
                  <path d="M7 4l5 5 5-5" />
                  <path d="M7 20l5-5 5 5" />
                </svg>
              </button>
              <button
                onClick={expandAll}
                aria-label="Expand all hosts and groups"
                title="Expand all hosts and groups"
                className="border-l border-zinc-300 px-1.5 py-0.5 text-zinc-500 transition-colors hover:bg-black/5 hover:text-zinc-900 dark:border-white/10 dark:text-slate-400 dark:hover:bg-white/10 dark:hover:text-slate-100"
              >
                <svg width={12} height={12} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2.2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
                  <path d="M7 9l5-5 5 5" />
                  <path d="M7 15l5 5 5-5" />
                </svg>
              </button>
            </div>
          )}
          <div className="mr-1 inline-flex overflow-hidden rounded-md border border-zinc-300 dark:border-white/10" role="group" aria-label="Layout direction">
            {(['horizontal', 'vertical'] as const).map((dir) => {
              const on = direction === dir;
              return (
                <button
                  key={dir}
                  onClick={() => chooseDirection(dir)}
                  aria-pressed={on}
                  className={`px-1.5 py-0.5 transition-colors hover:bg-black/5 hover:text-zinc-900 dark:hover:bg-white/10 dark:hover:text-slate-100 ${on ? 'bg-black/5 text-zinc-900 dark:bg-white/10 dark:text-slate-100' : 'text-zinc-500 dark:text-slate-500'}`}
                  title={dir === 'horizontal' ? 'Flow left to right' : 'Flow top to bottom'}
                >
                  <svg width={12} height={12} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2.2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
                    {dir === 'horizontal' ? (
                      <>
                        <path d="M3 12h15" />
                        <path d="m14 8 4 4-4 4" />
                      </>
                    ) : (
                      <>
                        <path d="M12 3v15" />
                        <path d="m8 14 4 4 4-4" />
                      </>
                    )}
                  </svg>
                </button>
              );
            })}
          </div>
          {view.z !== 1 && (
            <span className="mr-1 rounded bg-black/5 px-1.5 py-0.5 font-mono text-[9.5px] text-zinc-500 dark:bg-white/5 dark:text-slate-400">
              {(view.z * 100).toFixed(0)}%
            </span>
          )}
          {([0.25, 0.5, 0.75, 1] as const).map((z) => {
            const on = Math.abs(view.z - z) < 0.005;
            return (
              <button
                key={z}
                onClick={() => setZoom(z)}
                className={`rounded-md px-1.5 py-0.5 transition-colors hover:bg-black/5 hover:text-zinc-900 dark:hover:bg-white/8 dark:hover:text-slate-100 ${on ? 'bg-black/5 text-zinc-900 dark:bg-white/10 dark:text-slate-100' : 'text-zinc-500 dark:text-slate-400'}`}
                title={`${z * 100}%`}
              >
                {z * 100}
              </button>
            );
          })}
          <button
            onClick={() => { userAdjustedRef.current = false; fit(true); }}
            className={`rounded-md px-2 py-0.5 text-zinc-500 transition-colors hover:bg-black/5 hover:text-zinc-900 dark:text-slate-400 dark:hover:bg-white/8 dark:hover:text-slate-100 ${compact ? '' : 'min-w-[34px]'}`}
            title="Fit to view — collapses expanded groups when the result would be unreadable"
          >
            fit
          </button>
          <button
            onClick={() => zoomAt(1.25, size.w / 2, size.h / 2)}
            className="rounded-md px-2 py-0.5 text-zinc-500 transition-colors hover:bg-black/5 hover:text-zinc-900 dark:text-slate-400 dark:hover:bg-white/8 dark:hover:text-slate-100"
            title="Zoom in"
          >
            +
          </button>
          <button
            onClick={() => zoomAt(0.8, size.w / 2, size.h / 2)}
            className="rounded-md px-2 py-0.5 text-zinc-500 transition-colors hover:bg-black/5 hover:text-zinc-900 dark:text-slate-400 dark:hover:bg-white/8 dark:hover:text-slate-100"
            title="Zoom out"
          >
            −
          </button>
          <span className="mx-0.5 h-3.5 w-px bg-zinc-300 dark:bg-white/10" />
          <button
            onClick={() => { void downloadPng(3); }}
            disabled={exporting || layout.nodes.length === 0}
            className="inline-flex items-center gap-1 rounded-md px-2 py-0.5 text-zinc-500 transition-colors hover:bg-black/5 hover:text-zinc-900 disabled:cursor-default disabled:opacity-35 disabled:hover:bg-transparent disabled:hover:text-zinc-500 dark:text-slate-400 dark:hover:bg-white/8 dark:hover:text-slate-100 dark:disabled:hover:text-slate-400"
            title="Download hi-res PNG (3×)"
          >
            {exporting ? (
              <span className="inline-block h-3 w-3 animate-spin rounded-full border border-slate-500 border-t-transparent" />
            ) : (
              <svg width={12} height={12} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2.2} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
                <path d="M12 3v12" />
                <path d="m7 11 5 5 5-5" />
                <path d="M4 20h16" />
              </svg>
            )}
            {!compact && 'png'}
          </button>
        </div>
      </div>

      <div
        ref={containerRef}
        tabIndex={0}
        onKeyDown={onCanvasKey}
        className="relative flex-1 touch-none overflow-hidden select-none focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-cyan-500/50"
        style={{ backgroundColor: pal.canvas, cursor: panning ? 'grabbing' : 'grab' }}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={endPointer}
        onPointerCancel={endPointer}
        onClick={(e) => {
          // Click on empty canvas clears the pinned selection (a drag doesn't).
          if ((e.target as Element).closest('[data-node]')) return;
          if ((e.target as Element).closest('[data-graph-tooltip]')) return;
          const start = clickStartRef.current;
          if (start) {
            const rect = containerRef.current!.getBoundingClientRect();
            if (Math.hypot(e.clientX - rect.left - start.x, e.clientY - rect.top - start.y) > 4) return;
          }
          setSelectedId(null);
          setHoverId(null);
        }}
        onDoubleClick={(e) => {
          if (!(e.target as Element).closest('[data-node]')) {
            userAdjustedRef.current = false;
            fit();
          }
        }}
      >
        {!onlyFailed && viewGraph.nodes.length === 0 && (
          <div className="absolute inset-0 flex flex-col items-center justify-center gap-3">
            <div className="relative h-12 w-12">
              <span className="absolute inset-0 rounded-full border border-cyan-500/40 dark:border-cyan-400/40" style={{ animation: 'rg-pulse 2.2s ease-out infinite' }} />
              <span className="absolute inset-0 rounded-full border border-cyan-500/25 dark:border-cyan-400/25" style={{ animation: 'rg-pulse 2.2s ease-out infinite 1.1s' }} />
              <span className="absolute inset-[30%] rounded-full bg-cyan-500/20 dark:bg-cyan-400/20" />
            </div>
            <div className="text-center">
              <div className="text-xs font-medium text-zinc-500 dark:text-slate-400">Waiting for network activity</div>
              <div className="mt-0.5 text-[10px] text-zinc-400 dark:text-slate-600">Open a page in the session to build the resource graph</div>
            </div>
          </div>
        )}
        {onlyFailed && viewGraph.nodes.length === 0 && (
          <div className="absolute inset-0 flex flex-col items-center justify-center gap-2 text-center">
            <div className="text-xs font-medium text-zinc-500 dark:text-slate-400">No failed requests in this view</div>
            <button
              onClick={() => setOnlyFailed(false)}
              className="pointer-events-auto rounded-md border border-zinc-300 px-2 py-1 text-[10px] text-cyan-700 hover:bg-black/5 dark:border-white/15 dark:text-cyan-300 dark:hover:bg-white/10"
            >
              Show all requests
            </button>
          </div>
        )}

        <svg ref={svgRef} width={size.w} height={size.h} className="absolute inset-0">
          <defs>
            <radialGradient id="rg-vignette" cx="50%" cy="40%" r="80%">
              <stop offset="0%" stopColor={pal.vignetteFrom} />
              <stop offset="100%" stopColor={pal.vignetteTo} />
            </radialGradient>
            <pattern id="rg-grid-s" width="24" height="24" patternUnits="userSpaceOnUse">
              <path d="M 24 0 L 0 0 0 24" fill="none" stroke={pal.gridMinor} strokeWidth="0.5" />
            </pattern>
            <pattern id="rg-grid-l" width="120" height="120" patternUnits="userSpaceOnUse">
              <path d="M 120 0 L 0 0 0 120" fill="none" stroke={pal.gridMajor} strokeWidth="0.5" />
            </pattern>
          </defs>

          <rect data-export-bg width={size.w} height={size.h} fill="url(#rg-vignette)" />
          <rect data-export-bg width={size.w} height={size.h} fill="url(#rg-grid-s)" />
          <rect data-export-bg width={size.w} height={size.h} fill="url(#rg-grid-l)" />

          <g transform={`translate(${view.x}, ${view.y}) scale(${view.z})`}>
            {layout.columns.map((c) => (
              <g key={`${c.depth}:${c.x}:${c.top}`}>
                {vertical ? (
                  <>
                    <line
                      x1={c.x}
                      y1={c.top - 14}
                      x2={c.x + c.w}
                      y2={c.top - 14}
                      stroke={pal.columnLine}
                      strokeWidth={1}
                      strokeDasharray="2 6"
                    />
                    <text
                      x={c.x - 8}
                      y={c.top - 14}
                      textAnchor="end"
                      dominantBaseline="middle"
                      fontSize={9}
                      fontWeight={600}
                      fill={pal.columnLabel}
                      fontFamily="ui-monospace, monospace"
                      letterSpacing={1}
                    >
                      {c.label ? `L${c.depth}` : ''}
                    </text>
                  </>
                ) : (
                  <>
                    <line
                      x1={c.x - 14}
                      y1={c.top - 24}
                      x2={c.x - 14}
                      y2={c.bottom + 14}
                      stroke={pal.columnLine}
                      strokeWidth={1}
                      strokeDasharray="2 6"
                    />
                    <text
                      x={c.x - 14}
                      y={c.top - 32}
                      textAnchor="middle"
                      fontSize={9}
                      fontWeight={600}
                      fill={pal.columnLabel}
                      fontFamily="ui-monospace, monospace"
                      letterSpacing={1}
                    >
                      {c.label ? `L${c.depth}` : ''}
                    </text>
                  </>
                )}
              </g>
            ))}

            {layout.edges.map(renderEdge)}
            {layout.nodes.map((n) => (
              n.node.kind === 'root' ? renderRootNode(n)
                : n.node.kind === 'host' ? renderHostNode(n)
                  : n.node.kind === 'group' ? renderGroupNode(n)
                    : n.node.kind === 'more' ? renderMoreNode(n)
                      : renderResNode(n)
            ))}
          </g>
        </svg>

        {minimap}

        {tooltip}

        {!compact && viewGraph.nodes.length > 0 && (
          <div className="pointer-events-none absolute bottom-1.5 left-2.5 text-[9px] tracking-wide text-zinc-500 dark:text-slate-600">
            {overview
              ? 'overview · zoom in for labels · click a host or type group to expand'
              : 'drag · scroll · pinch · click to expand/select · double-click to fit · esc to clear'}
          </div>
        )}
      </div>
    </div>
  );
}
