# Streamer edge cases (chaos harness results)

Measured behaviour of the real streamer binary under viewer churn, races around
encoder replacement, control-message abuse, stalled sockets, and resize
geometry. The harness is [`scripts/ab/chaos.mjs`](../scripts/ab/chaos.mjs); it
boots one streamer per scenario, feeds real ISOL frames, and reads the real
viewer protocol (raw Annex-B over WebSocket, marker `0x02`).

This file holds observations only. The harness is the source of truth; rerun it
and diff `$OUT/chaos-results.json`.

## Reproduce

```bash
# fresh build of HEAD, then the whole suite (~2.5 min on an M-series Mac)
(cd browsers/isola-streamer && go build -o /tmp/isola-streamer .)
node scripts/ab/chaos.mjs --bin /tmp/isola-streamer --out /tmp/chaos

# subsets
node scripts/ab/chaos.mjs --only storm,geometry --bin /tmp/isola-streamer
node scripts/ab/chaos.mjs --only churn --cycles 60 --bin /tmp/isola-streamer
```

Results below are from `dm0xa/transport` at `810c8e8`, macOS 27.2 arm64,
ffmpeg 9.0.2, 10 CPUs, encode target 1280x720 @25fps, one streamer per
scenario, no other load on the box.

## Verdicts

|Scenario|Verdict|Headline|
|:---|:---|:---|
|`churn` (20 attach/detach cycles)|fine|p50 53.3 ms / p95 96.9 ms attach→IDR, 20/20 encoders spawned and reaped, fds flat, RSS plateau|
|`restart-attach`|fine|joiner during replacement: first packet an IDR in 33–36 ms, 0 deltas before a key, 1 shared run|
|`saturation` (shipped 6 Mbps)|fine|9.8 s stalled reader: `dropped=0`, all 321 packets delivered after resume|
|`saturation` (100 Mbps noise)|fine|same stall: 50 packets shed, viewer resynced, fresh joiner unaffected, still 1 encoder|
|`storm` (40 keyframe requests)|fine|1 restart, 39 refused, both viewers back to a live IDR in 50 ms|
|`abr` (10 steps / 25 s)|fine|top-rung viewer 0 resets, 0 seq gaps, 628 packets, ≤2 encoders|
|`abr-burst` (40 steps in 142 ms)|**rough edge**|40 encoder spawns (301/s), co-viewer lost 146 ms of frames|
|`slow-reader` (5 s socket pause)|fine|healthy viewer 125 packets in 5 s (25.0/s), `dropped=0` on both, encoder never stalled|
|`deadline` (13 s stall, 100 Mbps)|**bug**|write deadline fires; the viewer goes dark but keeps its hub subscription and encoder until the peer closes|
|`zero-frame`|fine|`stream_init` at 51.2 ms with no frames in existence; first IDR 51.2 ms after feeding starts|
|`geometry`|**bug**|`/resize?width=1&height=1` returns 200, zeroes the session geometry, strands the next viewer|

## Scenario detail

### churn — 20 attach/detach cycles, fresh socket each time

Config: one feed at 25 fps from boot; each cycle attaches a new WebSocket, waits
for its first IDR, closes, and waits for the child ffmpeg to disappear.

```
attach→first-IDR ms   p50 53.3   p95 96.9   max 96.9
first packet is IDR   yes (20/20)
deltas before IDR     0
ffmpeg spawns         20   (exactly one per attach)
reap failures         0 (none of the 20 cycles left a child)
ffmpeg alive after    0
encoder_runs after    0        subscribers after 0
fds 10 → 10    threads 9 → 12    RSS 40.6 MB → 60.9 MB
log: forced_keyframes 0, refused 0, write_errors 0, pump_timeouts 0
```

RSS is Go heap growth, not a leak: a 60-cycle run of the same scenario
(`--cycles 60`, p50 52.7 ms, p95 80.4 ms, 60 spawns, 0 reaps missed) plateaus at
61 MB from cycle 20 on, with fds flat at 10.

