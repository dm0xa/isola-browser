import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { createElement } from 'react';

import { graphOriginHost, type BrowserTab, type NetEntry } from './IsolaNetworkPanel';
import {
  applyExpansion,
  buildGraph,
  EMPTY_EXPANSION,
  exportCaption,
  exportFrame,
  focusEmphasis,
  GROUP_PREVIEW,
  layoutGraph,
  ResourceGraph,
  type LayoutNode,
} from './ResourceGraph';

const tab = (id: number, url: string, active = false): BrowserTab => ({ id, url, title: '', active });
const entry = (url: string, t = 'XHR'): NetEntry => ({ id: url, url, m: 'GET', s: 200, t });

describe('buildGraph origin', () => {
  it('keeps the opened page as origin when an earlier document is only a poll', () => {
    const entries = [
      entry('https://api.weather.example.com/v2/forecast', 'Document'),
      entry('https://example.com/weather/now'),
    ];

    const graph = buildGraph(entries, 'example.com');

    expect(graph.originId).toBe('host:example.com');
    const spine = graph.edges.find((e) => e.from === 'root:request');
    expect(spine?.to).toBe('host:example.com');
  });

  it('still shows the opened page after its requests have left the window', () => {
    const entries = [entry('https://api.weather.example.com/v2/forecast', 'Document')];

    const graph = buildGraph(entries, 'example.com');

    expect(graph.originId).toBe('host:example.com');
    expect(graph.nodes.find((n) => n.id === 'host:example.com')?.count).toBe(0);
  });

  it('falls back to the first document when no page is pinned', () => {
    const entries = [
      entry('https://api.weather.example.com/v2/forecast', 'Document'),
      entry('https://example.com/weather'),
    ];

    expect(buildGraph(entries).originId).toBe('host:api.weather.example.com');
  });
});

describe('focusEmphasis', () => {
  it('keeps the path from the entry lit, not only the last hop', () => {
    const edges = [
      { id: 'root->origin', from: 'root:request', to: 'host:example.com' },
      { id: 'origin->cdn', from: 'host:example.com', to: 'host:cdn.example.com' },
      { id: 'cdn->img', from: 'host:cdn.example.com', to: 'res:M0.jpg' },
      { id: 'other->cdn', from: 'host:other.test', to: 'host:cdn.example.com' },
      { id: 'cdn->other-img', from: 'host:cdn.example.com', to: 'res:other.jpg' },
    ];

    const emphasis = focusEmphasis('res:M0.jpg', edges);

    expect(emphasis.edges.has('root->origin')).toBe(true);
    expect(emphasis.edges.has('origin->cdn')).toBe(true);
    expect(emphasis.edges.has('cdn->img')).toBe(true);
    expect(emphasis.nodes.has('root:request')).toBe(true);
    expect(emphasis.nodes.has('host:example.com')).toBe(true);
    // A sibling resource is not on the path back to the entry.
    expect(emphasis.edges.has('cdn->other-img')).toBe(false);
    expect(emphasis.nodes.has('res:other.jpg')).toBe(false);
  });

  it('keeps a host → type group → resource path lit', () => {
    const edges = [
      { id: 'root', from: 'root:request', to: 'host:example.com' },
      { id: 'host-group', from: 'host:example.com', to: 'grp:host:example.com:script' },
      { id: 'group-res', from: 'grp:host:example.com:script', to: 'res:https://example.com/app.js' },
    ];

    const emphasis = focusEmphasis('res:https://example.com/app.js', edges);

    expect(emphasis.edges.has('host-group')).toBe(true);
    expect(emphasis.edges.has('group-res')).toBe(true);
    expect(emphasis.nodes.has('grp:host:example.com:script')).toBe(true);
    expect(emphasis.nodes.has('host:example.com')).toBe(true);
  });
});

