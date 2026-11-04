package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// H.264 profile used for every encode path. High profile (CABAC + 8x8
// transform) is supported by every modern hardware/software decoder and is
// ~15% more efficient than baseline at the same bitrate — the main quality
// lever on text-heavy pages.
const (
	h264Profile    = "high"
	h264ProfileHex = "6400" // avc1.<PP><CC> — High profile, no constraint flags
)

// codecStringForLevel returns an MSE/WebCodecs-compatible avc1 codec string.
// Format: avc1.PPCCLL (profile, constraint, level as 3-byte hex + 1-byte level).
func codecStringForLevel(width, height int) string {
	return fmt.Sprintf("avc1.%s%02X", h264ProfileHex, h264LevelCodec(width, height))
}

func h264LevelCodec(width, height int) int {
	pixels := width * height
	switch {
	case pixels > 2560*1440:
		return 0x33 // Level 5.1 — 4K
	case pixels > 1920*1080:
		return 0x32 // Level 5.0 — 1440p
	case pixels > 1280*720:
		return 0x28 // Level 4.0 — 1080p
	case pixels > 720*576:
		return 0x1F // Level 3.1
	default:
		return 0x1E // Level 3.0
	}
}

func h264LevelFFmpeg(width, height int) string {
	switch h264LevelCodec(width, height) {
	case 0x33:
		return "5.1"
	case 0x32:
		return "5.0"
	case 0x28:
		return "4.0"
	case 0x1F:
		return "3.1"
	default:
		return "3.0"
	}
}

// bitrateKbpsForResolution scales target bitrate with pixel area
// (6 Mbps @ 720p, linear — bits-per-pixel preserving, so a 3456px-wide capture
// keeps 720p's per-pixel quality instead of being flattened by a fixed cap).
// 60 Mbps is a guard against a malformed resolution only; the container's
// ISOLA_MAX_PIXELS budget keeps real values far below it.
func bitrateKbpsForResolution(width, height int) int {
	pixels := width * height
	base := 1280 * 720
	kbps := (6000 * pixels) / base
	if kbps < 2000 {
		kbps = 2000
	}
	if kbps > 60000 {
		kbps = 60000
	}
	return kbps
}

// effectiveEncodeFps caps CFR for the encoder. WebCodecs renders decoded
// frames immediately; the MSE path buffers, but a modern decoder keeps up at
// 30fps and the extra frames are what make scrolling look smooth.
func effectiveEncodeFps(width, height, targetFps int, fpsCap int) int {
	cap := targetFps
	if cap > fpsCap {
		cap = fpsCap
	}
	if cap < 8 {
		cap = 8
	}
	return cap
}

// encodeDimensions applies the active ABR scale to the capture size. At scale
// 1 it returns the capture dimensions unchanged; below 1 it returns even
// dimensions valid for yuv420p (capture coordinates still map through the
// capture size, not the encoded one).
func encodeDimensions(width, height int, scale float64) (int, int) {
	if scale >= 1 {
		return width, height
	}
	return evenDim(int(float64(width)*scale + 0.5)), evenDim(int(float64(height)*scale + 0.5))
}

// evenDim rounds a scaled dimension down to an even value (yuv420p requires
// even width/height), with a floor of 2.
func evenDim(v int) int {
	if v%2 != 0 {
		v--
	}
	if v < 2 {
		v = 2
	}
	return v
}

// scaledVf prefixes a scale filter to the encoder's pixel-format filter only
// when the rung actually resizes the frame, so the top rung's ffmpeg argv is
// unchanged.
func scaledVf(vf string, encW, encH, width, height int) string {
	if encW == width && encH == height {
		return vf
	}
	return fmt.Sprintf("scale=%d:%d,%s", encW, encH, vf)
}

// hwEncoder describes a detected hardware H.264 encoder.
type hwEncoder struct {
	name     string   // ffmpeg -c:v value
	preInput []string // flags that must precede -i (e.g. -vaapi_device)
	vf       string   // filter chain producing the encoder's required pixel format
	extra    []string // encoder tuning flags
}

var (
	hwEncOnce sync.Once
	hwEncSpec *hwEncoder
)

// detectHWEncoder probes ffmpeg once for a usable hardware H.264 encoder.
// QSV/VAAPI require /dev/dri (passed through by the orchestrator when the host
// exposes a GPU); NVENC only needs the nvidia driver. nil → libx264 fallback.
func detectHWEncoder() *hwEncoder {
	hwEncOnce.Do(func() {
		hwEncSpec = probeHWEncoder()
	})
	return hwEncSpec
}

