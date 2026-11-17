package main

import (
	"bytes"
	"log"
	"math"
	"sync"
	"time"
)

// frameBroadcaster owns the session's composed surface and fans damage
// updates out to the connected viewer sessions.
//
// The producer transmits damage only, so a viewer cannot rebuild the image
// from updates alone — it needs one complete surface to start from. Keeping
// the composed image here means a viewer that joins mid-session, or whose
// encoder has just restarted, is served a full frame on its next tick instead
// of waiting for the producer to repaint the whole viewport.
type frameBroadcaster struct {
	mu       sync.RWMutex
	subs     map[chan *Frame]struct{}
	incoming <-chan *Frame

	// surface is the most recent complete image, updated in place by applying
	// each incoming frame's damage. Encoders copy it under RLock, so they
	// never observe a half-applied update.
	surface []byte
	surfH   uint32
	surfStr uint32

	// changeSeq increments on every real pixel change and lastChangeAt marks
	// when it happened. The encode loop consults them to skip static frames.
	changeSeq    uint64
	lastChangeAt time.Time

	// damage accumulates the geometry of every applied update so the cost of
	// region-based encoding can be judged from a real session instead of an
	// assumption. See damageCounters.
	damage damageCounters

	// latest is the most recent update. New subscribers receive it
	// immediately so an encoder knows the stream is live without waiting for
	// the next paint.
	latest *Frame
}

// damageCounters aggregates the geometry of the updates applied to the
// composed surface.
//
// DamagedPx is the area the producer reported (summed over rects) and BBoxPx is
// the single bounding box those rects occupy — a region encoder that ships one
// rectangle per update pays the latter, one that ships each rect pays the
// former. SurfacePx is the full surface area the current full-frame encoder
// pays on every fed tick, so DamagedPx/SurfacePx is the fraction of the frame a
// region-based design would actually have to encode.
//
// Counters are cumulative and monotonic; a consumer that samples them at each
// fed tick can difference them to get exact per-frame numbers.
type damageCounters struct {
	Updates    uint64
	FullFrames uint64
	DamagedPx  uint64
	BBoxPx     uint64
	SurfacePx  uint64
}

func newFrameBroadcaster(incoming <-chan *Frame) *frameBroadcaster {
	fb := &frameBroadcaster{
		subs:     make(map[chan *Frame]struct{}),
		incoming: incoming,
	}
	go fb.run()
	return fb
}

func (fb *frameBroadcaster) run() {
	for f := range fb.incoming {
		fb.mu.Lock()
		fb.composeLocked(f)
		fb.latest = f

		for ch := range fb.subs {
			select {
			case ch <- f:
			default:
				// The notice carries no pixels a consumer depends on — they
				// read the composed surface instead — so dropping the oldest
				// pending notice is safe.
				select {
				case <-ch:
				default:
				}
				select {
				case ch <- f:
				default:
				}
			}
		}
		fb.mu.Unlock()
	}
}

// composeLocked applies a frame's damage to the composed surface, resizing it
// when the producer's geometry changes (resize, tab switch, restart).
func (fb *frameBroadcaster) composeLocked(f *Frame) {
	need := int(f.Stride) * int(f.Height)
	changed := false
	if need != len(fb.surface) || fb.surfStr != f.Stride {
		fb.surface = make([]byte, need)
		fb.surfH, fb.surfStr = f.Height, f.Stride
		changed = true
		log.Printf("broadcaster: surface now %dx%d stride=%d", f.Width, f.Height, f.Stride)
	}
	if applyRects(fb.surface, fb.surfStr, f) {
		changed = true
		fb.damage.record(f)
	}
	if changed {
		fb.changeSeq++
		fb.lastChangeAt = time.Now()
	}
}

// record folds one update's geometry into the cumulative damage counters.
// Only updates that actually changed a pixel are recorded: a frame the
// composer skipped never reaches an encoder, so counting it would dilute the
// measured damage fraction with frames that cost nothing.
func (dc *damageCounters) record(f *Frame) {
	if len(f.Dirty) == 0 {
		return
	}
	var damaged uint64
	minX, minY := f.Width, f.Height
	var maxX, maxY uint32
	for _, r := range f.Dirty {
		damaged += uint64(r.W) * uint64(r.H)
		if r.X < minX {
			minX = r.X
		}
		if r.Y < minY {
			minY = r.Y
		}
		if r.X+r.W > maxX {
			maxX = r.X + r.W
		}
		if r.Y+r.H > maxY {
			maxY = r.Y + r.H
		}
	}
	dc.DamagedPx += damaged
	dc.BBoxPx += uint64(maxX-minX) * uint64(maxY-minY)
	dc.SurfacePx += uint64(f.Stride/4) * uint64(f.Height)
	dc.Updates++
	if f.IsFull() {
		dc.FullFrames++
	}
}

// DamageCounters returns a snapshot of the cumulative capture-damage geometry.
func (fb *frameBroadcaster) DamageCounters() damageCounters {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	return fb.damage
}