describe('layoutGraph direction', () => {
  const entries = [
    entry('https://example.com/', 'Document'),
    entry('https://example.com/app.js', 'Script'),
  ];
  const graph = buildGraph(entries, 'example.com');
  const find = (nodes: LayoutNode[], id: string) => nodes.find((n) => n.id === id)!;

  it('advances depth along x and spreads siblings on y by default', () => {
    const { nodes } = layoutGraph(graph, 800, 600);
    const root = find(nodes, 'root:request');
    const host = find(nodes, 'host:example.com');
    const doc = find(nodes, 'res:https://example.com/');
    expect(root.x).toBeLessThan(host.x);
    expect(host.x).toBeLessThan(doc.x);
    // Siblings share their depth column but occupy separate rows.
    const js = find(nodes, 'res:https://example.com/app.js');
    expect(js.x).toBe(doc.x);
    expect(js.y).not.toBe(doc.y);
  });

  it('transposes the graph when vertical', () => {
    const { nodes } = layoutGraph(graph, 800, 600, 'vertical');
    const root = find(nodes, 'root:request');
    const host = find(nodes, 'host:example.com');
    const doc = find(nodes, 'res:https://example.com/');
    expect(root.y).toBeLessThan(host.y);
    expect(host.y).toBeLessThan(doc.y);
    // Siblings share their depth row but occupy separate columns.
    const js = find(nodes, 'res:https://example.com/app.js');
    expect(js.y).toBe(doc.y);
    expect(js.x).not.toBe(doc.x);
  });
});

describe('layoutGraph ordering', () => {
  // Real captures interleave third-party scripts with the page's own polls, so
  // the trigger scan used to draw an edge from the last ad host back into the
  // page. The cycle-tolerant layering then parked the ad host at depth 0 and
  // pushed the opened page to the far right.
  const entries = [
    entry('https://www.unibet.se/', 'Document'),
    entry('https://www.unibet.se/app.js', 'Script'),
    entry('https://ads.googlesyndication.example/tag.js', 'Script'),
    entry('https://connect.facebook.example/sdk.js', 'Script'),
    entry('https://lo.liveperson.example/lp.js', 'Script'),
    entry('https://www.unibet.se/api/poll', 'XHR'),
  ];

  it('keeps the opened page left of every third party it loaded', () => {
    const graph = buildGraph(entries, 'www.unibet.se');
    const { nodes } = layoutGraph(graph, 1280, 720);
    const origin = nodes.find((n) => n.id === 'host:www.unibet.se')!;

    expect(origin.depth).toBe(1);
    for (const other of nodes) {
      if (other.id === origin.id || other.node.kind !== 'host') continue;
      expect(other.x).toBeGreaterThan(origin.x);
    }
  });

  it('never infers a trigger edge back into the opened page', () => {
    const graph = buildGraph(entries, 'www.unibet.se');
    expect(graph.edges.some((e) => e.to === 'host:www.unibet.se' && e.from !== 'root:request'))
      .toBe(false);
  });

  it('keeps hosts that trigger each other right of the page instead of in the entry column', () => {
    const cyclic = [
      entry('https://example.com/', 'Document'),
      entry('https://example.com/app.js', 'Script'),
      entry('https://a.tracker.example/t.js', 'Script'),
      entry('https://b.tracker.example/b.js', 'Script'),
      entry('https://example.com/poll', 'XHR'),
      // b's script is followed by a request on a again: a ⇄ b is a cycle.
      entry('https://a.tracker.example/t2.js', 'Script'),
    ];
    const graph = buildGraph(cyclic, 'example.com');
    const { nodes } = layoutGraph(graph, 1280, 720);
    const origin = nodes.find((n) => n.id === 'host:example.com')!;

    expect(origin.depth).toBe(1);
    for (const n of nodes) {
      if (n.id === 'root:request' || n.id === origin.id) continue;
      expect(n.depth).toBeGreaterThan(1);
      expect(n.x).toBeGreaterThan(origin.x);
    }
  });
});