func probeHWEncoder() *hwEncoder {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil {
		log.Printf("encoder: hw probe failed (%v) — using libx264", err)
		return nil
	}
	encoders := string(out)
	hasDRI := false
	if _, statErr := os.Stat("/dev/dri/renderD128"); statErr == nil {
		hasDRI = true
	}
	// Ordered by preference. -encoders only reports what the ffmpeg *build*
	// supports, which is not the same as what this host can actually run: a
	// stock Debian ffmpeg lists h264_nvenc even with no Nvidia driver, and
	// picking it made every encode fail with "Cannot load libcuda.so.1". Each
	// candidate is therefore smoke-tested with a real one-frame encode before
	// it is trusted.
	var candidates []*hwEncoder
	if hasDRI && strings.Contains(encoders, "h264_qsv") {
		candidates = append(candidates, &hwEncoder{
			name:  "h264_qsv",
			vf:    "format=nv12",
			extra: []string{"-async_depth", "1", "-look_ahead", "0"},
		})
	}
	if hasDRI && strings.Contains(encoders, "h264_vaapi") {
		candidates = append(candidates, &hwEncoder{
			name:     "h264_vaapi",
			preInput: []string{"-vaapi_device", "/dev/dri/renderD128"},
			vf:       "format=nv12,hwupload",
			extra:    []string{"-async_depth", "1", "-rc_mode", "VBR"},
		})
	}
	if strings.Contains(encoders, "h264_nvenc") {
		candidates = append(candidates, &hwEncoder{
			name:  "h264_nvenc",
			vf:    "format=yuv420p",
			extra: []string{"-preset", "p1", "-tune", "ull", "-rc", "vbr", "-delay", "0"},
		})
	}
	for _, c := range candidates {
		if probeEncoderUsable(c) {
			return c
		}
		log.Printf("encoder: %s is compiled in but unusable on this host — skipping", c.name)
	}
	return nil
}

// probeEncoderUsable encodes a single throwaway frame and reports whether the
// encoder actually initialised (driver present, device accessible).
func probeEncoderUsable(e *hwEncoder) bool {
	args := []string{"-hide_banner", "-loglevel", "error"}
	args = append(args, e.preInput...)
	args = append(args, "-f", "lavfi", "-i", "color=c=black:s=256x144", "-frames:v", "1")
	if e.vf != "" {
		args = append(args, "-vf", e.vf)
	}
	args = append(args, "-c:v", e.name)
	args = append(args, e.extra...)
	args = append(args, "-f", "null", "-")
	if err := exec.Command("ffmpeg", args...).Run(); err != nil {
		return false
	}
	return true
}

// maxEncoderThreads caps the libx264 worker pool for one session.
//
// x264 sizes its pool from the number of *online* CPUs, which inside a
// `--cpus 3` container is the host count, not the quota the container may
// actually consume — a 64-core host would start 64 workers per session. The
// quota (encoderThreads) is the right bound; this constant only covers the case
// where no quota is set at all (a test run outside a container), so a
// quota-less process on a many-core host cannot spawn one worker per host core.
//
// It is deliberately not smaller than the quota. Measured inside `--cpus 3`
// (150-frame encode, 720p and 1080p, with and without competing load), total
// encode CPU is flat across thread counts — 0.20 and 0.42 core-seconds per
// second of video for every pool from 1 to 32 threads — so a tighter bound
// saves nothing. Only wall-clock latency moves: 0.57s vs 0.76s at 720p and
// 1.14s vs 1.59s at 1080p for 5 vs 2 threads. A smaller pool is therefore a
// pure latency regression, which matters when the encoder is behind after an
// IDR, a tab switch, or an ABR restart.
const maxEncoderThreads = 8

// encoderThreads is the thread count handed to libx264: the container's real
// CPU allowance, capped at maxEncoderThreads.
func encoderThreads() int {
	n := containerCPUs()
	if n > maxEncoderThreads {
		n = maxEncoderThreads
	}
	if n < 1 {
		n = 1
	}
	return n
}

