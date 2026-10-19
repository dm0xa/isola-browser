import { describe, expect, it } from 'vitest';
import {
  beginLaunch,
  endLaunch,
  getLaunchSnapshot,
  setLaunchStatus,
} from './launchProgress';

describe('launchProgress', () => {
  it('advances one launch flow in place and ignores updates after it ends', () => {
    expect(getLaunchSnapshot().active).toBe(false);

    beginLaunch();
    expect(getLaunchSnapshot()).toEqual({ active: true, status: 'provisioning' });

    // A second begin (StrictMode double effect, re-render) must not reset the
    // phase the user is already looking at.
    setLaunchStatus('running');
    beginLaunch();
    expect(getLaunchSnapshot()).toEqual({ active: true, status: 'running' });

    setLaunchStatus('queued', 'AT CAPACITY');
    expect(getLaunchSnapshot()).toEqual({
      active: true,
      status: 'queued',
      caption: 'AT CAPACITY',
    });

    endLaunch();
    expect(getLaunchSnapshot().active).toBe(false);

    // Late polls from the viewer must not resurrect the overlay.
    setLaunchStatus('running');
    expect(getLaunchSnapshot().active).toBe(false);
  });
});
