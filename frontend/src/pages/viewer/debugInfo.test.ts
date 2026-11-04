import { afterEach, describe, expect, it, vi } from 'vitest';

import type { IsolaStreamMetrics } from '../../hooks/useIsolaStream';
import {
  buildDebugInfo,
  copyTextToClipboard,
  formatDebugInfo,
  type ViewerDebugInput,
} from './debugInfo';

function metrics(overrides: Partial<IsolaStreamMetrics> = {}): IsolaStreamMetrics {
  return {
    connectMs: 120,
    firstFrameMs: 300,
    recentFps: 30,
    sequenceNumber: 42,
    keyframeCount: 3,
    wsState: 1,
    inputBackend: 'cdp',
    inputTarget: 'https://example.com/',
    inputTargetOK: true,
    activityMode: 'active',
    playoutLagMs: 40,
    codec: 'vp9',
    streamWidth: 1280,
    streamHeight: 720,
    targetFps: 30,
    targetBitrateKbps: 4000,
    bytesReceived: 204800,
    bitrateKbps: 3200,
    segmentRate: 2,
    bufferedSec: 0.4,
    pendingSegments: 1,
    playbackRate: 1,
    videoWidth: 1280,
    videoHeight: 720,
    lastSegmentAgeMs: 100,
    appendCount: 42,
    encoderSeq: 41,
    captureWidth: 1280,
    captureHeight: 720,
    rttMs: 25,
    netQuality: 'good',
    netRatio: 0.9,
    abrLevel: 0,
    decodeErrorCount: 0,
    lastRecoveryMs: null,
    encodeScale: 1,
    staticSecs: 0,
    skippedTicks: 0,
    keyframeReason: 'keyframe',
    keyframeAgeMs: 1200,
    rendererStalled: false,
    rendererStalledMs: 0,
    rendererRecoveries: 0,
    cdpProbeFailStreak: 0,
    cdpProbeFailAgoMs: -1,
    captureHealthOk: true,
    captureEmptyFrames: 0,
    captureEmptyWindow: 0,
    captureFallback: false,
    captureLastPaintMs: 20,
    captureLastFrameMs: 20,
    changesPerSec: 15,
    fullFramesPerSec: 0,
    damagedPct: 12,
    captureLastSentMs: 10,
    captureSentFrames: 100,
    captureDedupFrames: 5,
    packetsLost: 0,
    packetLossRatio: 0,
    serverDropped: 0,
    ...overrides,
  };
}

function input(overrides: Partial<ViewerDebugInput> = {}): ViewerDebugInput {
  return {
    transport: 'isola',
    sessionId: 'abcdef1234567890',
    targetUrl: 'https://example.com/',
    resolution: '1280x720',
    status: 'running',
    viewOnly: false,
    shadow: false,
    viewerCount: 2,
    remainingSec: 512,
    isUnlimited: false,
    quality: 'auto',
    loadMs: 1234,
    networkCaptureEnabled: true,
    browserTabCount: 3,
    isConnected: true,
    isFirstFrame: true,
    streamError: null,
    isReconnecting: false,
    clipboardSynced: true,
    showOverlay: false,
    blankCover: false,
    showError: false,
    latencyText: '40ms',
    fpsText: '30.0',
    metrics: metrics(),
    now: 1_700_000_000_000,
    ...overrides,
  };
}

describe('buildDebugInfo', () => {
  it('keeps the flat fields external tooling already reads', () => {
    const info = buildDebugInfo(input());

    expect(info.ts).toBe(1_700_000_000_000);
    expect(info.status).toBe('running');
    expect(info.transport).toBe('isola');
    expect(info.isConnected).toBe(true);
    expect(info.streamError).toBeNull();
    expect(info.loadMs).toBe(1234);
    expect(info.codec).toBe('vp9');
    expect(info.isola).toMatchObject({ sequenceNumber: 42 });
  });

  it('derives the short id and carries the session facts', () => {
    const info = buildDebugInfo(input());

    expect(info.session.idShort).toBe('abcdef12');
    expect(info.session.viewerCount).toBe(2);
    expect(info.session.remainingSec).toBe(512);
    expect(info.session.browserTabCount).toBe(3);
    expect(info.session.networkCaptureEnabled).toBe(true);
  });

  it('collects runtime facts from the browser', () => {
    const info = buildDebugInfo(input());

    expect(info.runtime.userAgent).toBe(navigator.userAgent);
    expect(info.runtime.viewport).toMatch(/^\d+x\d+$/);
    expect(info.generatedAt).toBe(new Date(1_700_000_000_000).toISOString());
  });

  it('tolerates a missing session id', () => {
    const info = buildDebugInfo(input({ sessionId: undefined }));

    expect(info.session.id).toBe('');
    expect(info.session.idShort).toBe('');
  });
});

