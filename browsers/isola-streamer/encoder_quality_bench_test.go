package main

// Offline preset/quality matrix for the libx264 path, which is tuned for
// conferencing (superfast + zerolatency) while the real workload is text.
//
// It is gated on ISOLA_QUALITY_BENCH=1 so the normal suite never spends a
// minute shelling out to ffmpeg:
//
//	ISOLA_QUALITY_BENCH=1 go test -run TestEncoderQualityMatrix -v -timeout 20m
//
// Every row encodes the same deterministic text-heavy corpus through the real
// startEncoder argv (pipes, filters, ffmpeg process), decodes it back, and
// scores it against the exact source frames with ffmpeg's ssim/psnr filters.
// No thresholds are asserted here — the numbers are the deliverable; the
// selection below is printed for the operator, not enforced.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	benchFrameCount = 100 // 4s at 25fps, long enough for the 2s GOP to repeat
	benchFPS        = 25
	benchClipBudget = 10 * time.Second

	// Encoder CPU the selection allows, in core-seconds per second of video.
	// A session container has 3 CPUs and the rest of the session (capture,
	// Electron, muxing) costs roughly 0.5-1.0 core, so 1.5 leaves headroom
	// without letting a preset eat the container.
	benchCPUCeiling = 1.5

	benchScrollPx    = 2 // page scroll per frame
	benchTextScale   = 1 // 5x7 font pixel -> 1x1 surface pixels (10-12px browser text)
	benchGlyphAdv    = 5*benchTextScale + 1
	benchLinePitch   = 7*benchTextScale + 1
	benchLeftMargin  = 24
	benchRightMargin = 24
)

// benchWords supplies realistic word shapes for dense body text. Short and
// long words mix so the horizontal high-frequency pattern is not periodic.
var benchWords = []string{
	"the", "stream", "browser", "frame", "quality", "encoder", "latency",
	"session", "window", "render", "layout", "config", "request", "response",
	"server", "client", "socket", "header", "payload", "frame", "rate",
	"video", "audio", "pixel", "buffer", "cursor", "scroll", "element", "text",
	"of", "and", "to", "is", "for", "with", "that", "this", "from", "not",
	"interface", "document", "container", "resolution", "throughput", "codec",
}

// benchCorpus renders one page: dense small text over a textured photo strip.
// The page is drawn once per resolution; a frame is a vertical window into it
// (scroll motion) with an animated video-call-sized panel stamped at a
// frame-dependent position. Every frame is deterministic and cheap to
// regenerate for scoring.
//
// The mixture matters: a page of only sharp text at 720p compresses to ~2.5 of
// the 6 Mbps budget and every preset scores within 0.0002 SSIM, which cannot
// rank them. The photo texture and the per-frame panel keep the VBV busy the
// way a real browser session (images, a video thumbnail) does.
type benchCorpus struct {
	w, h     int
	rowBytes int
	page     []byte // BGRA, h + scroll span rows
	scratch  []byte // BGRA scratch, exactly one frame
}

func newBenchCorpus(w, h int) *benchCorpus {
	c := &benchCorpus{w: w, h: h, rowBytes: w * 4}
	pageH := h + (benchFrameCount-1)*benchScrollPx + benchLinePitch
	c.page = make([]byte, c.rowBytes*pageH)
	c.scratch = make([]byte, c.rowBytes*h)

	// White paper, opaque.
	for i := 0; i < len(c.page); i += 4 {
		c.page[i], c.page[i+1], c.page[i+2], c.page[i+3] = 255, 255, 255, 255
	}

	// A photo-like strip: horizontal gradient plus fine per-pixel texture.
	photoH := pageH / 3
	for y := 0; y < photoH; y++ {
		gy := float64(y) / float64(photoH)
		for x := benchLeftMargin; x < w-benchRightMargin; x++ {
			gx := float64(x-benchLeftMargin) / float64(w-2*benchLeftMargin)
			r := int(40 + 170*gx + 30*gy)
			g := int(70 + 80*gy + 20*gx)
			b := int(110 + 90*(1-gx))
			hash := uint32(x*374761393) + uint32(y*668265263)
			hash = (hash ^ (hash >> 13)) * 1274126177
			j := int(hash&15) - 8
			off := y*c.rowBytes + x*4
			c.page[off], c.page[off+1], c.page[off+2], c.page[off+3] =
				clampByte(r+j), clampByte(g+j), clampByte(b+j), 255
		}
	}

	for line, y := 0, 0; y+7*benchTextScale <= pageH; line, y = line+1, y+benchLinePitch {
		x := benchLeftMargin
		seed := uint32(line)*2654435761 + 12345
		for x < w-benchRightMargin {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			word := benchWords[seed%uint32(len(benchWords))]
			drawBenchWord(c.page, w, pageH, x, y, word)
			x += len(word)*benchGlyphAdv + benchGlyphAdv
			if seed%7 == 0 {
				x += benchGlyphAdv // ragged line end, like a real paragraph
			}
		}
	}
	return c
}

