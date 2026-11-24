package main

import (
	"testing"
	"time"
)

func TestFrameURLKeyIgnoresQuery(t *testing.T) {
	a := frameURLKey("https://www.player.example/embed/abc123?autoplay=1&rel=0")
	b := frameURLKey("https://www.player.example/embed/abc123?wmode=transparent")
	if a != b {
		t.Fatalf("keys differ: %q vs %q", a, b)
	}
	if a != "player.example/embed/abc123" {
		t.Fatalf("key = %q", a)
	}
	if frameURLKey("https://player.example/embed/abc123") != a {
		t.Fatal("www and bare host must identify the same frame")
	}
}

func TestIframeHitContainsAndLocal(t *testing.T) {
	h := iframeHit{Hit: true, Src: "https://player.example/embed/x", L: 16, T: 517, W: 656, H: 369}
	if !h.contains(16, 517) || !h.contains(671, 885) {
		t.Fatal("inside edges must hit")
	}
	if h.contains(672, 517) || h.contains(16, 886) || h.contains(0, 0) {
		t.Fatal("outside the box must miss")
	}
	x, y := h.local(344, 700)
	if x != 328 || y != 183 {
		t.Fatalf("local = %d,%d want 328,183", x, y)
	}
	if (iframeHit{}).contains(10, 10) {
		t.Fatal("empty hit must miss")
	}
}

func TestParseIframeHit(t *testing.T) {
	h, ok := parseIframeHit(`{"hit":true,"src":"https://www.player.example/embed/abc","l":16,"t":517,"w":656,"h":369}`)
	if !ok || h.Src == "" || h.W != 656 {
		t.Fatalf("parsed %+v ok=%v", h, ok)
	}
	if _, ok := parseIframeHit(`{"hit":false}`); ok {
		t.Fatal("miss must not parse as a hit")
	}
	if _, ok := parseIframeHit(`{"hit":true,"src":"","w":10,"h":10}`); ok {
		t.Fatal("hit without a src cannot be routed")
	}
}

func TestMatchFrameTargetPrefersIframe(t *testing.T) {
	targets := []cdpTarget{
		{Type: "page", URL: "https://www.player.example/embed/abc?autoplay=1", WebSocketDebuggerURL: "ws://page"},
		{Type: "iframe", URL: "https://www.player.example/embed/abc?rel=0", WebSocketDebuggerURL: "ws://iframe"},
	}
	got, ok := matchFrameTarget(targets, "https://www.player.example/embed/abc?html5=1")
	if !ok || got != "ws://iframe" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	if _, ok := matchFrameTarget(targets, "https://other.example/?q=dd"); ok {
		t.Fatal("parent page must not match")
	}
}

func TestIframeRoutePlan(t *testing.T) {
	now := time.Now()
	hit := iframeHit{Hit: true, Src: "https://player.example/embed/x", L: 10, T: 10, W: 100, H: 50}
	fresh := now.Add(-100 * time.Millisecond)
	stale := now.Add(-time.Second)
	probed := now.Add(-10 * time.Millisecond)

	if got := iframeRoutePlan(now, hit, fresh, probed, false, "mousemove", 20, 20); got != planCached {
		t.Fatalf("fresh box: got %v want cached", got)
	}
	if got := iframeRoutePlan(now, hit, fresh, probed, false, "mousemove", 0, 0); got != planParent {
		t.Fatalf("outside, just probed: got %v want parent", got)
	}
	if got := iframeRoutePlan(now, hit, stale, now.Add(-time.Second), false, "mousemove", 20, 20); got != planProbe {
		t.Fatalf("stale box: got %v want probe", got)
	}
	if got := iframeRoutePlan(now, hit, fresh, probed, true, "mousemove", 0, 0); got != planCached {
		t.Fatalf("drag outside the box must stay on the frame")
	}
	if got := iframeRoutePlan(now, hit, fresh, probed, false, "mouseup", 20, 20); got != planParent {
		t.Fatalf("release without a press must stay on the parent")
	}
	if got := iframeRoutePlan(now, hit, fresh, probed, true, "mouseup", 0, 0); got != planCached {
		t.Fatalf("release of a frame press must stay on the frame")
	}
	if got := iframeRoutePlan(now, hit, fresh, probed, false, "mousedown", 20, 20); got != planCached {
		t.Fatalf("press inside a fresh box: got %v want cached", got)
	}
	if got := iframeRoutePlan(now, hit, fresh, probed, false, "mousedown", 0, 0); got != planProbe {
		t.Fatalf("press outside must probe even just after a probe")
	}
}

func TestMouseButtonParamsCarriesClickCount(t *testing.T) {
	t.Parallel()
	params := mouseButtonParams("mousePressed", "left", 10, 20, 2)
	if got, _ := params["clickCount"].(int); got != 2 {
		t.Fatalf("clickCount = %v, want 2", params["clickCount"])
	}
	// A streamer client that predates click_count must still produce a click.
	params = mouseButtonParams("mouseReleased", "left", 10, 20, 0)
	if got, _ := params["clickCount"].(int); got != 1 {
		t.Fatalf("default clickCount = %v, want 1", params["clickCount"])
	}
}
