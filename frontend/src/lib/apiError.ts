// apiErrorMessage reads the body of a failed API response.
// The backend sends { error: { code, message } }. Older callers sometimes
// sent { error: "..." }. Fall back to the status when neither is present.
export function apiErrorMessage(data: unknown, status: number): string {
  const fallback = `Request failed (${status})`;
  if (!data || typeof data !== 'object') return fallback;
  if (!('error' in data)) return fallback;
  const err: unknown = data.error;
  if (typeof err === 'string' && err.trim()) return err.trim();
  if (err && typeof err === 'object') {
    if ('message' in err && typeof err.message === 'string' && err.message.trim()) {
      return err.message.trim();
    }
    if ('code' in err && typeof err.code === 'string' && err.code.trim()) {
      return err.code.trim();
    }
  }
  return fallback;
}

// apiErrorText unwraps one failed axios request into the message a feature
// shows inline, falling back to a caller-specific sentence. Every feature uses
// it so {error:{code,message}} is surfaced the same way.
export function apiErrorText(err: unknown, fallback: string): string {
  if (err && typeof err === 'object' && 'response' in err) {
    const resp = err.response;
    if (resp && typeof resp === 'object') {
      const status = 'status' in resp && typeof resp.status === 'number' ? resp.status : 0;
      const data = 'data' in resp ? resp.data : undefined;
      return apiErrorMessage(data, status);
    }
  }
  if (err instanceof Error && err.message) return err.message;
  return fallback;
}

// apiErrorStatus is the HTTP status on a failed axios request, or 0 when the
// failure never reached a response (network error, abort).
export function apiErrorStatus(err: unknown): number {
  if (err && typeof err === 'object' && 'response' in err) {
    const resp = err.response;
    if (resp && typeof resp === 'object' && 'status' in resp && typeof resp.status === 'number') {
      return resp.status;
    }
  }
  return 0;
}
