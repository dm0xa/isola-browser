package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

const frameMagic = 0x4C4F5349 // 'ISOL'

// Bounds on what a single frame header may declare. The producer only ever
// sends one dirty rect, but a corrupt or hostile header must not be able to
// force a large allocation, so both are capped.
const (
	maxDirtyRects = 4096
	maxSurfaceDim = 8192
	// minSurfaceDim is the smallest geometry a resize may ask for. H.264 codes
	// 16x16 macroblocks, and anything below that cannot round-trip through the
	// encoder's even-dimension and macroblock assumptions.
	minSurfaceDim = 16
)

// Frame is one damage update from the producer. Dirty lists the rectangles
// that changed and Payload carries exactly those pixels — row-major within
// each rect, concatenated in Dirty order.
//
// The producer transmits damage only, so a caret blink costs a few KB instead
// of a full surface (14.7 MB at 1440p). A frame whose single rect covers the
// whole surface is a full refresh and can seed a surface from scratch; see
// IsFull.
type Frame struct {
	Width, Height, Stride uint32
	Timestamp             time.Time
	Dirty                 []Rect
	Payload               []byte // BGRA, len == payloadBytes(Dirty)
}

// Rect is a dirty rectangle within the frame.
type Rect struct{ X, Y, W, H uint32 }

// payloadBytes returns the wire size of the pixel payload for a set of rects.
//
// The sum is computed in uint64 because the rect fields are untrusted: W*H*4
// for two uint32 fields overflows int on a 32-bit build, and a header is
// allowed to reach this function with values a 64-bit int cannot hold.
func payloadBytes(rects []Rect) uint64 {
	var n uint64
	for _, r := range rects {
		n += uint64(r.W) * uint64(r.H) * 4
	}
	return n
}

// IsFull reports whether the frame covers the entire surface, i.e. whether it
// is a complete image rather than a damage update.
func (f *Frame) IsFull() bool {
	return len(f.Dirty) == 1 &&
		f.Dirty[0].X == 0 && f.Dirty[0].Y == 0 &&
		f.Dirty[0].W == f.Width && f.Dirty[0].H == f.Height
}

// frameReadTimeout bounds the rest of an in-progress frame once its first byte
// has arrived. Waiting for the first byte of a new frame is deliberately
// unbounded: an idle page (and every page while no viewer is attached) sends no
// damage for minutes, and that must not read as a dead producer. Once a frame
// has started, though, all of its bytes are on their way over a local socket,
// so a stall means a broken producer — drop it instead of pinning this reader
// goroutine and its buffers forever.
//
// A var, not a const, so tests can shorten it.
var frameReadTimeout = 5 * time.Second

// frameReadChunk is the allocation granularity for a pixel payload. Ordinary
// damage updates fit in one chunk; a full-surface refresh is read in doublings.
const frameReadChunk = 64 << 10

