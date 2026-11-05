import type { IsolaNetQuality } from './abr';

export interface IsolaStreamMetrics {
  connectMs: number | null;
  firstFrameMs: number | null;
  recentFps: number;
  sequenceNumber: number;
  keyframeCount: number;
  // Packet loss off the wire's per-run uint32 sequence number (header offset
  // 2). The socket is TCP, so a gap is not network loss: it is the streamer
  // shedding packets for this viewer (mailbox overflow), and the picture
  // cannot resync until a fresh IDR arrives. `packetsLost` is cumulative for
  // the whole view (it survives stream_reset; only the gap baseline resets),
  // and `packetLossRatio` is windowed: lost / (lost + received) over the
  // packets counted since the previous 1 Hz tick, 0 when that window carried
  // no packets (an idle stream is not evidence of loss).
  packetsLost: number;
  packetLossRatio: number;
  // The sandbox's own packet-shed counter, when the server reports it
  // (newer streamers send `dropped` per metrics message). 0 on older servers
  // that never include the field.
  serverDropped: number;
  wsState: number;
  inputBackend: string;
  // URL of the page target CDP input is bound to, and whether that is still
  // the active tab. inputTargetOK === false is the "clicks and keys do
  // nothing" state: input is landing in a tab the viewer cannot see, while
  // video (an independent capture) keeps flowing. The streamer re-aligns the
  // binding on its own; this pair exists so the overlay can show it.
  inputTarget: string;
  inputTargetOK: boolean;
  activityMode: string;
  playoutLagMs: number;
  codec: string;
  streamWidth: number;
  streamHeight: number;
  targetFps: number;
  targetBitrateKbps: number;
  bytesReceived: number;
  bitrateKbps: number;
  segmentRate: number;
  bufferedSec: number;
  pendingSegments: number;
  playbackRate: number;
  videoWidth: number;
  videoHeight: number;
  lastSegmentAgeMs: number;
  appendCount: number;
  encoderSeq: number;
  captureWidth: number;
  captureHeight: number;
  rttMs: number | null;
  netQuality: IsolaNetQuality;
  // Arrival ratio used to derive netQuality: measured receive bitrate over
  // what the server actually pushed (clamped by the encoder target). 0 when
  // there is too little signal to judge (idle egress, post-reset pause).
  netRatio: number;
  // Current ABR ladder rung (0 = full base bitrate, higher = stepped down).
  abrLevel: number;
  decodeErrorCount: number;
  lastRecoveryMs: number | null;
  // ── WebCodecs decode backpressure ──
  // decodeBacklogMs is the presentation delay the raw-Annex-B path is carrying
  // at the moment the tick runs: access units submitted to the VideoDecoder but
  // not yet painted, × the frame interval — stream time, not wall time, so it
  // is independent of how bursty the wire was. 0 when caught up (an idle page
  // included); ~one frame interval with a single frame in flight. Above
  // MAX_DECODE_BACKLOG_MS the client stops replaying the backlog and resumes on
  // the next IDR (see webCodecsVideoPath.ts).
  //
  // decodeDroppedFrames counts every access unit the backlog policy threw
  // away — both the ones abandoned in the decoder queue and the ones refused
  // while waiting for the replacement IDR — and decodeCatchUps counts the
  // skips themselves. Drops rising while catchUps stays flat would mean the
  // resync IDR never lands.
  decodeBacklogMs: number;
  decodeDroppedFrames: number;
  decodeCatchUps: number;
  encodeScale: number;
  staticSecs: number;
  skippedTicks: number;
  keyframeReason: string;
  keyframeAgeMs: number;
  // Renderer health. A wedged page renderer (heavy page on a CPU-only host,
  // e.g. WebGL) freezes the stream while every other number here looks
  // ordinary — static_secs and a low fps read the same as an idle page. The
  // runner replaces the renderer and reloads the tab; these fields are how the
  // toolbar explains it instead of leaving a frozen picture unexplained.
  rendererStalled: boolean;
  rendererStalledMs: number;
  rendererRecoveries: number;
  cdpProbeFailStreak: number;
  cdpProbeFailAgoMs: number;
  // Capture path health, reported by the sandbox every couple of seconds. The
  // renderer can be perfectly healthy (CDP answers) while the compositor hands
  // back empty paint images, which freezes the picture with no other symptom:
  // empty_frames counts those, and fallback marks the state where the sandbox
  // switched to screenshot-driven frames to keep the stream alive.
  captureHealthOk: boolean;
  captureEmptyFrames: number;
  captureEmptyWindow: number;
  captureFallback: boolean;
  captureLastPaintMs: number;
  captureLastFrameMs: number;
  // Wire accounting from the sandbox, and the only fields here that answer "is
  // the picture actually changing?". Every other liveness number is satisfied
  // by a no-op repaint: the 100 ms keepalive nudge repaints an unchanged page,
  // which refreshes capture_last_paint_ms and keeps recentFps at the decode
  // rate, so a frozen stream reads exactly like a moving one. changesPerSec /
  // damagedPct come from the encoder's own pixel comparison (a changed update
  // and how much of the surface it touched), and dedupFrames climbing while
  // sentFrames stalls is a frozen picture the sandbox is composing happily.
  changesPerSec: number;
  fullFramesPerSec: number;
  damagedPct: number;
  captureLastSentMs: number;
  captureSentFrames: number;
  captureDedupFrames: number;
}
