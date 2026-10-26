import { describe, expect, it } from 'vitest';

import { formatProbeData, postureGradeClass, probeStatusClass, tlsStateClass, tlsStateLabel, tlsValidity } from './intel';

describe('postureGradeClass', () => {
  it('grades A/B green, C amber, D/F red', () => {
    expect(postureGradeClass('A')).toContain('emerald');
    expect(postureGradeClass('B')).toContain('emerald');
    expect(postureGradeClass('C')).toContain('amber');
    expect(postureGradeClass('D')).toContain('red');
    expect(postureGradeClass('F')).toContain('red');
  });
});

describe('probeStatusClass', () => {
  it('distinguishes ok, empty, unreachable and error', () => {
    expect(probeStatusClass('ok')).toContain('emerald');
    expect(probeStatusClass('empty')).toContain('zinc');
    expect(probeStatusClass('unreachable')).toContain('red');
    expect(probeStatusClass('error')).toContain('amber');
  });
});

describe('formatProbeData', () => {
  it('pretty-prints objects and passes strings through', () => {
    expect(formatProbeData({ a: 1 })).toContain('"a": 1');
    expect(formatProbeData('plain')).toBe('plain');
    expect(formatProbeData(undefined)).toBe('—');
  });
});

describe('tlsValidity', () => {
  it('renders the certificate window as dates', () => {
    expect(tlsValidity({ valid_from: 1700000000, valid_to: 1800000000 })).toBe('2023-11-14 → 2027-01-15');
  });

  it('returns empty when the CDP host reported no window', () => {
    expect(tlsValidity({})).toBe('');
  });
});

describe('tlsStateLabel', () => {
  it('distinguishes a trusted chain from one accepted with errors ignored', () => {
    expect(tlsStateLabel('secure')).toBe('trusted');
    expect(tlsStateLabel('insecure')).toBe('untrusted cert');
    expect(tlsStateLabel('insecure-broken')).toBe('broken TLS');
    expect(tlsStateLabel('neutral')).toBe('no TLS');
  });

  it('stays quiet when the capture carried no state', () => {
    expect(tlsStateLabel(undefined)).toBe('');
    expect(tlsStateLabel('unknown')).toBe('');
  });

  it('classes unhealthy transport states as warnings', () => {
    expect(tlsStateClass('insecure')).toContain('red');
    expect(tlsStateClass('neutral')).toContain('amber');
    expect(tlsStateClass('secure')).toBe('');
  });
});