// containerCPUs reports how many CPUs this process may actually use, read from
// the cgroup the container was started with. It returns the host CPU count when
// no quota is set (cgroup "max", a v1 quota of -1, or no cgroup files at all —
// e.g. running the streamer outside a container in a test).
func containerCPUs() int {
	// cgroup v2: "<quota> <period>", or "max <period>" when unlimited.
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		if f := strings.Fields(string(b)); len(f) == 2 && f[0] != "max" {
			quota, qerr := strconv.Atoi(f[0])
			period, perr := strconv.Atoi(f[1])
			if qerr == nil && perr == nil && quota > 0 && period > 0 {
				return (quota + period - 1) / period
			}
		}
	}
	// cgroup v1: a negative quota (conventionally -1) means unlimited.
	qb, qerr := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	pb, perr := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if qerr == nil && perr == nil {
		quota, e1 := strconv.Atoi(strings.TrimSpace(string(qb)))
		period, e2 := strconv.Atoi(strings.TrimSpace(string(pb)))
		if e1 == nil && e2 == nil && quota > 0 && period > 0 {
			return (quota + period - 1) / period
		}
	}
	return runtime.NumCPU()
}

// libx264Tuning is the quality half of the libx264 argv: the tune, the preset
// and the x264 parameter string the encoder is launched with.
//
// It is a variable rather than a literal solely so the offline quality bench
// (encoder_quality_bench_test.go, gated on ISOLA_QUALITY_BENCH=1) can encode
// the same corpus through this exact argv assembly for each candidate preset;
// the server never writes it. params is a format string taking the GOP twice
// (keyint, min-keyint) so the 2s GOP stays tied to the encoder fps.
type libx264Tuning struct {
	tune   string
	preset string
	params string
}

var libx264Quality = libx264Tuning{
	tune:   "zerolatency",
	preset: "superfast",
	params: "bframes=0:ref=1:rc-lookahead=0:scenecut=0:keyint=%d:min-keyint=%d:aq-mode=2:subme=1:me=dia:force-cfr=1",
}

