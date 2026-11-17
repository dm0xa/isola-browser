package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
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

// Frame: one damage update. Dirty lists the changed rects, Payload carries
// exactly those pixels. A whole-surface rect is a full refresh (see IsFull).
type Frame struct {
	Width, Height, Stride uint32
	Timestamp             time.Time
	Dirty                 []Rect
	Payload               []byte // BGRA, len == payloadBytes(Dirty)
}

// Rect is a dirty rectangle within the frame.
type Rect struct{ X, Y, W, H uint32 }

// payloadBytes: uint64 because rect fields are untrusted and W*H*4 overflows 32-bit int.
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

// frameReadTimeout bounds only an in-progress frame: waiting for a first byte is
// unbounded (an idle page sends nothing for minutes); once started, a stall = broken producer.
const frameReadTimeout = 5 * time.Second

// frameReadChunk is the allocation granularity for a pixel payload. Ordinary
// damage updates fit in one chunk; a full-surface refresh is read in doublings.
const frameReadChunk = 64 << 10

// frameReadBuffer: recycled payload slots — measured 72 µs / 0 B per full frame vs
// 522 µs / 7.8 MB allocating. Three slots: the reader runs two frames ahead of composition.
type frameReadBuffer struct {
	slots [3][]byte
	seq   uint64
}

func (b *frameReadBuffer) acquire() (int, []byte) {
	slot := int(b.seq % uint64(len(b.slots)))
	b.seq++
	return slot, b.slots[slot][:0]
}

func (b *frameReadBuffer) release(slot int, buf []byte) {
	b.slots[slot] = buf
}

func readFrame(conn net.Conn) (*Frame, error) {
	return readFrameWithin(conn, frameReadTimeout)
}

// readFrameWithin: the timeout is a parameter — a shared variable is a data race in tests that shorten it.
func readFrameWithin(conn net.Conn, rest time.Duration) (*Frame, error) {
	return readFrameWith(conn, rest, nil)
}

// readFrameWith is readFrameWithin with a recyclable payload workspace. A nil
// buffer keeps the original per-frame allocation (tests and one-shot readers).
func readFrameWith(conn net.Conn, rest time.Duration, rb *frameReadBuffer) (*Frame, error) {
	hdr := make([]byte, 32)
	// No deadline while waiting for a frame to begin.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(conn, hdr[:1]); err != nil {
		return nil, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(rest)); err != nil {
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
		// Reject out-of-bounds damage: X+W is uint32 addition, so 0xFFFFFFFE+2 wraps to
		// 0 and passes a naive check — hence the subtraction form.
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

	payload, err := readPayloadInto(conn, rb, int(n))
	if err != nil {
		return nil, err
	}
	f.Payload = payload
	return f, nil
}

// readPayloadInto: recycled slot when supplied; growth stays incremental because
// n comes from an untrusted header (the 8192-max surface is 256 MiB).
func readPayloadInto(conn net.Conn, rb *frameReadBuffer, n int) ([]byte, error) {
	slot := -1
	buf := make([]byte, 0, min(n, frameReadChunk))
	if rb != nil {
		slot, buf = rb.acquire()
		if cap(buf) > n {
			buf = buf[:n]
			if _, err := io.ReadFull(conn, buf); err != nil {
				rb.release(slot, buf[:0])
				return nil, err
			}
			rb.release(slot, buf)
			return buf, nil
		}
		buf = buf[:0]
	}
	for len(buf) < n {
		if len(buf) == cap(buf) {
			grown := max(cap(buf)*2, frameReadChunk)
			if grown > n {
				grown = n
			}
			grownBuf := make([]byte, len(buf), grown)
			copy(grownBuf, buf)
			buf = grownBuf
		}
		end := min(n, cap(buf))
		if _, err := io.ReadFull(conn, buf[len(buf):end]); err != nil {
			if slot >= 0 {
				rb.release(slot, buf)
			}
			return nil, err
		}
		buf = buf[:end]
	}
	if slot >= 0 {
		rb.release(slot, buf)
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
				if errors.Is(err, net.ErrClosed) {
					return
				}
				// A persistent accept error (fd exhaustion in a 3-CPU container)
				// used to spin here at full core forever, starving capture.
				log.Printf("mux: frame accept: %v", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			go func() {
				defer conn.Close()
				// One workspace per producer connection: the payload buffers are
				// only ever touched by this goroutine and the broadcaster's
				// sequential compose (see frameReadBuffer).
				rb := &frameReadBuffer{}
				for {
					f, err := readFrameWith(conn, frameReadTimeout, rb)
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
