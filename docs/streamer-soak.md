# Soak: does a session stay healthy over time?

`scripts/ab/soak.mjs` drives **one** streamer process with **one** continuous
session for ten minutes while mixing in the workload a real session sees:
viewer churn, ABR steps in both directions, forced keyframes, a resize round
trip, and a two-minute static stretch. It samples process and hub state every
5 s and fails the run when something accumulates.

This page records a run of that harness and the verdict. It is the companion to
[`streamer-edges.md`](./streamer-edges.md) (one scenario per fresh process) and
the A/B harness (`scripts/ab/README.md`): those ask "is this build better" and
"does this edge work", this one asks "is it still true after ten minutes".

## What was run

```bash
(cd browsers/isola-streamer && go build -o /tmp/q-soak .)
node scripts/ab/soak.mjs --bin /tmp/q-soak --minutes 10 --out /tmp/isola-soak-final
```

| | |
|:---|:---|
|Binary|`dm0xa/transport` @ `990d4c5`, `go build` from `browsers/isola-streamer`|
|Host|darwin arm64, 10 CPUs (macOS 27.2), ffmpeg 9.0.2 on `PATH`|
|Session|1280x720 capture, 25 fps cap, 6000 kbps base, static keepalive 2000 ms|
|Duration|600 s, 120 samples at 5 s|
|Producer|Synthetic ISOL feeder over the frame socket: dense text page, one 96x64 animated block (the normal case), all-static during the quiet window|
|Viewers|One long-lived viewer for the whole run, plus 22 attach/hold/detach cycles at 20 s cadence|
|Perturbations|9 ABR steps every 45 s (`4200, 2700, 1800, 6000, 3600, 6000` cycling), 4 accepted + 2 refused `request_keyframe` at 60 s, one resize round trip at 2:30 / 3:00, static window 4:00–6:00|

The steady viewer is what "the session" means here: one WebSocket open for ten
minutes, decoding throughout, while everything else churns around it.

## Verdict

**PASS — 10 of 10 rules.** The session stays healthy: RSS plateaus (it is
*lower* in the last third than the middle third), fds return to baseline after
22 attach/detach cycles, every encoder run and ffmpeg child is reaped, the
packet rate holds the 25 fps cap for the whole run, change-driven suppression
drops the stream to the keepalive rate on a static page and wakes in 17 ms when
motion returns, and there is not one unexplained log line. Picture quality was
not touched: no encoder argv change and no change to what the viewer decodes.