// startEncoder launches ffmpeg as a subprocess, consuming raw BGRA frames on
// stdin and producing fragmented MP4 (fMP4) on stdout — or raw Annex-B H.264
// when rawH264 is set (WebCodecs clients decode access units directly, no
// container demuxing or MSE buffering).
// A hardware encoder is used when the host exposes one, otherwise libx264.
// scale is the active ABR encode scale: below 1 the frame is resized by the
// filter chain, and the raw input stays at the capture size.
func startEncoder(width, height, fps, bitrateKbps int, scale float64, rawH264 bool) (*exec.Cmd, io.WriteCloser, io.Reader, string, int, int, int, error) {
	encodeFps := effectiveEncodeFps(width, height, fps, fpsCapFor(rawH264))
	encW, encH := encodeDimensions(width, height, scale)
	// Honor the caller's bitrate (ISOLA_BITRATE_KBPS / resize); fall back to
	// the resolution-scaled default only when unset.
	if bitrateKbps <= 0 {
		bitrateKbps = bitrateKbpsForResolution(encW, encH)
	}
	codec := codecStringForLevel(encW, encH)

	// 2s GOP: every keyframe costs a full IDR, so a longer interval means
	// fewer quality dips and a lower average bitrate for the same picture.
	//
	// It is a bandwidth lever, not a CPU one: measured at 720p over 10s of
	// both motion and static content, 25 vs 50 vs 250 frames between IDRs all
	// cost 0.33/0.33/0.33 core-seconds per second of video. Viewers of a run
	// now share one encoder, so a late joiner waits for the next IDR — this
	// interval is the worst case for that wait, and it is bounded by
	// stream_init's first IDR plus the client's request_keyframe escape hatch.
	gop := 2 * encodeFps
	if gop < 15 {
		gop = 15
	}
	if gop > 120 {
		gop = 120
	}

	// 1s VBV. Capped VBR means static content costs almost nothing while
	// bursts stay bounded, so an idle page no longer burns the full target
	// rate on CBR filler. The cap is what the client's ABR ladder is sized
	// against.
	bufsizeKbps := bitrateKbps

	hw := detectHWEncoder()

	args := []string{
		"-hide_banner", "-loglevel", "warning",
		// Probing bounds only — deliberately not -fflags nobuffer. nobuffer
		// makes ffmpeg discard the frame it consumes while probing: measured
		// on a 250-frame clip it emitted 249 frames whose first picture was
		// capture frame 1, so every encoder start (session start, ABR step,
		// keyframe request) began one frame late and one frame stale. It buys
		// nothing here: the input's format, size and rate are all explicit.
		"-probesize", "32",
		"-analyzeduration", "0",
	}
	if hw != nil {
		args = append(args, hw.preInput...)
	}
	args = append(args,
		"-f", "rawvideo",
		"-pix_fmt", "bgra",
		"-s", fmt.Sprintf("%dx%d", width, height),
		"-r", strconv.Itoa(encodeFps),
		"-i", "pipe:0",
	)

	encName := "libx264"
	if hw != nil {
		encName = hw.name
		args = append(args,
			"-vf", scaledVf(hw.vf, encW, encH, width, height),
			"-c:v", hw.name,
			"-profile:v", h264Profile,
			// Pin the level the codec string advertises so the SPS matches
			// what MSE/WebCodecs were configured with.
			"-level:v", h264LevelFFmpeg(encW, encH),
		)
		args = append(args, hw.extra...)
		args = append(args,
			"-g", strconv.Itoa(gop),
			"-keyint_min", strconv.Itoa(gop),
			"-b:v", fmt.Sprintf("%dk", bitrateKbps),
			"-maxrate", fmt.Sprintf("%dk", bitrateKbps),
			"-bufsize", fmt.Sprintf("%dk", bufsizeKbps),
		)
	} else {
		// superfast + subme=1:me=dia: the cheapest setting that keeps High
		// profile, and deliberately not the quality maximum. The corpus is a
		// deterministic 100-frame 25fps
		// clip of dense small (5x7) antialiased text over a textured photo
		// strip plus an animated 480x320 panel, scrolled; each row is encoded
		// through this exact argv, decoded, and scored against the source with
		// ffmpeg ssim/psnr. Three runs per row, median; host 10 CPUs / 8
		// encoder threads. core-seconds per second of video / achieved kbps /
		// mean SSIM:
		//
		//   720p  @6Mbps target
		//     superfast dia  aq2            0.228 /  6573 / 0.9512
		//     veryfast  dia  aq2            0.244 /  6544 / 0.9552
		//     superfast dia  ref1 (shipped) 0.224 /  6577 / 0.9506
		//     superfast dia  ref1 aq0       0.230 /  6547 / 0.9553
		//     superfast hex3 aq2            0.294 /  6505 / 0.9635
		//     veryfast  hex2 aq2            0.303 /  6466 / 0.9646
		//     veryfast  hex3 aq2            0.322 /  6429 / 0.9643
		//     fast      hex3 aq2            0.440 /  6360 / 0.9684
		//     medium    hex3 aq2            0.463 /  6380 / 0.9686
		//   1080p @13.5Mbps target
		//     superfast dia  aq2            0.468 / 13106 / 0.9777
		//     veryfast  dia  aq2            0.495 / 12868 / 0.9785
		//     veryfast  hex2 aq2            0.593 / 13318 / 0.9846
		//     superfast dia  ref1 (shipped) 0.448 / 13121 / 0.9773
		//     superfast dia  ref1 aq0       0.466 / 13759 / 0.9831
		//     superfast hex3 aq2            0.685 / 13411 / 0.9839
		//     veryfast  hex3 ref1           0.680 / 13336 / 0.9847
		//     veryfast  hex3 aq2            0.744 / 13307 / 0.9847
		//     faster    hex3 aq2            1.113 / 13103 / 0.9860
		//     medium    hex3 aq2            1.182 / 13016 / 0.9862
		//     fast      hex3 aq2            1.136 / 13012 / 0.9862
		//
		// The finer presets are a real quality win under motion — hex alone is
		// +0.013 SSIM at 720p — but every step costs CPU on a host whose limit
		// is cores, not bits: veryfast+hex2 measured +33% at 720p and +24% at
		// 1080p end to end, and +13-19% on a live session, for a quality
		// difference that is invisible on a static page (0.0002 SSIM) and only
		// shows while the content moves. The product constraint is a lighter
		// session, so the shipped value stays on the cheapest tier; the rows
		// above are the menu if that trade is ever revisited.
		//
		// ref=1 is the one cheap saving: -4% CPU at 1080p and -2% at 720p for
		// -0.0004 SSIM, and one fewer reference frame to allocate on both ends.
		// aq-mode=0 measured *better* on this text corpus (720p 0.9553 vs
		// 0.9512, 1080p 0.9831 vs 0.9777 at the same CPU) because aq-mode=2
		// spends bits on dark flat regions that text-heavy pages do not have;
		// it is left at 2 because that gain is unproven on photographic pages,
		// which is a picture decision, not a CPU one.
		//
		// Thread count is not a lever: total CPU is flat across pools (see
		// maxEncoderThreads), and because zerolatency slices each frame, a
		// 3-thread container does slightly better than the 8 threads measured
		// here — these SSIM numbers are conservative.
		//
		// aq-mode=3 (dark-scene bias) is neutral at 720p (0.9681 vs 0.9684 at
		// fast hex) and loses at 1080p (0.9853 vs 0.9862 for more CPU) because
		// both this corpus and real browser pages are light. -preset ultrafast
		// was rejected despite being cheaper: it drops to Constrained
		// Baseline, which would invalidate the avc1.6400LL codec string and
		// cost ~24% bytes. cabac=0 was also rejected (~10% bytes for ~4%
		// CPU). zerolatency keeps B-frames and lookahead off for end-to-end
		// latency, and bframes=0/rc-lookahead=0/force-cfr=1 stay pinned.
		//
		// No nal-hrd=cbr: over a WebSocket the transport never needs padding,
		// and filler NALs would make an idle page cost the full target rate.
		// -maxrate/-bufsize still bound the VBV, so a static page emits almost
		// nothing while motion can still use the whole budget.
		tuning := libx264Quality
		x264Params := fmt.Sprintf(tuning.params, gop, gop)
		threads := encoderThreads()
		encName = fmt.Sprintf("libx264/%dt", threads)
		args = append(args,
			"-vf", scaledVf("format=yuv420p", encW, encH, width, height),
			"-c:v", "libx264",
			"-threads", strconv.Itoa(threads),
			"-preset", tuning.preset,
			"-tune", tuning.tune,
			"-profile:v", h264Profile,
			"-level:v", h264LevelFFmpeg(encW, encH),
			"-g", strconv.Itoa(gop),
			"-keyint_min", strconv.Itoa(gop),
			"-sc_threshold", "0",
			"-b:v", fmt.Sprintf("%dk", bitrateKbps),
			"-maxrate", fmt.Sprintf("%dk", bitrateKbps),
			"-bufsize", fmt.Sprintf("%dk", bufsizeKbps),
			"-x264-params", x264Params,
		)
	}

	if rawH264 {
		// Annex-B access units for WebCodecs: no container, no global
		// header — SPS/PPS stay in-band before each IDR. zerolatency
		// already repeats headers per IDR on this path.
		args = append(args, "-f", "h264", "pipe:1")
	} else {
		// Fragment sizing is subtle: frag_duration is measured in STREAM
		// time (dts deltas), not wall time. On a quiet page the pump feeds
		// one frame per keepalive window, so a 100ms+ fragment only closes
		// after several more feeds — a single change sat in the muxer for
		// 4-6s of wall time before MSE viewers saw it. 20ms is under one
		// frame interval (40ms at 25fps), so every fed frame closes the
		// previous fragment immediately; the pump's 40ms change-follow-up
		// feed is what pushes a lone change out within ~100ms of wall time.
		// Container overhead at these bitrates is negligible.
		args = append(args, "-f", "mp4",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-frag_duration", "20000",
			"pipe:1")
	}
	cmd := exec.Command("ffmpeg", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, "", 0, 0, 0, fmt.Errorf("ffmpeg stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, "", 0, 0, 0, fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, "", 0, 0, 0, fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				log.Printf("ffmpeg: %s", string(buf[:n]))
			}
			if err != nil {
				return
			}
		}
	}()
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, "", 0, 0, 0, fmt.Errorf("ffmpeg start: %w", err)
	}
	// cmd.Wait is NOT called here: a Cmd must be waited on exactly once, and
	// the stream hub's stop() owns that (after the stdout pump has drained,
	// since Wait closes the pipe). Waiting here as well races on Cmd internals.
	if encW != width || encH != height {
		log.Printf("encoder: capture %dx%d → encode %dx%d", width, height, encW, encH)
	}
	log.Printf("encoder: ffmpeg started capture=%dx%d encode=%dx%d@%dfps(capped from %d) %dkbps gop=%d codec=%s enc=%s raw=%v (pid=%d)",
		width, height, encW, encH, encodeFps, fps, bitrateKbps, gop, codec, encName, rawH264, cmd.Process.Pid)
	return cmd, stdin, stdout, codec, encW, encH, encodeFps, nil
}

// fpsCapFor selects the CFR ceiling per client mode.
//
// 25fps, not 30: encode cost is linear in frame count, and on a CPU-only host
// the difference between 25 and 30 is ~17% of a session's encoder budget for a
// cadence the eye cannot resolve on text and UI. The fed rate is still bounded
// by real change events — a static page is suppressed long before this cap.
// Both client modes share the ceiling.
func fpsCapFor(rawH264 bool) int {
	return 25
}
