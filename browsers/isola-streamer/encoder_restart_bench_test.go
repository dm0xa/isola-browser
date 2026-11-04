package main

// Encoder restart bench (docs/encoder-runtime-control.md).
//
// Measures the cost the stream hub pays for its only two levers — a forced
// keyframe (streamHub.requestKeyframe) and an ABR step (ws setBitrate) — both
// of which restart ffmpeg because libx264 on a live pipe cannot emit an IDR or
// be retargeted from the CLI.
//
// "Restart stall" is measured the way a viewer experiences it: from the
// startEncoder call (the moment the hub decides to replace the run) to the
// first fMP4 fragment carrying a sync sample, i.e. the first IDR that clears
// streamSub.awaitingKey.
//
//	Gated: ISOLA_ENCODER_BENCH=1 go test -run TestEncoderRestart -v ./...
//	Runs:  ISOLA_ENCODER_BENCH_RUNS (default 12)
//
// Not part of the normal suite: it skips unless the env gate is set, and each
// sample costs ~2s of wall time.

import (
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// benchFrame builds a synthetic BGRA capture frame: scrolling stripes plus
// per-pixel noise, so libx264 spends realistic bitrate on the first IDR.
func benchFrame(w, h, i int) []byte {
	frame := make([]byte, w*h*4)
	seed := uint32(0x9e3779b9) * uint32(i+1)
	for y := range h {
		row := frame[y*w*4:]
		stripe := ((y + i*3) / 11) & 1
		for x := range w {
			seed = seed*1664525 + 1013904223
			v := byte(0x28)
			if stripe == 1 {
				v = 0xd0
			}
			if ((x+i*7)%97) < 41 && (y/3)&1 == 1 {
				v = 0x10
			}
			v += byte(seed>>28) & 0x1f
			o := x * 4
			row[o], row[o+1], row[o+2], row[o+3] = v, v, v, 0xff
		}
	}
	return frame
}

func benchPercentiles(samples []time.Duration) (min, median, max, mean time.Duration) {
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	var sum time.Duration
	for _, s := range samples {
		sum += s
	}
	return samples[0], samples[len(samples)/2], samples[len(samples)-1], sum / time.Duration(len(samples))
}

// benchGate skips unless the bench is enabled, and returns the sample count.
func benchGate(t *testing.T) int {
	t.Helper()
	if os.Getenv("ISOLA_ENCODER_BENCH") != "1" {
		t.Skip("set ISOLA_ENCODER_BENCH=1 to run the encoder restart bench")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}
	runs := 12
	if v := os.Getenv("ISOLA_ENCODER_BENCH_RUNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			runs = n
		}
	}
	return runs
}

// benchFeedText describes the encode the bench drives, identical to what the
// hub asks startEncoder for at 720p: 25fps, 6 Mbps, scale 1.
const (
	benchWidth, benchHeight = 1280, 720
	restartBenchFPS         = 25
	benchKbps               = 6000
)

func benchFrames() [][]byte {
	frames := make([][]byte, 8)
	for i := range frames {
		frames[i] = benchFrame(benchWidth, benchHeight, i)
	}
	return frames
}

// benchFeeder writes frames at the encoder's frame rate until the pipe closes.
func benchFeeder(stdin io.Writer, frames [][]byte, fed *atomic.Int64) func() {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		interval := time.Second / restartBenchFPS
		start := time.Now()
		for i := 0; ; i++ {
			if i > 0 {
				if wait := start.Add(time.Duration(i) * interval).Sub(time.Now()); wait > 0 {
					time.Sleep(wait)
				}
			}
			if _, err := stdin.Write(frames[i%len(frames)]); err != nil {
				return
			}
			fed.Add(1)
		}
	}()
	return wg.Wait
}

// TestEncoderRestartStall measures today's restart cost on the fMP4 path:
// spawn → first IDR-bearing fragment, which is what the viewer waits for.
func TestEncoderRestartStall(t *testing.T) {
	runs := benchGate(t)
	frames := benchFrames()

	var (
		stalls   []time.Duration // startEncoder → first IDR fragment
		spawns   []time.Duration // startEncoder → fork returned
		teardown []time.Duration // kill + wait
		idrBytes []int
	)

	for run := range runs {
		t0 := time.Now()
		cmd, stdin, stdout, _, _, _, _, err := startEncoder(benchWidth, benchHeight, restartBenchFPS, benchKbps, 1, false)
		if err != nil {
			t.Fatalf("run %d: startEncoder: %v", run, err)
		}
		spawn := time.Since(t0)

		var fed atomic.Int64
		waitFeeder := benchFeeder(stdin, frames, &fed)

		reader := newFMP4Reader()
		buf := make([]byte, 64*1024)
		var stall time.Duration
		var keyLen, fedAtIDR int
		for stall == 0 {
			n, rerr := stdout.Read(buf)
			if n > 0 {
				for _, seg := range reader.Feed(buf[:n]) {
					if isIDRFragment(seg) {
						stall = time.Since(t0)
						keyLen = len(seg)
						fedAtIDR = int(fed.Load())
						break
					}
				}
			}
			if rerr != nil && stall == 0 {
				t.Fatalf("run %d: stdout ended before an IDR (%v)", run, rerr)
			}
		}

		killAt := time.Now()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		teardown = append(teardown, time.Since(killAt))
		_ = stdin.Close()
		waitFeeder()

		stalls = append(stalls, stall)
		spawns = append(spawns, spawn)
		idrBytes = append(idrBytes, keyLen)
		t.Logf("run %2d: spawn=%v stall=%v idr_bytes=%d frames_fed_before_idr=%d",
			run, spawn.Round(time.Microsecond), stall.Round(time.Microsecond), keyLen, fedAtIDR)
	}

	smin, smed, smax, smean := benchPercentiles(stalls)
	pmin, pmed, pmax, _ := benchPercentiles(spawns)
	tmin, tmed, tmax, _ := benchPercentiles(teardown)
	t.Logf("restart stall (spawn→first IDR, n=%d): min=%v median=%v max=%v mean=%v",
		len(stalls), smin.Round(time.Microsecond), smed.Round(time.Microsecond), smax.Round(time.Microsecond), smean.Round(time.Microsecond))
	t.Logf("fork cost (startEncoder return, n=%d): min=%v median=%v max=%v",
		len(spawns), pmin.Round(time.Microsecond), pmed.Round(time.Microsecond), pmax.Round(time.Microsecond))
	t.Logf("teardown (kill+wait, n=%d): min=%v median=%v max=%v",
		len(teardown), tmin.Round(time.Microsecond), tmed.Round(time.Microsecond), tmax.Round(time.Microsecond))
	t.Logf("first IDR fragment bytes: %d (frames fed at 25fps from spawn)", idrBytes[0])
}