`/debug/pprof/goroutine` is not available: the binary does not import
`net/http/pprof` (no `pprof` handler is registered in `mux.go`). Goroutine leaks
are therefore proxied by fd, thread and RSS behaviour: fds are flat, and both
thread count and RSS settle early and do not grow with more cycles (the
60-cycle run ends at the same 61 MB / 12 threads).

### restart-attach — join while the encoder is being replaced

Three rounds. A attaches, gets an IDR, sends `request_keyframe`; B attaches
15 ms later, i.e. while `spawnLocked` is doing the ffmpeg start under the hub
mutex (`stream_hub.go:554`). B therefore blocks on the mutex and lands on the
replacement run.

```
round   B first bin   B first IDR   first is key   deltas before key   runs/sub   A reset→IDR
0       32.8 ms       32.8 ms       yes            0                   1 / 2      46.2 ms
1       36.3 ms       36.3 ms       yes            0                   1 / 2      48.6 ms
2       35.0 ms       35.0 ms       yes            0                   1 / 2      48.8 ms
spawns 6 (2/round: initial + replacement), forced keyframes 3
```

The ordering invariant holds because `attach` queues `stream_init` (and, on a
live run, the cached IDR) under the hub mutex before the pumps can deliver
(`stream_hub.go:316-331`). A viewer never receives a delta it cannot decode.

### saturation — mailbox shedding behind OS socket buffers

A viewer reads until its first IDR, then stops pulling bytes for 9.8 s. A second
viewer attaches at t=9.2 s, i.e. while the first mailbox is full. Two runs, same
shape, different bitrate/content:

```
                             6 Mbps text      100 Mbps noise
packets produced (seq max)   321              322
packets received             321              272
dropped (server metric)      0                50
seq gaps                     0                50
bytes after resume           4.4 MB           102.9 MB
viewer survived              yes              yes
IDRs after resume            6                5
fresh joiner first packet    1.7 ms (IDR)     1.4 ms (IDR)
fresh joiner in 3 s          72 packets       73 packets
fresh joiner dropped         19               18
encoder runs / subscribers   1 / 2            1 / 2
```

What this means:

- The shed path is per viewer and never touches the encoder (`push`,
  `stream_hub.go:99-106`), and the second viewer attaches at full speed while
  the first is stuck (~1.5 ms to a picture). Verdict: fine.
- At the shipped 6 Mbps the 192-slot mailbox is **not reachable** in 9.8 s:
  ~4.4 MB of video fits entirely in the loopback socket buffers, so the
  per-viewer writer never has to shed and `dropped` stays 0. `mailboxSize`
  (`stream_hub.go:48`) is 192 packets ≈ 7.7 s at the 25 fps encode cap, but the
  kernel buffers and the 10 s `SetWriteDeadline` (`ws.go:109`) are what actually
  bound a stalled viewer at shipped bitrates. The `deadline` scenario measures
  which one gives out first.
- At 100 Mbps the same stall does overflow the mailbox (50 packets), the client
  sees 50 sequence gaps, and it recovers to a live IDR after resume. The
  recovery path works; it is just not exercised at shipped bitrates on a
  localhost-class transport.
- The 19 drops on the fresh joiner are the **poster wait**, not mailbox loss:
  the joiner is handed the last IDR immediately (`stream_hub.go:329-331`) but
  `awaitingKey` stays set, so deltas are discarded (`stream_hub.go:1006-1008`)
  until the next live IDR (≤2 s GOP). Fine for display, but it means a `dropped`
  counter alone cannot distinguish "mailbox overflow" from "joined mid-GOP".
- Feeding at 100 fps produced 50–51 packets in 2 s (measured on the shipped
  run; the harness skips this probe at 100 Mbps because one Node process cannot
  both generate and parse that much video): `pumpFrames` gates feeds on the
  encode interval, so a fast producer cannot force the mailbox to fill faster.
  Shedding is a function of time, not feed rate.