func readFrame(conn net.Conn) (*Frame, error) {
	hdr := make([]byte, 32)
	// No deadline while waiting for a frame to begin.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(conn, hdr[:1]); err != nil {
		return nil, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(frameReadTimeout)); err != nil {
		return nil, err
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	if _, err := io.ReadFull(conn, hdr[1:]); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint32(hdr[0:4]) != frameMagic {
		return nil, fmt.Errorf("bad magic")
	}
	f := &Frame{
		Width:  binary.LittleEndian.Uint32(hdr[4:8]),
		Height: binary.LittleEndian.Uint32(hdr[8:12]),
		Stride: binary.LittleEndian.Uint32(hdr[12:16]),
	}
	nDirty := binary.LittleEndian.Uint32(hdr[20:24])
	f.Timestamp = time.Unix(0, int64(binary.LittleEndian.Uint64(hdr[24:32])))

	// Stride is not used to size the payload, but the broadcaster allocates
	// stride*height for the composed surface, so an unchecked 0xFFFFFFFF would
	// be a multi-terabyte reservation. The producer always sends width*4.
	if f.Width == 0 || f.Height == 0 ||
		f.Width > maxSurfaceDim || f.Height > maxSurfaceDim ||
		f.Stride < f.Width*4 || f.Stride > maxSurfaceDim*4 {
		return nil, fmt.Errorf("invalid surface %dx%d stride=%d", f.Width, f.Height, f.Stride)
	}
	if nDirty == 0 || nDirty > maxDirtyRects {
		return nil, fmt.Errorf("invalid dirty rect count %d", nDirty)
	}

	dirtyBuf := make([]byte, nDirty*16)
	if _, err := io.ReadFull(conn, dirtyBuf); err != nil {
		return nil, err
	}
	f.Dirty = make([]Rect, 0, nDirty)
	for i := uint32(0); i < nDirty; i++ {
		off := i * 16
		r := Rect{
			X: binary.LittleEndian.Uint32(dirtyBuf[off:]),
			Y: binary.LittleEndian.Uint32(dirtyBuf[off+4:]),
			W: binary.LittleEndian.Uint32(dirtyBuf[off+8:]),
			H: binary.LittleEndian.Uint32(dirtyBuf[off+12:]),
		}
		// Reject out-of-bounds damage outright: the payload length is derived
		// from the rects, so a bogus rect would desynchronise the stream.
		//
		// The comparisons are written as subtraction because X+W is a uint32
		// addition: 0xFFFFFFFE+2 wraps to 0 and would pass a naive check,
		// after which the payload size derived from the rect wrapped int64
		// and panicked in make().
		if r.W == 0 || r.H == 0 || r.W > f.Width || r.H > f.Height ||
			r.X > f.Width-r.W || r.Y > f.Height-r.H {
			return nil, fmt.Errorf("dirty rect %d out of bounds: %dx%d+%d+%d in %dx%d",
				i, r.W, r.H, r.X, r.Y, f.Width, f.Height)
		}
		f.Dirty = append(f.Dirty, r)
	}

	// The payload is the concatenation of the rects, so overlapping damage
	// would let a 4 KiB header demand 4096 full surfaces. A truthful frame
	// never covers more pixels than the surface holds.
	n := payloadBytes(f.Dirty)
	if surface := uint64(f.Width) * uint64(f.Height) * 4; n > surface {
		return nil, fmt.Errorf("damage covers %d bytes, more than the %dx%d surface", n, f.Width, f.Height)
	}

	payload, err := readPayload(conn, int(n))
	if err != nil {
		return nil, err
	}
	f.Payload = payload
	return f, nil
}

// readPayload reads exactly n pixel bytes, growing the buffer as the bytes
// arrive. n comes from an untrusted header, so make([]byte, n) up front would
// let a header-only frame reserve the declared surface (256 MiB at the 8192
// maximum) before a single pixel shows up.
func readPayload(conn net.Conn, n int) ([]byte, error) {
	if n <= frameReadChunk {
		buf := make([]byte, n)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	buf := make([]byte, 0, frameReadChunk)
	for len(buf) < n {
		if len(buf) == cap(buf) {
			grown := cap(buf) * 2
			if grown > n {
				grown = n
			}
			grownBuf := make([]byte, len(buf), grown)
			copy(grownBuf, buf)
			buf = grownBuf
		}
		end := min(n, cap(buf))
		if _, err := io.ReadFull(conn, buf[len(buf):end]); err != nil {
			return nil, err
		}
		buf = buf[:end]
	}
	return buf, nil
}

// listenFrames creates a Unix domain socket listener and returns a channel
// of decoded frames. Dropping oldest on backpressure (2-frame buffer).
func listenFrames(socketPath string) (<-chan *Frame, error) {
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	ch := make(chan *Frame, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				continue
			}
			go func() {
				defer conn.Close()
				for {
					f, err := readFrame(conn)
					if err != nil {
						return
					}
					select {
					case ch <- f:
					default:
						<-ch // drop oldest
						ch <- f
					}
				}
			}()
		}
	}()
	return ch, nil
}
