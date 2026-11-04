package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"runtime"
	"testing"
	"time"
)

// encodeWireTestFrame builds one producer frame in the ISOL wire format
// (browsers/electron-runner/src/isol_protocol.js).
func encodeWireTestFrame(w, h, stride, format uint32, rects []Rect, payload []byte) []byte {
	buf := make([]byte, 32+16*len(rects)+len(payload))
	binary.LittleEndian.PutUint32(buf[0:4], frameMagic)
	binary.LittleEndian.PutUint32(buf[4:8], w)
	binary.LittleEndian.PutUint32(buf[8:12], h)
	binary.LittleEndian.PutUint32(buf[12:16], stride)
	binary.LittleEndian.PutUint32(buf[16:20], format)
	binary.LittleEndian.PutUint32(buf[20:24], uint32(len(rects)))
	binary.LittleEndian.PutUint64(buf[24:32], 1700000000000000000)
	for i, r := range rects {
		off := 32 + i*16
		binary.LittleEndian.PutUint32(buf[off:], r.X)
		binary.LittleEndian.PutUint32(buf[off+4:], r.Y)
		binary.LittleEndian.PutUint32(buf[off+8:], r.W)
		binary.LittleEndian.PutUint32(buf[off+12:], r.H)
	}
	copy(buf[32+16*len(rects):], payload)
	return buf
}

// wireConn adapts a byte slice to net.Conn so readFrame can be driven without a
// socket. Deadlines are no-ops: the fuzz target must not depend on wall time.
type wireConn struct{ *bytes.Reader }

func (c *wireConn) Close() error { return nil }
func (c *wireConn) Write(p []byte) (int, error) {
	return 0, io.ErrClosedPipe
}
func (c *wireConn) LocalAddr() net.Addr              { return wireAddr{} }
func (c *wireConn) RemoteAddr() net.Addr             { return wireAddr{} }
func (c *wireConn) SetDeadline(time.Time) error      { return nil }
func (c *wireConn) SetReadDeadline(time.Time) error  { return nil }
func (c *wireConn) SetWriteDeadline(time.Time) error { return nil }

type wireAddr struct{}

func (wireAddr) Network() string { return "wire" }
func (wireAddr) String() string  { return "wire" }

func readTestFrame(t *testing.T, raw []byte) (*Frame, error) {
	t.Helper()
	return readFrame(&wireConn{Reader: bytes.NewReader(raw)})
}

// TestReadFrameRoundTrip pins that a well-formed damage frame still decodes
// exactly as before: geometry, timestamp, rects and the tightly packed payload.
func TestReadFrameRoundTrip(t *testing.T) {
	payload := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	raw := encodeWireTestFrame(64, 32, 256, 0, []Rect{{X: 4, Y: 8, W: 2, H: 1}}, payload)

	f, err := readTestFrame(t, raw)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if f.Width != 64 || f.Height != 32 || f.Stride != 256 {
		t.Fatalf("geometry %dx%d stride=%d", f.Width, f.Height, f.Stride)
	}
	if len(f.Dirty) != 1 || f.Dirty[0] != (Rect{X: 4, Y: 8, W: 2, H: 1}) {
		t.Fatalf("dirty = %+v", f.Dirty)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Fatalf("payload = %v, want %v", f.Payload, payload)
	}
	if f.IsFull() {
		t.Fatal("a 2x1 damage rect reported as a full frame")
	}
}

