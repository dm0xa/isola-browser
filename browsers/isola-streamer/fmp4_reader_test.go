package main

import (
	"bytes"
	"io"
	"testing"
)

func TestFMP4ReaderInitAndFragments(t *testing.T) {
	r := newFMP4Reader()

	ftyp := makeBox("ftyp", 20)
	moov := makeBox("moov", 100)
	init := append(ftyp, moov...)
	moof1 := makeBox("moof", 50)
	mdat1 := makeBox("mdat", 200)
	frag1 := append(moof1, mdat1...)

	raw := append(init, frag1...)

	// Split init across ftyp / moov reads (the bug we saw live).
	segs := r.Feed(ftyp)
	if len(segs) != 0 {
		t.Fatalf("expected no segments after partial ftyp, got %d", len(segs))
	}
	segs = r.Feed(moov)
	if len(segs) != 1 || len(segs[0]) != len(init) {
		t.Fatalf("expected one init segment len=%d, got %d segs", len(init), len(segs))
	}

	segs = r.Feed(frag1[:100])
	if len(segs) != 0 {
		t.Fatalf("expected no segments from partial fragment")
	}
	segs = r.Feed(frag1[100:])
	if len(segs) != 1 || len(segs[0]) != len(frag1) {
		t.Fatalf("expected one media fragment len=%d, got %d", len(frag1), len(segs))
	}

	// Arbitrary 64 KiB splits like pipe reads.
	r2 := newFMP4Reader()
	var all [][]byte
	chunk := 65536
	for i := 0; i < len(raw); i += chunk {
		end := i + chunk
		if end > len(raw) {
			end = len(raw)
		}
		all = append(all, r2.Feed(raw[i:end])...)
	}
	if len(all) != 2 {
		t.Fatalf("expected init + 1 fragment, got %d segments", len(all))
	}
	if !bytes.Equal(all[0], init) {
		t.Fatal("init segment mismatch")
	}
	if !bytes.Equal(all[1], frag1) {
		t.Fatal("media fragment mismatch")
	}
}

func makeBox(typ string, contentLen int) []byte {
	size := 8 + contentLen
	b := make([]byte, size)
	b[4], b[5], b[6], b[7] = typ[0], typ[1], typ[2], typ[3]
	b[0] = byte(size >> 24)
	b[1] = byte(size >> 16)
	b[2] = byte(size >> 8)
	b[3] = byte(size)
	return b
}

