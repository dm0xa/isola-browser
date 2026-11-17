package main

import "io"

// maxAUStreamBytes: past this much unterminated data the stream is corrupt; drop it.
const maxAUStreamBytes = 8 << 20

// auSplitter reassembles Annex-B into access units; the boundary sits before the
// second VCL NAL, so parameter sets ride with the following picture. Emitted AUs
// alias the pipe buffer (linear scan, no per-AU copy) and are valid until the next feed.
type auSplitter struct {
	q pipeBuffer
}

type auPacket struct {
	data     []byte // complete AU, start codes included; aliases the splitter
	keyframe bool   // AU contains an IDR NAL (type 5)
}

// feed appends encoded bytes and returns every access unit that completed.
func (s *auSplitter) feed(data []byte) []auPacket {
	if len(data) == 0 {
		return nil
	}
	s.q.append(data)
	return s.split()
}

// readFrom reads the next chunk of ffmpeg stdout into the splitter's buffer and
// returns the access units it completed.
func (s *auSplitter) readFrom(src io.Reader) ([]auPacket, error) {
	n, err := s.q.readFrom(src)
	if n == 0 {
		return nil, err
	}
	return s.split(), err
}

// split emits every complete AU currently buffered and consumes it.
func (s *auSplitter) split() []auPacket {
	s.resync()
	var out []auPacket
	for {
		boundary, idr, ok := s.findBoundary()
		if !ok {
			s.trimOversized()
			return out
		}
		buf := s.q.pending()
		out = append(out, auPacket{data: buf[:boundary:boundary], keyframe: idr})
		s.q.consume(boundary)
	}
}

// trimOversized: keep a suffix starting at the last NAL start code — never splice a
// NAL onto a fragment of itself. Undecodable by definition; dropping is the only bounded choice.
func (s *auSplitter) trimOversized() {
	buf := s.q.pending()
	if len(buf) <= maxAUStreamBytes {
		return
	}
	// i > 0: a start code already at offset 0 would keep the whole window,
	// which is the one case that has to be dropped outright.
	for i := len(buf) - 3; i > 0; i-- {
		if buf[i] == 0 && buf[i+1] == 0 && buf[i+2] == 1 {
			s.q.consume(i)
			return
		}
	}
	s.q.consume(len(buf))
}

// resync: drop any non-start-code prefix; keep the last three bytes (a start code can straddle reads).
func (s *auSplitter) resync() {
	buf := s.q.pending()
	if len(buf) < 3 {
		return
	}
	if buf[0] == 0 && buf[1] == 0 && buf[2] == 1 {
		return
	}
	if len(buf) >= 4 && buf[0] == 0 && buf[1] == 0 && buf[2] == 0 && buf[3] == 1 {
		return
	}
	for i := 1; i+3 <= len(buf); i++ {
		if buf[i] == 0 && buf[i+1] == 0 && buf[i+2] == 1 {
			// Keep the 4-byte form whole when its leading zero is present, so
			// the bytes handed on are exactly what the encoder produced.
			if buf[i-1] == 0 {
				i--
			}
			s.q.consume(i)
			return
		}
	}
	// Three bytes, not two: a 4-byte start code can straddle two reads.
	const keep = 3
	if len(buf) > keep {
		s.q.consume(len(buf) - keep)
	}
}

// findBoundary: with zerolatency a picture is several slice NALs, so an AU begins
// at a VCL NAL with first_mb_in_slice == 0 — i.e. (buf[nal+1] & 0x80) != 0. The
// IDR flag folds into the same pass.
func (s *auSplitter) findBoundary() (int, bool, bool) {
	buf := s.q.pending()
	firstVCL := -1
	idr := false
	off := 0
	for {
		startCode, nal, ok := nextNAL(buf, off)
		if !ok || nal >= len(buf) {
			// A start code may be the last thing in the buffer; the NAL
			// header byte it introduces arrives with the next read.
			return 0, false, false
		}
		t := int(buf[nal] & 0x1F)
		if t >= 1 && t <= 5 && nal+1 < len(buf) {
			firstSliceOfPicture := buf[nal+1]&0x80 != 0
			if firstSliceOfPicture {
				if firstVCL < 0 {
					firstVCL = startCode
				} else {
					// This NAL starts the following AU; its own type does
					// not belong to the AU ending here.
					return startCode, idr, true
				}
			}
		}
		if t == 5 {
			idr = true
		}
		off = nal
	}
}

// nextNAL finds the next NAL boundary at or after `from`. It returns the
// offset of the start code (3- or 4-byte form) and the offset of the NAL
// header byte that follows it.
func nextNAL(buf []byte, from int) (startCode int, nalHeader int, ok bool) {
	i := from
	for i+3 <= len(buf) {
		if buf[i] == 0 && buf[i+1] == 0 && buf[i+2] == 1 {
			start := i
			if start > 0 && buf[start-1] == 0 {
				start-- // 4-byte start code 00 00 00 01
			}
			return start, i + 3, true
		}
		i++
	}
	return 0, 0, false
}
