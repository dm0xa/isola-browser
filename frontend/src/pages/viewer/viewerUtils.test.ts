import { describe, expect, it } from 'vitest';
import { sessionErrorDetail, shortInputTarget } from './viewerUtils';

describe('sessionErrorDetail', () => {
  it('claims expiry only for a missing session', () => {
    expect(sessionErrorDetail('Session not found')).toContain('does not exist or has expired');
  });

  it('does not claim expiry when the sandbox failed to start', () => {
    const detail = sessionErrorDetail('Session encountered an error');
    expect(detail).not.toContain('expired');
    expect(detail).toContain('sandbox');
  });

  it('reports a closed session without blaming the sandbox', () => {
    const detail = sessionErrorDetail('Session has ended');
    expect(detail).toContain('closed');
    expect(detail).not.toContain('sandbox');
  });

  it('falls back to the generic browser problem for stream errors', () => {
    expect(sessionErrorDetail('')).toContain('unexpected problem');
    expect(sessionErrorDetail('Stream closed')).toContain('unexpected problem');
  });
});

describe('shortInputTarget', () => {
  it('strips the scheme and truncates', () => {
    expect(shortInputTarget('https://example.com/a')).toBe('example.com/a');
    expect(shortInputTarget('')).toBe('...');
  });
});