describe('formatDebugInfo', () => {
  it('renders the runtime, session and stream sections', () => {
    const text = formatDebugInfo(buildDebugInfo(input()));

    expect(text).toContain('isola viewer debug ·');
    expect(text).toContain('runtime');
    expect(text).toContain('ua            ');
    expect(text).toContain('session');
    expect(text).toContain('isola · abcdef12');
    expect(text).toContain('status        running · viewonly no · shadow no · viewers 2');
    expect(text).toContain('stream');
    expect(text).toContain('codec         vp9');
  });

  it('formats a finite timer and marks an unlimited one', () => {
    const finite = formatDebugInfo(buildDebugInfo(input({ remainingSec: 512 })));
    expect(finite).toContain('timer         8:32');

    const unlimited = formatDebugInfo(buildDebugInfo(input({ isUnlimited: true })));
    expect(unlimited).toContain('timer         unlimited');
  });

  it('flags input bound to a non-active tab', () => {
    const text = formatDebugInfo(buildDebugInfo(input({
      metrics: metrics({ inputTargetOK: false, inputTarget: 'https://other.example/' }),
    })));

    expect(text).toContain('input target  https://other.example/ (NOT THE ACTIVE TAB)');
  });

  it('ends with JSON that round-trips the structured report', () => {
    const info = buildDebugInfo(input());
    const text = formatDebugInfo(info);
    const json = text.slice(text.indexOf('--- json ---') + '--- json ---\n'.length);

    expect(JSON.parse(json)).toEqual(info);
  });
});

describe('buildDebugInfo enrichments', () => {
  it('carries build identity, error text, audio and reconnect state', () => {
    const info = buildDebugInfo(input({
      version: '1.4.0',
      commit: 'abc1234',
      errorText: 'Session encountered an error',
      sessionError: 'Session encountered an error',
      audioActive: true,
      audioMuted: false,
      restarting: true,
      reconnects: 3,
    }));

    expect(info.app).toEqual({ version: '1.4.0', commit: 'abc1234' });
    expect(info.errorText).toBe('Session encountered an error');
    expect(info.sessionError).toBe('Session encountered an error');
    expect(info.session.audioActive).toBe(true);
    expect(info.session.audioMuted).toBe(false);
    expect(info.session.restarting).toBe(true);
    expect(info.session.reconnects).toBe(3);
  });

  it('defaults build identity and flags when unset', () => {
    const info = buildDebugInfo(input());

    expect(info.app).toEqual({ version: 'dev', commit: 'unknown' });
    expect(info.session.reconnects).toBe(0);
    expect(info.session.restarting).toBe(false);
  });

  it('probes browser capabilities without throwing', () => {
    const r = buildDebugInfo(input()).runtime;

    for (const key of ['online', 'mobile', 'secureContext', 'webCodecs', 'mediaSource'] as const) {
      expect(typeof r[key]).toBe('boolean');
    }
    expect(typeof r.readyState).toBe('string');
    expect(typeof r.uptimeSec).toBe('number');
  });
});

describe('formatDebugInfo enrichments', () => {
  it('renders the app, capability, audio, reconnect and error lines', () => {
    const text = formatDebugInfo(buildDebugInfo(input({
      version: '1.4.0',
      commit: 'abc1234',
      streamError: 'MSE not supported or codec unavailable',
    })));

    expect(text).toContain('app');
    expect(text).toContain('version       1.4.0 (abc1234)');
    expect(text).toContain('browser       secure context');
    expect(text).toContain('audio         active no · muted no');
    expect(text).toContain('reconnects    0 · restarting no');
    expect(text).toContain('errors');
    expect(text).toContain('stream        MSE not supported or codec unavailable');
    expect(text).toContain('session       none');
  });
});

describe('runtime href redaction', () => {
  const original = window.location.href;
  afterEach(() => {
    window.history.replaceState(null, '', original);
  });

  it('strips credential-bearing query and fragment params from href', () => {
    window.history.replaceState(null, '', '/session/abc?session_token=secret&debug=1#admin_token=jwt');
    const { runtime } = buildDebugInfo(input());

    expect(runtime.href).not.toContain('secret');
    expect(runtime.href).not.toContain('jwt');
    expect(runtime.href).toContain('debug=1');
  });
});

describe('copyTextToClipboard', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    // Remove any clipboard stub a test installed.
    delete (navigator as { clipboard?: unknown }).clipboard;
  });

  it('uses the async clipboard API when available', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });

    await expect(copyTextToClipboard('hi')).resolves.toBe(true);
    expect(writeText).toHaveBeenCalledWith('hi');
  });

  it('falls back to execCommand when the clipboard API is unavailable', async () => {
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
    const execCommand = vi.fn().mockReturnValue(true);
    (document as unknown as { execCommand: unknown }).execCommand = execCommand;

    await expect(copyTextToClipboard('fallback')).resolves.toBe(true);
    expect(execCommand).toHaveBeenCalledWith('copy');
    // The temporary textarea must not be left behind.
    expect(document.querySelector('textarea')).toBeNull();
  });

  it('resolves false when the clipboard API rejects and execCommand is unavailable', async () => {
    const writeText = vi.fn().mockRejectedValue(new Error('denied'));
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    (document as unknown as { execCommand: unknown }).execCommand = vi.fn().mockReturnValue(false);

    await expect(copyTextToClipboard('nope')).resolves.toBe(false);
  });
});