// clampByte narrows an int colour channel to [0,255].
func clampByte(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// drawBenchWord blits one word with the package's 5x7 bitmap font.
func drawBenchWord(dst []byte, w, h, ox, oy int, word string) {
	x := ox
	for i := range len(word) {
		glyph, ok := font5x7[word[i]]
		if !ok {
			glyph = font5x7['?']
		}
		drawBenchGlyph(dst, w, h, glyph, x, oy)
		x += benchGlyphAdv
	}
}

// drawBenchGlyph blits one glyph in near-black ink with one grey antialias
// ring, the way a browser rasterizes small text. The soft edge is what makes
// quantisation visible in SSIM instead of the glyphs staying pixel-exact.
func drawBenchGlyph(dst []byte, w, h int, glyph [5]byte, ox, oy int) {
	put := func(px, py int, v byte) {
		if px < 0 || py < 0 || px >= w || py >= h {
			return
		}
		off := py*w*4 + px*4
		dst[off], dst[off+1], dst[off+2], dst[off+3] = v, v, v, 255
	}
	lit := func(px, py int) bool {
		if px < 0 || py < 0 || px >= w || py >= h {
			return false
		}
		off := py*w*4 + px*4
		return dst[off] < 128 && dst[off] == dst[off+1] && dst[off] == dst[off+2]
	}
	for col := range 5 {
		bits := glyph[col]
		for row := range 7 {
			if bits&(1<<row) == 0 {
				continue
			}
			for dx := range benchTextScale {
				for dy := range benchTextScale {
					put(ox+col*benchTextScale+dx, oy+row*benchTextScale+dy, 20)
				}
			}
		}
	}
	// Antialias ring around the glyph box (one pixel wide).
	for col := -1; col <= 5*benchTextScale; col++ {
		for row := -1; row <= 7*benchTextScale; row++ {
			px, py := ox+col, oy+row
			if lit(px, py) {
				continue
			}
			if lit(px-1, py) || lit(px+1, py) || lit(px, py-1) || lit(px, py+1) {
				put(px, py, 165)
			}
		}
	}
}

// drawPanel stamps the per-frame animated region: shifting colour bars with
// banding and sparse highlights, at a slowly drifting position.
func (c *benchCorpus) drawPanel(i int) {
	const pw, ph = 480, 320
	px0 := 40 + (i*7)%(c.w-pw-80)
	py0 := 40
	for y := range ph {
		row := (py0 + y) * c.rowBytes
		shade := (y*3 + i*5) & 31
		for x := range pw {
			off := row + (px0+x)*4
			var r, g, b int
			switch (x/32 + i/3) & 3 {
			case 0:
				r, g, b = 220, 60, 40
			case 1:
				r, g, b = 40, 180, 90
			case 2:
				r, g, b = 50, 90, 220
			default:
				r, g, b = 230, 200, 60
			}
			r, g, b = r-shade, g-shade, b-shade
			h := uint32(x)*374761393 + uint32(y)*668265263 + uint32(i)*2246822519
			h = (h ^ (h >> 13)) * 1274126177
			if h&0x3ff < 22 {
				r, g, b = 245, 245, 245
			}
			c.scratch[off], c.scratch[off+1], c.scratch[off+2], c.scratch[off+3] =
				clampByte(r), clampByte(g), clampByte(b), 255
		}
	}
}

// frame renders frame i into the corpus scratch buffer and returns it.
func (c *benchCorpus) frame(i int) []byte {
	offset := i * benchScrollPx
	copy(c.scratch, c.page[offset*c.rowBytes:(offset+c.h)*c.rowBytes])
	c.drawPanel(i)
	return c.scratch
}

type benchResult struct {
	setting       string
	width, height int
	bitrateKbps   int
	bytes         int64
	kbps          float64
	ssim          float64
	psnrDB        float64
	coreSecPerSec float64
	wall          time.Duration
	decodedFrames int
	dropped       bool
}

// benchEncode feeds the whole corpus through startEncoder and returns the
// Annex-B output path plus the measured encode cost.
func benchEncode(t *testing.T, c *benchCorpus, bitrate int, tuning libx264Tuning, name string) (string, benchResult) {
	t.Helper()

	saved := libx264Quality
	libx264Quality = tuning
	defer func() { libx264Quality = saved }()

	path := filepath.Join(t.TempDir(), fmt.Sprintf("%s_%dx%d.h264",
		strings.NewReplacer("/", "_", " ", "_").Replace(name), c.w, c.h))

	start := time.Now()
	cmd, stdin, stdout, _, encW, encH, _, err := startEncoder(c.w, c.h, benchFPS, bitrate, 1.0, true)
	if err != nil {
		t.Fatalf("%s: startEncoder: %v", name, err)
	}
	if encW != c.w || encH != c.h {
		t.Fatalf("%s: encoder dims %dx%d, want %dx%d", name, encW, encH, c.w, c.h)
	}

	out, err := os.Create(path)
	if err != nil {
		t.Fatalf("%s: create output: %v", name, err)
	}
	copyDone := make(chan error, 1)
	go func() {
		_, cerr := io.Copy(out, stdout)
		copyDone <- cerr
	}()

	for i := range benchFrameCount {
		if _, err := stdin.Write(c.frame(i)); err != nil {
			t.Fatalf("%s: write frame %d: %v", name, i, err)
		}
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("%s: close stdin: %v", name, err)
	}
	if err := <-copyDone; err != nil {
		t.Fatalf("%s: drain stdout: %v", name, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%s: ffmpeg exit: %v", name, err)
	}
	wall := time.Since(start)

	if err := out.Close(); err != nil {
		t.Fatalf("%s: close output: %v", name, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: stat output: %v", name, err)
	}

	cpu := cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	videoSeconds := float64(benchFrameCount) / benchFPS
	res := benchResult{
		setting:       name,
		width:         c.w,
		height:        c.h,
		bitrateKbps:   bitrate,
		bytes:         fi.Size(),
		kbps:          float64(fi.Size()) * 8 / videoSeconds / 1000,
		coreSecPerSec: cpu.Seconds() / videoSeconds,
		wall:          wall,
		dropped:       wall > benchClipBudget,
	}
	res.decodedFrames = benchDecodedFrames(t, path)
	return path, res
}

// benchDecodedFrames counts the pictures the raw H.264 file actually contains,
// so a row whose encode dropped frames cannot silently pass as a comparison.
func benchDecodedFrames(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-count_frames", "-show_entries", "stream=nb_read_frames",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe frames: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("ffprobe frames: parse %q: %v", out, err)
	}
	return n
}

var (
	benchSSIMRe = regexp.MustCompile(`SSIM .*All:([0-9.]+) \(([0-9.]+)\)`)
	benchPSNRRe = regexp.MustCompile(`PSNR .*average:([0-9.]+|inf)`)
)

// benchScore decodes the encoded file and compares it frame by frame to the
// exact BGRA source, in one ffmpeg pass (ssim and psnr share the decode).
func benchScore(t *testing.T, c *benchCorpus, encoded string) (ssim, psnrDB float64) {
	t.Helper()

	const graph = "[0:v]format=yuv420p,split=2[refA][refB];" +
		"[1:v]format=yuv420p,split=2[encA][encB];" +
		"[refA][encA]ssim[ss];[refB][encB]psnr[ps]"
	args := []string{
		"-hide_banner", "-nostats",
		"-f", "rawvideo", "-pix_fmt", "bgra",
		"-s", fmt.Sprintf("%dx%d", c.w, c.h), "-r", strconv.Itoa(benchFPS),
		"-i", "pipe:0",
		"-i", encoded,
		"-lavfi", graph,
		"-map", "[ss]", "-map", "[ps]",
		"-f", "null", "-",
	}
	cmd := exec.Command("ffmpeg", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("score stdin: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("score start: %v", err)
	}
	for i := range benchFrameCount {
		if _, err := stdin.Write(c.frame(i)); err != nil {
			t.Fatalf("score write frame %d: %v", i, err)
		}
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("score ffmpeg: %v\n%s", err, stderr.String())
	}

	log := stderr.String()
	sm := benchSSIMRe.FindStringSubmatch(log)
	pm := benchPSNRRe.FindStringSubmatch(log)
	if sm == nil || pm == nil {
		t.Fatalf("score: no ssim/psnr summary in ffmpeg output:\n%s", log)
	}
	ssim, err = strconv.ParseFloat(sm[1], 64)
	if err != nil {
		t.Fatalf("score: ssim %q: %v", sm[1], err)
	}
	if pm[1] == "inf" {
		psnrDB = 99.0
	} else {
		psnrDB, err = strconv.ParseFloat(pm[1], 64)
		if err != nil {
			t.Fatalf("score: psnr %q: %v", pm[1], err)
		}
	}
	return ssim, psnrDB
}

// benchRunCount is how many times each row is encoded. One run is usually
// enough to rank presets, but x264's threaded output is not bit-exact across
// runs (~0.4% bitrate / ~0.0008 SSIM), so repeated runs are what separate a
// real effect from noise. ISOLA_QUALITY_BENCH_RUNS overrides it.
func benchRunCount(t *testing.T) int {
	if v := os.Getenv("ISOLA_QUALITY_BENCH_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("ISOLA_QUALITY_BENCH_RUNS=%q: want a positive integer", v)
		}
		return n
	}
	return 1
}

// medianResult returns the run with the median CPU cost (the stable cost
// signal), carrying that run's measured quality.
func medianResult(runs []benchResult) benchResult {
	cp := append([]benchResult(nil), runs...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].coreSecPerSec < cp[j].coreSecPerSec })
	return cp[len(cp)/2]
}

