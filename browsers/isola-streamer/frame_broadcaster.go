package main

import (
	"bytes"
	"log"
	"math"
	"sync"
	"time"
)

// frameBroadcaster owns the composed surface: damage-only updates cannot rebuild
// an image, so a joining/restarting viewer gets a full frame on its next tick.
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

// damageCounters: cumulative and monotonic — DamagedPx (per-rect area) vs BBoxPx
// (single rect), SurfacePx = what a full-frame encoder pays per tick. Difference per tick for exact rates.
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
		log.Printf("broadcaster: surface %dx%d stride=%d", f.Width, f.Height, f.Stride)
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

// record counts only pixel-changing updates; skipped frames never reach an encoder.
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

// damageRates: the per-second view. ChangesPerSec near zero while the viewer
// decodes 20 fps is the shape of a frozen picture every other metric calls healthy.
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
		// A full-width rect is one contiguous run: 2x faster than the per-row loop (72 vs 162 µs at 720p).
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

	log.Printf("broadcaster: +sub n=%d", n)
	return ch
}

// Unsubscribe removes a subscriber channel and closes it.
func (fb *frameBroadcaster) Unsubscribe(ch chan *Frame) {
	fb.mu.Lock()
	delete(fb.subs, ch)
	fb.mu.Unlock()
	close(ch)
}
