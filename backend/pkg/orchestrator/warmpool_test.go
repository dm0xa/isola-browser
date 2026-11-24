package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dm0xa/isola-browser/backend/pkg/config"
)

// newTestPool builds a WarmPool whose provisioner/prober are fakes, so the
// claim/reconcile logic is testable without a Docker daemon.
func newTestPool(t *testing.T, size int) (*WarmPool, *[]string, *[]string) {
	t.Helper()
	cfg := &config.Config{
		WarmPoolEnabled: true,
		WarmPoolSize:    size,
		Resolution:      "1920x1080",
		BrowserTheme:    "dark",
	}
	p := NewWarmPool(nil, cfg)
	// Tests must not pay the production spacing between fills.
	p.fillGap = 0

	var fakeMu sync.Mutex
	created, terminated := &[]string{}, &[]string{}
	seq := 0
	p.provisioner = func(ctx context.Context, params WarmPoolParams) (*SessionInfo, error) {
		fakeMu.Lock()
		defer fakeMu.Unlock()
		seq++
		id := "warm-container-" + string(rune('a'+seq-1))
		*created = append(*created, id)
		return &SessionInfo{
			SessionID:   params.SessionID,
			ContainerID: id,
			Host:        "127.0.0.1",
			Port:        5900,
			CdpPort:     9223,
		}, nil
	}
	p.prober = func(ctx context.Context, info *SessionInfo) error {
		fakeMu.Lock()
		defer fakeMu.Unlock()
		for _, id := range *terminated {
			if id == info.ContainerID {
				return errors.New("container dead")
			}
		}
		return nil
	}
	p.stop = func(ctx context.Context, containerID string) {
		fakeMu.Lock()
		defer fakeMu.Unlock()
		*terminated = append(*terminated, containerID)
	}
	p.navigator = func(ctx context.Context, host string, port int, targetURL string) error {
		return nil
	}
	return p, created, terminated
}

func TestWarmPoolDisabledByDefault(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{WarmPoolEnabled: false, WarmPoolSize: 1, Resolution: "1920x1080", BrowserTheme: "dark"}
	p := NewWarmPool(nil, cfg)
	if got, _ := p.Claim(context.Background(), "1920x1080", "dark", 30, false, ""); got != nil {
		t.Fatalf("Claim on disabled pool = %+v, want nil", got)
	}
}

func TestWarmPoolClaimMatchesExactParams(t *testing.T) {
	t.Parallel()
	p, created, terminated := newTestPool(t, 1)

	p.fillOne(context.Background(), warmMode{})
	if len(*created) != 1 {
		t.Fatalf("provisioner calls = %d, want 1", len(*created))
	}

	// Wrong theme must not claim and must not kill the container.
	if got, _ := p.Claim(context.Background(), "1920x1080", "light", 30, false, ""); got != nil {
		t.Fatalf("Claim with mismatched theme = %+v, want nil", got)
	}
	// A box larger than the pooled one cannot be served by this container.
	if got, _ := p.Claim(context.Background(), "2560x1440", "dark", 30, false, ""); got != nil {
		t.Fatalf("Claim for a larger resolution = %+v, want nil", got)
	}
	// Matching params claim the container.
	if got, _ := p.Claim(context.Background(), "1920x1080", "dark", 30, false, ""); got == nil {
		t.Fatal("Claim with matching params = nil, want container")
	}
	if len(*terminated) != 0 {
		t.Fatalf("terminated = %v, want none (claimed containers stay alive)", *terminated)
	}

	// The claim must start a replacement immediately: a browser boot takes tens
	// of seconds, so waiting for the next reconcile tick means the next user
	// pays the cold path too.
	waitForPoolSize(t, p, 1, 3*time.Second)
}

// waitForPoolSize polls until the pool holds want containers or the deadline
// passes. Used for the async refill paths.
func waitForPoolSize(t *testing.T, p *WarmPool, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		p.mu.Lock()
		n := len(p.warm)
		p.mu.Unlock()
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool size = %d, want %d within %v", n, want, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// countForTest reads the pool's idle count for a mode under the pool lock.
func (p *WarmPool) countForTest(mode warmMode) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.warmCountLocked(mode)
}

