import { cleanup, render } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import type { IsolaStreamMetrics } from '../../hooks/useIsolaStream';
import type { SessionParams } from '../../lib/sessionParams';
import { ViewerDebugOverlay } from './ViewerDebugOverlay';

afterEach(cleanup);

const params: SessionParams = {
  resolution: '1920x1080',
  frameRate: 30,
  browserType: 'chromium',
  timezone: 'Asia/Tokyo',
  locale: 'ja-JP',
  torEnabled: false,
  torCountry: '',
};

const metrics = {
  wsState: 1,
  connectMs: 10,
  firstFrameMs: 40,
  codec: 'avc1',
  streamWidth: 1280,
  streamHeight: 720,
  targetFps: 30,
  encodeScale: 1,
  videoWidth: 1280,
  videoHeight: 720,
  recentFps: 30,
  playoutLagMs: 20,
  playbackRate: 1,
  bufferedSec: 0,
  pendingSegments: 0,
  segmentRate: 1,
  sequenceNumber: 1,
  keyframeCount: 0,
  bytesReceived: 1024,
  bitrateKbps: 8000,
  targetBitrateKbps: 8000,
  lastSegmentAgeMs: 10,
  appendCount: 1,
  staticSecs: 0,
  skippedTicks: 0,
  rendererRecoveries: 0,
  cdpProbeFailStreak: 0,
  changesPerSec: 1,
  damagedPct: 1,
  fullFramesPerSec: 0,
  captureHealthOk: true,
  captureEmptyFrames: 0,
  captureEmptyWindow: 0,
  captureFallback: false,
  captureLastPaintMs: 1,
  captureSentFrames: 1,
  captureDedupFrames: 0,
  keyframeReason: 'timer',
  keyframeAgeMs: 10,
  decodeErrorCount: 0,
  lastRecoveryMs: null,
  inputBackend: 'cdp',
  inputTarget: 'https://example.com/',
  inputTargetOK: true,
  activityMode: 'active',
  encoderSeq: 1,
  netQuality: 'good',
  netRatio: 1,
  rttMs: 10,
  abrLevel: 0,
} as unknown as IsolaStreamMetrics;

function renderOverlay(sessionParams: SessionParams = params) {
  return render(
    <ViewerDebugOverlay
      enabled
      transport="isola"
      id="22f607d5-81d4"
      debugCopied={false}
      debugCopyFailed={false}
      copyDebugOverlay={() => {}}
      toggleDebug={() => {}}
      latencyColor=""
      latencyText="20ms"
      fpsColor=""
      fpsText="30"
      isolaMetrics={metrics}
      loadMs={123}
      loadSec="0.1"
      quality="auto"
      rendererStallVisible={false}
      rendererStalledSec={0}
      sessionParams={sessionParams}
    />,
  );
}

describe('ViewerDebugOverlay session parameters', () => {
  it('shows the launch parameters and page identity', () => {
    const { container } = renderOverlay();
    expect(container.textContent).toContain('1920x1080 · 30 fps · chromium');
    expect(container.textContent).toContain('identity tz Asia/Tokyo · locale ja-JP');
  });

  it('names the image defaults and the Tor exit when they apply', () => {
    const { container } = renderOverlay({ ...params, timezone: '', locale: '', torEnabled: true, torCountry: 'se' });
    expect(container.textContent).toContain('identity tz UTC (default) · locale en-US (default) · tor se');
  });
});
