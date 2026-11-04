package main

// Frame-path benches: what one composed frame costs on its way from the
// broadcaster's surface to the viewer socket.
//
// The path has three stages and each is measured on its own so an optimization
// can be attributed to one of them:
//
//	pumpFrames  Snapshot (surface copy) → watermark → write to ffmpeg stdin
//	pumpOutput  read ffmpeg stdout → reassemble → pack header+payload
//	deliver     lock the hub, push the packed packet to each viewer mailbox
//
//	go test -run '^$' -bench 'BenchmarkPumpFrame|BenchmarkAUPhys|BenchmarkDeliver|BenchmarkPumpOutput' -benchmem
//
// The pipe stage uses a real os.Pipe with a draining reader, because the
// kernel copy a real ffmpeg stdin pays is half of what the pump does with a
// snapshot buffer.
//
// Measured on Apple M1 Pro / darwin-arm64 / go1.27.3, median of 3 at
// -benchtime=500ms. "before" is HEAD 810c8e8, "after" is the read-in-place
// reassemblers:
//
//	                                     before                    after
//	720p/snapshot                 61,080 ns  60 GB/s       58,311 ns  63 GB/s
//	720p/snapshot+pipe         1,073,248 ns  3.4 GB/s     997,323 ns  3.7 GB/s
//	1080p/snapshot               173,603 ns  48 GB/s      155,414 ns  53 GB/s
//	1080p/snapshot+pipe        2,527,434 ns  3.3 GB/s   2,478,749 ns  3.3 GB/s
//	fmp4/read+pack               347,555 ns  4.78 MB/op  107,144 ns  1.47 MB/op
//	                              147 allocs                 96 allocs
//	annexb/read+split+pack     1,237,672 ns  1.99 MB/op  738,754 ns  1.00 MB/op
//	                              138 allocs                 90 allocs
//	PumpOutput                   344,007 ns  4.91 MB/op  136,260 ns  1.48 MB/op
//	                              146 allocs                 74 allocs
//	deliver/1viewer                   38.7 ns                    35.3 ns
//	deliver/4viewers                  48.4 ns                    47.7 ns
//
// What the split means:
//
//   - The feed side is the kernel pipe copy: 95% of one fed frame at 720p is
//     the write(2) into ffmpeg's stdin (1.13 ms of 3.69 MB), and the Snapshot
//     copy is 5%. The pump already overlaps them — one spare buffer is filled
//     while the other is being written — so neither is on the critical path at
//     a 40 ms tick. No change was justified there.
//   - The output side did pay for two extra copies per byte (append into the
//     reader, then extract out of it) and one extra allocation per packet. Both
//     reassemblers now read ffmpeg's stdout straight into their own buffer and
//     hand out slices of it, so the only copy left is the packet itself.
//   - deliver() has never been per-viewer under lock: one mutex acquisition and
//     one channel send per viewer, ~37 ns with one viewer and ~49 ns with four.
//     The 1 Hz metrics JSON is built on its own goroutine in ws.go.
import (
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

// benchSurface builds a broadcaster whose composed surface is one full
// w×h BGRA image.
func benchSurface(b *testing.B, w, h int) *frameBroadcaster {
	b.Helper()
	incoming := make(chan *Frame, 1)
	bc := newFrameBroadcaster(incoming)
	incoming <- &Frame{
		Width:   uint32(w),
		Height:  uint32(h),
		Stride:  uint32(w * 4),
		Dirty:   []Rect{{X: 0, Y: 0, W: uint32(w), H: uint32(h)}},
		Payload: benchFrame(w, h, 0),
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if stride, height, ok := bc.SurfaceDims(); ok && int(stride) == w*4 && int(height) == h {
			break
		}
		if time.Now().After(deadline) {
			b.Fatalf("surface %dx%d never composed", w, h)
		}
		time.Sleep(time.Millisecond)
	}
	return bc
}

// BenchmarkPumpFrame measures one fed frame exactly as pumpFrames costs it:
// copy the composed surface into a spare buffer, then push that buffer down
// ffmpeg's stdin pipe. snapshot-only isolates the copy from the syscall.
func BenchmarkPumpFrame(b *testing.B) {
	for _, res := range []struct {
		name string
		w, h int
	}{
		{"720p", 1280, 720},
		{"1080p", 1920, 1080},
	} {
		bc := benchSurface(b, res.w, res.h)
		size := res.w * res.h * 4

		b.Run(res.name+"/snapshot", func(b *testing.B) {
			buf := make([]byte, size)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for range b.N {
				if !bc.Snapshot(buf) {
					b.Fatal("snapshot failed")
				}
			}
		})

		b.Run(res.name+"/snapshot+pipe", func(b *testing.B) {
			pr, pw, err := os.Pipe()
			if err != nil {
				b.Fatal(err)
			}
			defer pr.Close()
			defer pw.Close()
			drained := make(chan struct{})
			go func() {
				_, _ = io.Copy(io.Discard, pr)
				close(drained)
			}()
			buf := make([]byte, size)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if !bc.Snapshot(buf) {
					b.Fatal("snapshot failed")
				}
				if _, werr := pw.Write(buf); werr != nil {
					b.Fatal(werr)
				}
			}
			b.StopTimer()
			pw.Close()
			<-drained
		})
	}
}

