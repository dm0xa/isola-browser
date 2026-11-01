import { describe, expect, it } from 'vitest';

import { abrTick, advanceArrivalWindow, copyChordStaysLocal, createArrivalWindow, mapClientToCapture, createFrameCounters, noteStreamedPacket, frameMetricsPatch, type AbrState, type AbrTick, type IsolaNetQuality } from './useIsolaStream';

const BASE_KBPS = 13500;
// abrLadderKbps(13500) — 100 / 70 / 45 / 30 % with a 500 kbps floor.
const LADDER = [13500, 9450, 6075, 4050];

function newState(overrides: Partial<AbrState> = {}): AbrState {
  return {
    level: 0,
    badSecs: 0,
    goodSecs: 0,
    lastChangeAt: 0,
    pausedUntil: 0,
    manual: false,
    ...overrides,
  };
}

// Ticks start well past `lastChangeAt`, so the 8s anti-oscillation interval
// never blocks the first step a test is trying to observe.
const T0 = 1_000_000;

type TickOverride = Partial<Omit<AbrTick, 'nowMs'>>;

/**
 * Feed a run of 1 Hz ticks to the controller, defaulting each tick to an idle
 * window so a test only has to state what it cares about. `targetKbps` tracks
 * the rung the viewer is on at the start of the tick.
 */
function run(state: AbrState, ticks: TickOverride[]) {
  const steps: number[] = [];
  let ratioEma: number | null = null;
  let netQuality: IsolaNetQuality = 'good';
  let netRatio = 0;

  ticks.forEach((override, i) => {
    const res = abrTick(state, {
      nowMs: T0 + i * 1000,
      sentKbps: 0,
      arrivalKbps: 0,
      targetKbps: LADDER[state.level],
      rttMs: 11,
      baseKbps: BASE_KBPS,
      ratioEma,
      ...override,
    });
    ratioEma = res.ratioEma;
    netQuality = res.netQuality;
    netRatio = res.netRatio;
    if (res.stepToKbps !== null) steps.push(res.stepToKbps);
  });

  return { steps, netQuality, netRatio, ratioEma };
}

describe('abrTick', () => {
  it('never steps down on an idle page that reports zero egress', () => {
    // Change-driven suppression ships one keepalive frame every couple of
    // seconds, so a static page reports sent_kbps 0 for most one-second
    // windows. That is the sender being idle, not loss: the metrics ride the
    // same socket as the video, so a zero window cannot mean the link dropped
    // everything. Read as arrival/target it walked a healthy 11ms session
    // 12750 → 3825 kbps (scale 0.5) in ~20s.
    const state = newState();
    const { steps } = run(
      state,
      Array.from({ length: 60 }, () => ({ sentKbps: 0, arrivalKbps: 0 })),
    );

    expect(steps).toEqual([]);
    expect(state.level).toBe(0);
  });

  it('reports an idle window as good and unmeasurable, not bad', () => {
    const state = newState();
    const { netQuality, netRatio } = run(state, [{ sentKbps: 0, arrivalKbps: 0 }]);

    expect(netQuality).toBe('good');
    expect(netRatio).toBe(0);
  });

  it('still steps down on sustained loss', () => {
    const state = newState();
    // 2000 kbps pushed, 1000 kbps arriving → ratio 0.5, under the 0.75 bad
    // threshold, for four consecutive seconds.
    const bad = { sentKbps: 2000, arrivalKbps: 1000 };
    const { steps, netQuality } = run(state, [bad, bad, bad, bad]);

    expect(steps).toEqual([LADDER[1]]);
    expect(state.level).toBe(1);
    expect(netQuality).toBe('bad');
  });

  it('does not carry a bad streak across an idle window', () => {
    // Three bad seconds leave badSecs one short of ABR_DOWN_SECS. An idle
    // window is no evidence either way, so it must reset the streak instead of
    // letting the next stray sample complete it.
    const state = newState();
    const bad = { sentKbps: 2000, arrivalKbps: 1000 };
    const { steps } = run(state, [bad, bad, bad, { sentKbps: 0, arrivalKbps: 0 }, bad]);

    expect(steps).toEqual([]);
    expect(state.level).toBe(0);
    expect(state.badSecs).toBe(1);
  });

  it('walks back up the ladder after a sustained good run', () => {
    const state = newState({ level: 3 });
    const good = { sentKbps: 4000, arrivalKbps: 4000 };
    const { steps } = run(
      state,
      Array.from({ length: 15 }, () => good),
    );

    expect(steps).toEqual([LADDER[2]]);
    expect(state.level).toBe(2);
  });

  it('leaves the rung alone while a manual override is active', () => {
    const state = newState({ manual: true });
    const bad = { sentKbps: 2000, arrivalKbps: 1000 };
    const { steps } = run(state, [bad, bad, bad, bad, bad, bad]);

    expect(steps).toEqual([]);
    expect(state.level).toBe(0);
  });

  it('treats a stalled RTT as bad even when everything arrives', () => {
    const state = newState();
    const slow = { sentKbps: 4000, arrivalKbps: 4000, rttMs: 500 };
    const { steps, netQuality } = run(state, [slow, slow, slow, slow]);

    expect(steps).toEqual([LADDER[1]]);
    expect(netQuality).toBe('bad');
  });
});

