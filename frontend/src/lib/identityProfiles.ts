// Launch identity: the page-visible timezone + locale pair a session presents.
//
// Two properties matter and only a curated table gives both:
//   * every pair is internally coherent — a ja-JP locale with an America/* zone
//     is exactly the combination anti-bot systems treat as a spoofed profile;
//   * every locale has one canonical zone, so pinning either side still yields
//     a believable counterpart instead of a random, unrelated value.
//
// The launcher defaults both selectors to "random": on every launch it rolls
// one coherent pair from this table. A warm-pool container is baked with the
// pool's identity, and the backend re-applies the rolled values over CDP after
// a claim, so randomisation costs no cold start.

export type IdentityProfile = {
  locale: string;
  /** English name, shown beside the tag so the dropdown is readable. */
  name: string;
  /** Canonical IANA zone for the locale. */
  timezone: string;
};

export type IdentityOption = { value: string; label: string };

export const IDENTITY_PROFILES: readonly IdentityProfile[] = [
  { locale: 'sv-SE', name: 'Swedish', timezone: 'Europe/Stockholm' },
  { locale: 'nb-NO', name: 'Norwegian', timezone: 'Europe/Oslo' },
  { locale: 'da-DK', name: 'Danish', timezone: 'Europe/Copenhagen' },
  { locale: 'fi-FI', name: 'Finnish', timezone: 'Europe/Helsinki' },
  { locale: 'en-GB', name: 'English (UK)', timezone: 'Europe/London' },
  { locale: 'de-DE', name: 'German', timezone: 'Europe/Berlin' },
  { locale: 'fr-FR', name: 'French', timezone: 'Europe/Paris' },
  { locale: 'nl-NL', name: 'Dutch', timezone: 'Europe/Amsterdam' },
  { locale: 'es-ES', name: 'Spanish', timezone: 'Europe/Madrid' },
  { locale: 'it-IT', name: 'Italian', timezone: 'Europe/Rome' },
  { locale: 'pl-PL', name: 'Polish', timezone: 'Europe/Warsaw' },
  { locale: 'pt-BR', name: 'Portuguese (Brazil)', timezone: 'America/Sao_Paulo' },
  { locale: 'en-US', name: 'English (US)', timezone: 'America/New_York' },
  { locale: 'ru-RU', name: 'Russian', timezone: 'Europe/Moscow' },
  { locale: 'tr-TR', name: 'Turkish', timezone: 'Europe/Istanbul' },
  { locale: 'ja-JP', name: 'Japanese', timezone: 'Asia/Tokyo' },
  { locale: 'ko-KR', name: 'Korean', timezone: 'Asia/Seoul' },
  { locale: 'zh-CN', name: 'Chinese (Simplified)', timezone: 'Asia/Shanghai' },
  { locale: 'hi-IN', name: 'Hindi', timezone: 'Asia/Kolkata' },
  { locale: 'en-AU', name: 'English (Australia)', timezone: 'Australia/Sydney' },
];

export const LOCALE_OPTIONS: readonly IdentityOption[] = IDENTITY_PROFILES.map((p) => ({
  value: p.locale,
  label: `${p.locale} — ${p.name}`,
}));

export const ZONE_OPTIONS: readonly IdentityOption[] = IDENTITY_PROFILES.map((p) => ({
  value: p.timezone,
  label: p.timezone,
}));

/** Empty choice = "random on launch". */
export function resolveIdentity(
  localeChoice: string,
  zoneChoice: string,
  rand: () => number = Math.random,
): { locale: string; timezone: string } {
  const locale = localeChoice.trim();
  const zone = zoneChoice.trim();
  if (IDENTITY_PROFILES.length === 0) {
    return { locale: locale || 'en-US', timezone: zone || 'UTC' };
  }
  const pick = <T>(items: readonly T[]): T =>
    items[Math.min(items.length - 1, Math.max(0, Math.floor(rand() * items.length)))];

  if (locale && zone) return { locale, timezone: zone };
  if (locale) {
    const match = IDENTITY_PROFILES.find((p) => p.locale === locale);
    return { locale, timezone: match ? match.timezone : pick(ZONE_OPTIONS).value };
  }
  if (zone) {
    const match = IDENTITY_PROFILES.find((p) => p.timezone === zone);
    return { locale: match ? match.locale : pick(LOCALE_OPTIONS).value, timezone: zone };
  }
  const profile = pick(IDENTITY_PROFILES);
  return { locale: profile.locale, timezone: profile.timezone };
}

/** One line for the collapsed Advanced row. */
export function identitySummary(localeChoice: string, zoneChoice: string): string {
  const locale = localeChoice.trim();
  const zone = zoneChoice.trim();
  if (!locale && !zone) return 'Random on launch';
  return `${locale || 'random'} · ${zone || 'random zone'}`;
}
