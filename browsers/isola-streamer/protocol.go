package main

import "encoding/binary"

// Message types for the WebSocket wire protocol.
const (
	msgTypeStreamInit      = "stream_init"
	msgTypeStreamReset     = "stream_reset"
	msgTypeStreamError     = "stream_error"
	msgTypeInput           = "input"
	msgTypeClipboardSync   = "clipboard_sync"
	msgTypeResize          = "resize"
	msgTypeMetrics         = "metrics"
	msgTypePing            = "ping"
	msgTypePong            = "pong"
	msgTypeSetBitrate      = "set_bitrate"
	msgTypeRequestKeyframe = "request_keyframe"
)

// videoSegmentHeader is the 10-byte binary prefix for fMP4 segments.
//
//	byte 0:     0x01 (video segment marker)
//	byte 1:     flags (bit0 = keyframe)
//	byte 2-5:   uint32 sequence number (LE)
//	byte 6-9:   uint32 payload length (LE)
const videoSegmentHeaderSize = 10
const videoSegmentMarker = 0x01
const videoSegmentFlagKeyframe = 0x01

func packVideoSegment(fmp4 []byte, seq uint32, isKeyframe bool) []byte {
	pkt := make([]byte, videoSegmentHeaderSize+len(fmp4))
	pkt[0] = videoSegmentMarker
	if isKeyframe {
		pkt[1] = videoSegmentFlagKeyframe
	}
	binary.LittleEndian.PutUint32(pkt[2:6], seq)
	binary.LittleEndian.PutUint32(pkt[6:10], uint32(len(fmp4)))
	copy(pkt[videoSegmentHeaderSize:], fmp4)
	return pkt
}

// videoFrameHeader is the 10-byte binary prefix for raw Annex-B access
// units served to WebCodecs clients.
//
//	byte 0:     0x02 (access unit marker)
//	byte 1:     flags (bit0 = keyframe/IDR)
//	byte 2-5:   uint32 sequence number (LE)
//	byte 6-9:   uint32 payload length (LE)
const videoFrameMarker = 0x02

// audioFrameMarker prefixes raw Opus packets on the viewer WebSocket.
const audioFrameMarker = 0x03

func packAccessUnit(au []byte, seq uint32, isKeyframe bool) []byte {
	pkt := make([]byte, videoSegmentHeaderSize+len(au))
	pkt[0] = videoFrameMarker
	if isKeyframe {
		pkt[1] = videoSegmentFlagKeyframe
	}
	binary.LittleEndian.PutUint32(pkt[2:6], seq)
	binary.LittleEndian.PutUint32(pkt[6:10], uint32(len(au)))
	copy(pkt[videoSegmentHeaderSize:], au)
	return pkt
}