describe('buildGraph type filter', () => {
  const entries = [
    entry('https://example.com/', 'Document'),
    entry('https://example.com/app.js', 'Script'),
    entry('https://cdn.example.com/logo.png', 'Image'),
  ];
  it('keeps every resource type when no filter is set', () => {
    const g = buildGraph(entries, 'example.com');
    expect(resourceClassesFor(g)).toEqual(['document', 'image', 'script']);
  });

  it('drops every other type when a filter is set', () => {
    const g = buildGraph(entries, 'example.com', new Set(['script']));
    expect(resourceClassesFor(g)).toEqual(['script']);
  });

  it('keeps the pinned origin even when its host has no matching resource', () => {
    const g = buildGraph(entries, 'example.com', new Set(['image']));
    expect(g.originId).toBe('host:example.com');
    // Only the CDN image survives, but the opened page still owns the spine.
    expect(resourceClassesFor(g)).toEqual(['image']);
  });

  it('carries the SHA-1 from the entry onto its resource node', () => {
    const g = buildGraph([{ ...entry('https://example.com/app.js', 'Script'), h: 'abc123' }], 'example.com');
    const node = g.nodes.find((n) => n.kind === 'resource');
    expect(node?.sha1).toBe('abc123');
  });
});

// Class list of the resource nodes a graph actually contains, sorted.
function resourceClassesFor(g: ReturnType<typeof buildGraph>): string[] {
  return g.nodes.filter((n) => n.kind === 'resource').map((n) => n.resourceClass as string).sort();
}

describe('applyExpansion', () => {
  const entries = [
    entry('https://example.com/', 'Document'),
    entry('https://example.com/app.js', 'Script'),
    entry('https://example.com/vendor.js', 'Script'),
    entry('https://cdn.example.com/logo.png', 'Image'),
  ];
  const full = buildGraph(entries, 'example.com');

  it('shows only host cards until a host is opened', () => {
    const g = applyExpansion(full, EMPTY_EXPANSION);
    expect(g.nodes.some((n) => n.kind === 'resource')).toBe(false);
    expect(g.nodes.find((n) => n.id === 'host:example.com')?.resourceCount).toBe(3);
  });

  it('opens a host into one row per resource class', () => {
    const g = applyExpansion(full, {
      hosts: new Set(['host:example.com']),
      groups: new Set(),
      all: new Set(),
    });
    const groups = g.nodes.filter((n) => n.kind === 'group');
    expect(groups.map((n) => [n.resourceClass, n.count])).toEqual([
      ['document', 1],
      ['script', 2],
    ]);
    expect(g.edges.filter((e) => e.to.startsWith('grp:')).length).toBe(2);
    // The class rows are still closed: no resource leaves are drawn yet.
    expect(g.nodes.some((n) => n.kind === 'resource')).toBe(false);
  });

  it('opens a class into a bounded preview plus a more row', () => {
    const many = Array.from({ length: GROUP_PREVIEW + 8 }, (_, i) =>
      entry(`https://example.com/app-${i}.js`, 'Script'));
    const origin = buildGraph(many, 'example.com');
    const hostId = 'host:example.com';
    const gid = 'grp:host:example.com:script';

    const preview = applyExpansion(origin, {
      hosts: new Set([hostId]),
      groups: new Set([gid]),
      all: new Set(),
    });
    expect(preview.nodes.filter((n) => n.kind === 'resource')).toHaveLength(GROUP_PREVIEW);
    const more = preview.nodes.find((n) => n.kind === 'more');
    expect(more?.count).toBe(8);
    expect(more?.moreAction).toBe('expand');

    const all = applyExpansion(origin, {
      hosts: new Set([hostId]),
      groups: new Set([gid]),
      all: new Set([gid]),
    });
    expect(all.nodes.filter((n) => n.kind === 'resource')).toHaveLength(GROUP_PREVIEW + 8);
    expect(all.nodes.find((n) => n.kind === 'more')?.moreAction).toBe('collapse');
  });

  it('lifts failed resources to the front and counts them on the group', () => {
    const failed = [
      { ...entry('https://example.com/a.js', 'Script'), f: true },
      entry('https://example.com/b.js', 'Script'),
    ];
    const origin = buildGraph(failed, 'example.com');
    const g = applyExpansion(origin, {
      hosts: new Set(['host:example.com']),
      groups: new Set(['grp:host:example.com:script']),
      all: new Set(),
    });
    expect(g.nodes.find((n) => n.id === 'grp:host:example.com:script')?.failedCount).toBe(1);
    expect(g.nodes.filter((n) => n.kind === 'resource')[0].id)
      .toBe('res:https://example.com/a.js');
  });

  it('orders equal-count resources by most recent', () => {
    const timed = [
      { ...entry('https://example.com/old.js', 'Script'), ts: 100 },
      { ...entry('https://example.com/new.js', 'Script'), ts: 200 },
    ];
    const origin = buildGraph(timed, 'example.com');
    const g = applyExpansion(origin, {
      hosts: new Set(['host:example.com']),
      groups: new Set(['grp:host:example.com:script']),
      all: new Set(),
    });
    expect(g.nodes.filter((n) => n.kind === 'resource')[0].id)
      .toBe('res:https://example.com/new.js');
  });
});