describe('frame counters', () => {
  it('starts empty', () => {
    expect(frameMetricsPatch(createFrameCounters())).toEqual({ sequenceNumber: 0, keyframeCount: 0 });
  });

  it('tracks the latest sequence number and counts only IDRs', () => {
    const c = createFrameCounters();
    noteStreamedPacket(c, 7, false);
    noteStreamedPacket(c, 8, true);
    noteStreamedPacket(c, 9, false);

    expect(frameMetricsPatch(c)).toEqual({ sequenceNumber: 9, keyframeCount: 1 });
  });

  it('keeps counting IDRs across the whole view', () => {
    const c = createFrameCounters();
    for (let i = 1; i <= 5; i++) noteStreamedPacket(c, i, true);
    expect(frameMetricsPatch(c).keyframeCount).toBe(5);
  });

  // The counters are kept in a ref, so they must mutate in place: a fresh
  // object per packet would be a new ref value, and the whole point of moving
  // them out of React state was that the frame path never re-renders.
  it('mutates in place so the ref identity is stable', () => {
    const c = createFrameCounters();
    noteStreamedPacket(c, 42, true);
    expect(c.sequenceNumber).toBe(42);
  });
});

describe('mapClientToCapture', () => {
  it('maps an exact-fit capture straight through', () => {
    // rect == frame == capture, no letterbox: a pure offset by rect.top.
    const rect = { left: 0, top: 36, width: 1000, height: 500 };
    expect(mapClientToCapture(250, 100, rect, 1000, 500, 1000, 500)).toEqual({ x: 250, y: 64 });
  });

  it('undoes the object-fit: contain letterbox offset', () => {
    // 2000x1000 shown in a 1400x500 box: scale 0.5, 200px pillarbox each side.
    const rect = { left: 0, top: 36, width: 1400, height: 500 };
    // Left edge of the image, not of the box.
    expect(mapClientToCapture(200, 36, rect, 2000, 1000, 2000, 1000)).toEqual({ x: 0, y: 0 });
    // Horizontal centre of the image.
    expect(mapClientToCapture(700, 36, rect, 2000, 1000, 2000, 1000)).toEqual({ x: 1000, y: 0 });
  });

  it('rescales frame pixels into capture pixels for a scaled-down encode', () => {
    // 1728x1117 encode of a 3456x2234 capture: each frame pixel is 2 capture px.
    const rect = { left: 0, top: 0, width: 1728, height: 1117 };
    expect(mapClientToCapture(864, 558, rect, 1728, 1117, 3456, 2234)).toEqual({ x: 1728, y: 1116 });
  });

  it('clamps to the capture bounds instead of leaving the frame', () => {
    const rect = { left: 0, top: 0, width: 1000, height: 500 };
    expect(mapClientToCapture(-500, -500, rect, 1000, 500, 1000, 500)).toEqual({ x: 0, y: 0 });
    expect(mapClientToCapture(99999, 99999, rect, 1000, 500, 1000, 500)).toEqual({ x: 999, y: 499 });
  });

  it('falls back to source size when the capture size is unknown', () => {
    const rect = { left: 0, top: 0, width: 1000, height: 500 };
    expect(mapClientToCapture(10, 10, rect, 1000, 500, 0, 0)).toEqual({ x: 10, y: 10 });
  });

  it('returns (0,0) before any frame geometry is known', () => {
    // A null canvas / not-yet-decoded video used to be read as width 0 here.
    const rect = { left: 0, top: 0, width: 1000, height: 500 };
    expect(mapClientToCapture(10, 10, rect, 0, 0, 1000, 500)).toEqual({ x: 0, y: 0 });
  });
});

