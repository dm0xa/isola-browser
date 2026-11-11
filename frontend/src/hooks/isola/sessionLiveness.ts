/**
 * Session statuses in which a socket retry can still succeed. `provisioning` is
 * included because the viewer may attach as soon as the API hands out `ws_url`,
 * while the container is still coming up.
 */
const RETRYABLE_SESSION_STATUS: Record<string, true> = {
  provisioning: true,
  running: true,
};

/**
 * True when the status reported by `GET /api/sessions/{id}` means the sandbox is
 * gone for good, so reconnecting would only hammer the relay until the row is
 * cleaned up.
 *
 * A terminated or errored session keeps answering 200 for the rest of its TTL,
 * so the status field — not the HTTP code — is the signal. An unknown or missing
 * status must not stop the loop: the probe's only job is to recognize a
 * definitive terminal status, and stopping on a shape we do not understand would
 * strand a live session on a parse quirk.
 */
export function isSessionTerminated(status: unknown): boolean {
  if (typeof status !== 'string' || status === '') return false;
  return !Object.prototype.hasOwnProperty.call(RETRYABLE_SESSION_STATUS, status);
}
