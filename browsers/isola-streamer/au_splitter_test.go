package main

import (
	"bytes"
	"testing"
)

// testAU builds one Annex-B picture: SPS, PPS, SEI, then two slice NALs of the
// same picture (the sliced shape x264 zerolatency produces).
func testAU(idr bool, payload int) []byte {
	sc := []byte{0, 0, 0, 1}
	nalType := byte(0x41) // non-IDR slice
	if idr {
		nalType = 0x65
	}
	var au []byte
	au = append(au, sc...)
	au = append(au, 0x67)
	au = append(au, bytes.Repeat([]byte{0x11}, 16)...)
	au = append(au, sc...)
	au = append(au, 0x68)
	au = append(au, bytes.Repeat([]byte{0x22}, 8)...)
	au = append(au, sc...)
	au = append(au, 0x06)
	au = append(au, bytes.Repeat([]byte{0x33}, 4)...)
	// First slice of the picture: first_mb_in_slice == 0 (high bit set).
	au = append(au, sc...)
	au = append(au, nalType)
	au = append(au, 0x88)
	au = append(au, bytes.Repeat([]byte{0x44}, payload)...)
	// Second slice of the same picture: high bit clear, so this is not a
	// boundary.
	au = append(au, sc...)
	au = append(au, nalType)
	au = append(au, 0x04)
	au = append(au, bytes.Repeat([]byte{0x55}, payload/2)...)
	return au
}

// emittedAU is one AU copied out of the splitter, because an emitted AU only
// lives until the next feed call — exactly like every real caller, which packs
// it into a viewer packet immediately.
type emittedAU struct {
	data     []byte
	keyframe bool
}

func drainAUs(dst []emittedAU, aus []auPacket) []emittedAU {
	for _, au := range aus {
		dst = append(dst, emittedAU{data: append([]byte(nil), au.data...), keyframe: au.keyframe})
	}
	return dst
}

// TestAUSplitterBoundariesAndKeyframes pins the boundary rule the wire format
// depends on: an AU ends where the next picture's first VCL NAL begins, so the
// parameter sets ffmpeg emits between two pictures stay attached to the
// picture that follows them.
func TestAUSplitterBoundariesAndKeyframes(t *testing.T) {
	aus := [][]byte{testAU(true, 100), testAU(false, 60), testAU(false, 80)}
	var stream []byte
	for _, au := range aus {
		stream = append(stream, au...)
	}

	s := &auSplitter{}
	got := drainAUs(nil, s.feed(stream))
	// The last picture has no successor yet, so it stays buffered.
	if len(got) != len(aus)-1 {
		t.Fatalf("emitted %d AUs before flush, want %d", len(got), len(aus)-1)
	}
	got = drainAUs(got, s.feed(testAU(false, 40)))
	if len(got) != len(aus) {
		t.Fatalf("emitted %d AUs after flush, want %d", len(got), len(aus))
	}
	if !got[0].keyframe {
		t.Fatal("AU carrying the IDR was not flagged as a keyframe")
	}
	if got[1].keyframe {
		t.Fatal("AU carrying only a P slice was flagged as a keyframe")
	}

	// The emitted AUs are a prefix of the input: nothing dropped, nothing
	// duplicated.
	var joined []byte
	for _, au := range got {
		joined = append(joined, au.data...)
	}
	if !bytes.HasPrefix(joined, stream) {
		t.Fatal("emitted AUs do not reproduce the fed stream in order")
	}
	// Everything up to the trailing picture's first slice is emitted; that
	// picture's leading parameter sets ride with the AU before it.
	if want := len(stream) + 43; len(joined) != want {
		t.Fatalf("emitted %d bytes, want %d (everything but the trailing picture)", len(joined), want)
	}
	// Each emitted AU starts with a start code and carries its own slices.
	for i, au := range got {
		if !bytes.HasPrefix(au.data, []byte{0, 0, 0, 1}) {
			t.Fatalf("AU %d does not start with a start code", i)
		}
	}
}

