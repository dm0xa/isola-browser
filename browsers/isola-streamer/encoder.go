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

// H.264 High profile: CABAC + 8x8 transform, ~15% cheaper than baseline — the main quality lever on text.
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

// bitrateKbpsForResolution: linear in pixel area, 6 Mbps @ 720p; 60 Mbps guards malformed input only.
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

// encodeDimensions: ABR scale applied to the capture size; yuv420p-even. Input stays at capture size.
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
		log.Printf("encoder: hw probe %v, libx264", err)
		return nil
	}
	encoders := string(out)
	hasDRI := false
	if _, statErr := os.Stat("/dev/dri/renderD128"); statErr == nil {
		hasDRI = true
	}
	// -encoders lists what the build supports, not the host: each candidate is smoke-tested with a real one-frame encode.
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
		log.Printf("encoder: %s unusable, skipping", c.name)
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

// maxEncoderThreads caps x264 workers when no cgroup quota exists. Measured: total
// encode CPU is flat across pools (0.20-0.42 core-s/s at 720p/1080p), only latency
// moves (0.57s vs 0.76s for 5 vs 2 threads) — so the bound is never below the quota.
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

// containerCPUs: cgroup quota, or the host count when no quota is set (tests outside a container).
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

// libx264Tuning: a var so ISOLA_QUALITY_BENCH can replay the same argv; params takes the GOP twice.
type libx264Tuning struct {
	tune   string
	preset string
	params string
}

var libx264Quality = libx264Tuning{
	tune:   "zerolatency",
	preset: "superfast",
	params: "bframes=0:ref=2:rc-lookahead=0:scenecut=0:keyint=%d:min-keyint=%d:aq-mode=2:subme=1:me=dia:force-cfr=1",
}

// startEncoder: raw BGRA on stdin → fMP4 (or raw Annex-B when rawH264) on stdout.
// Hardware encoder when available, else libx264. scale < 1 resizes in the filter chain.
func startEncoder(width, height, fps, bitrateKbps int, scale float64, rawH264 bool) (*exec.Cmd, io.WriteCloser, io.Reader, string, int, int, int, error) {
	encodeFps := effectiveEncodeFps(width, height, fps, fpsCapFor(rawH264))
	encW, encH := encodeDimensions(width, height, scale)
	// Honor the caller's bitrate (ISOLA_BITRATE_KBPS / resize); fall back to
	// the resolution-scaled default only when unset.
	if bitrateKbps <= 0 {
		bitrateKbps = bitrateKbpsForResolution(encW, encH)
	}
	codec := codecStringForLevel(encW, encH)

	// 2s GOP: fewer IDRs = lower average bitrate; CPU is flat across intervals
	// (0.33 core-s/s at 720p for 25/50/250). Late joiners use request_keyframe.
	gop := 2 * encodeFps
	if gop < 15 {
		gop = 15
	}
	if gop > 120 {
		gop = 120
	}

	// 1s VBV, capped VBR: static content costs almost nothing; the cap sizes the client's ABR ladder.
	bufsizeKbps := bitrateKbps

	hw := detectHWEncoder()

	args := []string{
		"-hide_banner", "-loglevel", "warning",
		// No -fflags nobuffer: it drops the probe frame, starting every encode one frame late and stale.
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

	if hw != nil {

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
		// superfast + dia: cheapest tier that keeps High profile. Full preset sweep
		// (ssim/psnr over a 100-frame text corpus, three reps, median):
		//   superfast dia aq2  0.228 core-s/s, SSIM 0.9512 @720p
		//   veryfast hex2 aq2  0.303, SSIM 0.9646 — +33% end-to-end CPU for +0.013 SSIM
		//   medium hex3 aq2    0.463, SSIM 0.9686
		// aq-mode=0 is better on text at the same CPU; thread count is not a lever
		// (see maxEncoderThreads). ultrafast/cabac=0 rejected (Baseline, ~24% bytes);
		// no nal-hrd=cbr — filler NALs would cost an idle page the full target rate.
		tuning := libx264Quality
		x264Params := fmt.Sprintf(tuning.params, gop, gop)
		threads := encoderThreads()
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
		// frag_duration counts stream time: on a quiet page a 100ms fragment took
		// 4-6s wall time to close. 20ms is under one frame interval, so every feed
		// closes the previous fragment immediately.
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
				msg := string(buf[:n])
				// x264 stats/config noise is not a problem report; only
				// surface genuine errors and warnings.
				if !strings.Contains(msg, "x264 [info]") && !strings.Contains(msg, "[libx264") && !strings.Contains(msg, "configuration:") {
					log.Printf("ffmpeg: %s", msg)
				}
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
		log.Printf("encoder: %dx%d → %dx%d", width, height, encW, encH)
	}
	log.Printf("encoder: ffmpeg %dx%d@%d %dkbps %s pid=%d",
		encW, encH, encodeFps, bitrateKbps, codec, cmd.Process.Pid)
	return cmd, stdin, stdout, codec, encW, encH, encodeFps, nil
}

// fpsCapFor: 25fps shared by both modes — ~17% of the encoder budget vs 30 for an
// invisible cadence gain; real change events still bound the fed rate.
func fpsCapFor(rawH264 bool) int {
	return 25
}