// TestEncoderQualityMatrix is the deliverable: a printed matrix, no asserts.
func TestEncoderQualityMatrix(t *testing.T) {
	if os.Getenv("ISOLA_QUALITY_BENCH") != "1" {
		t.Skip("set ISOLA_QUALITY_BENCH=1 to run the libx264 quality matrix")
	}
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	// A hardware encoder would bypass the libx264 argv entirely; the bench
	// only measures the software fallback the matrix is about.
	if hw := detectHWEncoder(); hw != nil {
		t.Skipf("hardware encoder %s present; this matrix measures libx264", hw.name)
	}

	// bframes=0 / rc-lookahead=0 / force-cfr=1 are invariant: they are
	// latency, not quality. Every variant keeps them.
	const (
		paramsBase     = "bframes=0:ref=2:rc-lookahead=0:scenecut=0:keyint=%d:min-keyint=%d:aq-mode=2:subme=1:me=dia:force-cfr=1"
		paramsHex3     = "bframes=0:ref=2:rc-lookahead=0:scenecut=0:keyint=%d:min-keyint=%d:aq-mode=2:subme=3:me=hex:force-cfr=1"
		paramsHex3AQ3  = "bframes=0:ref=2:rc-lookahead=0:scenecut=0:keyint=%d:min-keyint=%d:aq-mode=3:subme=3:me=hex:force-cfr=1"
		paramsHex2     = "bframes=0:ref=2:rc-lookahead=0:scenecut=0:keyint=%d:min-keyint=%d:aq-mode=2:subme=2:me=hex:force-cfr=1"
		paramsUmh3     = "bframes=0:ref=2:rc-lookahead=0:scenecut=0:keyint=%d:min-keyint=%d:aq-mode=2:subme=3:me=umh:force-cfr=1"
		paramsHex3R1   = "bframes=0:ref=1:rc-lookahead=0:scenecut=0:keyint=%d:min-keyint=%d:aq-mode=2:subme=3:me=hex:force-cfr=1"
		paramsDiaR1    = "bframes=0:ref=1:rc-lookahead=0:scenecut=0:keyint=%d:min-keyint=%d:aq-mode=2:subme=1:me=dia:force-cfr=1"
		paramsDiaR1Aq0 = "bframes=0:ref=1:rc-lookahead=0:scenecut=0:keyint=%d:min-keyint=%d:aq-mode=0:subme=1:me=dia:force-cfr=1"
	)

	// The shipped tuning is always row 1, so the matrix ranks candidates
	// against what actually runs; duplicates of it are dropped.
	candidates := []struct {
		name   string
		tuning libx264Tuning
	}{
		{"production (current)", libx264Quality},
		{"superfast dia aq2 (previous)", libx264Tuning{"zerolatency", "superfast", paramsBase}},
		{"veryfast dia aq2", libx264Tuning{"zerolatency", "veryfast", paramsBase}},
		{"faster dia aq2", libx264Tuning{"zerolatency", "faster", paramsBase}},
		{"medium dia aq2", libx264Tuning{"zerolatency", "medium", paramsBase}},
		{"veryfast hex3 aq2", libx264Tuning{"zerolatency", "veryfast", paramsHex3}},
		{"faster hex3 aq2", libx264Tuning{"zerolatency", "faster", paramsHex3}},
		{"medium hex3 aq2", libx264Tuning{"zerolatency", "medium", paramsHex3}},
		{"fast hex3 aq3", libx264Tuning{"zerolatency", "fast", paramsHex3AQ3}},
		{"superfast hex3 aq2", libx264Tuning{"zerolatency", "superfast", paramsHex3}},
		{"veryfast hex2 aq2", libx264Tuning{"zerolatency", "veryfast", paramsHex2}},
		{"veryfast umh3 aq2", libx264Tuning{"zerolatency", "veryfast", paramsUmh3}},
		{"veryfast hex3 ref1", libx264Tuning{"zerolatency", "veryfast", paramsHex3R1}},
		{"superfast dia ref1", libx264Tuning{"zerolatency", "superfast", paramsDiaR1}},
		{"superfast dia ref1 aq0", libx264Tuning{"zerolatency", "superfast", paramsDiaR1Aq0}},
	}
	var variants []struct {
		name   string
		tuning libx264Tuning
	}
	for _, v := range candidates {
		dup := false
		for _, kept := range variants {
			if kept.tuning == v.tuning {
				dup = true
				break
			}
		}
		if !dup {
			variants = append(variants, v)
		}
	}
	resolutions := []struct{ w, h int }{{1280, 720}, {1920, 1080}}
	runs := benchRunCount(t)

	fmt.Printf("\n=== libx264 quality matrix: %d frames @ %dfps, %d runs/row, %d host CPUs, %d encoder threads, ceiling %.1f core-sec/sec ===\n",
		benchFrameCount, benchFPS, runs, containerCPUs(), encoderThreads(), benchCPUCeiling)

	var all []benchResult
	for _, r := range resolutions {
		c := newBenchCorpus(r.w, r.h)
		bitrate := bitrateKbpsForResolution(r.w, r.h)
		fmt.Printf("\n--- %dx%d @ %dkbps target ---\n", r.w, r.h, bitrate)
		fmt.Printf("%-27s %8s %8s %8s %8s %8s %6s %6s\n",
			"setting", "kbps", "SSIM", "PSNR dB", "core/s", "wall s", "frames", "cpu max")
		for _, v := range variants {
			rowRuns := make([]benchResult, 0, runs)
			for run := 0; run < runs; run++ {
				path, res := benchEncode(t, c, bitrate, v.tuning, v.name)
				ssim, psnr := benchScore(t, c, path)
				res.ssim, res.psnrDB = ssim, psnr
				rowRuns = append(rowRuns, res)
			}
			res := medianResult(rowRuns)
			maxCPU := 0.0
			for _, rr := range rowRuns {
				if rr.coreSecPerSec > maxCPU {
					maxCPU = rr.coreSecPerSec
				}
			}
			all = append(all, res)
			marker := ""
			if res.dropped {
				marker = "  DROPPED (>10s budget)"
			}
			fmt.Printf("%-27s %8.0f %8.5f %8.2f %8.3f %8.2f %6d %6.3f%s\n",
				res.setting, res.kbps, res.ssim, res.psnrDB, res.coreSecPerSec,
				res.wall.Seconds(), res.decodedFrames, maxCPU, marker)
		}
	}

	printBenchSelection(all)
}