// TestWarmPoolClaimMatchesTorMode pins the baked-mode contract: a Tor session
// can only be served by a container created with Tor on (the proxy and the
// egress scoping are set at boot), and a plain request must never take the
// Tor-baked box.
func TestWarmPoolClaimMatchesTorMode(t *testing.T) {
	t.Parallel()
	p, created, _ := newTestPool(t, 1)
	p.torSize = 1
	p.torCountry = "se"

	if !p.fillOne(context.Background(), warmMode{}) {
		t.Fatal("plain fillOne failed")
	}
	if !p.fillOne(context.Background(), warmMode{tor: true, country: "se"}) {
		t.Fatal("tor fillOne failed")
	}
	if len(*created) != 2 {
		t.Fatalf("provisioner calls = %d, want one per mode", len(*created))
	}

	ctx := context.Background()
	// A pinned exit country the pool was not built for must not borrow the box.
	if got, _ := p.Claim(ctx, "1920x1080", "dark", 30, true, "no"); got != nil {
		t.Fatalf("Claim with a different Tor exit country = %+v, want nil", got)
	}
	// The plain container must not serve a Tor session (it has no proxy).
	if got, _ := p.Claim(ctx, "1920x1080", "dark", 30, false, ""); got == nil {
		t.Fatal("Claim for the plain mode = nil, want the plain container")
	}
	if got, _ := p.Claim(ctx, "1920x1080", "dark", 30, true, "se"); got == nil {
		t.Fatal("Claim for the Tor mode = nil, want the Tor-baked container")
	}
	// Both are now claimed; a further claim of either mode must miss.
	if got, _ := p.Claim(ctx, "1920x1080", "dark", 30, true, "se"); got != nil {
		t.Fatalf("Claim with an empty pool = %+v, want nil", got)
	}
}

// TestWarmPoolFillsPlainAndTorSlots checks the fill worker keeps one slot per
// enabled mode, and that a failed Tor fill does not stop the plain slot.
func TestWarmPoolFillsPlainAndTorSlots(t *testing.T) {
	t.Parallel()
	p, _, _ := newTestPool(t, 1)
	p.torSize = 1
	p.torCountry = ""

	p.replenish(context.Background()).Wait()

	if got := p.countForTest(warmMode{}); got != 1 {
		t.Fatalf("plain containers = %d, want 1", got)
	}
	if got := p.countForTest(warmMode{tor: true}); got != 1 {
		t.Fatalf("tor containers = %d, want 1", got)
	}
}

// TestWarmPoolTorFailureBacksOff: a network that cannot bootstrap Tor must not
// boot and kill a container every reconcile tick, and the plain slot must not
// be starved by the Tor attempt's budget.
func TestWarmPoolTorFailureBacksOff(t *testing.T) {
	t.Parallel()
	p, _, _ := newTestPool(t, 1)
	p.torSize = 1

	failTor := true
	p.provisioner = func(ctx context.Context, params WarmPoolParams) (*SessionInfo, error) {
		if params.Mode.tor && failTor {
			return nil, errors.New("tor bootstrap failed")
		}
		return &SessionInfo{SessionID: params.SessionID, ContainerID: "warm-" + params.Mode.String(), Host: "127.0.0.1", Port: 5900}, nil
	}

	p.replenish(context.Background()).Wait()

	p.mu.Lock()
	blockedUntil := p.torFillsBlockedUntil
	p.mu.Unlock()
	if !blockedUntil.After(p.nower()) {
		t.Fatalf("torFillsBlockedUntil = %v, want a future backoff", blockedUntil)
	}
	if got := p.countForTest(warmMode{}); got != 1 {
		t.Fatalf("plain containers = %d, want 1 (a Tor failure must not starve the plain slot)", got)
	}

	// Within the backoff window Tor fills are skipped entirely.
	p.mu.Lock()
	fillMode, ok := p.nextFillLocked()
	p.mu.Unlock()
	if ok && fillMode.tor {
		t.Fatal("nextFillLocked picked a Tor fill inside the backoff window")
	}

	// After the backoff a Tor fill is attempted again (and can succeed).
	p.mu.Lock()
	p.torFillsBlockedUntil = time.Time{}
	p.mu.Unlock()
	failTor = false
	p.replenish(context.Background()).Wait()
	if got := p.countForTest(warmMode{tor: true}); got != 1 {
		t.Fatalf("tor containers after backoff = %d, want 1", got)
	}
}