// benchFMP4Stream builds a byte stream shaped like ffmpeg's fragmented MP4
// output: one init segment, then moof+mdat fragments where every 50th is an
// IDR-sized keyframe. Payload bytes are irrelevant to the reader — it only
// parses box headers — so the zeros stay cheap to generate.
func benchFMP4Stream(frames, fragBytes, keyBytes, keyEvery int) []byte {
	out := make([]byte, 0, (frames+1)*(fragBytes+keyBytes))
	ftyp := makeBox("ftyp", 24)
	moov := makeBox("moov", 640)
	out = append(out, ftyp...)
	out = append(out, moov...)
	for i := range frames {
		payload := fragBytes
		if keyEvery > 0 && i%keyEvery == 0 {
			payload = keyBytes
		}
		out = append(out, makeBox("moof", 260)...)
		out = append(out, makeBox("mdat", payload)...)
	}
	return out
}

// benchAnnexBStream builds an Annex-B H.264 stream of access units, each one
// SPS+PPS+SEI followed by two slice NALs of the same picture (zerolatency
// slices a frame across threads). Every keyEvery-th picture is an IDR.
func benchAnnexBStream(aus, auBytes, keyBytes, keyEvery int) []byte {
	sc := []byte{0, 0, 0, 1}
	sp := func(buf []byte, n int) []byte {
		buf = append(buf, sc...)
		buf = append(buf, 0x67)
		for i := 0; i < 24; i++ {
			buf = append(buf, byte(i))
		}
		buf = append(buf, sc...)
		buf = append(buf, 0x68)
		for i := 0; i < 9; i++ {
			buf = append(buf, byte(i))
		}
		buf = append(buf, sc...)
		buf = append(buf, 0x06) // SEI
		for i := 0; i < 12; i++ {
			buf = append(buf, byte(i))
		}
		return buf
	}
	var out []byte
	for i := range aus {
		nalType := byte(0x41) // non-IDR slice
		payload := auBytes
		if keyEvery > 0 && i%keyEvery == 0 {
			nalType = 0x65 // IDR
			payload = keyBytes
		}
		out = sp(out, 0)
		// First slice of the picture: first_mb_in_slice == 0 → high bit set.
		out = append(out, sc...)
		out = append(out, nalType)
		for j := 0; j < payload; j++ {
			b := byte(j)
			if j == 0 {
				b = 0x88
			}
			out = append(out, b)
		}
		// Second slice of the same picture: high bit clear.
		out = append(out, sc...)
		out = append(out, nalType)
		for j := 0; j < payload/2; j++ {
			out = append(out, byte(j))
		}
	}
	return out
}

// chunkReader hands data to the reader in fixed-size pieces and then reports
// EOF, the shape of ffmpeg's stdout as the pumps see it.
type chunkReader struct {
	data  []byte
	chunk int
	off   int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:min(r.off+r.chunk, len(r.data))])
	r.off += n
	return n, nil
}

func (r *chunkReader) reset() { r.off = 0 }

