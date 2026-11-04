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
func payloadBytes(rects []Rect) int {
	n := 0
	for _, r := range rects {
		n += int(r.W) * int(r.H) * 4
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

func readFrame(conn net.Conn) (*Frame, error) {
	hdr := make([]byte, 32)
	if _, err := io.ReadFull(conn, hdr); err != nil {
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

	if f.Width == 0 || f.Height == 0 ||
		f.Width > maxSurfaceDim || f.Height > maxSurfaceDim ||
		f.Stride < f.Width*4 {
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
		if r.W == 0 || r.H == 0 || r.X+r.W > f.Width || r.Y+r.H > f.Height {
			return nil, fmt.Errorf("dirty rect %d out of bounds: %dx%d+%d+%d in %dx%d",
				i, r.W, r.H, r.X, r.Y, f.Width, f.Height)
		}
		f.Dirty = append(f.Dirty, r)
	}

	f.Payload = make([]byte, payloadBytes(f.Dirty))
	if _, err := io.ReadFull(conn, f.Payload); err != nil {
		return nil, err
	}
	return f, nil
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