describe('advanceArrivalWindow', () => {
  it('waits for a second message before reporting a rate', () => {
    const win = createArrivalWindow();
    advanceArrivalWindow(win, 1000, 5000);
    expect(win).toEqual({ atMs: 1000, bytes: 5000, kbps: 0, fresh: false });
  });

  it('rates exactly the bytes received inside the window the server reported', () => {
    const win = createArrivalWindow();
    advanceArrivalWindow(win, 1000, 5000);
    // 80 kB in one second = 640 kbps, the same second sent_kbps describes.
    advanceArrivalWindow(win, 2000, 5000 + 80_000);
    expect(win.kbps).toBe(640);
    expect(win.fresh).toBe(true);
  });

  it('keeps the last usable sample when a window has no duration', () => {
    // Two metrics messages in one task, or a clock hiccup: a 0 ms window has no
    // rate, and reporting 0 there is indistinguishable from total loss.
    const win = createArrivalWindow();
    advanceArrivalWindow(win, 1000, 5000);
    advanceArrivalWindow(win, 2000, 85_000);
    advanceArrivalWindow(win, 2000, 85_000);
    expect(win.kbps).toBe(640);
    expect(win.fresh).toBe(true);
  });

  it('reports a burst-and-quiet page as healthy, not as loss', () => {
    // The reported bug: a pointer dragged across links repaints → one burst of
    // frames, then no egress at all until the next interaction. A free-running
    // client window that starts after the burst counts those bytes in the
    // previous window and reads 0 while sent_kbps still describes the burst, so
    // the ratio hit 0 and the toolbar signal went red for a second. Measuring
    // the same window the server measured keeps the two in agreement.
    const win = createArrivalWindow();
    advanceArrivalWindow(win, T0 - 1000, 0); // prime: the marker needs a predecessor
    const state = newState();
    let ratioEma: number | null = null;
    let quality: IsolaNetQuality = 'good';
    for (let i = 0; i < 3; i++) {
      advanceArrivalWindow(win, T0 + i * 1000, win.bytes + 80_000);
      const res = abrTick(state, {
        nowMs: T0 + i * 1000 + 500,
        sentKbps: 640,
        arrivalKbps: win.kbps,
        targetKbps: LADDER[0],
        rttMs: 6,
        baseKbps: BASE_KBPS,
        ratioEma,
      });
      ratioEma = res.ratioEma;
      quality = res.netQuality;
    }
    expect(quality).toBe('good');
  });
});

describe('copyChordStaysLocal', () => {
  // The stream container holds the remote page's canvas/video plus the tab
  // strip, so "focus inside the container" means the chord is aimed at the
  // remote browser.
  const container = document.createElement('div');
  const canvas = document.createElement('canvas');
  const urlField = document.createElement('input');
  container.append(canvas, urlField);
  document.body.append(container);

  it('relays the chord when the viewer itself has focus', () => {
    expect(copyChordStaysLocal(container, container)).toBe(false);
  });

  it('relays the chord when a child of the viewer has focus', () => {
    expect(copyChordStaysLocal(container, canvas)).toBe(false);
  });

  it('keeps the chord native when focus is on host UI outside the viewer', () => {
    const toolbarButton = document.createElement('button');
    document.body.append(toolbarButton);
    expect(copyChordStaysLocal(container, toolbarButton)).toBe(true);
  });

  it('keeps the chord native when nothing is focused', () => {
    // document.activeElement is null in a fresh document; a stale selection
    // anchored in the toolbar must not be mistaken for a remote target.
    expect(copyChordStaysLocal(container, null)).toBe(true);
  });
});