// BenchmarkAUPhys measures the escort of encoded payload: ffmpeg stdout bytes
// → complete segments/access units → one packed viewer packet, with the same
// read-into-place path pumpOutput uses. The only remaining copy is the packet
// itself.
func BenchmarkAUPhys(b *testing.B) {
	fmp4 := benchFMP4Stream(48, 24*1024, 180*1024, 48)
	annexb := benchAnnexBStream(48, 12*1024, 60*1024, 48)

	b.Run("fmp4/read+pack", func(b *testing.B) {
		r := newFMP4Reader()
		src := &chunkReader{data: fmp4, chunk: 64 * 1024}
		// Warm up: consume the init segment and size the buffer.
		for {
			_, err := r.readSegments(src)
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
		src.reset()
		var seq uint32
		segs := 0
		b.SetBytes(int64(len(fmp4)))
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			for {
				batch, err := r.readSegments(src)
				for _, seg := range batch {
					isKey := isKeyframeFragment(seg)
					pkt := packVideoSegment(seg, seq, isKey)
					seq++
					if len(pkt) != len(seg)+videoSegmentHeaderSize {
						b.Fatalf("packed %d, want %d", len(pkt), len(seg)+videoSegmentHeaderSize)
					}
					segs++
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			src.reset()
		}
		b.StopTimer()
		b.ReportMetric(float64(segs)/float64(b.N), "segments/op")
	})

	b.Run("annexb/read+split+pack", func(b *testing.B) {
		s := &auSplitter{}
		src := &chunkReader{data: annexb, chunk: 64 * 1024}
		var seq uint32
		aus := 0
		b.SetBytes(int64(len(annexb)))
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			for {
				batch, err := s.readFrom(src)
				for _, au := range batch {
					pkt := packAccessUnit(au.data, seq, au.keyframe)
					seq++
					if len(pkt) != len(au.data)+videoSegmentHeaderSize {
						b.Fatalf("packed %d, want %d", len(pkt), len(au.data)+videoSegmentHeaderSize)
					}
					aus++
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			src.reset()
		}
		b.StopTimer()
		b.ReportMetric(float64(aus)/float64(b.N), "aus/op")
	})
}

// BenchmarkDeliver measures the fan-out of one packed packet: the hub mutex,
// the keyframe bookkeeping, and one channel send per viewer.
func BenchmarkDeliver(b *testing.B) {
	for _, viewers := range []int{1, 4} {
		b.Run(map[int]string{1: "1viewer", 4: "4viewers"}[viewers], func(b *testing.B) {
			hub := &streamHub{runs: make(map[runKey]*streamRun)}
			run := &streamRun{hub: hub, subs: make(map[*streamSub]struct{})}
			var stop sync.WaitGroup
			stop.Add(viewers)
			for range viewers {
				sub := newStreamSub()
				hub.mu.Lock()
				run.subs[sub] = struct{}{}
				hub.mu.Unlock()
				go func(s *streamSub) {
					defer stop.Done()
					for ev := range s.ch {
						_ = len(ev.binary)
					}
				}(sub)
			}
			pkt := make([]byte, videoSegmentHeaderSize+24*1024)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				run.deliver(pkt, false, false)
			}
			b.StopTimer()
			for sub := range run.subs {
				close(sub.ch)
			}
			stop.Wait()
		})
	}
}

// repeatReader replays an in-memory byte stream forever, so pumpOutput sees a
// continuous ffmpeg stdout.
type repeatReader struct {
	data []byte
	off  int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if r.off == len(r.data) {
			r.off = 0
		}
		c := copy(p[n:], r.data[r.off:])
		r.off += c
		n += c
	}
	return n, nil
}

// BenchmarkPumpOutput measures the whole read path without the pipe syscall:
// stdout → segments → packed packets → mailbox, with the test goroutine acting
// as the viewer's socket writer and counting what arrives.
func BenchmarkPumpOutput(b *testing.B) {
	const fragmentsPerPass = 48
	stream := benchFMP4Stream(fragmentsPerPass, 24*1024, 180*1024, fragmentsPerPass)
	hub := &streamHub{runs: make(map[runKey]*streamRun)}
	run := &streamRun{
		hub:  hub,
		key:  runKey{kbps: 2000, width: 1280, height: 720, fps: 25},
		stop: make(chan struct{}),
	}
	sub := newStreamSub()
	hub.mu.Lock()
	run.subs = map[*streamSub]struct{}{sub: struct{}{}}
	hub.mu.Unlock()

	run.stdout = &repeatReader{data: stream}
	var wg sync.WaitGroup
	wg.Add(1)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	b.ResetTimer()
	go run.pumpOutput(&wg)
	for range b.N {
		for range fragmentsPerPass {
			for {
				ev := <-sub.ch
				if ev.binary != nil {
					break
				}
			}
		}
	}
	b.StopTimer()
	close(run.stop)
	wg.Wait()
}
