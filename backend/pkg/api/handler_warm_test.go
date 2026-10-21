package api

import (
	"context"
	"testing"

	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

// TestWarmResizeTarget pins when a claimed container must be reshaped: only a
// request that fits inside the pooled box, and never when the sizes already
// agree. Anything unparsable leaves the container at its pooled size.
func TestWarmResizeTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		baked, requested string
		wantW, wantH     int
		want             bool
	}{
		{"1920x1080", "1920x1080", 0, 0, false},
		{"1920x1080", "1728x1080", 1728, 1080, true},
		{"1920x1080", "1920x960", 1920, 960, true},
		{"1920x1080", "1280x720", 1280, 720, true},
		{"1920x1080", "1920x1200", 0, 0, false},
		{"1920x1080", "2560x1440", 0, 0, false},
		{"1920x1080", "", 0, 0, false},
		{"", "1280x720", 0, 0, false},
		{"nonsense", "1280x720", 0, 0, false},
		{"1920x1080", "nonsense", 0, 0, false},
	}

	for _, tc := range cases {
		w, h, ok := warmResizeTarget(tc.baked, tc.requested)
		if ok != tc.want || w != tc.wantW || h != tc.wantH {
			t.Errorf("warmResizeTarget(%q, %q) = (%d, %d, %v), want (%d, %d, %v)",
				tc.baked, tc.requested, w, h, ok, tc.wantW, tc.wantH, tc.want)
		}
	}
}

// TestResizeDisplayUsesInjectedFn pins the test seam: a warm claim must reshape
// through the injected function rather than touching a Docker daemon.
func TestResizeDisplayUsesInjectedFn(t *testing.T) {
	t.Parallel()

	h := New(store.NewMemBridgeStore(store.NewMemStore()), store.NewMemCache(), nil, &config.Config{})
	var gotW, gotH, gotPort int
	h.resizeDisplayFn = func(ctx context.Context, host string, port, width, height int) error {
		gotPort, gotW, gotH = port, width, height
		return nil
	}

	if err := h.resizeDisplay(context.Background(), "127.0.0.1", 5900, 1728, 1080); err != nil {
		t.Fatalf("resizeDisplay = %v, want nil", err)
	}
	if gotPort != 5900 || gotW != 1728 || gotH != 1080 {
		t.Fatalf("resize call = port %d %dx%d, want 5900 1728x1080", gotPort, gotW, gotH)
	}
}
