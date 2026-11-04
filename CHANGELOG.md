# Changelog

Notable changes to isola, in
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. `release.yml`
publishes the section matching the tag.

## [Unreleased]

### Added

- The viewer now accounts for packet gaps on the video socket: `seq` jumps
  (TCP, so they mean the streamer shed packets) are counted as `packetsLost`
  with a 1 Hz `packetLossRatio`, and once a burst reaches five missing packets
  it asks for a keyframe through the existing recovery budget instead of
  waiting on the IDR the encoder may never send. The debug metrics also show
  the server's `dropped` counter when a newer streamer reports it.

### Changed

- A viewer can no longer restart the session's encoder on demand, indefinitely:
  `request_keyframe` is now budgeted per connection — one forced restart per
  30 seconds, four per connection — and a share-link watcher is additionally
  limited to one forwarded request per 30 seconds by the relay. A viewer that
  still has no picture keeps a small retry allowance (three restarts, spaced by
  the existing 5 s cooldown) so a restart that produced no IDR cannot wedge it
  until it reconnects. Every forced restart replaces the encoder run for
  everyone watching it, so one stuck decoder could previously reset the host's
  stream forever.
- libx264 now runs `-preset veryfast` with `subme=2:me=hex` instead of
  `superfast` with `subme=1:me:dia`. The old tuning came from a corpus that
  never left the near-lossless regime, so it optimised motion-search cost while
  the bitrate budget sat unused; on text-heavy pages it was leaving quality on
  the table. Measured on the offline bench (`encoder_quality_bench_test.go`,
  gated on `ISOLA_QUALITY_BENCH=1`) against a dense-text corpus, 3 runs per row:
  720p SSIM 0.951 → 0.965 for 0.227 → 0.303 core-seconds per second of video,
  1080p 0.978 → 0.985 for 0.478 → 0.593. Finer presets buy a few thousandths
  more SSIM for two to four times the CPU, so this is the knee rather than the
  maximum.

### Fixed

- The 1 Hz `metrics` snapshot now carries `dropped`, the number of video packets
  this viewer's socket shed (mailbox overflow or discarded while waiting for a
  keyframe), so a stalling viewer is visible instead of looking like a slow
  encoder.
- `docs/transport.md` claimed an ABR `set_bitrate` reset every other viewer of
  the run; only the requesting viewer moves rungs, and the previous run stops
  when its last viewer leaves. The same page also said the streamer does not
  read the capture `ts_ns` header field; it does, nothing downstream uses it.

## [1.5.3] - 2026-11-04
