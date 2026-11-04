package main

import (
	"encoding/binary"
	"io"
)

// fmp4ReadChunk is how much ffmpeg stdout one pump read asks for: large enough
// that a normal fragment lands in a read or two, small enough that a keyframe
// does not make the pump hold a syscall longer than a frame interval.
const fmp4ReadChunk = 64 * 1024

// pipeBuffer accumulates bytes read from an encoder's stdout and reclaims the
// consumed prefix in place.
//
// The previous shape — append into a growing slice, then re-slice forward past
// every extracted segment — paid a full copy of the stream on the append and
// abandoned one buffer after another as the slice walked forward. Keeping a
// read offset and a write offset lets one allocation serve the whole run: the
// pump reads straight into buf[end:], the parser consumes from buf[off:end],
// and reserve() moves the pending tail back to the front instead of growing.
//
// Each encoder pump owns one; nothing here is safe for concurrent use.
type pipeBuffer struct {
	buf []byte // always full length: the whole allocation is addressable
	off int    // start of the unconsumed bytes
	end int    // end of the buffered bytes
}

// pending returns the bytes read but not yet consumed. The parser must not keep
// this slice past the next read: a later reserve() may move it to the front.
func (p *pipeBuffer) pending() []byte { return p.buf[p.off:p.end] }

// consume drops n bytes off the front of the pending window.
func (p *pipeBuffer) consume(n int) {
	p.off += n
	if p.off >= p.end {
		p.off, p.end = 0, 0
	}
}

// append copies data into the accumulation buffer. It exists for callers that
// already hold a read buffer (tests, feeds that are not a pipe); the pump uses
// readFrom instead so ffmpeg's bytes are copied once.
func (p *pipeBuffer) append(data []byte) {
	p.reserve(len(data))
	p.end += copy(p.buf[p.end:], data)
}

// readFrom reads the next chunk straight from src into the accumulation
// buffer and returns what it read.
func (p *pipeBuffer) readFrom(src io.Reader) (int, error) {
	p.reserve(fmp4ReadChunk)
	n, err := src.Read(p.buf[p.end:])
	p.end += n
	return n, err
}

// reserve makes room for n more bytes, first moving any pending tail to the
// front of the buffer so the space already paid for is reused.
func (p *pipeBuffer) reserve(n int) {
	if len(p.buf)-p.end >= n {
		return
	}
	if p.off > 0 {
		live := p.end - p.off
		copy(p.buf, p.buf[p.off:p.end])
		p.off, p.end = 0, live
		if len(p.buf)-p.end >= n {
			return
		}
	}
	grown := len(p.buf) * 2
	if grown < p.end+n {
		grown = p.end + n
	}
	nb := make([]byte, grown)
	copy(nb, p.buf[:p.end])
	p.buf = nb
}

// fmp4Reader reassembles ffmpeg fragmented-MP4 output into MSE-ready segments:
//   - init: ftyp + moov (one append)
//   - media: moof + mdat (one append per fragment)
//
// Segments alias the reader's accumulation buffer and are valid until the next
// Feed/readSegments call. The pump packs each one into a viewer packet — the
// single copy left on this path — immediately, so nothing outlives its bytes.
type fmp4Reader struct {
	q        pipeBuffer
	initSent bool
}

func newFMP4Reader() *fmp4Reader {
	return &fmp4Reader{}
}

// Feed appends raw ffmpeg stdout bytes and returns any complete segments.
func (r *fmp4Reader) Feed(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	r.q.append(data)
	return r.extract()
}

// readSegments reads the next chunk of ffmpeg stdout into the accumulation
// buffer and returns the segments that completed. The read goes straight into
// the buffer, so a pumped byte is copied once (into the caller's packet)
// instead of twice.
func (r *fmp4Reader) readSegments(src io.Reader) ([][]byte, error) {
	n, err := r.q.readFrom(src)
	if n == 0 {
		return nil, err
	}
	return r.extract(), err
}

func (r *fmp4Reader) extract() [][]byte {
	var out [][]byte
	for {
		seg, ok := r.extractOne()
		if !ok {
			return out
		}
		out = append(out, seg)
	}
}

func (r *fmp4Reader) extractOne() ([]byte, bool) {
	for len(r.q.pending()) >= 8 {
		buf := r.q.pending()
		if !r.initSent {
			if string(buf[4:8]) != "ftyp" {
				// Drop bytes until ftyp (shouldn't happen with ffmpeg).
				r.q.consume(1)
				continue
			}
			ftypSize, ok := boxSize(buf, 0)
			if !ok || len(buf) < ftypSize+8 {
				return nil, false
			}
			moovOff := ftypSize
			if string(buf[moovOff+4:moovOff+8]) != "moov" {
				return nil, false
			}
			moovSize, ok := boxSize(buf, moovOff)
			if !ok {
				return nil, false
			}
			initEnd := moovOff + moovSize
			if len(buf) < initEnd {
				return nil, false
			}
			seg := buf[:initEnd:initEnd]
			r.q.consume(initEnd)
			r.initSent = true
			return seg, true
		}

		if string(buf[4:8]) != "moof" {
			// Skip stray boxes between fragments.
			size, ok := boxSize(buf, 0)
			if !ok || len(buf) < size {
				return nil, false
			}
			r.q.consume(size)
			continue
		}
		moofSize, ok := boxSize(buf, 0)
		if !ok || len(buf) < moofSize+8 {
			return nil, false
		}
		mdatOff := moofSize
		if string(buf[mdatOff+4:mdatOff+8]) != "mdat" {
			return nil, false
		}
		mdatSize, ok := boxSize(buf, mdatOff)
		if !ok {
			return nil, false
		}
		fragEnd := mdatOff + mdatSize
		if len(buf) < fragEnd {
			return nil, false
		}
		seg := buf[:fragEnd:fragEnd]
		r.q.consume(fragEnd)
		return seg, true
	}
	return nil, false
}

func boxSize(data []byte, off int) (int, bool) {
	if off+8 > len(data) {
		return 0, false
	}
	size32 := binary.BigEndian.Uint32(data[off : off+4])
	if size32 == 0 {
		return 0, false
	}
	if size32 == 1 {
		// 64-bit extended size — not expected from our ffmpeg settings.
		if off+16 > len(data) {
			return 0, false
		}
		size64 := binary.BigEndian.Uint64(data[off+8 : off+16])
		if size64 < 16 || size64 > 1<<30 {
			return 0, false
		}
		return int(size64), true
	}
	if size32 < 8 {
		return 0, false
	}
	return int(size32), true
}