Three log lines are expected rather than healthy-or-not, and are still reported
(see [Known findings](#known-findings-not-session-health-signals)).

## Per-minute table

`anom` counts *unexplained* log lines matching `error|panic|timeout|leak|refused`
up to that minute; `attachP50` is the median attach-to-first-packet latency of
the churn viewers in that minute.

```
 min   phase   rssMB  fds  thr  ff runs subs    pps attachP50 anom
   0  motion    63.6   14   13   1    1    1  22.78      59.7    0
   1  motion    73.4   15   13   1    1    1  24.97        60    0
   2  motion    73.4   15   13   1    1    1  24.97      59.6    0
   3  motion      74   15   13   1    1    1  23.77      67.8    0
   4  static    74.6   15   13   1    1    1   2.53         -    0
   5  static    73.7   14   13   1    1    1    0.5         -    0
   6  motion      75   15   13   1    1    1  24.95      67.2    0
   7  motion    76.7   14   13   1    1    1     25       1.9    0
   8  motion    75.6   15   13   1    1    1  24.95      53.3    0
   9  motion    76.5   14   13   1    1    1  24.98      61.6    0
```

Reading it:

- **RSS** starts at 41.8 MB, steps to 60 MB as the first frames flow, to 64 MB
  at the first ABR restart and to 75 MB by t=35 s, then sits in a 73–77 MB band.
  The steps line up with allocations the session makes once, not per event:
  each encoder run allocates a pair of 3.6 MB pump buffers and the Go heap grows
  to its steady target. Peak 77.3 MB at t=240 s, last-four-minute mean 76.0 MB.
  GC moves the level by a megabyte or two between minutes (75.6–76.7 in the last
  three), and the process finished at 76.8 MB with a live viewer attached: no
  drift.
- **fds** only ever take the values 14 and 15; **threads** settle at 13 by
  minute 0 and never move again (the t=0 baseline sample caught 11, before the
  runtime grew its thread pool).
- **ffmpeg / runs** stay at 1 per sample: one encoder run serves the steady
  viewer, and a churn viewer's second run lives for ~1 s between samples.
- **pps** is the viewer's received packets per second. Minute 4 is 2.53 rather
  than 0.5 because its first 5 s interval still covered motion; minute 5 is the
  fully quiet minute at the 0.5/s keepalive rate.
- **attachP50** in minute 7 is 1.9 ms because the steady viewer had stepped
  back to the 6000 kbps base rung at that point, so the churn viewers joined the
  live run and were handed its cached poster IDR instead of starting an encoder.

## Rules, limits and measured values

Every limit is enforced in `soak.mjs` (`LIMITS`) and each one is chosen so that
the workload could not accidentally pass it.

|Rule|Limit|Measured|Why this limit|
|:---|:---|:---|:---|
|R1 RSS plateaus|OLS slope over the second half ≤ 2048 KB/min, and last-third max − middle-third max ≤ 12 MB|slope **+673.6 KB/min**, mid-third max 79 184 KB → last-third max 78 576 KB (**−608 KB**), peak 77.3 MB|The workload's live set is ~75 MB. One leaked 1280x720 BGRA surface (3.5 MB) per restart would be 56 MB across this run's 16 restarts, and one per attach 77 MB across 22 attaches — 5–10x the allowance. 2 MB/min over the last five minutes is 10 MB, i.e. three surfaces, so a real per-object leak cannot hide inside it while Go's sawtooth and one resize allocation can. The second condition is the actual "plateau": the last third must not sit above the middle third.|
|R2 fds and threads return to baseline|final fds ≤ baseline + 4, final threads ≤ baseline + 6, same one-viewer state both times|fds **14 → 14**, threads 11 → 13 (13 from minute 0 on)|A leaked subscription is at least one socket and one writer goroutine per attach (22 this run); a leaked encoder pipe pair is 2 fds per spawn (33 spawns). Neither can hide inside +4/+6, while a warm runtime's thread pool and a transient `lsof`/`ps` fd can.|
|R3 encoders reaped|after the last detach: ffmpeg children 0, `encoder_runs` 0, `subscribers` 0, live spawned pids 0|**0 / 0 / 0 / 0** (within 500 ms of the detach)|Anything else is a run whose ffmpeg outlives its last viewer — the failure mode a long session turns into dozens of orphan encoders.|
|R4 no unexplained anomaly lines|0 lines matching `error|panic|timeout|leak|refused`, except the documented known findings|**0** (298 CDP-retry, 33 x264-argv and 2 budget lines were classified and are reported below)|Those tokens mark every read, write, restart and budget failure path in the streamer. Nothing in this workload can legitimately produce one.|
|R5 packet rate does not decay|last motion-minute mean ≥ 0.85x first, and every motion minute ≥ 15 packets/s|**22.78 → 24.98/s**; minimum motion minute 22.78 (minute 0, which contains startup and the first ABR step); motion-sample mean 24.8, max 25.4|15/s is 60 % of the 25 fps cap: a stalled pump, a suppression latch stuck on, or an encoder following a slowed producer shows as single digits. 0.85x compares like with like — the ABR ladder cycles back to the 6000 kbps base by the end.|
|R6 static suppression and wake-up|static-window mean packets/s in [0.15, 2.5] (keepalive is 0.5/s), ≤ 10 damage updates during it, wake-up ≤ 400 ms|**0.50/s**, **1** damage update in 120 s, wake **17.4 ms**|Suppression is one fed frame per 2000 ms keepalive window, so 0.5/s ± the sample boundary; more than 2.5/s means the pump is encoding an unchanged surface. 400 ms is one 40 ms tick plus an encode plus the muxer's follow-up feed — an order of magnitude above the healthy path, so only a stuck latch trips it.|
|R7 attach latency|p95 ≤ 600 ms, max ≤ 2600 ms, first packet always an IDR|p50 **61.6 ms**, p95 **69.8 ms**, max **71.5 ms**, 22/22 first packet key|A joiner that matches a live run's rung gets the cached poster IDR (measured 1.5–1.9 ms, 5 attaches); one that needs its own run waits for that run's first IDR (measured 53.3–71.5 ms, 17 attaches). The hard bound is the 2 s GOP for a joiner whose run has not produced an IDR yet, hence the 2600 ms max. First-packet-is-key is the invariant a viewer cannot survive losing.|
|R8 restart recovery|every ABR and accepted-keyframe restart resets and delivers a frame ≤ 3000 ms later; a budget-refused request is a true no-op; `encoder_runs` ≤ 2 at every sample|9 ABR + 4 accepted keyframes, worst recovery **85.4 ms**; the steady viewer's 15 reset→packet times were **44–84 ms**; 2 refusals, stream continued in 22 ms; max `encoder_runs` **1**|Restarting ffmpeg is the only keyframe mechanism, so a restart that does not recover is a frozen picture. 3 s bounds a spawn (measured ~50 ms here, 68 ms in a 3-CPU container) plus a first IDR with two orders of margin.|
|R9 stream integrity|steady viewer seq gaps 0, `dropped` 0, never closed by the server|**0 / 0 / no**|The hub numbers packets per run and sheds rather than stalls a slow viewer. A gap or a drop in a viewer on loopback at 25 fps/6 Mbps means the mailbox or the writer path is failing, not the network.|
|R10 resize round trip|both `/resize` calls 200, session geometry back to the boot size, video after each|1024x576 → 1280x720, both **200**, recover **99.3 / 102.8 ms**|A resize re-keys every run; the round trip proves the session comes back to its original geometry and the producer follows it (a run whose pump never gets a matching frame stalls forever — see `docs/streamer-edges.md`).|

## Anomalies

**Unexplained log lines: none.** Zero lines matched
`error|panic|timeout|leak|refused` in 120 samples across ten minutes without
being one of the three documented classes below. The steady viewer also never
saw a sequence gap, never had a packet shed, and was never closed by the server.

## Known findings (not session-health signals)

Each was reported by the harness with its sample index and is *counted*, not
silently dropped. `soak.mjs` classifies them in `ALLOWED_ANOMALIES`.

|Finding|First seen|Count|Suspect|
|:---|:---|:---|:---|
|`cdp: connect failed: … connection refused (retry in 2s)`|sample 0, t=0.3 s|298|Harness, not the streamer: the binary runs standalone against the synthetic ISOL producer, so there is no renderer at `127.0.0.1:9222` for the capture client to attach to. Unreachable in a real session.|
|`ffmpeg: [libx264 @ …] Error parsing option 'me:dia:force-cfr = 1'`|sample 0, t=0.3 s|33 (one per encoder spawn)|**Real, pre-existing.** `browsers/isola-streamer/encoder.go:293` writes `me:dia` where libx264 wants `me=dia`, so libx264 rejects the tail of `-x264-params`. Verified against ffmpeg 9.0.2: the broken string produces a bitstream **byte-identical** to the same string with the `me:dia:force-cfr=1` tail removed, while writing the tail correctly (`me=dia:force-cfr=1`) changes it — so the tail is dropped and everything before it still applies (both variants put IDRs at frames 0/50/100, i.e. `keyint=50` is in effect). ffmpeg keeps encoding. Consequence: motion estimation runs at x264's default rather than `dia`, and `force-cfr` never applied. Fixing the typo would change what the encoder produces, which this wave is explicitly not allowed to do — so it is reported, not changed.|
|`ws: keyframe request refused (connection budget exhausted): viewer used 4/4`|sample 95, t=475.1 s|2 (then permanent)|**Designed, but worth knowing.** `browsers/isola-streamer/stream_hub.go:568-569` caps forced restarts at 4 per connection for the life of that connection (spaced by a 30 s window). A session that asks once a minute therefore loses `request_keyframe` after 4:05 and never regains it — the harness's 5th and 6th requests at 475 s and 535 s were refused. R8 asserts the refusal is a no-op (no reset; next frame in 22 ms), which it is. The remaining recovery lever for such a client is `set_bitrate`, which also replaces the run and therefore always yields an IDR.|

### Two more findings from an earlier iteration of the same harness

Both were reproducible only with a *bursty* producer: the first version of the
feeder allocated a 3.7 MB buffer per frame, and its GC pauses turned the feed
into 40–51 fps bursts separated by multi-second stalls (a workload a real CDP
producer can produce when a page repaints in bursts). They disappeared when the
feeder was made allocation-free and paced by the socket, but they are real
behaviour of the streamer under a bursty producer:

1. **The 25 fps cap is not enforced on wall-clock feed rate.**
   In that run, sample 26 (t=130 s) recorded **48 packets/s to the viewer while
   the producer was feeding 51 frames/s**; samples 22 and 27 tracked 37.2 vs
   41.4 and 6.8 vs 7.5 the same way, i.e. the encoder emits one packet per fed
   frame instead of one per 40 ms. Suspect:
   `browsers/isola-streamer/stream_hub.go:1010-1019` — the edge-triggered feed
   path checks `time.Since(lastFeedAt) >= interval` (line 1014), but the
   `ticker.C` path (line 1017) calls `enqueue()` with no interval check and
   `enqueue()` itself has no rate guard, so a ticker tick and a frame event
   inside the same 40 ms window both feed. The encoder's input is `-r 25`
   (constant timestamps) with no output fps filter, so media timestamps stay
   25 fps while wall-clock emission follows the producer. With a smooth 25.0 fps
   producer (this run's feeder: mean 24.99, min 24.6, max 25.4) the viewer
   receives 24.8/s and the cap holds.
2. **A run being torn down logs a torn final frame.**
   Sample 22 (t=114.5 s) of that run captured
   `ffmpeg: [rawvideo @ …] Invalid buffer size, packet size 983040 < expected
   frame_size 3686400` plus `Packet corrupt` / `Error submitting packet to
   decoder: Invalid argument`. 983040 bytes is 192 of 720 rows — the residue of
   a 3.6 MB frame in a 64 KB pipe when stdin closes.
   `browsers/isola-streamer/stream_hub.go:812` (`stop`) closes `stdin`, kills
   ffmpeg and waits; the writer goroutine is mid-`Write`, so the dying run's
   input is truncated. It can only affect the run being killed (each run has its
   own pipe), and the process is exiting, so it is log noise rather than a
   session fault — but a log alert on `rawvideo` would trip on every noisy
   teardown.

## Reproducibility

The same binary was run a second time with the exact acceptance command
(`node scripts/ab/soak.mjs --bin /tmp/q-soak --minutes 10`, default
`--out /tmp/isola-soak`) and reached **PASS 10/10** again, with the same shape:

| |Committed run (`/tmp/isola-soak-final`)|Second run (default `--out`)|
|:---|:---|:---|
|Verdict|PASS 10/10|PASS 10/10|
|Attach/detach cycles, ffmpeg spawns|22, 33|22, 33|
|RSS second-half slope|+673.6 KB/min|**+22.2 KB/min**|
|RSS mid-third → last-third max|79 184 → 78 576 KB|76 320 → 76 352 KB (+32 KB)|
|fds baseline → final|14 → 14|14 → 15|
|Attach p50 / p95 / max|61.6 / 69.8 / 71.5 ms|58.6 / 67.3 / 84.6 ms|
|Static window packets/s|0.50|0.50|
|Wake-up|17.4 ms|43.6 ms|
|Worst restart recovery|85.4 ms|220.6 ms|
|Unexplained anomaly lines|0|0|

Two independent runs agreeing on every structural number (cycle counts, spawn
counts, finding counts) and landing inside the same limits is what makes the
verdict reportable; the RSS *slope* is the noisiest of the rules once the
plateau is flat (+22 KB/min and +674 KB/min are both noise around a constant
level, which is why the plateau half of R1 compares thirds as well).

The committed [`streamer-soak-samples.csv`](./streamer-soak-samples.csv) is the
first run's; the second run's artifacts were left at `/tmp/isola-soak/`.

## Limitations

- **No renderer.** The harness replaces CDP capture with a synthetic ISOL
  producer, so capture-side health (renderer watchdog, tabs/console/clipboard
  listeners, input replay) is out of scope; that is what the CDP retry lines
  are.
- **One session, loopback.** Two viewers at most at any instant, no proxy, no
  TLS, no lossy network. `docs/streamer-edges.md` covers the hostile viewer
  cases (stalled reader, saturation, keyframe storms) one process each.
- **Host-specific absolutes.** RSS in MB, attach latency in ms and CPU cost are
  this 10-CPU macOS host; the pass/fail rules are ratios, slopes and
  return-to-baseline comparisons, which is what makes them portable.
- **Corpus shape.** Full-surface damage every frame is the worst case for
  encode cost and the best case for change detection; a real page usually
  damages a fraction of the surface (see `/debug/encoder` `damage_pct`).

## Raw samples

[`streamer-soak-samples.csv`](./streamer-soak-samples.csv) — the 120 samples
this page is built from (7.5 KB). Columns:

|Column|Meaning|
|:---|:---|
|`idx`, `t_s`|sample index and seconds since the session's long-lived viewer attached|
|`phase`|`motion`, `static`, `wake` (first 5 s after the quiet window) or `resize`|
|`rss_kb`, `fds`, `threads`, `ffmpeg`|streamer process state (`ps`/`lsof`), child ffmpeg count from `pgrep -P`|
|`subscribers`, `encoder_runs`|`GET /debug/encoder`|
|`packets_total`, `pps`|packets the long-lived viewer has received, and the rate over the sample interval|
|`feed_fps`|frames the harness producer actually fed over the same interval (24.6–25.4 here)|
|`damage_updates`, `resolution`|`/debug/encoder` damage counter and session geometry|
|`anomalies_total`|cumulative unexplained anomaly log lines|

`/tmp/isola-soak-final/soak-result.json` (not committed) holds the rule results,
every attach, every perturbation event, the allowed-finding counts and the
streamer's log tail.

## Re-run after the fixes

The two encoder-side findings above were fixed on `dm0xa/transport` (`me=dia`
repaired and pinned by `TestX264Params*`; the feed-rate hole closed by hoisting
the interval guard into the single path both the damage event and the ticker
call). The soak was re-run unchanged on the fixed build:

```
node scripts/ab/soak.mjs --bin /tmp/isola-soak2 --minutes 10
VERDICT: PASS (10/10 rules)
```

|rule|number|
|:---|:---|
|RSS|second-half slope 739 KB/min, 79.7 → 82.4 MB (+2.6 MB, limit +12 MB)|
|fds / threads|14 → 15 / 11 → 13 over 22 attach-detach cycles|
|encoders after churn|0 children, 0 runs, 0 subscribers|
|packet rate|motion minute 21.5 → 22.5 /s; static window 0.5 /s (keepalive)|
|attach latency|p50 56 ms, p95 72 ms, max 87 ms, first packet always a keyframe|
|restarts|9 ABR + 4 keyframe, worst recovery 88 ms, never more than 1 run|
|stream integrity|0 seq gaps, 0 dropped, no server close|
|resize|1024x576 (95.9 ms to IDR), back to 1280x720 (87.8 ms)|

The harness now POSTs `/resize`, matching the method the streamer requires; a
`GET` returns 405 by design.

## What was not touched

No Go or TypeScript source changes: this wave is measurement only. The two
encoder-side findings above (`encoder.go:293`, and the feed-rate/suppression
paths in `stream_hub.go`) are reported with line references rather than fixed,
because both would change what the viewer decodes or how the encoder is driven,
which the product constraints for this wave forbid.
