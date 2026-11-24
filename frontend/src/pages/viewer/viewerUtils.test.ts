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

  it('names the runtime action for a sandbox that could not be reached', () => {
    const detail = sessionErrorDetail('Session encountered an error', 'sandbox_publish_unreachable');
    expect(detail).toContain('port publishing');
    expect(detail).toContain('colima restart');
    expect(detail).toContain('scripts/doctor.sh');
  });

  it('distinguishes a stream that never started from a dead runtime', () => {
    expect(sessionErrorDetail('Session encountered an error', 'stream_not_ready')).toContain('video stream');
    expect(sessionErrorDetail('Session encountered an error', 'create_failed')).toContain('could not create');
  });

  it('points at the doctor script when the reason is unknown or absent', () => {
    expect(sessionErrorDetail('Session encountered an error')).toContain('scripts/doctor.sh');
    expect(sessionErrorDetail('Session encountered an error', 'some_future_code')).toContain('scripts/doctor.sh');
  });
});

describe('shortInputTarget', () => {
  it('strips the scheme and truncates', () => {
    expect(shortInputTarget('https://example.com/a')).toBe('example.com/a');
    expect(shortInputTarget('')).toBe('...');
  });
});
