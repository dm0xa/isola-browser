export const MAX_PLAYOUT_LAG_SEC = 0.25;
export const LIVE_EDGE_SEC = 0.08;
export const MAX_AHEAD_SEC = 1.0;
// Pending segments hold fragments that arrived before the SourceBuffer could
// take them. With 200ms fragments this is ~3s of headroom, matching what the
// 1s-fragment era got from a cap of 3.
export const MAX_PENDING_SEGMENTS = 15;
// Consecutive appendBuffer throws tolerated before giving up on the
// SourceBuffer and forcing an encoder restart (request_keyframe →
// stream_reset/stream_init → normal MediaSource rebuild).
export const APPEND_MAX_RETRIES = 8;
export const APPEND_RETRY_MS = 250;
export const MSE_CODEC_FALLBACK = 'avc1.42E01E';
// On-demand keyframe recovery: after a decoder error or a reset with no IDR in
// sight, wait this long before asking the server to force one.
//
// Must be longer than the streamer's keyframeRestartGrace (4s) AND one GOP
// (2s at the encode rate). A late joiner on a static page is fed at full
// rate until the next IDR, which lands within that GOP. A shorter wait
// restarts ffmpeg before a cold encoder's first IDR is flushed, and the
// replacement is killed the same way.
export const KEYFRAME_REQUEST_TIMEOUT_MS = 5000;
// Keyframe requests tolerated without an IDR before giving up on the
// connection and letting the reconnect cycle rebuild everything.
export const MAX_KEYFRAME_REQUESTS = 4;

// ── WebCodecs decode backpressure ────────────────────────────────────────
// VideoDecoder.decode() is fire-and-forget: every access unit handed to it is
// queued and painted in order, so after a network burst (or a tab that was
// throttled for a moment) the queue holds the past, and the viewer watches it
// replay. The sink in webCodecsVideoPath.ts measures that queue in stream time
// (submitted-but-unpainted access units × the frame interval) and, above this
// budget, stops replaying it.
//
// 250 ms is two and a half times the 100 ms presentation target: short jitter
// and a single slow frame stay below it, a dropped 500 ms burst does not.
export const MAX_DECODE_BACKLOG_MS = 250;
// Minimum spacing between two skip-aheads. Each one abandons the decoder queue
// and asks the streamer for a fresh IDR, which is an ffmpeg restart on the
// server; back-to-back skips would spend the MAX_KEYFRAME_REQUESTS budget and
// trip the reconnect path instead of recovering.
export const DECODE_CATCHUP_MIN_INTERVAL_MS = 1000;
