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
