package orchestrator

import (
	"strings"
	"testing"
)

// TestContainerLabels pins the one-of-two label sets invariant. A warm-pool
// container that also carries isola.session_id is invisible-but-dangerous: the
// orphan sweep reads that placeholder id as an untracked session and terminates
// the container — even after it has been claimed and is serving a user.
func TestContainerLabels(t *testing.T) {
	t.Parallel()

	session := containerLabels("sess-123", "user-9", false)
	joined := strings.Join(session, " ")
	if !strings.Contains(joined, "isola.stack=1") {
		t.Errorf("session labels missing isola.stack=1: %v", session)
	}
	if !strings.Contains(joined, "isola.session_id=sess-123") {
		t.Errorf("session labels missing isola.session_id: %v", session)
	}
	if !strings.Contains(joined, "isola.user_id=user-9") {
		t.Errorf("session labels missing isola.user_id: %v", session)
	}
	if strings.Contains(joined, "isola.warm") {
		t.Errorf("session container must not be labeled warm: %v", session)
	}

	warm := containerLabels("warm-abc", "warm-pool", true)
	joinedWarm := strings.Join(warm, " ")
	if !strings.Contains(joinedWarm, "isola.stack=1") {
		t.Errorf("warm labels missing isola.stack=1: %v", warm)
	}
	if !strings.Contains(joinedWarm, "isola.warm=1") {
		t.Errorf("warm labels missing isola.warm=1: %v", warm)
	}
	if !strings.Contains(joinedWarm, "isola.warm_id=warm-abc") {
		t.Errorf("warm labels missing isola.warm_id: %v", warm)
	}
	if strings.Contains(joinedWarm, "isola.session_id") {
		t.Fatalf("warm container must NOT carry isola.session_id (orphan sweep would kill it): %v", warm)
	}
	if strings.Contains(joinedWarm, "isola.user_id") {
		t.Errorf("warm container must not carry a user id label: %v", warm)
	}

	// Labels must be passed as flag/value pairs.
	for i := 0; i < len(warm); i += 2 {
		if warm[i] != "--label" {
			t.Fatalf("label args not in --label/value pairs: %v", warm)
		}
	}
}
