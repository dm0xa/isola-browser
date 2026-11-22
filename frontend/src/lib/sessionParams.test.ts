import { describe, expect, it } from 'vitest';

import {
  EMPTY_SESSION_PARAMS,
  mergeSessionParams,
  sessionParamsTooltip,
  type SessionParams,
} from './sessionParams';

const params: SessionParams = {
  resolution: '1920x1080',
  frameRate: 30,
  browserType: 'chromium',
  timezone: 'Asia/Tokyo',
  locale: 'ja-JP',
  torEnabled: false,
  torCountry: '',
};

describe('sessionParamsTooltip', () => {
  it('lists every launch parameter on its own line', () => {
    const text = sessionParamsTooltip('1f3b9b0f-5bf1', params);
    expect(text.split('\n')).toEqual([
      'session 1f3b9b0f-5bf1',
      '1920x1080 · 30 fps · chromium',
      'timezone Asia/Tokyo · locale ja-JP',
      'tor off',
    ]);
  });

  it('names the image defaults instead of leaving blanks', () => {
    const text = sessionParamsTooltip('abc', EMPTY_SESSION_PARAMS);
    expect(text).toContain('timezone UTC (default) · locale en-US (default)');
    expect(text).toContain('fps default');
    expect(text).toContain('resolution ?');
  });

  it('includes the Tor exit pin when Tor is on', () => {
    expect(sessionParamsTooltip('abc', { ...params, torEnabled: true, torCountry: 'se,no' })).toContain('tor on (se,no)');
    expect(sessionParamsTooltip('abc', { ...params, torEnabled: true })).toContain('tor on');
  });

  it('tolerates a missing id', () => {
    expect(sessionParamsTooltip(undefined, params).startsWith('session ?')).toBe(true);
  });
});

describe('mergeSessionParams', () => {
  const payload = {
    resolution: '1920x1080',
    frame_rate: 25,
    browser_type: 'chromium',
    timezone: 'Asia/Tokyo',
    locale: 'ja-JP',
    tor_enabled: true,
    tor_country: 'se',
  };

  it('maps the snake_case session payload onto the parameters', () => {
    expect(mergeSessionParams(EMPTY_SESSION_PARAMS, payload)).toEqual({
      resolution: '1920x1080',
      frameRate: 25,
      browserType: 'chromium',
      timezone: 'Asia/Tokyo',
      locale: 'ja-JP',
      torEnabled: true,
      torCountry: 'se',
    });
  });

  it('keeps the previous object when the payload did not change', () => {
    const first = mergeSessionParams(EMPTY_SESSION_PARAMS, payload);
    expect(mergeSessionParams(first, payload)).toBe(first);
  });

  it('keeps fields the response omitted', () => {
    const first = mergeSessionParams(EMPTY_SESSION_PARAMS, payload);
    // A session with no declared identity omits timezone/locale entirely.
    const next = mergeSessionParams(first, { resolution: '1280x720' });
    expect(next.resolution).toBe('1280x720');
    expect(next.timezone).toBe('Asia/Tokyo');
    expect(next.locale).toBe('ja-JP');
  });

  it('detects a single changed field', () => {
    const first = mergeSessionParams(EMPTY_SESSION_PARAMS, payload);
    const next = mergeSessionParams(first, { ...payload, tor_enabled: false });
    expect(next).not.toBe(first);
    expect(next.torEnabled).toBe(false);
  });
});
