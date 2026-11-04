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

## Multi-viewer rungs

`multi-rung.sh` drives the same corpus with N viewers, each asking for its own
ABR rung via `set_bitrate`. It exists to measure the encoder fan-out: every
distinct rung a viewer asks for used to be its own ffmpeg, and a session now
runs at most two (`maxConcurrentRuns` in `stream_hub.go`).

```bash
# three viewers on three rungs, session base 4000 kbps
./scripts/ab/multi-rung.sh cap /tmp/ab-new 17020 1280 720 4000 200 4000,2800,1800
# SCROLL=1 makes every row dirty every frame: the CPU worst case, not a quality corpus
```

The JSON line carries the same `cpu_per_stream_s` and `ffmpeg_starts` as the
single-viewer arm, plus per viewer `want_kbps`, `landed_kbps`, `landed_wh`, the
ordered `inits` it received (so a viewer that was moved by the run cap is
visible), `resets` and `packets`.

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

## Edge/chaos harness

`chaos.mjs` drives the paths a steady A/B run never reaches: viewers coming and
going, attaching exactly while the encoder is being replaced, keyframe request
storms, ABR stepping, a viewer that stops reading its socket, and resize
geometry nobody meant to request. It boots one real streamer per scenario with
its own port and frame socket, so scenarios cannot contaminate each other, and
prints one JSON object per scenario plus a summary.

```bash
# build and run everything (~3 minutes)
node scripts/ab/chaos.mjs

# use an existing build; keep logs under a directory you can inspect
node scripts/ab/chaos.mjs --bin /tmp/isola-streamer --out /tmp/chaos

# one or a few scenarios
node scripts/ab/chaos.mjs --only storm,geometry --bin /tmp/isola-streamer

# longer churn run (default 20 cycles)
node scripts/ab/chaos.mjs --only churn --cycles 60 --bin /tmp/isola-streamer
```

Scenarios: `churn`, `restart-attach`, `saturation` (shipped 6 Mbps and a noisy
100 Mbps run), `storm`, `abr`, `abr-burst`, `slow-reader`, `deadline`,
`zero-frame`, `geometry`. Every verdict is computed from measured counters,
never from log strings alone; logs and `/debug/encoder` samples are included in
the JSON.

Notes that matter when reading the numbers:

- `/debug/pprof/goroutine` is **not** registered (the binary does not import
  `net/http/pprof`), so goroutine leaks are proxied by fd count, thread count
  and RSS trends across churn cycles.
- The slow-reader/saturation/deadline scenarios use a raw TCP WebSocket client
  rather than the global `WebSocket`, because they need `socket.pause()`:
  stopping the kernel receive path is what makes the server's per-viewer mailbox
  fill and, past 10 s, trips the socket write deadline.
- The per-viewer mailbox holds 192 packets ≈ 7.7s at the 25fps encoder cap, but
  on loopback the kernel buffers absorb several MB before the server's writer
  blocks. At the shipped 6 Mbps a 9s stall therefore does not overflow the
  mailbox at all; the saturation scenario runs a 100 Mbps noise stream to reach
  the shed path. This is measured, not assumed — see `docs/streamer-edges.md`.
- Results land in `$OUT/chaos-results.json`; per-scenario streamer logs are at
  `$OUT/<scenario>/streamer.log`.

## Soak harness

`soak.mjs` answers a different question from the other two: does a session stay
healthy over time? `driver.mjs` measures one steady stream and `chaos.mjs` runs
each edge in a fresh process, so neither can see a leak, an fd or thread that
never comes back, an encoder run that is never reaped, or a change-driven
suppression latch that never wakes up — all of which are trends, not events.

It boots **one** streamer, **one** feeder and **one** long-lived viewer, then
mixes the normal workload into that single session for N minutes: viewer churn
every 20 s, ABR steps every 45 s (cycling down and back up), a forced keyframe
every 60 s, one resize round trip, and one two-minute static stretch that
exercises change-driven suppression and its wake-up. Every 5 s it samples RSS,
fds, thread count, child ffmpeg count, `subscribers`, `encoder_runs`, the
steady viewer's packets/s, and any log line matching
`error|panic|timeout|leak|refused`. Each attachment records its
first-packet latency.

```bash
# canonical 10-minute run against a build you made yourself
(cd browsers/isola-streamer && go build -o /tmp/q-soak .)
node scripts/ab/soak.mjs --bin /tmp/q-soak --minutes 10

# 2-minute smoke: the phase boundaries scale, the cadences do not
node scripts/ab/soak.mjs --bin /tmp/q-soak --minutes 2 --out /tmp/isola-soak-smoke
```

It prints a per-minute table and a PASS/FAIL verdict from ten rules (RSS slope
and plateau, fd/thread return-to-baseline, encoders reaped, anomaly log lines,
packet-rate decay, static suppression + wake-up, attach latency, restart
recovery, stream integrity, resize round trip). Exit code 0 = PASS, 1 = FAIL,
2 = harness error. Artifacts: `$OUT/soak-samples.csv` (the raw 5 s samples),
`$OUT/soak-result.json` (rules, events, attaches, anomalies), and
`$OUT/soak/streamer.log`.

Two things to know before reading a result:

- The hub's `request_keyframe` budget is 4 per connection for the life of that
  connection, so after the fourth accepted restart a session that keeps asking
  gets refusals. The harness counts those as a reported finding and only
  requires that the refusal be a no-op (no reset, frames keep arriving) — it is
  not a failure. `set_bitrate` remains an IDR lever either way.
- A run shorter than five minutes cannot satisfy the RSS-plateau rule: Go's
  heap is still warming up through the first minute and the resize allocates new
  surfaces. `--minutes 2` is for smoke-testing the harness, not for a verdict.

`docs/streamer-soak.md` holds a 10-minute run's numbers and the verdict.
