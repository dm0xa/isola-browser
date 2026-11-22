import { describe, expect, it } from 'vitest';

import {
  IDENTITY_PROFILES,
  LOCALE_OPTIONS,
  ZONE_OPTIONS,
  identitySummary,
  resolveIdentity,
} from './identityProfiles';

describe('IDENTITY_PROFILES', () => {
  it('is a coherent, collision-free table', () => {
    expect(IDENTITY_PROFILES.length).toBeGreaterThanOrEqual(10);
    const locales = new Set(IDENTITY_PROFILES.map((p) => p.locale));
    const zones = new Set(IDENTITY_PROFILES.map((p) => p.timezone));
    expect(locales.size).toBe(IDENTITY_PROFILES.length);
    expect(zones.size).toBe(IDENTITY_PROFILES.length);
    for (const profile of IDENTITY_PROFILES) {
      expect(profile.locale).toMatch(/^[a-z]{2,3}-[A-Z]{2}$/);
      expect(profile.timezone).toMatch(/^[A-Za-z]+\/[A-Za-z_]+$/);
    }
    expect(LOCALE_OPTIONS.map((o) => o.value)).toEqual(IDENTITY_PROFILES.map((p) => p.locale));
    expect(ZONE_OPTIONS.map((o) => o.value)).toEqual(IDENTITY_PROFILES.map((p) => p.timezone));
  });
});

describe('resolveIdentity', () => {
  it('rolls one coherent pair when both sides are random', () => {
    expect(resolveIdentity('', '', () => 0)).toEqual({ locale: 'sv-SE', timezone: 'Europe/Stockholm' });
    const last = IDENTITY_PROFILES[IDENTITY_PROFILES.length - 1];
    expect(resolveIdentity('', '', () => 0.999999)).toEqual({
      locale: last.locale,
      timezone: last.timezone,
    });
  });

  it('derives the canonical zone from a pinned locale', () => {
    expect(resolveIdentity('ja-JP', '', () => 0)).toEqual({ locale: 'ja-JP', timezone: 'Asia/Tokyo' });
  });

  it('derives the canonical locale from a pinned zone', () => {
    expect(resolveIdentity('', 'Europe/Berlin', () => 0)).toEqual({ locale: 'de-DE', timezone: 'Europe/Berlin' });
  });

  it('leans on the table for an unknown pinned value so the other side still makes sense', () => {
    const fromLocale = resolveIdentity('xx-YY', '', () => 0);
    expect(fromLocale.locale).toBe('xx-YY');
    expect(ZONE_OPTIONS.map((o) => o.value)).toContain(fromLocale.timezone);

    const fromZone = resolveIdentity('', 'Antarctica/Troll', () => 0);
    expect(fromZone.timezone).toBe('Antarctica/Troll');
    expect(LOCALE_OPTIONS.map((o) => o.value)).toContain(fromZone.locale);
  });

  it('leaves an explicitly pinned pair untouched', () => {
    expect(resolveIdentity('fr-FR', 'Asia/Tokyo', () => 0)).toEqual({ locale: 'fr-FR', timezone: 'Asia/Tokyo' });
  });
});

describe('identitySummary', () => {
  it('describes a random launch and pinned choices', () => {
    expect(identitySummary('', '')).toBe('Random on launch');
    expect(identitySummary('sv-SE', 'Europe/Stockholm')).toBe('sv-SE · Europe/Stockholm');
    expect(identitySummary('sv-SE', '')).toBe('sv-SE · random zone');
    expect(identitySummary('', 'Asia/Tokyo')).toBe('random · Asia/Tokyo');
  });
});