// damageRates is the per-second view of the cumulative damage counters: how
// many updates actually changed pixels, how many of those covered the whole
// surface, and how much of the surface one changed update touched on average.
//
// The cumulative counters alone cannot answer the question a frozen-picture
// report asks — "is this picture changing right now?" — because a session that
// has been static for a minute and one that went static a second ago have the
// same totals. The rates can: ChangesPerSec near zero while the viewer decodes
// 20 fps means the encoder is being fed duplicate frames, which is the shape of
// a frozen picture that every other metric calls healthy.
type damageRates struct {
	ChangesPerSec int64
	FullPerSec    int64
	// DamagedPct is the mean share of the surface one changed update touched,
	// in percent: near zero means a handful of pixels, 100 a full frame.
	DamagedPct float64
}

// damageMeter differences two snapshots of the cumulative damage counters.
type damageMeter struct {
	prev   damageCounters
	prevAt time.Time
}

// sample returns the rates since the previous call and moves the baseline. The
// first call only establishes the baseline and reports zeroes.
func (m *damageMeter) sample(cur damageCounters, now time.Time) damageRates {
	first := m.prevAt.IsZero()
	dt := now.Sub(m.prevAt).Seconds()
	prev := m.prev
	m.prev, m.prevAt = cur, now
	if first || dt <= 0 {
		return damageRates{}
	}

	updates := deltaU64(cur.Updates, prev.Updates)
	rates := damageRates{
		ChangesPerSec: int64(math.Round(float64(updates) / dt)),
		FullPerSec:    int64(math.Round(float64(deltaU64(cur.FullFrames, prev.FullFrames)) / dt)),
	}
	if surface := deltaU64(cur.SurfacePx, prev.SurfacePx); surface > 0 {
		pct := 100 * float64(deltaU64(cur.DamagedPx, prev.DamagedPx)) / float64(surface)
		rates.DamagedPct = math.Round(math.Min(pct, 100)*100) / 100
	}
	return rates
}

// deltaU64 differences two cumulative counters. A decrease means the counters
// were reset under us — the broadcaster is replaced on resize and encoder
// restarts — so the new total is the delta.
func deltaU64(cur, prev uint64) uint64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}

// applyRects copies a frame's damage payload into dst, which must be at least
// stride*height bytes, and reports whether any pixel actually differed.
// Payload layout matches Dirty order.
func applyRects(dst []byte, stride uint32, f *Frame) bool {
	changed := false
	off := 0
	for _, r := range f.Dirty {
		rowBytes := int(r.W) * 4
		src := off
		dstRow := int(r.Y)*int(stride) + int(r.X)*4
		// A full-width rect is one contiguous run in both buffers, which is
		// exactly what a full repaint sends. The per-row loop below pays 720
		// bytes.Equal + 720 copy calls for it (162 µs vs 72 µs measured on
		// 1280x720); a single pair over the run is 2x faster.
		if r.X == 0 && rowBytes == int(stride) {
			n := rowBytes * int(r.H)
			if !bytes.Equal(dst[dstRow:dstRow+n], f.Payload[src:src+n]) {
				changed = true
				copy(dst[dstRow:dstRow+n], f.Payload[src:src+n])
			}
			off += n
			continue
		}
		for range int(r.H) {
			if !bytes.Equal(dst[dstRow:dstRow+rowBytes], f.Payload[src:src+rowBytes]) {
				changed = true
			}
			copy(dst[dstRow:dstRow+rowBytes], f.Payload[src:src+rowBytes])
			src += rowBytes
			dstRow += int(stride)
		}
		off += rowBytes * int(r.H)
	}
	return changed
}

// SurfaceDims reports the composed surface geometry, or ok=false when the
// producer has not sent anything yet.
func (fb *frameBroadcaster) SurfaceDims() (stride, height uint32, ok bool) {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	if len(fb.surface) == 0 {
		return 0, 0, false
	}
	return fb.surfStr, fb.surfH, true
}

// ChangeState reports the sequence number and timestamp of the last real pixel
// change to the composed surface. It returns (0, zero time) when no surface is
// available yet.
func (fb *frameBroadcaster) ChangeState() (seq uint64, lastChange time.Time) {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	if len(fb.surface) == 0 {
		return 0, time.Time{}
	}
	return fb.changeSeq, fb.lastChangeAt
}

// Snapshot copies the current composed surface into dst, which must match the
// live surface size. It reports false when no surface is available yet or the
// geometry changed under the caller.
func (fb *frameBroadcaster) Snapshot(dst []byte) bool {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	if len(fb.surface) == 0 || len(fb.surface) != len(dst) {
		return false
	}
	copy(dst, fb.surface)
	return true
}

// Subscribe returns a channel that receives frame notices. The latest frame
// (if any) is sent immediately, so an encoder always has data to work with.
func (fb *frameBroadcaster) Subscribe() chan *Frame {
	ch := make(chan *Frame, 1)

	fb.mu.Lock()
	if fb.latest != nil {
		ch <- fb.latest
	}
	fb.subs[ch] = struct{}{}
	n := len(fb.subs)
	fb.mu.Unlock()

	log.Printf("broadcaster: subscriber added (total=%d, seeded=%v)", n, fb.latest != nil)
	return ch
}

func (fb *frameBroadcaster) subscriberCount() int {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	return len(fb.subs)
}

// Unsubscribe removes a subscriber channel and closes it.
func (fb *frameBroadcaster) Unsubscribe(ch chan *Frame) {
	fb.mu.Lock()
	delete(fb.subs, ch)
	fb.mu.Unlock()
	close(ch)
	log.Printf("broadcaster: subscriber removed (total=%d)", fb.subscriberCount())
}
