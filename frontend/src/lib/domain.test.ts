import { describe, expect, it } from 'vitest';

import { registrableDomain } from './domain';

describe('registrableDomain', () => {
  it('keeps the last two labels of an ordinary host', () => {
    expect(registrableDomain('en.wikipedia.org')).toBe('wikipedia.org');
    expect(registrableDomain('external-content.duckduckgo.com')).toBe('duckduckgo.com');
    expect(registrableDomain('duckduckgo.com')).toBe('duckduckgo.com');
  });

  it('keeps three labels under a multi-label suffix', () => {
    expect(registrableDomain('cdn.example.co.uk')).toBe('example.co.uk');
    expect(registrableDomain('example.co.uk')).toBe('example.co.uk');
    expect(registrableDomain('a.b.example.com.se')).toBe('example.com.se');
  });

  it('never groups two hosts of the same TLD alone', () => {
    expect(registrableDomain('tracker.example')).not.toBe(registrableDomain('cdn.example.net'));
    expect(registrableDomain('www.unibet.com')).not.toBe(registrableDomain('www.unibet.se'));
  });

  it('returns IP literals and single-label hosts unchanged', () => {
    expect(registrableDomain('127.0.0.1')).toBe('127.0.0.1');
    expect(registrableDomain('10.1.2.3')).toBe('10.1.2.3');
    expect(registrableDomain('[::1]')).toBe('[::1]');
    expect(registrableDomain('localhost')).toBe('localhost');
  });

  it('normalises case, surrounding space and a trailing dot', () => {
    expect(registrableDomain('  EN.Wikipedia.ORG. ')).toBe('wikipedia.org');
    expect(registrableDomain('')).toBe('');
  });
});