### storm — 40 keyframe requests from two viewers at once

Two viewers on one run, both send 20 `request_keyframe` back to back.

```
forced restarts        1      (first request wins)
refused / ignored      39     (log: 39 × "encoder has not emitted an IDR yet" or "restart cooldown")
ffmpeg spawns          1 during the storm (initial + this replacement = 2 total)
last reset → IDR       49.7 / 49.8 ms (A / B)
encoder_runs after     1
```

Bounded by the per-viewer budget (`stream_hub.go:471-474`) and the hub-wide
5 s cooldown (`stream_hub.go:455`, checked at `stream_hub.go:547`). No thrash,
and both viewers recover an IDR. Verdict: fine.

### abr — 10 `set_bitrate` steps in 25 s, second viewer on the top rung

```
top-rung viewer: resets 0, packets 628, max gap 59.5 ms, seq gaps 0, dropped 0
stepper:         10 resets (all reason=bitrate_change)
ffmpeg spawns:   7  (steps that left the base rung; steps back to base re-attach to the existing run)
encoder runs:    1 or 2 at every sample, never 3; 1 after the last step
```

Only the stepping viewer's run is replaced; the top-rung viewer keeps its
encoder and sees no gap beyond the 60 ms startup figure. Verdict: fine.

### abr-burst — the same path, unthrottled

`set_bitrate` has no budget or cooldown, unlike `request_keyframe`. 40 messages
alternating between the two non-base rungs, as fast as the socket takes them:

```
steps processed        40 in 142 ms
ffmpeg spawns          40  (301/s over the reset span)
top-rung viewer        resets 0, seq gaps 0, max gap during burst 187.8 ms
                       (146.3 ms growth caused by the burst)
encoder runs after     2 (top rung + the stepper's current rung)
```

Each spawn is only `cmd.Start()` under the hub mutex — ffmpeg initialises after
the mutex is released — so the co-viewer loses ~150 ms rather than seconds, and
the killed children are reaped (39 `ffmpeg exited` logs, 0 pump timeouts). But
one authenticated viewer can force an encoder process per message indefinitely
(`ws.go:182-202` → `hub.attach`), over two orders of magnitude above the ~1 Hz
the client's ABR controller uses. Verdict: **rough edge**, not a bug: the
control path is client-driven by design, but it is the one restart path with no
rate limit. A cooldown/dedupe on `set_bitrate` (or reusing the keyframe
window/budget) would close the asymmetry.

### slow-reader — 5 s socket pause with a healthy co-viewer

Two viewers on one run; one pauses its TCP socket for 5 s while the page keeps
changing.

```
healthy viewer     125 packets during the 5 s (25.0/s), max gap 45.5 ms,
                   0 resets, dropped 0
slow viewer after  all 206 packets delivered after resume (none missing),
                   0 dropped, 5 IDRs, connection alive
encoder            1 run, 1 ffmpeg child throughout
```

5 s at 25 fps is 125 packets, below the 192-slot mailbox, so no shedding is
expected — and none happened. The important part is the encoder and the other
viewer: a stalled socket costs neither. Verdict: fine. See `saturation` for
what happens past the mailbox window.

### deadline — past the 10 s socket-write deadline

One viewer at 100 Mbps noise (so the socket buffers fill in well under a
second and the writer really blocks), paused for 13 s, then resumed:

```
log                      ws: binary write error: write tcp ...: i/o timeout   (1)
packets in the last 1.5 s after resume   0
closed_by_server         false  (the client never sees a close)
subscribers             1        encoder_runs 1      ffmpeg children 1
after the client closes subscribers 0        encoder_runs 0      ffmpeg 0
```

