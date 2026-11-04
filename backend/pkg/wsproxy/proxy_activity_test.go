package wsproxy

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestProxyTrackActivityDebounces pins the hot-path contract: trackActivity is
// called once per relayed message (25–50 times a second per viewer), so a
// second call inside activityDebounce must not write anything. Only presence
// in the tracker matters to flushActivity; the timestamp is the debounce clock.
func TestProxyTrackActivityDebounces(t *testing.T) {
	p := New(nil, nil, nil)

	load := func() int64 {
		slot, ok := p.activity.Load("sess-a")
		if !ok {
			t.Fatal("the first activity tick was not recorded")
		}
		return slot.(*atomic.Int64).Load()
	}

	p.trackActivity("sess-a")
	first := load()

	time.Sleep(2 * time.Millisecond)
	p.trackActivity("sess-a")
	if got := load(); got != first {
		t.Fatal("a tick inside the debounce window replaced the recorded timestamp")
	}

	// Past the window a new tick must land.
	original := activityDebounce
	activityDebounce = time.Millisecond
	defer func() { activityDebounce = original }()
	time.Sleep(5 * time.Millisecond)
	p.trackActivity("sess-a")
	if got := load(); got == first {
		t.Fatal("a tick past the debounce window was swallowed")
	}

	// Draining is what the flusher consumes, and it must empty the tracker so
	// sessions that went quiet stop being written every interval.
	drained := p.drainActivity()
	if len(drained) != 1 || drained[0] != "sess-a" {
		t.Fatalf("drainActivity() = %v, want [sess-a]", drained)
	}
	if _, ok := p.activity.Load("sess-a"); ok {
		t.Fatal("drainActivity left the entry behind")
	}
}

// TestProxyTrackActivityConcurrent hammers the tracker from many goroutines;
// with -race it proves the debounce path is race-free.
func TestProxyTrackActivityConcurrent(t *testing.T) {
	p := New(nil, nil, nil)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				p.trackActivity("sess-race")
				_ = p.drainActivity()
			}
		}()
	}
	wg.Wait()
}

// BenchmarkProxyTrackActivity measures the per-message cost the relay pays at
// 25–50 fps per viewer. The debounced path is a load plus a compare; the old
// implementation took the proxy-wide mutex on every message.
func BenchmarkProxyTrackActivity(b *testing.B) {
	p := New(nil, nil, nil)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p.trackActivity("sess-bench")
		}
	})
}