// TestReadFrameRejectsWrappingRects is the hostile-header case: X+W and Y+H are
// uint32 additions, so 0xFFFFFFFE+2 wraps to 0 and used to pass the bounds
// check. The payload size derived from such a rect overflowed int64 and then
// panicked in make(). Every bound must be checked without arithmetic that can
// wrap.
func TestReadFrameRejectsWrappingRects(t *testing.T) {
	huge := ^uint32(0) - 1 // 0xFFFFFFFE
	cases := []struct {
		name string
		rect Rect
	}{
		{"x+w wraps", Rect{X: 2, Y: 0, W: huge, H: 1}},
		{"y+h wraps", Rect{X: 0, Y: 2, W: 1, H: huge}},
		{"both wrap", Rect{X: 2, Y: 2, W: huge, H: huge}},
		{"x beyond surface", Rect{X: 1 << 31, Y: 0, W: 2, H: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := encodeWireTestFrame(64, 64, 256, 0, []Rect{tc.rect}, nil)
			if f, err := readTestFrame(t, raw); err == nil {
				t.Fatalf("accepted hostile rect %+v as %dx%d with %d payload bytes",
					tc.rect, f.Width, f.Height, len(f.Payload))
			}
		})
	}
}

// TestReadFrameRejectsOverlappingDamage pins the cumulative payload bound: the
// rect list may not claim more pixels than the surface holds, or a header of a
// few hundred bytes could legitimately demand 4096 full surfaces of payload.
func TestReadFrameRejectsOverlappingDamage(t *testing.T) {
	w, h := uint32(64), uint32(64)
	rects := []Rect{{0, 0, w, h}, {0, 0, w, h}}
	payload := make([]byte, 2*int(w*h*4))
	raw := encodeWireTestFrame(w, h, w*4, 0, rects, payload)
	if _, err := readTestFrame(t, raw); err == nil {
		t.Fatal("accepted damage covering twice the surface")
	}
}

// TestReadFrameRejectsHostileStride pins the stride bound. Stride is not used
// to size the payload, but the broadcaster allocates stride*height for the
// composed surface, so an unchecked 0xFFFFFFFF stride is a 35 TB allocation.
func TestReadFrameRejectsHostileStride(t *testing.T) {
	rects := []Rect{{0, 0, 16, 16}}
	raw := encodeWireTestFrame(16, 16, ^uint32(0), 0, rects, make([]byte, 16*16*4))
	if _, err := readTestFrame(t, raw); err == nil {
		t.Fatal("accepted stride=0xFFFFFFFF on a 16-pixel-wide surface")
	}
	// A stride below width*4 was already rejected and must stay rejected.
	raw = encodeWireTestFrame(16, 16, 16, 0, rects, make([]byte, 16*16*4))
	if _, err := readTestFrame(t, raw); err == nil {
		t.Fatal("accepted stride 16 on a 16 px wide surface")
	}
	// The largest surface the protocol allows still decodes at its full stride.
	side := uint32(maxSurfaceDim)
	raw = encodeWireTestFrame(side, side, side*4, 0, []Rect{{0, 0, 16, 16}}, make([]byte, 16*16*4))
	if f, err := readTestFrame(t, raw); err != nil || f.Width != side || f.Stride != side*4 {
		t.Fatalf("max surface %dx%d stride=%d rejected: %v", side, side, side*4, err)
	}
}

// TestReadFramePayloadAllocationFollowsBytes pins that a declared size alone
// cannot reserve memory: a header claiming a full 8192x8192 surface (256 MiB)
// followed by no pixels must cost kilobytes, not the declared size.
func TestReadFramePayloadAllocationFollowsBytes(t *testing.T) {
	const side = uint32(8192)
	raw := encodeWireTestFrame(side, side, side*4, 0, []Rect{{0, 0, side, side}}, nil) // no payload at all

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := readTestFrame(t, raw)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("frame with a truncated payload was accepted")
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("a header-only frame grew TotalAlloc by %d bytes (declared payload is %d); allocation must track received bytes",
			grew, uint64(side)*uint64(side)*4)
	}
}

// TestReadFramePartialHeaderTimesOut pins that a producer which starts a frame
// and stalls cannot hold the reader (and its goroutine) forever. Waiting for
// the first byte of a new frame stays unbounded — an idle page sends no damage
// for minutes — but a frame already in progress must finish.
func TestReadFramePartialHeaderTimesOut(t *testing.T) {
	old := frameReadTimeout
	frameReadTimeout = 250 * time.Millisecond
	defer func() { frameReadTimeout = old }()

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	// net.Pipe is unbuffered, so the write must run alongside the reader.
	go func() {
		_, _ = server.Write([]byte{0x49}) // one byte of magic, then silence
	}()

	done := make(chan error, 1)
	go func() {
		_, err := readFrame(client)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("readFrame accepted a stalled partial header")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("readFrame blocked forever on a partial header")
	}
}

