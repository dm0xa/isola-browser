// Address-bar rules for the imperative isola tab strip.
//
// Extracted from the rebuild so the two decisions that must not be guessed are
// unit-tested: which tab the landing-page seed describes, and when an in-flight
// edit survives a rebuild.
//
// Both exist because the address bar describes the ACTIVE tab, while the runner
// broadcasts the whole tab list: a rebuild that carries the field's text over
// without checking WHICH tab it belonged to left the previous URL sitting in a
// freshly opened tab.

// Placeholder URLs carry no page of their own (browser cold start, a tab that has
// not navigated yet).
export function isPlaceholderUrl(u: string | undefined): boolean {
  return !u || u === 'about:blank' || u === 'chrome://newtab/' || u === 'edge://newtab/';
}

/**
 * The URL the address bar may fall back to for the active tab.
 *
 * The seed is the URL typed on the landing page. It describes the tab the session
 * STARTED with, and only until that tab reports a real URL — it exists so the
 * first load does not flash about:blank. A tab opened later is blank on purpose,
 * so seeding it showed the landing URL in a brand-new tab.
 *
 * `activeTabId === null` means no tabs broadcast has arrived yet: the seed is all
 * there is (cold start).
 */
export function seedForActiveTab({ activeTabId, initialTabId, seed }: {
  activeTabId: number | null;
  initialTabId: number | null;
  seed: string;
}): string {
  if (!seed) return '';
  if (activeTabId === null) return seed;
  return activeTabId === initialTabId ? seed : '';
}

/**
 * Whether the address bar's in-flight edit should be carried across a tab-bar
 * rebuild.
 *
 * Only when the ACTIVE tab is unchanged. Otherwise the text describes the tab we
 * just left — opening a tab from the address bar left the previous URL in the box
 * instead of the new tab's blank address. Focus is kept either way, so keystrokes
 * can never leak to the remote page.
 */
export function shouldCarryUrlEdit({ urlFocused, activeTabChanged }: {
  urlFocused: boolean;
  activeTabChanged: boolean;
}): boolean {
  return urlFocused && !activeTabChanged;
}

/**
 * Whether a tabs broadcast may leave the strip (address field, chips) as it is.
 *
 * Rebuilding replaces the address input, so a broadcast landing mid-edit would
 * destroy what the user typed and drop focus to <body> (where the window-level
 * key handlers relay keystrokes into the remote page). Only a real edit is
 * protected: a field that still shows the value the bar rendered has nothing to
 * lose. Skipping those broadcasts is what froze a warm-claimed session on the
 * `about:blank` the viewer rendered before the claim navigated — the real URL
 * and title arrived afterwards and every repaint was discarded. A host change
 * (fullscreen) and a changed tab set must always repaint.
 */
export function shouldSkipTabBarRebuild({ urlFocused, urlEdited, structureChanged, hostChanged }: {
  urlFocused: boolean;
  urlEdited: boolean;
  structureChanged: boolean;
  hostChanged: boolean;
}): boolean {
  return urlFocused && urlEdited && !structureChanged && !hostChanged;
}

/** The fields of a runner `tabs` broadcast this module reads. */
export interface TabLike {
  id: number;
  url: string;
  title: string;
}

/** Registered host of an absolute URL, lowercased, without a leading www. */
function hostOf(url: string): string {
  const m = /^[a-z][a-z0-9+.-]*:\/\/([^/?#]+)/i.exec(url);
  return m ? m[1].replace(/^www\./i, '') : '';
}

/**
 * The distinguishing tail of a URL: its last path segment, capped at `n`.
 *
 * Pages that title themselves by their own URL (file previews, share links)
 * differ only at the end, which is exactly where a chip's ellipsis cuts.
 */
function tailOf(url: string, n = 10): string {
  const m = /^[a-z][a-z0-9+.-]*:\/\/[^/?#]*([^?#]*)/i.exec(url);
  // A scheme-less URL (about:blank) has no path to distinguish with.
  if (!m) return '';
  const path = m[1].replace(/\/+$/, '');
  const segment = path.split('/').filter(Boolean).pop() ?? '';
  if (!segment) return '';
  return `…${segment.length > n ? segment.slice(-n) : segment}`;
}

/**
 * Chip labels for a tab list.
 *
 * A page's title is what a tab is called, but tabs that report the same title
 * (nine `ntfy` file previews named after their own URL) are indistinguishable
 * once the strip truncates them from the right. Those trade the shared prefix
 * for their URL's tail; anything still ambiguous falls back to its runner id,
 * which is unique and stable across broadcasts.
 */
export function tabLabels<T extends TabLike>(tabs: T[]): Map<number, string> {
  const base = tabs.map((t) => (t.title || '').trim() || hostOf(t.url));
  const counts = new Map<string, number>();
  for (const b of base) if (b) counts.set(b, (counts.get(b) ?? 0) + 1);
  const labels = new Map<number, string>();
  const collided = new Map<number, string>();
  base.forEach((b, i) => {
    if (b && counts.get(b) === 1) labels.set(tabs[i].id, b);
    else collided.set(i, tailOf(tabs[i].url));
  });
  const tailCounts = new Map<string, number>();
  for (const tail of collided.values()) if (tail) tailCounts.set(tail, (tailCounts.get(tail) ?? 0) + 1);
  for (const [i, tail] of collided) {
    labels.set(tabs[i].id, tail && tailCounts.get(tail) === 1 ? tail : `Tab ${tabs[i].id}`);
  }
  return labels;
}