// TestAUSplitterArbitraryChunking pins that the splitter is independent of how
// ffmpeg's stdout is chunked, including a read that ends exactly on a start
// code.
func TestAUSplitterArbitraryChunking(t *testing.T) {
	var stream []byte
	const pictures = 12
	for i := 0; i < pictures; i++ {
		stream = append(stream, testAU(i%4 == 0, 40+i)...)
	}
	// Trailing picture so the last one completes.
	stream = append(stream, testAU(false, 30)...)

	var want []emittedAU
	{
		s := &auSplitter{}
		want = drainAUs(nil, s.feed(stream))
	}
	if len(want) != pictures {
		t.Fatalf("whole feed emitted %d AUs, want %d", len(want), pictures)
	}
	if !want[0].keyframe {
		t.Fatal("first AU not flagged as keyframe")
	}

	for _, chunk := range []int{1, 3, 7, 64, 4096, len(stream)} {
		s := &auSplitter{}
		var got []emittedAU
		for off := 0; off < len(stream); off += chunk {
			got = drainAUs(got, s.feed(stream[off:min(off+chunk, len(stream))]))
		}
		if len(got) != len(want) {
			t.Fatalf("chunk %d: emitted %d AUs, want %d", chunk, len(got), len(want))
		}
		for i := range want {
			if got[i].keyframe != want[i].keyframe {
				t.Fatalf("chunk %d: AU %d keyframe=%v, want %v", chunk, i, got[i].keyframe, want[i].keyframe)
			}
			if !bytes.Equal(got[i].data, want[i].data) {
				t.Fatalf("chunk %d: AU %d payload mismatch (%d vs %d bytes)", chunk, i, len(got[i].data), len(want[i].data))
			}
		}
	}
}

// TestAUSplitterBufferReuse pins the property that made the splitter cheap: the
// accumulation buffer is reclaimed in place, not reallocated or walked forward
// through fresh backing arrays, and steady-state splitting copies no AU.
func TestAUSplitterBufferReuse(t *testing.T) {
	au := testAU(false, 4096)

	s := &auSplitter{}
	for range 8 {
		s.feed(au)
	}
	capAfterWarmup := cap(s.q.buf)

	allocs := testing.AllocsPerRun(100, func() {
		s.feed(au)
	})
	// One allocation is the returned []auPacket growing from nil. The point
	// is that no per-AU payload copy happens.
	if allocs > 1 {
		t.Fatalf("steady-state feed allocated %v times per call, want <= 1 (the result slice)", allocs)
	}
	if cap(s.q.buf) != capAfterWarmup {
		t.Fatalf("internal buffer grew from %d to %d during steady state", capAfterWarmup, cap(s.q.buf))
	}
	if s.q.off > len(au) {
		t.Fatalf("consumed prefix reached %d with a %d-byte AU: buffer is not being reclaimed", s.q.off, len(au))
	}
}

// TestAUSplitterLargeAUGrowth checks that a keyframe larger than the current
// buffer grows it once, keeps the AU intact across reads, and still flags it.
func TestAUSplitterLargeAUGrowth(t *testing.T) {
	s := &auSplitter{}
	big := testAU(true, 400*1024)
	sentinel := testAU(false, 10)

	var got []emittedAU
	for off := 0; off < len(big); off += 64 * 1024 {
		got = drainAUs(got, s.feed(big[off:min(off+64*1024, len(big))]))
	}
	if len(got) != 0 {
		t.Fatalf("emitted %d AUs before the boundary arrived, want 0", len(got))
	}
	got = drainAUs(got, s.feed(sentinel))
	if len(got) != 1 {
		t.Fatalf("emitted %d AUs, want 1", len(got))
	}
	if !got[0].keyframe {
		t.Fatal("large AU carrying an IDR was not flagged as a keyframe")
	}
	// The AU runs to the next picture's first slice, so it carries the
	// sentinel's parameter sets too.
	if !bytes.HasPrefix(got[0].data, big) {
		t.Fatal("large AU lost bytes across reads")
	}
	if len(got[0].data) != len(big)+43 {
		t.Fatalf("AU length %d, want %d", len(got[0].data), len(big)+43)
	}
}