The deadline fires as designed (`ws.go:109`), but the error path only stops the
drain goroutine: `writeBinary` fails, `closeDone()` runs, and the goroutine
returns (`ws.go:152-156`). Nothing closes the connection, and nothing detaches
the subscription. The handler stays blocked in `ReadMessage` — a read deadline
is never set — so `defer hub.release(sub)` (`ws.go:176`) never runs.

Consequences, all measured: the viewer receives no further video and is not told
why; the session keeps reporting one subscriber and one encoder run, and the
frame pump keeps feeding a mailbox that can never drain (`/debug/encoder`
`encoder_runs 1`, one live ffmpeg child); the run is only released when the peer
eventually closes the TCP connection. A client that stalls for 10 s — a phone
losing connectivity without a TCP reset, a network partition, a badly written
embedder — therefore leaves a ghost viewer that holds an encoder and an ffmpeg
process for as long as TCP keeps the half-open socket, and its reconnect creates
a second subscriber on top of it.

Verdict: **bug**. The comment on the deadline says a wedged socket "errors out
and tears the session down" (`ws.go:104-105`); it does neither.

### zero-frame — attach before any frame exists

A viewer attaches with no producer frames ever sent, waits 2.6 s, then the feed
starts.

```
stream_init at          51.2 ms (before any frame exists)
binary packets in 2.6 s 0
first IDR               51.2 ms after the first feed
first packet is IDR     yes
encoder_runs            1, subscribers 1
```

The run exists from attach, `pumpFrames` waits on an empty surface, and the
first composed frame produces an IDR immediately. Verdict: fine.

### geometry — resize validation

```
resize 801x601 → 200 "ok", re-keyed, viewer reset reason=resize, IDR 49.4 ms later, /debug 800x600
resize 1x1     → 200 "ok", no re-key, /debug still reports 1280x720 (env fallback)
resize 0x600   → 400
resize 8193x600→ 400
```

The 1x1 request is the bug. `width &^= 1` (`mux.go:92-93`) runs *after* the
`width <= 0` check (`mux.go:86`), so 1 is accepted and becomes 0;
`currentWidth/currentHeight` are then set to 0 (`mux.go:94-97`), the runner is
handed a resize to 0x0 (`mux.go:101-103`; the harness has no runner ctrl socket,
so `writeCtrlCommand` returns false there, but a live session does deliver it),
and `resizeAll(0,0)` returns immediately (`stream_hub.go:611-614`), leaving
every encoder at its old geometry. Measured consequences, in order:

1. `/debug/encoder` starts reporting the env fallback `1280x720` while the live
   encoder is `800x600`.
2. A viewer attaching afterwards reads `width==0` and falls back to
   `ISOLA_WIDTH/HEIGHT` (`ws.go:69-73`), so it asks for `1280x720` against a
   producer surface that is still `800x600`. It receives `stream_init` and
   **0 video packets in 3 s**; `pumpFrames` logs "waiting for producer geometry
   1280x720 (surface is 800x600)" twice, `/debug/encoder` shows two runs and two
   subscribers — a viewer that will never decode until something else resizes.
3. Resizing back to `800x600` recovers it: the stale run is re-keyed onto the
   existing `800x600` run, the viewer gets `stream_reset` + `stream_init`, and
   the next live IDR lands 1966 ms later (mid-GOP wait, ≤2 s by design).

Reachability matters for severity: `POST /api/sessions/{id}/resize` clamps to a
64 px minimum (`backend/pkg/api/handler.go:939-941`), so a public-API caller
cannot send 1 today. The streamer's own `/resize` is container-local and takes
any caller that can reach the port (`127.0.0.1::<ephemeral>` published by the
orchestrator), and it is the component that owns its own state; the clamp is
defence at the wrong layer. `resize_test.go:78` covers 1023x767 → 1022x766 and
rejects 0/9000, but not 1 — a one-line assertion in the existing validation test
would pin it.