func TestWarmPoolClaimRequiresExactResolution(t *testing.T) {
	t.Parallel()
	p, _, terminated := newTestPool(t, 1)

	p.fillOne(context.Background(), warmMode{})

	// A claim must need no reshape. Reshaping an idle offscreen window before a
	// viewer attaches costs seconds (Chromium applies the resize lazily), which
	// measured worse than the cold start it was meant to save — so a smaller
	// request cold-starts instead of claiming.
	if info, _ := p.Claim(context.Background(), "1728x1080", "dark", 30, false, ""); info != nil {
		t.Fatalf("Claim for a smaller resolution = %+v, want nil (must not reshape)", info)
	}
	if len(*terminated) != 0 {
		t.Fatalf("terminated = %v, want none (the container stays pooled)", *terminated)
	}
	if len(p.warm) != 1 {
		t.Fatalf("pool = %d, want the container kept", len(p.warm))
	}

	info, baked := p.Claim(context.Background(), "1920x1080", "dark", 30, false, "")
	if info == nil {
		t.Fatal("Claim at the baked resolution = nil, want the pooled container")
	}
	if baked != "1920x1080" {
		t.Fatalf("baked resolution = %q, want 1920x1080", baked)
	}
}

func TestWarmPoolClaimRejectsResolutionsItCannotServe(t *testing.T) {
	t.Parallel()
	p, _, terminated := newTestPool(t, 1)

	p.fillOne(context.Background(), warmMode{})

	// Wider, taller, or unparsable: none of these can be clamped inside the
	// baked box, so they must cold-start and leave the container pooled.
	for _, res := range []string{"1920x1200", "2560x1440", "nonsense", ""} {
		if got, _ := p.Claim(context.Background(), res, "dark", 30, false, ""); got != nil {
			t.Fatalf("Claim for %q = %+v, want nil", res, got)
		}
	}
	if len(*terminated) != 0 {
		t.Fatalf("terminated = %v, want none", *terminated)
	}
	if len(p.warm) != 1 {
		t.Fatalf("pool = %d, want the container kept", len(p.warm))
	}
}

func TestWarmPoolEmptyPoolClaimMisses(t *testing.T) {
	t.Parallel()
	p, _, terminated := newTestPool(t, 1)
	// Provisioning keeps failing, so the pool never fills and claims must miss
	// (and must not tear anything down).
	p.provisioner = func(ctx context.Context, params WarmPoolParams) (*SessionInfo, error) {
		return nil, errors.New("no capacity")
	}

	p.reconcile(context.Background())

	if got, _ := p.Claim(context.Background(), "1920x1080", "dark", 30, false, ""); got != nil {
		t.Fatalf("Claim on empty pool = %+v, want nil", got)
	}
	if len(*terminated) != 0 {
		t.Fatalf("terminated = %v, want none", *terminated)
	}
}

func TestWarmPoolClaimDropsDeadContainer(t *testing.T) {
	t.Parallel()
	p, _, terminated := newTestPool(t, 1)

	p.fillOne(context.Background(), warmMode{})
	// The fake prober treats the container as dead once it appears in the
	// terminated list, so kill it manually and observe the claim probe.
	claimID := p.warm[0].info.ContainerID
	*terminated = append(*terminated, claimID)

	if got, _ := p.Claim(context.Background(), "1920x1080", "dark", 30, false, ""); got != nil {
		t.Fatalf("Claim of dead container = %+v, want nil", got)
	}
	// The claim probe retries once, then tears the container down.
	if len(*terminated) != 2 {
		t.Fatalf("terminated = %v, want the dead container torn down", *terminated)
	}
}