// unterminatedStream builds n bytes that can never yield a boundary: one NAL
// whose first_mb_in_slice is not zero (so it is not a first slice) followed by
// payload with no further start code.
func unterminatedStream(n int) []byte {
	buf := make([]byte, max(n, 6))
	copy(buf, []byte{0, 0, 0, 1, 0x41, 0x04})
	return buf[:n]
}

// feedUnterminated pushes n bytes of unterminated stream through the splitter
// in pipe-sized chunks and returns everything it emitted.
func feedUnterminated(s *auSplitter, n, chunk int) []emittedAU {
	var got []emittedAU
	data := unterminatedStream(n)
	for off := 0; off < n; off += chunk {
		got = drainAUs(got, s.feed(data[off:min(off+chunk, n)]))
	}
	return got
}

// TestAUSplitterBoundsUnterminatedStream is the unbounded-allocation case: a
// producer (or a corrupt muxer) that never emits a second picture's first
// slice used to make the splitter buffer the whole stream forever. There is no
// boundary to emit, so the buffered bytes must be dropped once the stream stops
// looking like encoded video.
func TestAUSplitterBoundsUnterminatedStream(t *testing.T) {
	s := &auSplitter{}
	got := feedUnterminated(s, maxAUStreamBytes+1<<20, 64<<10)
	if len(got) != 0 {
		t.Fatalf("emitted %d AUs from a stream with no NAL start code", len(got))
	}
	if pending := len(s.q.pending()); pending > maxAUStreamBytes {
		t.Fatalf("buffered %d bytes of unterminated stream; want at most %d", pending, maxAUStreamBytes)
	}
}

// TestAUSplitterRecoversAfterOversizedStream pins that dropping the
// unterminated prefix does not desynchronise the splitter: the next real
// picture still comes out whole, without the discarded bytes glued to it.
func TestAUSplitterRecoversAfterOversizedStream(t *testing.T) {
	s := &auSplitter{}
	feedUnterminated(s, maxAUStreamBytes+1<<20, 64<<10)

	first, second := testAU(true, 100), testAU(false, 60)
	got := drainAUs(nil, s.feed(append(first, second...)))
	if len(got) != 1 {
		t.Fatalf("emitted %d AUs after recovery, want 1", len(got))
	}
	if !got[0].keyframe {
		t.Fatal("recovered IDR AU not flagged as a keyframe")
	}
	if !bytes.HasPrefix(got[0].data, first) {
		t.Fatalf("recovered AU is %d bytes and does not start with the fed picture (%d bytes)", len(got[0].data), len(first))
	}
	if len(got[0].data) > len(first)+64 {
		t.Fatalf("recovered AU carries %d extra bytes from the dropped stream", len(got[0].data)-len(first))
	}
}

// TestAUSplitterNeverEmitsTruncatedAU pins the completeness rule from both
// directions: a picture whose successor has only begun (no payload byte after
// its NAL header) stays buffered, and the emitted AU ends exactly at the next
// picture's start code.
func TestAUSplitterNeverEmitsTruncatedAU(t *testing.T) {
	first := testAU(false, 40)
	second := testAU(true, 40)

	// Split the second picture inside its first slice's NAL header: the export
	// boundary needs the byte after the header to read first_mb_in_slice.
	sliceAt := bytes.Index(second, []byte{0, 0, 0, 1, 0x65})
	if sliceAt <= 4 {
		t.Fatalf("test AU has no IDR slice to cut at")
	}
	cut := sliceAt + 4 + 1
	// The AU before that boundary carries the following picture's parameter
	// sets (the wire rule the splitter is built around), so that is the exact
	// expected payload.
	want := append(append([]byte(nil), first...), second[:sliceAt]...)

	s := &auSplitter{}
	if got := drainAUs(nil, s.feed(append(append([]byte(nil), first...), second[:cut]...))); len(got) != 0 {
		t.Fatalf("emitted %d AUs although the boundary's slice header was incomplete", len(got))
	}
	got := drainAUs(nil, s.feed(second[cut:]))
	if len(got) != 1 {
		t.Fatalf("emitted %d AUs after completing the header, want 1", len(got))
	}
	if !bytes.Equal(got[0].data, want) {
		t.Fatalf("emitted AU is %d bytes, want exactly the first picture plus its trailing parameter sets (%d bytes)", len(got[0].data), len(want))
	}
	if got[0].keyframe {
		t.Fatal("non-IDR picture flagged as a keyframe")
	}

	// A stream that ends mid-picture never produces that picture.
	s = &auSplitter{}
	if got := drainAUs(nil, s.feed(append(first, second[:len(second)/2]...))); len(got) != 1 {
		t.Fatalf("emitted %d AUs; a truncated trailing picture must stay buffered", len(got))
	}
}