// TestEncoderRestartBreakdown splits the stall into the two things an
// in-process encoder would remove and the one it would not: process + CLI +
// filtergraph startup (spawn → first stdout byte) and the IDR encode + mux that
// follows it. Raw Annex-B output is used because a NAL type 5 is an
// unambiguous IDR boundary; the fMP4 path adds one frame of fragment latency
// (see TestEncoderRestartStall).
func TestEncoderRestartBreakdown(t *testing.T) {
	runs := benchGate(t)
	frames := benchFrames()

	var firstByte, firstIDR, initToIDR []time.Duration
	for run := range runs {
		t0 := time.Now()
		cmd, stdin, stdout, _, _, _, _, err := startEncoder(benchWidth, benchHeight, restartBenchFPS, benchKbps, 1, true)
		if err != nil {
			t.Fatalf("run %d: startEncoder: %v", run, err)
		}

		var fed atomic.Int64
		waitFeeder := benchFeeder(stdin, frames, &fed)

		buf := make([]byte, 64*1024)
		var pending []byte
		var byteAt, idrAt time.Duration
		for idrAt == 0 {
			n, rerr := stdout.Read(buf)
			if n > 0 {
				if byteAt == 0 {
					byteAt = time.Since(t0)
				}
				pending = append(pending, buf[:n]...)
				if hasIDRNAL(pending) {
					idrAt = time.Since(t0)
				}
			}
			if rerr != nil && idrAt == 0 {
				t.Fatalf("run %d: stdout ended before an IDR (%v)", run, rerr)
			}
		}
		killAt := time.Now()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdin.Close()
		waitFeeder()

		firstByte = append(firstByte, byteAt)
		firstIDR = append(firstIDR, idrAt)
		initToIDR = append(initToIDR, idrAt-byteAt)
		t.Logf("run %2d: first_byte=%v first_idr=%v init_to_idr=%v teardown=%v",
			run, byteAt.Round(time.Microsecond), idrAt.Round(time.Microsecond),
			(idrAt - byteAt).Round(time.Microsecond), time.Since(killAt).Round(time.Microsecond))
	}

	reportDur := func(name string, xs []time.Duration) {
		min, med, max, mean := benchPercentiles(xs)
		t.Logf("%s (n=%d): min=%v median=%v max=%v mean=%v", name, len(xs),
			min.Round(time.Microsecond), med.Round(time.Microsecond),
			max.Round(time.Microsecond), mean.Round(time.Microsecond))
	}
	reportDur("process+CLI startup (spawn→first stdout byte)", firstByte)
	reportDur("spawn→first IDR", firstIDR)
	reportDur("first byte→first IDR", initToIDR)
}

// hasIDRNAL reports whether the Annex-B buffer holds a type 5 (IDR) NAL unit.
func hasIDRNAL(data []byte) bool {
	for i := 0; i+3 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if data[i+3]&0x1f == 5 {
				return true
			}
			i += 2
		}
	}
	return false
}

// isIDRFragment reports whether the fragment's first sample is a sync sample.
// isKeyframeFragment answers "no non-sync flag set", which also matches a
// fragment whose trun cannot be parsed; the bench wants a positive trun
// sample-flags check so a parse gap cannot be mistaken for an IDR.
func isIDRFragment(data []byte) bool {
	moof := findBoxDeep(data, "moof")
	if moof < 0 {
		return false
	}
	traf := findBoxDeep(data[moof:], "traf")
	if traf < 0 {
		return false
	}
	traf += moof
	trun := findBoxDeep(data[traf:], "trun")
	if trun < 0 {
		return false
	}
	trun += traf
	if trun+24 > len(data) {
		return false
	}
	flags := binary.BigEndian.Uint32(data[trun+8 : trun+12])
	const (
		trunDataOffsetPresent   = 0x000001
		trunFirstSampleFlagsReq = 0x000004
	)
	if flags&trunDataOffsetPresent == 0 || flags&trunFirstSampleFlagsReq == 0 {
		return false
	}
	// version/flags, sample_count, data_offset, then first_sample_flags.
	sampleFlags := binary.BigEndian.Uint32(data[trun+20 : trun+24])
	const sampleIsNonSync = 0x00010000
	return sampleFlags&sampleIsNonSync == 0
}
