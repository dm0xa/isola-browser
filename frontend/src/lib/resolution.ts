// Resolution helpers shared by the launcher and the viewer.
//
// The sandbox captures a fixed viewport, so both paths reshape the session's
// pixel budget to the viewer's aspect ratio: RunPage when it picks the initial
// size, ViewerPage when fullscreen or a window drag changes the capture box.
// Without that, object-fit: contain letterboxes the old shape instead of
// letting the page reflow.

export type CaptureSize = { width: number; height: number };

/** Parses a `WxH` resolution, or null when it is not a usable pair. */
export function parseResolution(resolution: string): CaptureSize | null {
  const match = /^(\d+)x(\d+)$/.exec(resolution.trim());
  if (!match) return null;
  const width = Number(match[1]);
  const height = Number(match[2]);
  if (width < 2 || height < 2) return null;
  return { width, height };
}

/**
 * Reshapes `resolution` to `aspect`, staying inside the original box so the
 * pixel budget implied by the session's own resolution is preserved. Even
 * dimensions only: H.264 4:2:0 cannot encode odd ones.
 */
export function fitResolutionToAspect(resolution: string, aspect: number): string {
  const box = parseResolution(resolution);
  if (!box || !Number.isFinite(aspect) || aspect <= 0) return resolution;
  let width = box.width;
  let height = Math.round(width / aspect);
  if (height > box.height) {
    height = box.height;
    width = Math.round(height * aspect);
  }
  width -= width % 2;
  height -= height % 2;
  if (width < 2 || height < 2) return resolution;
  return `${width}x${height}`;
}

/**
 * The sandbox size to request for a capture box of `boxWidth`x`boxHeight`, or
 * null when `current` already matches it closely enough that a resize — and the
 * encoder restart it costs — would not change what the viewer sees.
 *
 * `budget` is the session's launch resolution, which is the pixel budget every
 * request reshapes: it must not shrink, or toggling fullscreen back and forth
 * would degrade the picture a step at a time.
 */
export function captureResolutionFor(
  budget: string,
  current: CaptureSize,
  boxWidth: number,
  boxHeight: number,
  tolerance = 0.02,
): CaptureSize | null {
  if (!(boxWidth >= 64) || !(boxHeight >= 64)) return null;
  const target = parseResolution(fitResolutionToAspect(budget, boxWidth / boxHeight));
  if (!target) return null;
  const close = (a: number, b: number) => Math.abs(a - b) / Math.max(a, b) <= tolerance;
  return close(target.width, current.width) && close(target.height, current.height) ? null : target;
}