// auHasIDR reports whether an Annex-B access unit carries a type-5 NAL.
func auHasIDR(au []byte) bool {
	for off := 0; ; {
		_, nal, ok := nextNAL(au, off)
		if !ok || nal >= len(au) {
			return false
		}
		if au[nal]&0x1F == 5 {
			return true
		}
		off = nal
	}
}

// FuzzAUSplitter feeds arbitrary bytes to the Annex-B reassembler. Invariants:
// never panic, never emit an empty or mid-NAL AU, keep the emitted stream a
// byte-exact prefix of the fed stream when nothing was dropped, and keep the
// buffered tail bounded.
func FuzzAUSplitter(f *testing.F) {
	f.Add([]byte{})
	f.Add(testAU(true, 40))
	f.Add(append(testAU(true, 40), testAU(false, 30)...))
	f.Add(append(testAU(false, 10), testAU(false, 10)...))
	f.Add([]byte{0, 0, 1})
	f.Add([]byte{0, 0, 0, 1, 0x65, 0x88})
	f.Add([]byte{0, 0, 1, 0x67, 0x00, 0, 0, 1, 0x65, 0x88, 0, 0, 1, 0x41, 0x80})
	// Unterminated NAL (no boundary), and a start code split across reads.
	f.Add(unterminatedStream(4096))
	f.Add([]byte{0, 0, 0, 1, 0x41, 0x04, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		s := &auSplitter{}
		cursor := 0
		for off := 0; off < len(data); {
			n := 1 + int(data[off])%63
			if len(data) > 4096 {
				n = 64 << 10
			}
			if off+n > len(data) {
				n = len(data) - off
			}
			for _, au := range s.feed(data[off : off+n]) {
				if len(au.data) == 0 {
					t.Fatal("emitted an empty access unit")
				}
				if len(au.data) >= 4 && au.data[0] == 0 && au.data[1] == 0 && au.data[2] == 0 && au.data[3] == 1 {
					// 4-byte start code
				} else if len(au.data) >= 3 && au.data[0] == 0 && au.data[1] == 0 && au.data[2] == 1 {
					// 3-byte start code
				} else {
					t.Fatalf("access unit does not start with a start code: %x", au.data[:min(len(au.data), 8)])
				}
				if au.keyframe != auHasIDR(au.data) {
					t.Fatalf("keyframe=%v but IDR NAL present=%v", au.keyframe, auHasIDR(au.data))
				}
				// Every emitted byte must be a slice of the fed stream, at or
				// after the previous AU's end: never invented, never reordered.
				idx := bytes.Index(data[cursor:], au.data)
				if idx < 0 {
					t.Fatalf("AU (%d bytes) does not occur in the fed stream at or after offset %d", len(au.data), cursor)
				}
				cursor += idx + len(au.data)
			}
			off += n
		}
		if pending := len(s.q.pending()); pending > maxAUStreamBytes+(64<<10) {
			t.Fatalf("buffered %d bytes of unterminated stream", pending)
		}
	})
}