// FuzzISOLFrameHeader drives the frame parser with arbitrary wire bytes from an
// untrusted producer. The properties checked are the ones the rest of the
// streamer and the broadcaster rely on: no panic, and a decoded frame whose
// rects and payload are self-consistent and inside the declared surface.
func FuzzISOLFrameHeader(f *testing.F) {
	full := make([]byte, 16*16*4)
	f.Add(encodeWireTestFrame(16, 16, 64, 0, []Rect{{0, 0, 16, 16}}, full))
	f.Add(encodeWireTestFrame(1280, 720, 1280*4, 0, []Rect{{10, 20, 100, 40}}, make([]byte, 100*40*4)))
	f.Add(encodeWireTestFrame(64, 64, 256, 0, []Rect{{0, 0, 1, 1}, {1, 1, 1, 1}}, make([]byte, 8)))
	f.Add([]byte{})
	f.Add(encodeWireTestFrame(8192, 8192, 8192*4, 0, []Rect{{0, 0, 8192, 8192}}, nil))
	// Hostile rects: X+W and Y+H wrap to zero in uint32 arithmetic.
	f.Add(encodeWireTestFrame(64, 64, 256, 0, []Rect{{X: 2, Y: 2, W: ^uint32(0) - 1, H: ^uint32(0) - 1}}, nil))
	f.Add(encodeWireTestFrame(16, 16, ^uint32(0), 0, []Rect{{0, 0, 16, 16}}, make([]byte, 16*16*4)))

	f.Fuzz(func(t *testing.T, data []byte) {
		frame, err := readFrame(&wireConn{Reader: bytes.NewReader(data)})
		if err != nil {
			if frame != nil {
				t.Fatal("non-nil frame returned with an error")
			}
			return
		}
		if frame.Width == 0 || frame.Height == 0 || frame.Stride < frame.Width*4 {
			t.Fatalf("accepted degenerate geometry %dx%d stride=%d", frame.Width, frame.Height, frame.Stride)
		}
		var total uint64
		for _, r := range frame.Dirty {
			if r.W == 0 || r.H == 0 || uint64(r.X)+uint64(r.W) > uint64(frame.Width) ||
				uint64(r.Y)+uint64(r.H) > uint64(frame.Height) {
				t.Fatalf("accepted out-of-surface rect %+v in %dx%d", r, frame.Width, frame.Height)
			}
			total += uint64(r.W) * uint64(r.H) * 4
		}
		if total > uint64(frame.Width)*uint64(frame.Height)*4 {
			t.Fatalf("accepted %d payload bytes for a %dx%d surface", total, frame.Width, frame.Height)
		}
		if uint64(len(frame.Payload)) != total {
			t.Fatalf("payload %d bytes, rects imply %d", len(frame.Payload), total)
		}
	})
}

// TestReadFrameSurvivesArbitraryBytes is the deterministic sibling of the fuzz
// target: every truncation of a valid frame must return an error, never a
// partial frame and never a panic.
func TestReadFrameSurvivesArbitraryBytes(t *testing.T) {
	raw := encodeWireTestFrame(32, 32, 128, 0, []Rect{{0, 0, 32, 32}}, make([]byte, 32*32*4))
	for cut := range len(raw) {
		if f, err := readTestFrame(t, raw[:cut]); err == nil {
			t.Fatalf("truncated at %d/%d bytes: accepted frame %+v", cut, len(raw), f)
		}
	}
	if _, err := readTestFrame(t, raw); err != nil {
		t.Fatalf("complete frame rejected: %v", err)
	}
}

var _ net.Conn = (*wireConn)(nil)