// Regression: the teardown paths used to call the test-only p.stop hook
// directly. NewWarmPool never installs it, so an idle container that died in
// production (OOM, runtime restart) panicked the whole process from the
// reconcile or claim goroutine. A pool with no stop hook and no orchestrator
// must prune quietly instead.
func TestWarmPoolTeardownWithoutStopHookDoesNotPanic(t *testing.T) {
	t.Parallel()
	p, _, _ := newTestPool(t, 1)
	p.stop = nil // production shape: the hook is only installed by tests

	p.prober = func(ctx context.Context, info *SessionInfo) error {
		return errors.New("container dead")
	}
	if !p.fillOne(context.Background(), warmMode{}) {
		t.Fatal("fillOne failed")
	}
	p.mu.Lock()
	victim := p.warm[0].info.ContainerID
	p.mu.Unlock()

	// Claim path: the last-mile probe fails, so the claimed container is
	// discarded and the pool refills.
	if got, _ := p.Claim(context.Background(), "1920x1080", "dark", 30, false, ""); got != nil {
		t.Fatalf("Claim of dead container = %+v, want nil", got)
	}

	// Reconcile path: two strikes prune the container the refill added.
	p.reconcile(context.Background())
	p.reconcile(context.Background())
	if poolTracks(p, victim) {
		t.Fatalf("dead container %s still tracked after reconcile", victim)
	}
}

func TestWarmPoolReconcileRefillsAndPrunes(t *testing.T) {
	t.Parallel()
	p, created, terminated := newTestPool(t, 2)

	p.reconcile(context.Background())
	if len(*created) != 2 {
		t.Fatalf("created = %v, want 2 warm containers", *created)
	}

	// Kill one. A single failed probe is only a strike (mirroring the session
	// health checker), so the container must survive the first pass.
	p.mu.Lock()
	victim := p.warm[0].info.ContainerID
	p.mu.Unlock()
	*terminated = append(*terminated, victim)

	p.reconcile(context.Background())
	if len(*created) != 2 {
		t.Fatalf("created = %v, want 2 (one strike must not replace)", *created)
	}
	if !poolTracks(p, victim) {
		t.Fatal("one failed probe pruned a container; a transient probe failure must not cost a warm container")
	}

	// The second consecutive failure prunes the dead container and replaces it.
	p.reconcile(context.Background())
	if len(*created) != 3 {
		t.Fatalf("created = %v, want 3 (one replacement)", *created)
	}
	if poolTracks(p, victim) {
		t.Fatal("dead container still tracked by the pool after two strikes")
	}
	p.mu.Lock()
	tracked := len(p.warm)
	p.mu.Unlock()
	if tracked != 2 {
		t.Fatalf("pool size = %d, want 2", tracked)
	}
}

// poolTracks reports whether the pool still holds a container by id.
func poolTracks(p *WarmPool, containerID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, wc := range p.warm {
		if wc.info.ContainerID == containerID {
			return true
		}
	}
	return false
}

func TestWarmPoolNavigateRequest(t *testing.T) {
	t.Parallel()
	// The streamer /navigate endpoint receives the escaped target URL. This
	// locks the wire format the browser image implements.
	target := "https://example.com/?q=a b"
	req := navigateRequestURL("127.0.0.1", 5900, target)
	want := "http://127.0.0.1:5900/navigate?url=" + "https%3A%2F%2Fexample.com%2F%3Fq%3Da+b"
	if req != want {
		t.Fatalf("navigate URL = %q, want %q", req, want)
	}
}

func TestWarmPoolDisableOnUnsupportedNavigate(t *testing.T) {
	t.Parallel()
	p, _, _ := newTestPool(t, 1)
	p.NotifyNavigateDisabled()
	if !p.Disabled() {
		t.Fatal("pool should be disabled after NotifyNavigateDisabled")
	}
	if got, _ := p.Claim(context.Background(), "1920x1080", "dark", 30, false, ""); got != nil {
		t.Fatalf("Claim on disabled pool = %+v, want nil", got)
	}
}

func TestWarmPoolFillDisablesOnOldImage(t *testing.T) {
	t.Parallel()
	p, _, terminated := newTestPool(t, 1)
	// Simulate a browser image whose streamer predates /navigate: the
	// capability probe must disable the pool and never publish the container.
	p.navigator = func(ctx context.Context, host string, port int, targetURL string) error {
		return ErrNavigateUnsupported
	}

	p.fillOne(context.Background(), warmMode{})

	if !p.Disabled() {
		t.Fatal("pool should disable itself when the image cannot navigate")
	}
	if len(p.warm) != 0 {
		t.Fatalf("pool holds %d containers, want 0 (unusable image)", len(p.warm))
	}
	if len(*terminated) != 1 {
		t.Fatalf("terminated = %v, want the unusable container stopped", *terminated)
	}
}

