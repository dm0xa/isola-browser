import { describe, expect, it } from 'vitest';

import {
  getMaxSessions,
  getNetworkLogMaxEntries,
  initAppConfig,
  isTimerEnabled,
} from './config';

describe('session timer flag', () => {
  it('defaults to enabled when the backend does not send it', () => {
    initAppConfig({});
    expect(isTimerEnabled()).toBe(true);
  });

  it('stays enabled when timer is true', () => {
    initAppConfig({ timer: true });
    expect(isTimerEnabled()).toBe(true);
  });

  it('is disabled when the backend sends timer: false', () => {
    initAppConfig({ timer: false });
    expect(isTimerEnabled()).toBe(false);
  });
});

describe('max_sessions', () => {
  it('is 0 (unlimited) when the backend does not send it', () => {
    initAppConfig({});
    expect(getMaxSessions()).toBe(0);
  });

  it('reports the configured cap', () => {
    initAppConfig({ max_sessions: 5 });
    expect(getMaxSessions()).toBe(5);
  });

  it('normalises a missing or invalid cap to 0', () => {
    initAppConfig({ max_sessions: -1 });
    expect(getMaxSessions()).toBe(0);
    initAppConfig({ max_sessions: undefined });
    expect(getMaxSessions()).toBe(0);
  });
});

describe('network_log_max_entries', () => {
  it('defaults to 1000 when the backend does not send it', () => {
    initAppConfig({});
    expect(getNetworkLogMaxEntries()).toBe(1000);
  });

  it('reports the configured window', () => {
    initAppConfig({ network_log_max_entries: 2500 });
    expect(getNetworkLogMaxEntries()).toBe(2500);
  });

  it('normalises a missing or invalid window to the default', () => {
    initAppConfig({ network_log_max_entries: 0 });
    expect(getNetworkLogMaxEntries()).toBe(1000);
    initAppConfig({ network_log_max_entries: -5 });
    expect(getNetworkLogMaxEntries()).toBe(1000);
    initAppConfig({ network_log_max_entries: Number.NaN });
    expect(getNetworkLogMaxEntries()).toBe(1000);
  });

  it('caps an absurd window at the shared maximum', () => {
    initAppConfig({ network_log_max_entries: 999999 });
    expect(getNetworkLogMaxEntries()).toBe(10000);
  });
});
