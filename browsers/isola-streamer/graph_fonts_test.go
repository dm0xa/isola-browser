package main

import (
	"sync"
	"testing"

	"golang.org/x/image/font"
)

// TestFaceConcurrent builds faces from several goroutines, one canvas each.
//
// Run with -race. net/http serves every /graph.png request in its own
// goroutine; a shared face cache used to be both a fatal map race and a
// corrupted glyph buffer (opentype.Face keeps an internal *sfnt.Buffer and is
// documented as not safe for concurrent use). This pins the per-render design.
func TestFaceConcurrent(t *testing.T) {
	kinds := []string{"regular", "bold", "mono", "monob", "unknown"}
	const workers = 24
	const iterations = 25

	faces := make(chan font.Face, workers*iterations)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			c := &gCanvas{scale: 1}
			for i := 0; i < iterations; i++ {
				kind := kinds[(worker+i)%len(kinds)]
				size := float64(6 + (worker*iterations+i)%40)
				f := c.face(kind, size)
				if f == nil {
					t.Errorf("face(%q, %v) returned nil", kind, size)
					return
				}
				faces <- f
			}
		}(w)
	}
	wg.Wait()
	close(faces)

	if len(faces) != workers*iterations {
		t.Fatalf("got %d faces, want %d", len(faces), workers*iterations)
	}
}

// TestFaceCacheIsPerCanvas proves two renders never share a face instance, and
// that one render reuses its own cache instead of rebuilding.
func TestFaceCacheIsPerCanvas(t *testing.T) {
	a := &gCanvas{scale: 1}
	b := &gCanvas{scale: 1}

	first := a.face("regular", 12)
	if again := a.face("regular", 12); again != first {
		t.Fatal("a canvas rebuilt a face it already had cached")
	}
	if len(a.faces) != 1 {
		t.Fatalf("canvas cache holds %d faces after one key, want 1", len(a.faces))
	}

	other := b.face("regular", 12)
	if other == first {
		t.Fatal("two canvases share one font face — opentype.Face is not safe for concurrent use")
	}
	if len(b.faces) != 1 {
		t.Fatalf("second canvas cache holds %d faces, want 1", len(b.faces))
	}
}
