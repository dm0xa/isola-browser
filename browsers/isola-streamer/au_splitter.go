package main

import "io"

// maxAUStreamBytes bounds how much encoded data may accumulate without a
// picture boundary. A 1080p IDR is well under a megabyte at the rates this
// streamer runs; past this much unterminated data the stream is corrupt or
// hostile and the buffered bytes are dropped rather than growing without
// bound.
const maxAUStreamBytes = 8 << 20

// auSplitter reassembles an Annex-B H.264 byte stream (as produced by
// ffmpeg -f h264) into complete access units. An access unit boundary sits
// immediately before the second VCL NAL (type 1-5) of a run: non-VCL NALs
// (SPS/PPS/SEI) that arrive between two pictures belong to the *following*
// picture, so the emitted AU always carries its parameter sets with it —
// exactly what a WebCodecs VideoDecoder consuming Annex-B expects.
//
// The splitter owns a pipeBuffer instead of copying each AU out of an
// append-and-compact slice: the old shape copied every AU once for the
// splitter and again for its wire header, and re-scanned the whole buffered
// prefix per AU (findBoundary always started at offset 0). Emitted AUs now
// alias the buffer and the scan resumes at the byte after the previous AU, so
// splitting is linear in the bytes read.
//
// Emitted AUs are valid until the next feed call. The pump packs each one into
// a viewer packet (the single remaining copy) immediately, and reserves the
// header itself, so nothing outlives the bytes it points at.
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

// trimOversized drops buffered bytes when no access-unit boundary was found in
// them and they exceed maxAUStreamBytes. The retained suffix starts at the last
// NAL start code in the window (or nothing at all), so a NAL is never spliced
// onto a fragment of itself: whatever is kept begins on a NAL boundary, and the
// splitter still never emits a partial AU. A stream this far along without a
// second picture is not decodable video, so dropping it is the only bounded
// choice.
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

// resync drops any prefix that cannot be part of an access unit. A well-formed
// stream begins on a start code and subsequent split points are start codes, so
// this is a three-byte check in the normal case; after a corrupt read or a
// dropped oversized window it stops the garbage from riding into the first
// emitted AU. The last three bytes are kept because a start code can straddle
// two reads.
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

// findBoundary returns the offset at which the next access unit starts,
// relative to the pending window, and whether the AU before it carries an IDR.
// With x264 zerolatency (sliced threads) a picture arrives as several slice
// NALs, so "first VCL NAL" is not enough: a new AU begins at a VCL NAL whose
// first_mb_in_slice == 0. first_mb_in_slice is the first ue(v) field of the
// slice header — it is 0 exactly when the first payload bit after the NAL
// header is 1, i.e. (buf[nal+1] & 0x80) != 0.
//
// The IDR flag is folded into this single pass: every NAL seen before the
// boundary belongs to the AU, so a type-5 NAL there makes it a keyframe.
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