Also observed: `/resize` accepts `GET` (and any other method), unlike
`/navigate`, which requires `POST`. The handler has no method check, so a local
`GET /resize?...` mutates session geometry. Not changed here; listed under
"left alone" below.

## Findings that were not covered by an existing test

1. **Bug — a write-deadline expiry leaves a ghost viewer.** When the 10 s
   socket write times out, the writer goroutine exits but the connection is not
   closed and the hub subscription is not released (measured: `subscribers 1`,
   `encoder_runs 1`, ffmpeg alive, 0 packets delivered after resume, no close
   seen by the client; released only when the peer closes). The run stays
   alive for a viewer that can never receive it. Reachable by any client
   whose socket stops draining for 10 s (network partition, sleeping phone
   without RST). Evidence: `ws.go:109` (deadline), `ws.go:152-156` (error path),
   `ws.go:176` (release only on handler return). No test covers the timeout
   path.
2. **Bug — `/resize` accepts an unrepresentable geometry.** 1x1 passes
   validation, is masked to 0x0, and the session is left in a state where a
   later viewer is pinned forever against a stale producer geometry (measured:
   0 packets in 3 s, two live runs, `waiting for producer geometry` logged).
   Container-local reachability only today (the public API clamps to ≥64 px),
   but the streamer owns its own state. Missing test: `resize_test.go` never
   requests a dimension that masks to 0.
3. **Rough edge — `set_bitrate` is unbudgeted.** 40 control messages produced
   40 ffmpeg spawns in 142 ms (301/s) and cost the co-viewer 146 ms of frames;
   `request_keyframe` is capped at 4 per 30 s per viewer. No test drives the
   ABR control path under load.
4. **Boundary worth knowing — the mailbox is not the first line of defence at
   shipped bitrates.** ~4.4 MB of 6 Mbps video fits entirely in the loopback
   socket buffers, so a 9.8 s stall sheds nothing; the shed path only engages at
   ~100 Mbps. `stream_hub_test.go:189` (`TestStreamSubDroppedCountsOverflow`)
   tests `push` directly, not this end-to-end interaction.

## Surfaces that were clean, and why that is believable

- **Viewer churn / lifecycle:** exactly one encoder per attach, always reaped;
  no leftover child processes, fds flat at 10, threads 9 → 12 as the Go runtime
  grows its pool, and RSS plateaus by cycle 20 (held to cycle 60 in the longer
  run) after the Go heap settles.
- **Restart races:** a joiner either lands on the old run and is migrated, or
  blocks on the hub mutex and lands on the new one; either way `stream_init`
  precedes all video and the first video packet is an IDR (0 violations in 20
  churn cycles + 3 restart rounds).
- **Keyframe budget:** 40 concurrent requests → 1 restart, 39 refusals, both
  viewers recovered. The budget, the 5 s hub cooldown and the retry allowance
  all behaved as documented.
- **ABR ladder:** steps never reset another viewer and never leave more than
  one extra encoder alive; returning to the base rung re-attaches to the
  existing run instead of spawning a third.
- **Zero-frame start and a 5 s stalled reader:** both are inert with respect to
  the encoder and the other viewers.
- **Write deadline, up to the expiry itself:** the timer fires at 10 s as
  designed and the healthy-viewer path is unaffected; only the post-expiry
  cleanup is missing (finding 1).

## Left alone deliberately

No production source was changed (harness-only work). Things seen but not
touched:

- `/resize` has no HTTP method check (state-changing `GET`), unlike `/navigate`.
- `request_keyframe` while `awaitingKey` bypasses the connection budget for up
  to 3 retries (`stream_hub.go:481-483`); the hub cooldown still spaces them, so
  it is bounded, but the code comment ("budget bounds the total") overstates
  what that branch does.
- `dropped` conflates "mailbox overflow" with "waiting for the first live IDR",
  so a late joiner always reports a small nonzero count while showing the
  poster. Accurate for "packets this viewer could not use", misleading if read
  as transport loss.
