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
 */
export function sessionErrorDetail(sessionError: string): string {
  switch (sessionError) {
    case 'Session not found':
      return 'The browser session you are looking for does not exist or has expired.';
    case 'Session has ended':
      return 'This session was closed. Start a new one to keep browsing.';
    case 'Session encountered an error':
      return 'The sandbox could not start. If it fails again, check the isola log for the reason.';
    default:
      return 'The browser encountered an unexpected problem.';
  }
}