// printBenchSelection reports, per resolution, which row beats the current
// setting at equal-or-lower achieved bitrate while staying inside the CPU
// ceiling. It prints a recommendation only; the operator lands it.
func printBenchSelection(all []benchResult) {
	fmt.Printf("\n--- selection (baseline = production row; ceiling %.1f core-s/s) ---\n", benchCPUCeiling)
	for _, r := range []struct{ w, h int }{{1280, 720}, {1920, 1080}} {
		var base *benchResult
		var best *benchResult
		for i := range all {
			row := &all[i]
			if row.width != r.w || row.height != r.h || row.dropped {
				continue
			}
			if strings.HasSuffix(row.setting, "(current)") {
				base = row
			}
		}
		if base == nil {
			continue
		}
		for i := range all {
			row := &all[i]
			if row.width != r.w || row.height != r.h || row.dropped || row == base {
				continue
			}
			if row.kbps > base.kbps || row.coreSecPerSec > benchCPUCeiling {
				continue
			}
			if best == nil || row.ssim > best.ssim {
				best = row
			}
		}
		if best == nil {
			fmt.Printf("%dx%d: no variant beats baseline at equal/lower bitrate inside the ceiling\n", r.w, r.h)
			continue
		}
		fmt.Printf("%dx%d: %s — SSIM %.5f vs %.5f (+%.5f), kbps %.0f vs %.0f, CPU %.3f vs %.3f core-s/s\n",
			r.w, r.h, best.setting, best.ssim, base.ssim, best.ssim-base.ssim,
			best.kbps, base.kbps, best.coreSecPerSec, base.coreSecPerSec)
	}
}