func TestWarmPoolDrainStopsAll(t *testing.T) {
	t.Parallel()
	p, _, terminated := newTestPool(t, 2)
	p.fillOne(context.Background(), warmMode{})
	p.fillOne(context.Background(), warmMode{})
	p.Drain(context.Background())
	if len(*terminated) != 2 {
		t.Fatalf("terminated = %v, want both warm containers stopped", *terminated)
	}
	if len(p.warm) != 0 {
		t.Fatalf("pool holds %d containers after drain, want 0", len(p.warm))
	}
	// After draining, the pool must stop serving and stop filling.
	if !p.Disabled() {
		t.Fatal("pool should be disabled after Drain")
	}
	if got, _ := p.Claim(context.Background(), "1920x1080", "dark", 30, false, ""); got != nil {
		t.Fatalf("Claim after Drain = %+v, want nil", got)
	}
	// A second Drain is a no-op, not a double-stop of the same container.
	p.Drain(context.Background())
	if len(*terminated) != 2 {
		t.Fatalf("terminated = %v, want no additional stops", *terminated)
	}
}

// TestWarmPoolDrainStopsInFlightFill pins the mid-fill shutdown case: the
// container exists on the host (the daemon created it, provisionWarm marked it
// pending) but its fill has not published it yet. Drain must still stop it, or
// it lingers until the next start's leftover sweep.
func TestWarmPoolDrainStopsInFlightFill(t *testing.T) {
	t.Parallel()
	p, _, terminated := newTestPool(t, 1)

	// A fill is mid-boot: the container exists but is not in the pool yet.
	p.markPending("warm-inflight")
	if len(p.warm) != 0 {
		t.Fatalf("precondition: pool should be empty, has %d", len(p.warm))
	}

	p.Drain(context.Background())

	if len(*terminated) != 1 || (*terminated)[0] != "warm-inflight" {
		t.Fatalf("terminated = %v, want the in-flight container stopped", *terminated)
	}
}

func TestWarmPoolOrphanCollectorKeepsActiveSessions(t *testing.T) {
	t.Parallel()
	p, _, terminated := newTestPool(t, 1)
	// Two warm-labeled containers survived a backend restart; one is backing
	// a live session (in the DB), the other is a true leftover.
	leftover, live := "warm-leftover", "warm-live-session"
	p.sweepLister = func(ctx context.Context) []string { return []string{leftover, live} }
	p.SetActiveContainers(func(ctx context.Context) (map[string]bool, error) {
		return map[string]bool{live: true}, nil
	})

	p.collectOrphans(context.Background())

	if len(*terminated) != 1 || (*terminated)[0] != leftover {
		t.Fatalf("terminated = %v, want exactly the leftover container", *terminated)
	}
}

func TestWarmPoolOrphanCollectorKeepsPooledContainers(t *testing.T) {
	t.Parallel()
	p, _, terminated := newTestPool(t, 1)

	p.fillOne(context.Background(), warmMode{})
	pooled := p.warm[0].info.ContainerID
	p.sweepLister = func(ctx context.Context) []string { return []string{pooled, "warm-orphan"} }

	p.collectOrphans(context.Background())

	if len(*terminated) != 1 || (*terminated)[0] != "warm-orphan" {
		t.Fatalf("terminated = %v, want only the untracked container", *terminated)
	}
}

func TestWarmPoolOrphanCollectorMatchesTruncatedIDs(t *testing.T) {
	t.Parallel()
	p, _, terminated := newTestPool(t, 1)

	p.fillOne(context.Background(), warmMode{})
	pooled := p.warm[0].info.ContainerID
	// `docker ps --format '{{.ID}}'` reports a 12-character prefix while the
	// create reported the full ID; the pool's own container must survive.
	p.sweepLister = func(ctx context.Context) []string { return []string{pooled[:12], "warm-orphan"} }

	p.collectOrphans(context.Background())

	if len(*terminated) != 1 || (*terminated)[0] != "warm-orphan" {
		t.Fatalf("terminated = %v, want only the untracked container", *terminated)
	}
}

