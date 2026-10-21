package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

// TestSessionCacheKeys pins every cache entry that belongs to a session. The two
// teardown paths (FinalizeRunningSession and CleanupOrphanContainers) free this
// set through the same function, so a key added to one path only cannot exist —
// and a key silently dropped here is a leak, which this test catches.
func TestSessionCacheKeys(t *testing.T) {
	sid := "0e30b942-d68c-4a6f-9a11-2f0e2a1b3c4d"
	want := []string{
		"session:" + sid,
		"session:" + sid + ":transport",
		"session:" + sid + ":cdp",
		"session:" + sid + ":control_token",
		"session:" + sid + ":view_password",
		"viewer_count:" + sid,
	}

	got := sessionCacheKeys(sid)
	if len(got) != len(want) {
		t.Fatalf("sessionCacheKeys(%q) returned %d keys, want %d: %v", sid, len(got), len(want), got)
	}
	seen := make(map[string]bool, len(got))
	for _, key := range got {
		if seen[key] {
			t.Fatalf("sessionCacheKeys(%q) contains %q twice", sid, key)
		}
		seen[key] = true
	}
	for _, key := range want {
		if !seen[key] {
			t.Errorf("sessionCacheKeys(%q) is missing %q", sid, key)
		}
	}
}

// TestFinalizeRunningSessionClearsEveryCacheKey is the behavioural half: after a
// session is finalized nothing it cached may answer a later lookup.
func TestFinalizeRunningSessionClearsEveryCacheKey(t *testing.T) {
	stubDocker(t, "exit 0\n")

	ms := store.NewMemStore()
	defer ms.Close()
	st := store.NewMemBridgeStore(ms)
	cache := store.NewMemCache()
	defer cache.Close()

	id := seedRestored(t, ms, models.SessionStatusRunning, "alive123")
	sess := &models.Session{ID: id, Status: models.SessionStatusRunning, ContainerID: "alive123"}
	ctx := context.Background()
	for _, key := range sessionCacheKeys(id.String()) {
		if err := cache.Set(ctx, key, "value", time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	o, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	o.FinalizeRunningSession(ctx, st, cache, sess)

	for _, key := range sessionCacheKeys(id.String()) {
		if _, err := cache.Get(ctx, key); err == nil {
			t.Errorf("cache key %q survived finalization", key)
		}
	}
}
