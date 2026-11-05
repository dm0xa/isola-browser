package main

import (
	"encoding/binary"
	"io"
)

// fmp4ReadChunk is how much ffmpeg stdout one pump read asks for: large enough
// that a normal fragment lands in a read or two, small enough that a keyframe
// does not make the pump hold a syscall longer than a frame interval.
const fmp4ReadChunk = 64 * 1024

// maxFMP4Box bounds one fMP4 box the reader is willing to buffer or wait for.
//
// ffmpeg's fragment sizes are set by the GOP and the bitrate, not by this
// reader: a measured 1080p 2 s fragment (12 Mbit/s, x264) is a 512-byte moof
// plus a 3.1 MiB mdat, and the init pair is under 1 KiB. 32 MiB leaves ~10x
// headroom for a bigger keyframe or a higher-bitrate stream while keeping a
// corrupt or hostile box header (which may declare up to 4 GiB) from growing
// the accumulation buffer without bound.
const maxFMP4Box = 32 << 20

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

// maxEmptyReads bounds consecutive zero-byte reads before the pump is told the
// source made no progress. io.Reader permits (0, nil); an OS pipe never returns
// it, but handing it back to the pump's read loop would spin that loop forever
// without delivering a byte.
const maxEmptyReads = 100

// readFrom reads the next chunk straight from src into the accumulation
// buffer and returns what it read.
func (p *pipeBuffer) readFrom(src io.Reader) (int, error) {
	p.reserve(fmp4ReadChunk)
	for empty := 0; ; {
		n, err := src.Read(p.buf[p.end:])
		p.end += n
		if n > 0 || err != nil {
			return n, err
		}
		empty++
		if empty >= maxEmptyReads {
			return 0, io.ErrNoProgress
		}
	}
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
				r.resync()
				continue
			}
			ftypSize, ok := boxSize(buf, 0)
			if !ok || ftypSize > maxFMP4Box {
				// The ftyp header itself is unusable, so the parse position
				// is lost. resync skips this occurrence because its declared
				// size does not validate.
				r.resync()
				continue
			}
			if len(buf) < ftypSize+8 {
				return nil, false
			}
			moovOff := ftypSize
			moovSize, ok := boxSize(buf, moovOff)
			if string(buf[moovOff+4:moovOff+8]) != "moov" || !ok || moovSize > maxFMP4Box {
				// An ftyp whose moov never follows cannot seed a decoder.
				// Drop it and wait for the next init pair instead of
				// blocking on this one forever.
				r.q.consume(ftypSize)
				r.resync()
				continue
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
			if !ok || size > maxFMP4Box {
				r.resync()
				continue
			}
			if len(buf) < size {
				return nil, false
			}
			r.q.consume(size)
			continue
		}
		moofSize, ok := boxSize(buf, 0)
		if !ok || moofSize > maxFMP4Box {
			r.resync()
			continue
		}
		if len(buf) < moofSize+8 {
			return nil, false
		}
		mdatOff := moofSize
		mdatSize, ok := boxSize(buf, mdatOff)
		if string(buf[mdatOff+4:mdatOff+8]) != "mdat" || !ok || mdatSize > maxFMP4Box {
			// A moof that is not followed by its mdat: this fragment is
			// unrecoverable, but the next one must still reach viewers.
			r.q.consume(moofSize)
			r.resync()
			continue
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

// resync drops bytes from the front of the buffer until the next plausible box
// start of the kind the reader is waiting for: ftyp before the init segment,
// moof after it. It is only reached on malformed input — a declared size that
// cannot be trusted means the parse position is lost, and honouring it would
// either buffer gigabytes or wedge the stream forever. The last seven bytes are
// kept because a box header can straddle two reads.
func (r *fmp4Reader) resync() {
	buf := r.q.pending()
	want := "moof"
	if !r.initSent {
		want = "ftyp"
	}
	for i := 4; i+4 <= len(buf); i++ {
		if string(buf[i:i+4]) != want {
			continue
		}
		if size, ok := boxSize(buf, i-4); ok && size <= maxFMP4Box {
			r.q.consume(i - 4)
			return
		}
	}
	const keep = 7
	if len(buf) > keep {
		r.q.consume(len(buf) - keep)
	}
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
