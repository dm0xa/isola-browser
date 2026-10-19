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