describe('layoutGraph wrapping', () => {
  const many = Array.from({ length: 60 }, (_, i) => entry(`https://example.com/f-${i}.js`, 'Script'));
  const full = buildGraph(many, 'example.com');
  const expanded = applyExpansion(full, {
    hosts: new Set(['host:example.com']),
    groups: new Set(['grp:host:example.com:script']),
    all: new Set(['grp:host:example.com:script']),
  });

  it('wraps a long sibling run into sub-columns and bounds the cross axis', () => {
    const { columns, totalH } = layoutGraph(expanded, 800, 400);
    expect(columns.filter((c) => c.depth === 3).length).toBeGreaterThan(1);
    expect(totalH).toBeLessThan(800);
  });

  it('keeps the entry point at the top instead of centring it', () => {
    const { nodes } = layoutGraph(expanded, 800, 400);
    const root = nodes.find((n) => n.id === 'root:request')!;
    expect(root.y).toBeLessThan(60);
  });

  it('does not overlap nodes inside a wrapped sub-column', () => {
    const { nodes, columns } = layoutGraph(expanded, 800, 400);
    for (const col of columns.filter((c) => c.depth === 3)) {
      const inside = nodes
        .filter((n) => n.depth === 3 && n.x >= col.x && n.x < col.x + col.w)
        .sort((a, b) => a.y - b.y);
      for (let i = 1; i < inside.length; i++) {
        expect(inside[i].y).toBeGreaterThanOrEqual(inside[i - 1].y + inside[i - 1].h);
      }
    }
  });
});

describe('graphOriginHost', () => {
  it('uses the active tab, not a background one', () => {
    const host = graphOriginHost(
      [tab(1, 'https://shop.example.com/', false), tab(2, 'https://example.com/weather', true)],
      '',
    );
    expect(host).toBe('example.com');
  });

  it('uses the filtered tab when one is selected', () => {
    const host = graphOriginHost(
      [tab(1, 'https://shop.example.com/', true), tab(2, 'https://example.com/', false)],
      2,
    );
    expect(host).toBe('example.com');
  });

  it('ignores a blank tab', () => {
    expect(graphOriginHost([tab(1, 'about:blank', true)], '')).toBe('');
  });
});

