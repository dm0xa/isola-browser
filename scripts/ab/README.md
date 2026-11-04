# Streamer A/B harness

Compares two streamer builds end to end — real binary, real ISOL frames in, real
WebCodecs viewer out — and reports quality, bytes, CPU and encoder restarts per
run. It answers "is this build better than that one", which the in-package
benches cannot: they call `startEncoder` directly and skip the hub, the frame
pump, and the viewer socket.

```bash
# build both arms
git worktree add --detach /tmp/ab-base <old-ref>
(cd /tmp/ab-base/browsers/isola-streamer && go build -o /tmp/ab-old .)
(cd browsers/isola-streamer && go build -o /tmp/ab-new .)

# one arm per invocation; compare with jq or by eye
./scripts/ab/ab-run.sh old /tmp/ab-old 17010 1280 720 6000 100
./scripts/ab/ab-run.sh new /tmp/ab-new 17011 1280 720 6000 100
```

Each run prints one JSON line:

|Field|Meaning|
|:---|:---|
|`ssim`, `psnr_db`|decoded stream vs the exact source frames (ffmpeg `ssim`/`psnr`)|
|`stream_bytes`|Annex-B payload the viewer received, headers stripped|
|`cpu_per_stream_s`|rusage of the streamer tree per second of video (encoder included)|
|`first_packet_ms`|viewer attach → first access unit|
|`encoder_restarts`, `forced_keyframes`, `keyframe_refusals`|hub behaviour from the log|

Artifacts (captured stream, source reference, logs) land in `$AB_OUT/<arm>`,
default `/tmp/isola-ab/out`.

## Caveats

The corpus is static text plus one small animating block. That is deliberate:
the encoder is CFR and the frame pump may repeat or skip a feed, so a corpus
that moves from frame to frame cannot be matched to the reference sequence —
`SCROLL=1` produces exactly that unscoreable case, useful only for inspecting
behaviour. For quality under real motion use the in-package bench instead
(`ISOLA_QUALITY_BENCH=1 go test -run TestEncoderQualityMatrix`), which feeds
frames directly and therefore keeps 1:1 frame order.

CPU numbers include the `ffmpeg` subprocess (the streamer reaps it). Compare
arms on the same host in the same session; absolute values move with load.
