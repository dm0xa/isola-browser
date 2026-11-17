package main

import (
	"encoding/binary"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
)

// Audio relay: ffmpeg captures the PulseAudio monitor as Opus-over-RTP on
// :5006; this reader strips the RTP header and forwards each Opus packet to
// viewers as marker-0x03 frames on the video WebSocket. Forwarding is gated
// by the /audio toggle (default off — the viewer's audio button flips it).

var audioOn atomic.Bool

var audioFns = struct {
	sync.Mutex
	m   map[int]func([]byte)
	seq int
}{m: make(map[int]func([]byte))}

func registerAudioListener(fn func([]byte)) func() {
	audioFns.Lock()
	defer audioFns.Unlock()
	audioFns.seq++
	id := audioFns.seq
	audioFns.m[id] = fn
	return func() {
		audioFns.Lock()
		delete(audioFns.m, id)
		audioFns.Unlock()
	}
}

func broadcastAudio(pkt []byte) {
	if !audioOn.Load() {
		return
	}
	audioFns.Lock()
	fns := make([]func([]byte), 0, len(audioFns.m))
	for _, fn := range audioFns.m {
		fns = append(fns, fn)
	}
	audioFns.Unlock()
	for _, fn := range fns {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("audio: listener panic: %v", r)
				}
			}()
			fn(pkt)
		}()
	}
}

// readAudioRTP consumes Opus-over-RTP from ffmpeg. One Opus packet per RTP
// packet (ffmpeg -frame_duration 20 → 20ms frames).
func readAudioRTP() {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:5006")
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Printf("audio: rtp listen: %v", err)
		return
	}
	defer conn.Close()
	if err := conn.SetReadBuffer(256 * 1024); err != nil {
		log.Printf("audio: read buffer: %v", err)
	}
	log.Printf("audio: rtp :5006")

	buf := make([]byte, 2048)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("audio: read error: %v", err)
			return
		}
		payload, ok := rtpPayload(buf[:n])
		if !ok || len(payload) == 0 {
			continue
		}
		broadcastAudio(packAudioPacket(payload))
	}
}

// rtpPayload returns the RTP payload after the fixed header, CSRC list and
// any header extension. RTCP is rejected: ffmpeg's RTP muxer emits RTCP
// Sender Reports on the same UDP port (RFC 5761 single-port mode), and RTCP
// shares the RTP version bits, so a version check alone would forward a
// 28-byte SR body to the viewer as an Opus packet and poison its decoder
// with "EncodingError: Decoding error".
func rtpPayload(pkt []byte) ([]byte, bool) {
	if len(pkt) < 12 || pkt[0]>>6 != 2 {
		return nil, false
	}
	// RFC 5761 §4.2: the second octet of an RTCP packet is 192–223 (packet
	// types 200–207 plus the marker bit). An RTP Opus stream uses a dynamic
	// payload type well outside that range.
	if b := pkt[1]; b >= 192 && b <= 223 {
		return nil, false
	}
	cc := int(pkt[0] & 0x0F)
	off := 12 + 4*cc
	if off > len(pkt) {
		return nil, false
	}
	if pkt[0]&0x10 != 0 { // extension bit
		if len(pkt) < off+4 {
			return nil, false
		}
		extLen := int(binary.BigEndian.Uint16(pkt[off+2 : off+4]))
		off += 4 + 4*extLen
	}
	if off >= len(pkt) {
		return nil, false
	}
	return pkt[off:], true
}

// packAudioPacket frames one Opus packet: marker 0x03, no flags, seq,
// length — same 10-byte header family as video access units.
func packAudioPacket(opus []byte) []byte {
	pkt := make([]byte, videoSegmentHeaderSize+len(opus))
	pkt[0] = audioFrameMarker
	binary.LittleEndian.PutUint32(pkt[2:6], nextAudioSeq())
	binary.LittleEndian.PutUint32(pkt[6:10], uint32(len(opus)))
	copy(pkt[videoSegmentHeaderSize:], opus)
	return pkt
}

var audioSeq atomic.Uint32

func nextAudioSeq() uint32 { return audioSeq.Add(1) }

// audioToggleHandler is the streamer's /audio endpoint used by ToggleAudio.
func audioToggleHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	prev := audioOn.Swap(req.Enabled)
	if prev != req.Enabled {
		log.Printf("audio: %s", map[bool]string{true: "ON", false: "OFF"}[req.Enabled])
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"audio_enabled": req.Enabled})
}