describe('disclosure click stability', () => {
  beforeEach(() => {
    vi.stubGlobal('ResizeObserver', class {
      private cb: ResizeObserverCallback;
      constructor(cb: ResizeObserverCallback) { this.cb = cb; }
      observe() {
        this.cb([
          { contentRect: { width: 1200, height: 500 } } as ResizeObserverEntry,
        ], this as unknown as ResizeObserver);
      }
      unobserve() {}
      disconnect() {}
    });
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('keeps the zoom and pan when a type group is clicked', async () => {
    const entries: NetEntry[] = [
      { id: 'doc', url: 'https://example.com/', m: 'GET', s: 200, t: 'Document' },
      ...Array.from({ length: 20 }, (_, i): NetEntry => ({
        id: `s${i}`,
        url: `https://example.com/app-${i}.js`,
        m: 'GET',
        s: 200,
        t: 'Script',
      })),
    ];
    const view = render(createElement(ResourceGraph, { entries, originHost: 'example.com' }));
    const graphTransform = () => view.container
      .querySelector('svg.absolute > g[transform]')
      ?.getAttribute('transform');

    await waitFor(() => {
      if (!view.container.querySelector('[aria-label^="Script on example.com"]')) {
        throw new Error('type group not open');
      }
    });
    // Let the one-time initial framing settle before taking the baseline.
    await new Promise<void>((resolve) => { setTimeout(resolve, 120); });
    const before = graphTransform();

    fireEvent.click(view.container.querySelector('[aria-label^="Script on example.com"]')!);
    await waitFor(() => {
      if (view.container.querySelectorAll('g[data-node]').length < 10) {
        throw new Error('group did not expand');
      }
    });

    expect(graphTransform()).toBe(before);
  });

  it('selects a resource without filtering; the tooltip funnel filters', async () => {
    const onSelect = vi.fn();
    const entries: NetEntry[] = [
      { id: 'doc', url: 'https://example.com/', m: 'GET', s: 200, t: 'Document' },
      { id: 'app', url: 'https://example.com/app.js', m: 'GET', s: 200, t: 'Script' },
    ];
    const view = render(createElement(ResourceGraph, {
      entries,
      originHost: 'example.com',
      onSelect,
    }));

    await waitFor(() => {
      if (!view.container.querySelector('[aria-label^="Script on example.com"]')) {
        throw new Error('type group not open');
      }
    });
    fireEvent.click(view.container.querySelector('[aria-label^="Script on example.com"]')!);

    let resource: Element | undefined;
    await waitFor(() => {
      resource = [...view.container.querySelectorAll('g[data-node]')]
        .find((g) => g.textContent?.includes('app.js'));
      if (!resource) throw new Error('resource not open');
    });

    fireEvent.click(resource!);
    expect(onSelect).not.toHaveBeenCalled();

    fireEvent.click(screen.getByTitle('Show only example.com'));
    expect(onSelect).toHaveBeenCalledWith('example.com');
  });

  it('opens with only the origin host expanded and every type group collapsed', async () => {
    const entries: NetEntry[] = [
      { id: 'doc', url: 'https://example.com/', m: 'GET', s: 200, t: 'Document' },
      { id: 'app', url: 'https://example.com/app.js', m: 'GET', s: 200, t: 'Script' },
      { id: 'hero', url: 'https://example.com/hero.png', m: 'GET', s: 200, t: 'Image' },
    ];
    const view = render(createElement(ResourceGraph, { entries, originHost: 'example.com' }));

    await waitFor(() => {
      if (!view.container.querySelector('[aria-label^="Script on example.com"]')) {
        throw new Error('type groups not open');
      }
    });

    expect(view.container.querySelector('[aria-label^="example.com:"]')?.getAttribute('aria-expanded'))
      .toBe('true');
    expect(view.container.querySelector('[aria-label^="Script on example.com"]')?.getAttribute('aria-expanded'))
      .toBe('false');
    expect(view.container.querySelector('[aria-label^="Image on example.com"]')?.getAttribute('aria-expanded'))
      .toBe('false');
    expect([...view.container.querySelectorAll('g[data-node]')]
      .some((g) => g.textContent?.includes('app.js'))).toBe(false);
  });

  it('expands and collapses everything from the toolbar icons', async () => {
    const entries: NetEntry[] = [
      { id: 'doc', url: 'https://example.com/', m: 'GET', s: 200, t: 'Document' },
      { id: 'app', url: 'https://example.com/app.js', m: 'GET', s: 200, t: 'Script' },
      { id: 'hero', url: 'https://example.com/hero.png', m: 'GET', s: 200, t: 'Image' },
    ];
    const view = render(createElement(ResourceGraph, { entries, originHost: 'example.com' }));

    await waitFor(() => {
      if (!view.container.querySelector('[aria-label^="Script on example.com"]')) {
        throw new Error('type groups not open');
      }
    });
    const hasResource = (needle: string) => [...view.container.querySelectorAll('g[data-node]')]
      .some((g) => g.textContent?.includes(needle));
    expect(hasResource('app.js')).toBe(false);

    fireEvent.click(screen.getByLabelText('Expand all hosts and groups'));
    await waitFor(() => {
      if (!hasResource('app.js')) throw new Error('expand all did not open resources');
    });

    fireEvent.click(screen.getByLabelText('Collapse all hosts and groups'));
    await waitFor(() => {
      if (hasResource('app.js')) throw new Error('collapse all did not close resources');
    });
  });

  it('fades siblings while a group is open and clears the fade on the empty canvas', async () => {
    const entries: NetEntry[] = [
      { id: 'doc', url: 'https://example.com/', m: 'GET', s: 200, t: 'Document' },
      { id: 'app', url: 'https://example.com/app.js', m: 'GET', s: 200, t: 'Script' },
      { id: 'vendor', url: 'https://example.com/vendor.js', m: 'GET', s: 200, t: 'Script' },
      { id: 'hero', url: 'https://example.com/hero.png', m: 'GET', s: 200, t: 'Image' },
      { id: 'logo', url: 'https://example.com/logo.png', m: 'GET', s: 200, t: 'Image' },
    ];
    const view = render(createElement(ResourceGraph, { entries, originHost: 'example.com' }));

    await waitFor(() => {
      if (!view.container.querySelector('[aria-label^="Script on example.com"]')) {
        throw new Error('type groups not open');
      }
    });
    fireEvent.click(view.container.querySelector('[aria-label^="Script on example.com"]')!);
    await waitFor(() => {
      const files = [...view.container.querySelectorAll('g[data-node]')];
      if (!files.some((g) => g.textContent?.includes('app.js'))) {
        throw new Error('group did not expand');
      }
    });

    const scriptGroup = () => view.container.querySelector('[aria-label^="Script on example.com"]');
    const imageGroup = () => view.container.querySelector('[aria-label^="Image on example.com"]');
    const appFile = () => [...view.container.querySelectorAll('g[data-node]')]
      .find((g) => g.textContent?.includes('app.js'));

    expect(scriptGroup()?.getAttribute('opacity')).toBe('1');
    expect(appFile()?.getAttribute('opacity')).toBe('1');
    expect(imageGroup()?.getAttribute('opacity')).toBe('0.14');

    // A pinned group's card is hover-only: it must disappear when the pointer
    // leaves so it cannot cover the rows it just revealed.
    fireEvent.mouseEnter(scriptGroup()!);
    expect(view.container.querySelector('[data-graph-tooltip]')).not.toBeNull();
    fireEvent.mouseLeave(scriptGroup()!);
    await new Promise<void>((resolve) => { setTimeout(resolve, 260); });
    expect(view.container.querySelector('[data-graph-tooltip]')).toBeNull();
    expect(imageGroup()?.getAttribute('opacity')).toBe('0.14');

    fireEvent.click(view.container.querySelector('.relative.flex-1')!);

    expect(scriptGroup()?.getAttribute('opacity')).toBe('1');
    expect(imageGroup()?.getAttribute('opacity')).toBe('1');
  });
});

// The tooltip's copy buttons used to lose their click to the canvas' pointer
// capture, and a hover-only card vanished before the pointer could reach them.
// Pinning the node keeps the card open here; the pointerdown assertion covers
// the capture path.
describe('tooltip copy buttons', () => {
  const sha = 'abcdef0123456789abcdef0123456789abcdef01';
  const url = 'https://example.com/assets/app.js';

  beforeEach(() => {
    // The graph measures its container; jsdom has no ResizeObserver.
    vi.stubGlobal('ResizeObserver', class {
      observe() {}
      unobserve() {}
      disconnect() {}
    });
  });

  afterEach(() => {
    // Renders are not auto-cleaned (vitest runs without `globals`).
    cleanup();
    vi.unstubAllGlobals();
    delete (HTMLElement.prototype as unknown as Record<string, unknown>).setPointerCapture;
    delete (navigator as unknown as Record<string, unknown>).clipboard;
  });

  const renderPinned = () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    const capture = vi.fn();
    Object.defineProperty(HTMLElement.prototype, 'setPointerCapture', { value: capture, configurable: true });

    const view = render(createElement(ResourceGraph, {
      entries: [{ id: 'r1', url, m: 'GET', s: 200, t: 'Script', h: sha }],
      originHost: 'example.com',
    }));

    // A single type facet bypasses the summary grouping, so the resource card
    // itself is on screen and can be pinned.
    fireEvent.click(screen.getByTitle('Show only Script'));
    const node = [...view.container.querySelectorAll('g[data-node]')]
      .find((g) => g.textContent?.includes('app.js'));
    expect(node).toBeTruthy();
    fireEvent.click(node!);

    return { writeText, capture };
  };

  it('copies the full URL', async () => {
    const { writeText, capture } = renderPinned();
    const button = screen.getByTitle('Copy URL');

    fireEvent.pointerDown(button);
    fireEvent.click(button);

    await waitFor(() => expect(writeText).toHaveBeenCalledWith(url));
    expect(capture).not.toHaveBeenCalled();
    expect(button).toHaveTextContent('✓');
  });

  it('copies the full SHA-1', async () => {
    const { writeText, capture } = renderPinned();
    const button = screen.getByTitle('Copy SHA-1');

    fireEvent.pointerDown(button);
    fireEvent.click(button);

    await waitFor(() => expect(writeText).toHaveBeenCalledWith(sha));
    expect(capture).not.toHaveBeenCalled();
    expect(button).toHaveTextContent('✓');
  });
});

describe('exportFrame', () => {
  it('frames the painted box, which can be wider than the nominal layout', () => {
    // A label or selection ring spilling past the estimated card size used to be
    // cropped, because the frame came from layout.totalW.
    const frame = exportFrame({ x: 120, y: 30, width: 900, height: 400 }, { w: 747, h: 405 });

    expect(frame.w).toBe(900 + 72 * 2);
    expect(frame.h).toBe(400 + 72 * 2);
    expect(frame.w).toBeGreaterThanOrEqual(747 + 72 * 2);
    // Content with a non-zero origin is shifted into the padded frame.
    expect(frame.dx).toBe(72 - 120);
    expect(frame.dy).toBe(72 - 30);
  });

  it('falls back to the layout size when no box can be measured', () => {
    expect(exportFrame(null, { w: 800, h: 500 })).toEqual({
      w: 800 + 72 * 2,
      h: 500 + 72 * 2,
      dx: 72,
      dy: 72,
    });
  });

  it('ignores empty or non-finite boxes', () => {
    const empty = exportFrame({ x: 0, y: 0, width: 0, height: 0 }, { w: 800, h: 500 });
    const broken = exportFrame({ x: Number.NaN, y: 0, width: 10, height: 10 }, { w: 800, h: 500 });

    expect(empty.w).toBe(800 + 72 * 2);
    expect(broken.w).toBe(800 + 72 * 2);
    expect(broken.dx).toBe(72);
  });
});

describe('exportCaption', () => {
  const at = new Date('2026-10-21T10:45:00Z');

  it('leads with the origin and ends with the capture time', () => {
    const caption = exportCaption({
      origin: 'duckduckgo.com',
      requests: 77,
      hosts: 12,
      resources: 165,
      failed: 2,
      types: [],
      failedOnly: false,
      at,
    });

    expect(caption).toBe(
      'duckduckgo.com · 77 requests · 12 hosts · 165 resources · 2 failed · 2026-10-21 10:45Z',
    );
  });

  it('singularises counts and hides a zero failure count', () => {
    const caption = exportCaption({
      origin: 'example.se',
      requests: 1,
      hosts: 1,
      resources: 1,
      failed: 0,
      types: [],
      failedOnly: false,
      at,
    });

    expect(caption).toBe('example.se · 1 request · 1 host · 1 resource · 2026-10-21 10:45Z');
  });

  it('records active filters so a filtered image cannot pass for the whole page', () => {
    const caption = exportCaption({
      origin: 'example.se',
      requests: 4,
      hosts: 2,
      resources: 4,
      failed: 1,
      types: ['Script', 'Font'],
      failedOnly: true,
      at,
    });

    expect(caption).toContain('Script + Font');
    expect(caption).toContain('failed only');
  });
});
