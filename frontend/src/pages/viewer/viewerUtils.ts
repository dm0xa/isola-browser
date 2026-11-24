/** Short form of the page CDP input target for the debug overlay. */
export function shortInputTarget(url: string): string {
  if (!url) return '...';
  const bare = url.replace(/^[a-z]+:\/\//i, '');
  return bare.length > 44 ? `${bare.slice(0, 43)}…` : bare;
}

/**
 * Secondary line under the viewer error screen. Only a 404 means the session is
 * gone; a launch failure ("Session encountered an error") is the sandbox — the
 * old copy claimed expiry for both, which sent people looking for a dead ID
 * instead of the docker/orchestrator failure.
 *
 * `errorCode` is the backend's stable reason (`GET /api/sessions/{id}` →
 * `error_code`), set only when container creation failed. It turns the generic
 * "check the log" line into the one action that actually helps.
 */
export function sessionErrorDetail(sessionError: string, errorCode?: string): string {
  switch (sessionError) {
    case 'Session not found':
      return 'The browser session you are looking for does not exist or has expired.';
    case 'Session has ended':
      return 'This session was closed. Start a new one to keep browsing.';
    case 'Session encountered an error':
      return launchFailureDetail(errorCode);
    default:
      return 'The browser encountered an unexpected problem.';
  }
}

/** launchFailureDetail maps the backend's error_code to an actionable sentence. */
function launchFailureDetail(errorCode?: string): string {
  switch (errorCode) {
    case 'sandbox_publish_unreachable':
      return 'The sandbox started but this host could not reach it: Docker port publishing is down. Run scripts/doctor.sh, then restart the container runtime (colima restart).';
    case 'stream_not_ready':
      return 'The sandbox started but its video stream never became ready. Try again; if it repeats, run scripts/doctor.sh.';
    case 'create_failed':
      return 'Docker could not create the sandbox container. Run scripts/doctor.sh to check that the container runtime is reachable.';
    default:
      return 'The sandbox could not start. Run scripts/doctor.sh for a verdict, or check the isola log for the reason.';
  }
}