// TestFMP4ReaderReadSegmentsMatchesFeed pins that reading ffmpeg's stdout
// straight into the reader's buffer produces exactly the segments the
// append-a-chunk Feed path produces, including the init segment split across
// reads.
func TestFMP4ReaderReadSegmentsMatchesFeed(t *testing.T) {
	init := append(makeBox("ftyp", 20), makeBox("moov", 100)...)
	var raw []byte
	raw = append(raw, init...)
	const fragments = 6
	for i := 0; i < fragments; i++ {
		raw = append(raw, makeBox("moof", 50)...)
		raw = append(raw, makeBox("mdat", 300+i*10)...)
	}

	ref := newFMP4Reader()
	want := ref.Feed(raw)
	if len(want) != fragments+1 {
		t.Fatalf("Feed produced %d segments, want %d", len(want), fragments+1)
	}

	r := newFMP4Reader()
	src := bytes.NewReader(raw)
	var got [][]byte
	for {
		segs, err := r.readSegments(src)
		got = append(got, segs...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("readSegments produced %d segments, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("segment %d differs: %d vs %d bytes", i, len(got[i]), len(want[i]))
		}
	}
}

// TestFMP4ReaderBufferReuse pins that the reader stops allocating once its
// buffer is at working size: a fragmenter must not spend one buffer per
// fragment on a stream that runs for hours.
func TestFMP4ReaderBufferReuse(t *testing.T) {
	frag := append(makeBox("moof", 50), makeBox("mdat", 24*1024)...)
	init := append(makeBox("ftyp", 20), makeBox("moov", 100)...)

	r := newFMP4Reader()
	r.Feed(init)
	for range 8 {
		if segs := r.Feed(frag); len(segs) != 1 {
			t.Fatalf("emitted %d segments, want 1", len(segs))
		}
	}
	grown := cap(r.q.buf)

	allocs := testing.AllocsPerRun(100, func() {
		if segs := r.Feed(frag); len(segs) != 1 {
			t.Fatalf("emitted %d segments, want 1", len(segs))
		}
	})
	if allocs > 1 {
		t.Fatalf("steady-state Feed allocated %v times per call, want <= 1 (the result slice)", allocs)
	}
	if cap(r.q.buf) != grown {
		t.Fatalf("internal buffer grew from %d to %d on a steady stream", grown, cap(r.q.buf))
	}
	if r.q.off > len(frag)+len(init) {
		t.Fatalf("consumed prefix reached %d: buffer is not being reclaimed", r.q.off)
	}
}

// fmp4TestInit builds the init segment ffmpeg emits first: ftyp then moov.
func fmp4TestInit() []byte {
	return append(makeBox("ftyp", 20), makeBox("moov", 100)...)
}

// fmp4TestFrag builds one media segment: moof then mdat.
func fmp4TestFrag(mdatLen int) []byte {
	return append(makeBox("moof", 50), makeBox("mdat", mdatLen)...)
}

// TestFMP4ReaderRejectsAbsurdBoxSize is the unbounded-allocation case: a box
// header may declare up to 4 GiB, and the reader used to wait for every byte of
// it, growing its accumulation buffer without limit while a corrupt or hostile
// producer kept sending. A size that cannot belong to a real fragment must be
// treated as corruption and dropped, not buffered.
func TestFMP4ReaderRejectsAbsurdBoxSize(t *testing.T) {
	ftyp := make([]byte, 8)
	ftyp[0], ftyp[1], ftyp[2], ftyp[3] = 0xFF, 0xFF, 0xFF, 0xF0
	copy(ftyp[4:8], "ftyp")

	r := newFMP4Reader()
	if segs := r.Feed(ftyp); len(segs) != 0 {
		t.Fatalf("emitted %d segments from an 8-byte header", len(segs))
	}
	junk := bytes.Repeat([]byte{0xAA}, 2<<20)
	if segs := r.Feed(junk); len(segs) != 0 {
		t.Fatalf("emitted %d segments from junk", len(segs))
	}
	if got := len(r.q.pending()); got > 64<<10 {
		t.Fatalf("buffered %d bytes behind a 4 GiB box declaration; a corrupt size must be dropped", got)
	}
	if got := cap(r.q.buf); got > 4<<20 {
		t.Fatalf("buffer grew to %d bytes behind a corrupt box", got)
	}
}

// TestFMP4ReaderResyncsAfterMissingMdat pins that a moof which is not followed
// by its mdat does not wedge the reader: the fragment is unrecoverable, but the
// next one still has to reach viewers.
func TestFMP4ReaderResyncsAfterMissingMdat(t *testing.T) {
	r := newFMP4Reader()
	if segs := r.Feed(fmp4TestInit()); len(segs) != 1 {
		t.Fatalf("init: emitted %d segments", len(segs))
	}
	frag := fmp4TestFrag(300)
	bad := append(makeBox("moof", 50), makeBox("free", 100)...)
	if segs := r.Feed(append(bad, frag...)); len(segs) != 1 || !bytes.Equal(segs[0], frag) {
		t.Fatalf("after a moof without mdat: emitted %d segments, want the following fragment", len(segs))
	}
}

// TestFMP4ReaderResyncsAfterZeroSizeBox pins the ISO-BMFF "size 0" form, which
// means "extends to end of file" and cannot be honoured on a live stream. It
// used to make the reader wait forever while buffering everything after it.
func TestFMP4ReaderResyncsAfterZeroSizeBox(t *testing.T) {
	r := newFMP4Reader()
	if segs := r.Feed(fmp4TestInit()); len(segs) != 1 {
		t.Fatalf("init: emitted %d segments", len(segs))
	}
	zero := make([]byte, 8)
	copy(zero[4:8], "free") // size32 == 0
	frag := fmp4TestFrag(120)
	if segs := r.Feed(append(zero, frag...)); len(segs) != 1 || !bytes.Equal(segs[0], frag) {
		t.Fatalf("after a size-0 box: emitted %d segments, want the following fragment", len(segs))
	}
}

// TestFMP4ReaderResyncsAfterBadMoov pins that an ftyp without a usable moov is
// dropped and the reader waits for the next init pair instead of blocking on
// the first one forever.
func TestFMP4ReaderResyncsAfterBadMoov(t *testing.T) {
	r := newFMP4Reader()
	bad := append(makeBox("ftyp", 20), makeBox("free", 40)...)
	if segs := r.Feed(bad); len(segs) != 0 {
		t.Fatalf("emitted %d segments from ftyp+free", len(segs))
	}
	good := fmp4TestInit()
	if segs := r.Feed(good); len(segs) != 1 || !bytes.Equal(segs[0], good) {
		t.Fatalf("after a bad init: emitted %d segments, want the next ftyp+moov", len(segs))
	}
}

// TestFMP4ReaderCorpusByteEquality extends the Feed-vs-readSegments byte
// equality check to a larger and messier corpus: real-sized keyframe fragments
// (3 MiB, the measured size of a 1080p 2 s fragment), a garbage prefix that
// forces a resync, and arbitrary read boundaries. Both paths must produce
// exactly the same bytes in the same order.
func TestFMP4ReaderCorpusByteEquality(t *testing.T) {
	var raw []byte
	raw = append(raw, makeBox("free", 16)...) // junk before the init segment
	raw = append(raw, fmp4TestInit()...)
	for i := range 5 {
		raw = append(raw, fmp4TestFrag(3<<20)...)
		raw = append(raw, fmp4TestFrag(200+i*37)...)
	}
	raw = append(raw, makeBox("free", 8)...) // trailing junk is never a segment

	// Segments alias the reader's buffer and only live until the next
	// Feed/readSegments call (the pump packs each one immediately), so copy
	// them as they are produced.
	var want [][]byte
	for _, seg := range newFMP4Reader().Feed(raw) {
		want = append(want, append([]byte(nil), seg...))
	}
	if len(want) != 1+10 {
		t.Fatalf("Feed produced %d segments, want 11", len(want))
	}

	for _, chunk := range []int{1, 3, 64, 4096, 64 << 10, len(raw)} {
		r := newFMP4Reader()
		src := bytes.NewReader(raw)
		var got [][]byte
		for {
			segs, err := r.readSegments(src)
			for _, seg := range segs {
				got = append(got, append([]byte(nil), seg...))
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("chunk %d: %v", chunk, err)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("chunk %d: %d segments, want %d", chunk, len(got), len(want))
		}
		for i := range want {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("chunk %d: segment %d differs (%d vs %d bytes)", chunk, i, len(got[i]), len(want[i]))
			}
		}
	}

	// The resync over the garbage prefix must not have eaten real bytes.
	if !bytes.Equal(want[0], fmp4TestInit()) {
		t.Fatal("init segment differs from the fed ftyp+moov")
	}
}

// stallReader returns zero bytes with no error, which io.Reader allows and a
// pipe never does. Returning (0, nil) to the pump used to spin the output loop
// forever without progress.
type stallReader struct{ reads int }

func (s *stallReader) Read([]byte) (int, error) {
	s.reads++
	return 0, nil
}

func TestReadSegmentsZeroLengthReads(t *testing.T) {
	r := newFMP4Reader()
	src := &stallReader{}
	for range 1000 {
		if _, err := r.readSegments(src); err != nil {
			if err != io.ErrNoProgress {
				t.Fatalf("readSegments: %v, want io.ErrNoProgress", err)
			}
			return
		}
	}
	t.Fatalf("readSegments returned no error for 1000 zero-byte reads (spins the pump)")
}

func TestAUSplitterZeroLengthReads(t *testing.T) {
	s := &auSplitter{}
	src := &stallReader{}
	for range 1000 {
		if _, err := s.readFrom(src); err != nil {
			if err != io.ErrNoProgress {
				t.Fatalf("readFrom: %v, want io.ErrNoProgress", err)
			}
			return
		}
	}
	t.Fatalf("readFrom returned no error for 1000 zero-byte reads (spins the pump)")
}

// checkFMP4Segment asserts that an emitted segment is exactly one complete init
// pair or one complete moof+mdat fragment: no truncated box may reach a viewer.
func checkFMP4Segment(t *testing.T, seg []byte, wantInit bool) {
	t.Helper()
	wantTypes := []string{"moof", "mdat"}
	if wantInit {
		wantTypes = []string{"ftyp", "moov"}
	}
	off := 0
	for i, typ := range wantTypes {
		size, ok := boxSize(seg, off)
		if !ok || off+size > len(seg) {
			t.Fatalf("segment box %d (%s) at %d: size %d by %d bytes", i, typ, off, size, len(seg))
		}
		if got := string(seg[off+4 : off+8]); got != typ {
			t.Fatalf("segment box %d type %q, want %q", i, got, typ)
		}
		off += size
	}
	if off != len(seg) {
		t.Fatalf("segment has %d trailing bytes after its boxes", len(seg)-off)
	}
}

// FuzzFMP4Reader feeds arbitrary bytes to the fragment reassembler. Invariants:
// never panic, never emit a truncated or fabricated segment, emit the init pair
// before any media, keep segments in stream order, and keep buffering bounded.
func FuzzFMP4Reader(f *testing.F) {
	f.Add([]byte{})
	f.Add(fmp4TestInit())
	f.Add(append(fmp4TestInit(), fmp4TestFrag(300)...))
	f.Add(append(fmp4TestInit(), fmp4TestFrag(0)...))
	f.Add(append(fmp4TestInit(), append(makeBox("moof", 50), makeBox("free", 40)...)...))
	f.Add(append([]byte{1, 2, 3}, fmp4TestInit()...))
	f.Add(makeBox("ftyp", 20))
	f.Add(append(fmp4TestInit(), append(make([]byte, 8), fmp4TestFrag(64)...)...))
	// Hostile sizes: a 4 GiB ftyp, and a moof that is not followed by mdat.
	huge := make([]byte, 8)
	huge[0], huge[1], huge[2], huge[3] = 0xFF, 0xFF, 0xFF, 0xF0
	copy(huge[4:8], "ftyp")
	f.Add(huge)
	f.Add(append(fmp4TestInit(), append(makeBox("moof", 50), makeBox("free", 40)...)...))

	f.Fuzz(func(t *testing.T, data []byte) {
		r := newFMP4Reader()
		cursor := 0
		sawInit := false
		segments := 0
		for off := 0; off < len(data); {
			n := 1 + int(data[off])%63
			if len(data) > 4096 {
				n = fmp4ReadChunk
			}
			if off+n > len(data) {
				n = len(data) - off
			}
			for _, seg := range r.Feed(data[off : off+n]) {
				if len(seg) == 0 {
					t.Fatal("emitted an empty segment")
				}
				checkFMP4Segment(t, seg, !sawInit)
				sawInit = true
				segments++
				idx := bytes.Index(data[cursor:], seg)
				if idx < 0 {
					t.Fatalf("segment %d (%d bytes) does not occur in the fed stream after offset %d", segments, len(seg), cursor)
				}
				cursor += idx + len(seg)
			}
			off += n
		}
		if got := len(r.q.pending()); got > 2*maxFMP4Box+fmp4ReadChunk {
			t.Fatalf("buffered %d bytes; a corrupt stream must not grow the buffer without bound", got)
		}
		if got := cap(r.q.buf); got > 4*maxFMP4Box+4*fmp4ReadChunk {
			t.Fatalf("internal buffer grew to %d bytes", got)
		}
	})
}
