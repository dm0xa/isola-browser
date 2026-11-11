import { describe, expect, it } from 'vitest';
import { isSessionTerminated } from './sessionLiveness';

describe('isSessionTerminated', () => {
  it('keeps retrying while the sandbox can still serve a socket', () => {
    expect(isSessionTerminated('running')).toBe(false);
    expect(isSessionTerminated('provisioning')).toBe(false);
  });

  it('stops once the sandbox is gone', () => {
    expect(isSessionTerminated('terminated')).toBe(true);
    expect(isSessionTerminated('error')).toBe(true);
  });

  it('does not stop on a status it cannot interpret', () => {
    expect(isSessionTerminated(undefined)).toBe(false);
    expect(isSessionTerminated('')).toBe(false);
    expect(isSessionTerminated({ status: 'terminated' })).toBe(false);
  });
});
