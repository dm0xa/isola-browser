import { useSyncExternalStore } from 'react';

/**
 * Shared launch progress for the "starting remote browser" flow.
 *
 * Launching a session crosses a route boundary: RunPage creates the session,
 * then navigates to ViewerPage, which owns the stream. Keeping the overlay in
 * either page remounted it at the hand-off (logo animation restarting, phases
 * jumping back to "Provisioning container"). This module-level store lets the
 * overlay live above the router, so the same overlay instance advances through
 * all phases: provisioning → connecting → first frame.
 */
export type LaunchSnapshot = {
  active: boolean;
  /** Backend session status (provisioning, running, ...) or 'queued'. */
  status: string;
  caption?: string;
};

const inactiveSnapshot: LaunchSnapshot = { active: false, status: 'provisioning' };

let snapshot: LaunchSnapshot = inactiveSnapshot;
const listeners = new Set<() => void>();

function publish(next: LaunchSnapshot) {
  snapshot = next;
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function getSnapshot(): LaunchSnapshot {
  return snapshot;
}

/** Start a fresh launch flow (idempotent for a launch already in progress). */
export function beginLaunch() {
  if (snapshot.active) return;
  publish({ active: true, status: 'provisioning' });
}

/** Advance the visible status; ignored once the flow has ended. */
export function setLaunchStatus(status: string, caption?: string) {
  if (!snapshot.active) return;
  if (snapshot.status === status && snapshot.caption === caption) return;
  publish({ active: true, status, caption });
}

/** Hand off to the viewer / show an error: the launch overlay goes away. */
export function endLaunch() {
  if (!snapshot.active) return;
  publish(inactiveSnapshot);
}

export function getLaunchSnapshot(): LaunchSnapshot {
  return snapshot;
}

export function useLaunchProgress(): LaunchSnapshot {
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot);
}
