import { describe, expect, it } from 'vitest';
import { isPlaceholderUrl, seedForActiveTab, shouldCarryUrlEdit } from './addressBar';

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
