import { describe, expect, it } from 'vitest';
import { isPlaceholderUrl, seedForActiveTab, shouldCarryUrlEdit, shouldSkipTabBarRebuild, tabLabels } from './addressBar';

describe('isPlaceholderUrl', () => {
  it('treats blank and new-tab URLs as placeholders', () => {
    expect(isPlaceholderUrl('')).toBe(true);
    expect(isPlaceholderUrl(undefined)).toBe(true);
    expect(isPlaceholderUrl('about:blank')).toBe(true);
    expect(isPlaceholderUrl('chrome://newtab/')).toBe(true);
    expect(isPlaceholderUrl('edge://newtab/')).toBe(true);
  });

  it('treats a real page as a real page', () => {
    expect(isPlaceholderUrl('https://example.com/')).toBe(false);
  });
});

describe('seedForActiveTab', () => {
  it('seeds the tab the session started with', () => {
    expect(seedForActiveTab({ activeTabId: 1, initialTabId: 1, seed: 'https://landing.se/' }))
      .toBe('https://landing.se/');
  });

  it('does not seed a tab opened later', () => {
    // The bug: opening a new tab showed the landing URL in the address bar
    // instead of the new tab's blank address.
    expect(seedForActiveTab({ activeTabId: 2, initialTabId: 1, seed: 'https://landing.se/' }))
      .toBe('');
  });

  it('seeds while no tabs have been reported yet (cold start)', () => {
    expect(seedForActiveTab({ activeTabId: null, initialTabId: null, seed: 'https://landing.se/' }))
      .toBe('https://landing.se/');
  });

  it('has nothing to seed without a seed URL', () => {
    expect(seedForActiveTab({ activeTabId: 1, initialTabId: 1, seed: '' })).toBe('');
  });

  it('does not seed a later tab even before the initial tab is known', () => {
    // Cold start can deliver a tab list before the first id is latched; only the
    // tab that matches the latched id may inherit the landing URL.
    expect(seedForActiveTab({ activeTabId: 2, initialTabId: null, seed: 'https://landing.se/' }))
      .toBe('');
  });
});

describe('shouldCarryUrlEdit', () => {
  it('carries an edit when the active tab is unchanged', () => {
    // A tab close or a chip action must not lose what is being typed.
    expect(shouldCarryUrlEdit({ urlFocused: true, activeTabChanged: false })).toBe(true);
  });

  it('drops the edit when the active tab changed', () => {
    // The bug: with the address bar focused, opening a new tab kept the previous
    // tab's URL in the field.
    expect(shouldCarryUrlEdit({ urlFocused: true, activeTabChanged: true })).toBe(false);
  });

  it('has no edit to carry when the field is not focused', () => {
    expect(shouldCarryUrlEdit({ urlFocused: false, activeTabChanged: false })).toBe(false);
  });

  it('has no edit to carry when the field is not focused, even across a tab change', () => {
    expect(shouldCarryUrlEdit({ urlFocused: false, activeTabChanged: true })).toBe(false);
  });
});

describe('shouldSkipTabBarRebuild', () => {
  it('skips a broadcast that lands while the user is editing', () => {
    // Repainting replaces the input: the typed text and focus would be lost,
    // and the window-level key handlers would start relaying to the remote page.
    expect(shouldSkipTabBarRebuild({
      urlFocused: true, urlEdited: true, structureChanged: false, hostChanged: false,
    })).toBe(true);
  });

  it('repaints a focused field that holds no edit', () => {
    // The bug: the viewer autofocuses the address bar while the page is still
    // about:blank, so a warm claim's late URL/title broadcasts were all
    // discarded and the strip stayed on an unnamed about:blank tab.
    expect(shouldSkipTabBarRebuild({
      urlFocused: true, urlEdited: false, structureChanged: false, hostChanged: false,
    })).toBe(false);
  });

  it('repaints when the tab set changed, even mid-edit', () => {
    expect(shouldSkipTabBarRebuild({
      urlFocused: true, urlEdited: true, structureChanged: true, hostChanged: false,
    })).toBe(false);
  });

  it('repaints when the strip moved hosts, even mid-edit', () => {
    expect(shouldSkipTabBarRebuild({
      urlFocused: true, urlEdited: true, structureChanged: false, hostChanged: true,
    })).toBe(false);
  });

  it('repaints an unfocused strip regardless of edit state', () => {
    expect(shouldSkipTabBarRebuild({
      urlFocused: false, urlEdited: false, structureChanged: false, hostChanged: false,
    })).toBe(false);
  });
});

describe('tabLabels', () => {
  const tab = (id: number, url: string, title = '') => ({ id, url, title, active: false });

  it('names each tab by its title', () => {
    const labels = tabLabels([
      tab(1, 'https://ipinfo.io/', 'ipinfo at DuckDuckGo'),
      tab(2, 'https://ntfy.sh/app', 'ntfy'),
    ]);
    expect(labels.get(1)).toBe('ipinfo at DuckDuckGo');
    expect(labels.get(2)).toBe('ntfy');
  });

  it('falls back to the host when a page has no title', () => {
    const labels = tabLabels([tab(1, 'https://www.example.com/a/b')]);
    expect(labels.get(1)).toBe('example.com');
  });

  it('gives tabs that share a title the distinguishing end of their URL', () => {
    // Nine file-preview tabs all titled "ntfy": the ellipsis cut everything
    // that told them apart, so the chip showed the same word nine times.
    const labels = tabLabels([
      tab(1, 'https://ntfy.sh/file/AAAA1111', 'ntfy'),
      tab(2, 'https://ntfy.sh/file/BBBB2222', 'ntfy'),
    ]);
    expect(labels.get(1)).toBe('…AAAA1111');
    expect(labels.get(2)).toBe('…BBBB2222');
  });

  it('keeps untitled tabs of the same host distinct', () => {
    const labels = tabLabels([
      tab(1, 'https://ntfy.sh/file/AAAA1111'),
      tab(2, 'https://ntfy.sh/file/BBBB2222'),
    ]);
    expect(labels.get(1)).toBe('…AAAA1111');
    expect(labels.get(2)).toBe('…BBBB2222');
  });

  it('falls back to the runner id when even the URL matches', () => {
    const labels = tabLabels([
      tab(7, 'https://ntfy.sh/file/AAAA1111', 'ntfy'),
      tab(8, 'https://ntfy.sh/file/AAAA1111', 'ntfy'),
    ]);
    expect(labels.get(7)).toBe('Tab 7');
    expect(labels.get(8)).toBe('Tab 8');
  });

  it('numbers a tab that has neither a title nor a URL path', () => {
    expect(tabLabels([tab(3, 'about:blank')]).get(3)).toBe('Tab 3');
  });

  it('caps a long path segment to its last ten characters', () => {
    const labels = tabLabels([
      tab(1, 'https://ntfy.sh/file/0123456789abcdef', 'ntfy'),
      tab(2, 'https://ntfy.sh/file/fedcba9876543210', 'ntfy'),
    ]);
    expect(labels.get(1)).toBe('…6789abcdef');
    expect(labels.get(2)).toBe('…9876543210');
  });
});
