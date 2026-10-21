import { describe, it, expect } from 'vitest';
import { captureResolutionFor, fitResolutionToAspect, parseResolution } from './resolution';

describe('parseResolution', () => {
  it('parses a WxH pair', () => {
    expect(parseResolution('1920x1080')).toEqual({ width: 1920, height: 1080 });
    expect(parseResolution(' 1280x720 ')).toEqual({ width: 1280, height: 720 });
  });

  it('rejects unusable input', () => {
    expect(parseResolution('')).toBeNull();
    expect(parseResolution('1920')).toBeNull();
    expect(parseResolution('1920*1080')).toBeNull();
    expect(parseResolution('1x1')).toBeNull();
  });
});

describe('fitResolutionToAspect', () => {
  it('reshapes inside the original box, so the pixel budget is kept', () => {
    // 16:9 -> 16:10: full width, shorter, never wider than the original.
    expect(fitResolutionToAspect('1920x1080', 1.6)).toBe('1728x1080');
    // Wider aspect than the box: height-limited, width follows the aspect.
    expect(fitResolutionToAspect('1920x1080', 2)).toBe('1920x960');
  });

  it('returns even dimensions', () => {
    const out = fitResolutionToAspect('1920x1080', 1.7777);
    const [w, h] = out.split('x').map(Number);
    expect(w % 2).toBe(0);
    expect(h % 2).toBe(0);
  });

  it('leaves the resolution alone when the aspect is unusable', () => {
    expect(fitResolutionToAspect('1920x1080', 0)).toBe('1920x1080');
    expect(fitResolutionToAspect('1920x1080', Number.NaN)).toBe('1920x1080');
    expect(fitResolutionToAspect('nonsense', 1.6)).toBe('nonsense');
  });
});

describe('captureResolutionFor', () => {
  // The numbers from a real session: launched at 1920x1018 for a 1720x913
  // windowed box, then fullscreen turns the box into 1728x1036.
  const BUDGET = '1920x1018';
  const LAUNCH = { width: 1920, height: 1018 };

  it('keeps the launch geometry when the box has not really changed', () => {
    expect(captureResolutionFor(BUDGET, LAUNCH, 1720, 913)).toBeNull();
    // A few pixels of drag are not worth an encoder restart.
    expect(captureResolutionFor(BUDGET, LAUNCH, 1726, 916)).toBeNull();
  });

  it('reshapes for fullscreen, inside the budget', () => {
    expect(captureResolutionFor(BUDGET, LAUNCH, 1728, 1036)).toEqual({ width: 1698, height: 1018 });
  });

  it('restores the windowed shape when fullscreen is left', () => {
    // `current` is what the sandbox has now, not the launch size.
    expect(captureResolutionFor(BUDGET, { width: 1698, height: 1018 }, 1720, 913))
      .toEqual({ width: 1918, height: 1018 });
  });

  it('never grows past the budget', () => {
    const target = captureResolutionFor(BUDGET, LAUNCH, 5000, 1000);
    expect(target).not.toBeNull();
    expect(target!.width).toBeLessThanOrEqual(1920);
    expect(target!.height).toBeLessThanOrEqual(1018);
  });

  it('ignores an unusable box or budget', () => {
    expect(captureResolutionFor(BUDGET, LAUNCH, 10, 900)).toBeNull();
    expect(captureResolutionFor(BUDGET, LAUNCH, 900, 0)).toBeNull();
    expect(captureResolutionFor('nonsense', LAUNCH, 1720, 913)).toBeNull();
  });
});