func TestWarmPoolOrphanCollectorMatchesTruncatedActiveIDs(t *testing.T) {
	t.Parallel()
	p, _, terminated := newTestPool(t, 1)
	live := "64b17003e6e31dc34dfdf27f774ff285d2d3d73f60b29d8dd02851ceaf81fc9e"
	p.sweepLister = func(ctx context.Context) []string { return []string{live[:12]} }
	p.SetActiveContainers(func(ctx context.Context) (map[string]bool, error) {
		return map[string]bool{live: true}, nil
	})

	p.collectOrphans(context.Background())

	if len(*terminated) != 0 {
		t.Fatalf("terminated = %v, want none (claimed container is in use)", *terminated)
	}
}

func TestWarmPoolFillsSlotsOneAtATime(t *testing.T) {
	t.Parallel()
	p, _, _ := newTestPool(t, 3)

	var mu sync.Mutex
	inFlight, maxInFlight, seq := 0, 0, 0
	release := make(chan struct{})
	p.provisioner = func(ctx context.Context, params WarmPoolParams) (*SessionInfo, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		seq++
		id := "warm-seq-" + string(rune('a'+seq-1))
		mu.Unlock()

		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		mu.Lock()
		inFlight--
		mu.Unlock()
		return &SessionInfo{SessionID: params.SessionID, ContainerID: id, Host: "127.0.0.1", Port: 5900}, nil
	}

	wg := p.replenish(context.Background())

	// Wait until the first fill is inside the provisioner, then let all of them
	// run: a size-3 pool must still only ever have one boot in flight.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		started := inFlight
		mu.Unlock()
		if started > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no fill started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	wg.Wait()

	mu.Lock()
	max := maxInFlight
	mu.Unlock()
	if max != 1 {
		t.Fatalf("max concurrent fills = %d, want 1 (slots must fill one at a time)", max)
	}
	if len(p.warm) != 3 {
		t.Fatalf("pool = %d containers, want 3", len(p.warm))
	}
}

func TestWarmPoolStopsFillingAfterFailure(t *testing.T) {
	t.Parallel()
	p, _, _ := newTestPool(t, 3)
	calls := 0
	p.provisioner = func(ctx context.Context, params WarmPoolParams) (*SessionInfo, error) {
		calls++
		return nil, errors.New("no capacity")
	}

	p.replenish(context.Background()).Wait()

	if calls != 1 {
		t.Fatalf("provisioner calls = %d, want 1 (a failure ends the pass instead of retrying)", calls)
	}
	if len(p.warm) != 0 {
		t.Fatalf("pool = %d containers, want 0", len(p.warm))
	}
	if p.fillWorker {
		t.Fatal("fill worker still marked active after a failed pass")
	}
}

func TestWarmPoolOrphanCollectorSkipsWhileFilling(t *testing.T) {
	t.Parallel()
	p, _, terminated := newTestPool(t, 1)
	// A fill is in flight: the container it just created is legitimately
	// untracked until CreateSession returns, so nothing may be collected.
	p.filling = map[warmMode]int{{}: 1}
	p.sweepLister = func(ctx context.Context) []string { return []string{"warm-mid-boot"} }

	p.collectOrphans(context.Background())

	if len(*terminated) != 0 {
		t.Fatalf("terminated = %v, want none while a fill is in flight", *terminated)
	}
}

func TestWarmPoolOrphanCollectorSkipsOnLookupError(t *testing.T) {
	t.Parallel()
	p, _, terminated := newTestPool(t, 1)
	p.sweepLister = func(ctx context.Context) []string { return []string{"warm-a"} }
	p.SetActiveContainers(func(ctx context.Context) (map[string]bool, error) {
		return nil, errors.New("db down")
	})

	p.collectOrphans(context.Background())

	if len(*terminated) != 0 {
		t.Fatalf("terminated = %v, want none (no DB answer means no safe collection)", *terminated)
	}
}
