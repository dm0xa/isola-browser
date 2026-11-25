package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/dm0xa/isola-browser/backend/pkg/config"
)

// WarmPool keeps pre-started Electron containers: a claim reaches "running" in
// ~0.2-0.5 s vs ~2 s cold (4 GiB Podman VM). Warm containers carry isola.warm=1
// and no session label (the sweep never sees them); no DB row ticks while idle;
// Claim only accepts exact parameter matches; every failure falls back to cold.
type WarmPool struct {
	orch *Orchestrator
	// size / torSize are the idle counts per mode. Tor cannot be enabled after
	// boot (the entrypoint starts the daemon as its own UID and scopes direct
	// egress away from the browser), so Tor sessions need their own slots.
	size       int
	torSize    int
	torCountry string        // exit pin baked into every Tor-baked container
	fillGap    time.Duration // pause between consecutive fills (see warmFillGap)
	image      string
	resolution string
	theme      string
	fps        int
	streamMode string
	dnsServers []string
	dnsMode    string
	// networkLogMaxEntries is baked into every warm container: the capture
	// window cannot be changed after launch, and a claimed container must
	// match a cold one.
	networkLogMaxEntries int
	// intelEnabled is baked in for the same reason: a claimed container must
	// carry the same INTEL_PROBES as the session that would have cold-started.
	intelEnabled bool
	// intelCAPEM is baked in so a claimed container trusts the same CAs.
	intelCAPEM string
	// timezone / locale are the deployment defaults baked into every warm
	// container: the handler refuses a claim whose request resolves to a
	// different pair (env cannot change after launch).
	timezone string
	locale   string

	// injectable for tests
	provisioner func(ctx context.Context, p WarmPoolParams) (*SessionInfo, error)
	prober      func(ctx context.Context, info *SessionInfo) error
	navigator   func(ctx context.Context, host string, port int, targetURL string) error
	nower       func() time.Time
	stop        func(ctx context.Context, containerID string)

	// activeContainers returns the container IDs referenced by active sessions.
	// Used by the orphan collector; nil means "nothing is active".
	activeContainers func(ctx context.Context) (map[string]bool, error)
	// sweepLister lists warm-labeled containers already on the host (injectable
	// for tests; defaults to the orchestrator's docker ps).
	sweepLister func(ctx context.Context) []string

	mu         sync.Mutex
	warm       []*warmContainer
	filling    map[warmMode]int // fills in flight per mode (guards against over-provisioning)
	fillWorker bool             // one worker walks the empty slots, one at a time
	pending    map[string]bool  // containers created by an in-flight fill, not yet in warm
	misses     map[string]int   // consecutive probe failures per container id
	started    bool
	disabled   bool
	// publishDegraded is set when a probe proved the container is alive inside
	// but the host cannot dial its published port. Claims are not served (the
	// cold path takes over) and idle containers are kept instead of torn down
	// until a probe answers again.
	publishDegraded bool
	// torFillsBlockedUntil suppresses Tor fills after a failed one: a network
	// that cannot bootstrap Tor would otherwise boot and destroy a container
	// every reconcile tick, and each attempt holds the shared fill worker for
	// its whole budget, starving the plain slots.
	torFillsBlockedUntil time.Time

	// fillCtx bounds every fill and is cancelled by Drain, so a shutdown stops
	// a browser boot in progress instead of orphaning its container (the
	// daemon creates the container before the fill finishes waiting on it).
	fillCtx    context.Context
	fillCancel context.CancelFunc
	fillsWG    sync.WaitGroup
}

// markPending: exists on the host but not yet in the pool; Drain stops these too.
func (p *WarmPool) markPending(containerID string) {
	p.mu.Lock()
	if p.pending == nil {
		p.pending = map[string]bool{}
	}
	p.pending[containerID] = true
	p.mu.Unlock()
}

// clearPending forgets a container once its fill has resolved it either way.
func (p *WarmPool) clearPending(containerID string) {
	p.mu.Lock()
	delete(p.pending, containerID)
	p.mu.Unlock()
}

// warmMissLimit is how many consecutive probes must fail before an idle warm
// container is torn down (mirrors containerMissLimit for live sessions).
const warmMissLimit = 2

// warmClaimPendingTTL: shields a claimed container until activation; must not hide a never-activated claim forever.
const warmClaimPendingTTL = 2 * time.Minute

// warmFillGap: one fill worker, spaced bursts; stops a fast failure becoming a retry loop.
const warmFillGap = 2 * time.Second

// noteMiss increments the consecutive probe-failure count for a container.
func (p *WarmPool) noteMiss(containerID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.misses == nil {
		p.misses = map[string]int{}
	}
	p.misses[containerID]++
	return p.misses[containerID]
}

// clearMiss forgets a container's failure streak (it answered again).
func (p *WarmPool) clearMiss(containerID string) {
	p.mu.Lock()
	delete(p.misses, containerID)
	p.mu.Unlock()
}

// notePublishUnreachable marks the host publish path as broken and reports
// whether this call is the transition, so the warning is logged once per outage.
func (p *WarmPool) notePublishUnreachable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	first := !p.publishDegraded
	p.publishDegraded = true
	return first
}

// clearPublishUnreachable clears the outage flag once a probe answers again,
// reporting whether it was set.
func (p *WarmPool) clearPublishUnreachable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	was := p.publishDegraded
	p.publishDegraded = false
	return was
}

// requeueClaim returns a claimed container to the idle pool when the probe
// proved it alive but unreachable through the host. The pending shield is
// removed in the same critical section as the reinsert, so the container is
// tracked by warm or by pending at every instant. Drain may have stopped it
// first; that branch re-terminates idempotently rather than re-adding a dead
// container to a disabled pool.
func (p *WarmPool) requeueClaim(claimed *warmContainer) {
	id := claimed.info.ContainerID
	p.mu.Lock()
	if p.disabled {
		p.mu.Unlock()
		// Drain ran while the claim was popped; it may have missed the
		// container, so stop it here to avoid a leak.
		p.terminate(context.Background(), id)
		return
	}
	delete(p.pending, id)
	p.warm = append(p.warm, claimed)
	p.mu.Unlock()
}

// warmContainer is one pre-started container held by the pool.
type warmContainer struct {
	info      *warmContainerInfo
	startedAt time.Time
}

// warmMode is the baked session mode a container can serve. mode.tor is the
// part a running container cannot change: a Tor session's egress is pinned to
// the tor UID at boot, and a plain session has no proxy at all.
type warmMode struct {
	tor     bool
	country string // Tor exit pin (only meaningful when tor)
}

func (m warmMode) String() string {
	if !m.tor {
		return "plain"
	}
	if m.country == "" {
		return "tor"
	}
	return "tor:" + m.country
}

// warmContainerInfo mirrors SessionInfo plus the exact claim parameters the
// container was created with, so mismatched requests can be rejected.
type warmContainerInfo struct {
	SessionInfo
	Resolution string
	Theme      string
	FPS        int
	Mode       warmMode
}

// WarmPoolParams describes how a warm container is provisioned.
type WarmPoolParams struct {
	SessionID  string
	Image      string
	Resolution string
	Theme      string
	FPS        int
	StreamMode string
	Mode       warmMode
}

// NewWarmPool creates the pool; call Start to begin filling it.
func NewWarmPool(orch *Orchestrator, cfg *config.Config) *WarmPool {
	theme := cfg.BrowserTheme
	if theme == "" || theme == "auto" {
		theme = "dark"
	}
	p := &WarmPool{
		orch:                 orch,
		size:                 cfg.WarmPoolSize,
		torSize:              cfg.WarmPoolTorSize,
		torCountry:           cfg.TorExitCountry,
		fillGap:              warmFillGap,
		image:                cfg.SessionImage(),
		resolution:           cfg.Resolution,
		theme:                theme,
		fps:                  30, // handler's isola default; entrypoint caps at 25
		streamMode:           "isola",
		filling:              map[warmMode]int{},
		dnsServers:           cfg.DNSServers,
		dnsMode:              cfg.DNSMode,
		networkLogMaxEntries: cfg.NetworkLogMaxEntries,
		intelEnabled:         cfg.IntelEnabled,
		intelCAPEM:           cfg.IntelCAPEM,
		timezone:             cfg.SessionTimezone,
		locale:               cfg.SessionLocale,
		nower:                time.Now,
	}
	p.provisioner = p.provisionWarm
	p.prober = p.probeWarm
	p.navigator = p.Navigate
	if orch != nil {
		// Without an orchestrator (test pools) there is no docker-backed
		// sweep; collectOrphans treats a nil sweepLister as "nothing to do".
		p.sweepLister = func(ctx context.Context) []string { return p.orch.listWarmContainers(ctx) }
	}
	return p
}

// SetActiveContainers wires the active-session lookup used by the startup
// leftover sweep (main.go supplies it from the session store).
func (p *WarmPool) SetActiveContainers(fn func(ctx context.Context) (map[string]bool, error)) {
	p.activeContainers = fn
}

// Start fills the pool and reconciles it until ctx is cancelled. Draining on
// shutdown stops warm containers so a restarted backend starts clean.
func (p *WarmPool) Start(ctx context.Context, interval time.Duration) {
	p.mu.Lock()
	if p.started || p.disabled {
		p.mu.Unlock()
		return
	}
	p.started = true
	p.fillCtx, p.fillCancel = context.WithCancel(context.Background())
	p.mu.Unlock()

	log.Info().Int("size", p.size).Str("resolution", p.resolution).Msg("warm pool: start")
	p.collectOrphans(ctx)
	p.reconcile(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			p.Drain(drainCtx)
			cancel()
			log.Info().Msg("warm pool: stop")
			return
		case <-ticker.C:
			p.reconcile(ctx)
		}
	}
}

// Claim pops a matching warm container (nil = cold fallback). Resolution/theme/fps
// must match exactly — a lazy reshape measured 3.2-3.7 s to first frame, worse than cold.
// tor/torCountry select the baked mode: a container's Tor wiring is fixed at boot.
func (p *WarmPool) Claim(ctx context.Context, resolution, theme string, fps int, tor bool, torCountry string) (*SessionInfo, string) {
	mode := warmMode{tor: tor, country: torCountry}
	if !tor {
		mode.country = ""
	}
	p.mu.Lock()
	if p.disabled || p.publishDegraded {
		// Nothing in the pool can be reached through the host while the
		// publish path is down, so serve no claims: the handler cold-starts.
		p.mu.Unlock()
		return nil, ""
	}
	var claimed *warmContainer
	baked := ""
	for i, wc := range p.warm {
		if wc.info.Theme != theme || wc.info.FPS != fps || wc.info.Mode != mode {
			continue
		}
		if wc.info.Resolution != resolution {
			continue
		}
		claimed = wc
		baked = wc.info.Resolution
		p.warm = append(p.warm[:i], p.warm[i+1:]...)
		// Between Claim and ActivateSession the orphan collector would stop an
		// untracked warm container; pending shields it.
		if p.pending == nil {
			p.pending = map[string]bool{}
		}
		p.pending[wc.info.ContainerID] = true
		break
	}
	p.mu.Unlock()

	if claimed == nil {
		return nil, ""
	}

	// Last-mile health check: the container was verified when it entered the
	// pool, but it may have died since (OOM, runtime restart). One retry so a
	// momentary streamer hiccup does not cost the user their fast path.
	if err := p.probeWithRetry(ctx, &claimed.info.SessionInfo); err != nil {
		if errors.Is(err, ErrPublishUnreachable) {
			// The container answers on its own loopback, so it is healthy: the
			// host's port publishing is what broke. Put it back instead of
			// destroying a live sandbox, and stop serving claims until a probe
			// answers again (cold launches still work).
			p.requeueClaim(claimed)
			if p.notePublishUnreachable() {
				log.Warn().Err(err).Str("container_id", claimed.info.ContainerID).
					Msg("warm pool: claim blocked, host publishing unreachable")
			}
			return nil, ""
		}
		log.Warn().Err(err).Str("container_id", claimed.info.ContainerID).
			Msg("warm pool: claim probe, discarded")
		p.clearPending(claimed.info.ContainerID)
		p.terminate(context.Background(), claimed.info.ContainerID)
		p.replenish(ctx)
		return nil, ""
	}

	// Shield the claim from the orphan sweep until ActivateSession records it,
	// with a deadline so a claim that never activates cannot hide it forever.
	claimedID := claimed.info.ContainerID
	time.AfterFunc(warmClaimPendingTTL, func() { p.clearPending(claimedID) })

	// Start the replacement now, not on the next reconcile tick: a browser
	// boot takes many seconds, so waiting means the next user pays the cold
	// path too.
	p.replenish(ctx)

	log.Info().
		Str("container_id", claimed.info.ContainerID).
		Str("warm_age", p.nower().Sub(claimed.startedAt).Round(time.Millisecond).String()).
		Str("mode", mode.String()).
		Str("baked_resolution", baked).
		Str("requested_resolution", resolution).
		Msg("warm pool: claimed")
	return &claimed.info.SessionInfo, baked
}

// NotifyNavigateDisabled lets the handler report that the running browser
// image predates the /navigate streamer endpoint. Claims would produce a
// session stuck on about:blank, so the pool stops serving until restart.
func (p *WarmPool) NotifyNavigateDisabled() {
	p.mu.Lock()
	if p.disabled {
		p.mu.Unlock()
		return
	}
	p.disabled = true
	warm := p.warm
	p.warm = nil
	p.mu.Unlock()
	for _, wc := range warm {
		p.terminate(context.Background(), wc.info.ContainerID)
	}
	log.Warn().Msg("warm pool: no navigate, disabled")
}

func (p *WarmPool) Disabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.disabled
}

// Navigate points the claimed container's active tab at the session's target
// URL via the streamer's /navigate endpoint (same trust boundary as /audio
// and /resize).
func (p *WarmPool) Navigate(ctx context.Context, host string, port int, targetURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		navigateRequestURL(host, port, targetURL), nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNavigateUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("navigate returned status %d", resp.StatusCode)
	}
	return nil
}

// navigateRequestURL builds the streamer /navigate request URL. Kept as a
// function so tests can pin the wire format the browser image implements.
func navigateRequestURL(host string, port int, targetURL string) string {
	return fmt.Sprintf("http://%s/navigate?url=%s",
		net.JoinHostPort(host, fmt.Sprintf("%d", port)), url.QueryEscape(targetURL))
}

// ErrNavigateUnsupported reports that the browser image's streamer has no
// /navigate endpoint (pre-warm-pool image).
var ErrNavigateUnsupported = errors.New("streamer does not expose /navigate")

// checkNavigate: 503s retry (runner ctrl still dialing); 404 (old image) fails immediately.
func (p *WarmPool) checkNavigate(ctx context.Context, info *SessionInfo) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := p.navigator(ctx, info.Host, info.Port, "about:blank")
		if err == nil || errors.Is(err, ErrNavigateUnsupported) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// containerIDMatches: accepts a truncated ID — `docker ps` prints 12 chars, creates print 64.
func containerIDMatches(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// containerIDIn reports whether id matches any ID in set.
func containerIDIn(set map[string]bool, id string) bool {
	for known := range set {
		if containerIDMatches(known, id) {
			return true
		}
	}
	return false
}

// collectOrphans removes warm-labeled containers the pool does not own: from a
// previous process, a late-materialized failed create, or any failed fill.
// Skipped while a fill is in flight.
func (p *WarmPool) collectOrphans(ctx context.Context) {
	p.mu.Lock()
	if p.disabled || p.fillsInFlightLocked() > 0 || p.sweepLister == nil {
		p.mu.Unlock()
		return
	}
	tracked := make(map[string]bool, len(p.warm)+len(p.pending))
	for _, wc := range p.warm {
		tracked[wc.info.ContainerID] = true
	}
	for id := range p.pending {
		tracked[id] = true
	}
	p.mu.Unlock()

	ids := p.sweepLister(ctx)
	if len(ids) == 0 {
		return
	}
	// Containers still backing an active session must survive: a claim that
	// happened before a restart is a user's live session.
	active := map[string]bool{}
	if p.activeContainers != nil {
		set, err := p.activeContainers(ctx)
		if err != nil {
			log.Warn().Err(err).Msg("warm pool: lookup, sweep skipped")
			return
		}
		active = set
	}
	for _, id := range ids {
		if containerIDIn(tracked, id) || containerIDIn(active, id) {
			continue
		}
		log.Info().Str("container_id", id).Msg("warm pool: removing untracked")
		p.terminate(ctx, id)
	}
}

// reconcile: prune dead entries by identity BEFORE filling, so a claim never resurrects one.
func (p *WarmPool) reconcile(ctx context.Context) {
	p.mu.Lock()
	if p.disabled {
		p.mu.Unlock()
		return
	}
	snapshot := make([]*warmContainer, len(p.warm))
	copy(snapshot, p.warm)
	p.mu.Unlock()

	// Containers the pool does not know about cannot be probed below, and are
	// exactly what the collector exists for.
	p.collectOrphans(ctx)

	dead := make(map[string]bool)
	for _, wc := range snapshot {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := p.prober(probeCtx, &wc.info.SessionInfo)
		cancel()
		if err == nil {
			p.clearMiss(wc.info.ContainerID)
			if p.clearPublishUnreachable() {
				log.Info().Msg("warm pool: host publishing recovered")
			}
			continue
		}
		if errors.Is(err, ErrPublishUnreachable) {
			// Alive inside, unreachable from the host: a broken publish path is
			// not a dead container, so keep it and do not count a miss. Claims
			// stay off until a probe answers again.
			p.clearMiss(wc.info.ContainerID)
			if p.notePublishUnreachable() {
				log.Warn().Err(err).Str("container_id", wc.info.ContainerID).
					Msg("warm pool: host publishing unreachable, keeping containers")
			}
			continue
		}
		// Two strikes, mirroring the session health checker: one failed probe
		// (a streamer mid-restart, a paused VM) must not kill a usable
		// container and leave the pool empty for the next user.
		if misses := p.noteMiss(wc.info.ContainerID); misses < warmMissLimit {
			log.Debug().Err(err).Str("container_id", wc.info.ContainerID).
				Int("misses", misses).Msg("warm pool: probe, confirming")
			continue
		}
		log.Warn().Err(err).Str("container_id", wc.info.ContainerID).Msg("warm pool: idle died, replacing")
		p.clearMiss(wc.info.ContainerID)
		dead[wc.info.ContainerID] = true
		p.terminate(ctx, wc.info.ContainerID)
	}

	p.mu.Lock()
	kept := p.warm[:0]
	for _, wc := range p.warm {
		if !dead[wc.info.ContainerID] {
			kept = append(kept, wc)
		}
	}
	p.warm = kept
	p.mu.Unlock()

	// Wait for the refill so the periodic pass does not overlap itself.
	p.replenish(ctx).Wait()
}

// replenish: one fill worker provisions slots one at a time — a size-N pool never boots N at once.
func (p *WarmPool) replenish(ctx context.Context) *sync.WaitGroup {
	wg := &sync.WaitGroup{}

	p.mu.Lock()
	if p.disabled || p.fillWorker || !p.hasEmptySlotLocked() {
		p.mu.Unlock()
		return wg
	}
	p.fillWorker = true
	// Parent the fill on the pool's context when Start has run, so Drain can
	// abort boots in flight.
	fillCtx := ctx
	if p.fillCtx != nil {
		fillCtx = p.fillCtx
	}
	p.mu.Unlock()

	wg.Add(1)
	p.fillsWG.Add(1)
	go func() {
		defer wg.Done()
		defer p.fillsWG.Done()
		p.fillSlots(fillCtx)
	}()
	return wg
}

// warmTorFillBudget covers the Tor circuit gate on top of the browser boot;
// warmFillBudget is the plain (no gate) budget.
const (
	warmFillBudget    = 90 * time.Second
	warmTorFillBudget = 150 * time.Second
	// warmTorFillRetryGap: after a failed Tor fill, wait this long before the
	// next attempt, so a network that blocks Tor cannot churn boots.
	warmTorFillRetryGap = 5 * time.Minute
)

// warmModesLocked lists the modes the pool keeps slots for, plain first.
func (p *WarmPool) warmModesLocked() []warmMode {
	modes := []warmMode{{}}
	if p.torSize > 0 && !p.nower().Before(p.torFillsBlockedUntil) {
		modes = append(modes, warmMode{tor: true, country: p.torCountry})
	}
	return modes
}

// modeTarget is how many idle containers a mode keeps.
func (p *WarmPool) modeTarget(mode warmMode) int {
	if mode.tor {
		return p.torSize
	}
	return p.size
}

// warmCountLocked counts the containers the pool holds for a mode.
func (p *WarmPool) warmCountLocked(mode warmMode) int {
	n := 0
	for _, wc := range p.warm {
		if wc.info.Mode == mode {
			n++
		}
	}
	return n
}

// fillsInFlightLocked is the total number of fills in flight.
func (p *WarmPool) fillsInFlightLocked() int {
	n := 0
	for _, c := range p.filling {
		n += c
	}
	return n
}

// hasEmptySlotLocked reports whether any enabled mode still has a free slot.
func (p *WarmPool) hasEmptySlotLocked() bool {
	_, ok := p.nextFillLocked()
	return ok
}

// nextFillLocked picks the mode with an empty slot, counting fills in flight.
func (p *WarmPool) nextFillLocked() (warmMode, bool) {
	if p.disabled {
		return warmMode{}, false
	}
	for _, mode := range p.warmModesLocked() {
		if p.warmCountLocked(mode)+p.filling[mode] < p.modeTarget(mode) {
			return mode, true
		}
	}
	return warmMode{}, false
}

// fillSlots provisions empty slots one at a time until the pool is full, a fill
// fails, or the pool shuts down. A failure ends the pass instead of retrying
// immediately: the next reconcile tick picks the slot up again.
func (p *WarmPool) fillSlots(ctx context.Context) {
	for {
		p.mu.Lock()
		mode, ok := p.nextFillLocked()
		if !ok {
			p.fillWorker = false
			p.mu.Unlock()
			return
		}
		if p.filling == nil {
			p.filling = map[warmMode]int{}
		}
		p.filling[mode]++
		p.mu.Unlock()

		ok = p.fillOne(ctx, mode)

		p.mu.Lock()
		p.filling[mode]--
		p.mu.Unlock()

		if !ok {
			p.mu.Lock()
			p.fillWorker = false
			p.mu.Unlock()
			return
		}

		// Space the boots. The container just built also took its own boot time,
		// so consecutive starts stay apart even on a fast host.
		select {
		case <-ctx.Done():
			p.mu.Lock()
			p.fillWorker = false
			p.mu.Unlock()
			return
		case <-time.After(p.fillGap):
		}
	}
}

// fillOne provisions one warm container for a mode and adds it to the pool.
func (p *WarmPool) fillOne(ctx context.Context, mode warmMode) bool {
	budget := warmFillBudget
	if mode.tor {
		budget = warmTorFillBudget
	}
	createCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	info, err := p.provisioner(createCtx, WarmPoolParams{
		SessionID:  "warm-" + uuid.NewString(),
		Image:      p.image,
		Resolution: p.resolution,
		Theme:      p.theme,
		FPS:        p.fps,
		StreamMode: p.streamMode,
		Mode:       mode,
	})
	if err != nil {
		log.Warn().Err(err).Str("mode", mode.String()).Msg("warm pool: provision")
		p.blockTorFills(mode)
		return false
	}
	// A warm container is only useful once the in-container stream answers,
	// and only if the image can be re-pointed at a session URL.
	if err := p.checkNavigate(createCtx, info); err != nil {
		if errors.Is(err, ErrNavigateUnsupported) {
			p.terminate(ctx, info.ContainerID)
			p.NotifyNavigateDisabled()
			return false
		}
		log.Warn().Err(err).Str("container_id", info.ContainerID).
			Msg("warm pool: navigate probe")
		p.terminate(ctx, info.ContainerID)
		p.blockTorFills(mode)
		return false
	}
	p.mu.Lock()
	poolSize := 0
	if p.disabled || p.warmCountLocked(mode) >= p.modeTarget(mode) {
		// Shrink raced with a config change or disable: give it back.
		p.mu.Unlock()
		p.terminate(ctx, info.ContainerID)
		return true
	}
	p.warm = append(p.warm, &warmContainer{info: &warmContainerInfo{
		SessionInfo: *info,
		Resolution:  p.resolution,
		Theme:       p.theme,
		FPS:         p.fps,
		Mode:        mode,
	}, startedAt: p.nower()})
	poolSize = len(p.warm)
	p.mu.Unlock()
	log.Info().Str("container_id", info.ContainerID).
		Str("mode", mode.String()).
		Int("pool_size", poolSize).Msg("warm pool: container ready")
	return true
}

// blockTorFills pauses Tor fills after a failed one. A blocked Tor network
// would otherwise create and destroy a container every reconcile tick, and each
// attempt occupies the shared fill worker for its whole budget.
func (p *WarmPool) blockTorFills(mode warmMode) {
	if !mode.tor {
		return
	}
	p.mu.Lock()
	p.torFillsBlockedUntil = p.nower().Add(warmTorFillRetryGap)
	p.mu.Unlock()
	log.Warn().Str("retry_after", warmTorFillRetryGap.String()).
		Msg("warm pool: tor fill blocked, backing off")
}

// Drain stops every warm container, bounded by ctx; safe concurrently with Start/Claim.
func (p *WarmPool) Drain(ctx context.Context) {
	p.mu.Lock()
	warm := p.warm
	pending := make([]string, 0, len(p.pending))
	for id := range p.pending {
		pending = append(pending, id)
	}
	p.warm = nil
	p.pending = nil
	p.disabled = true
	fillCancel := p.fillCancel
	p.mu.Unlock()

	// Abort boots in flight and wait for their own cleanup, or a mid-boot shutdown leaves a container.
	if fillCancel != nil {
		fillCancel()
		if !waitTimeout(&p.fillsWG, 15*time.Second) {
			log.Warn().Msg("warm pool: shutdown, fills in flight")
		}
	}

	stopped := len(warm)
	for _, wc := range warm {
		stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		// Route through p.terminate so the injectable stop hook is honored
		// (tests install one; p.orch may be nil without it).
		p.terminate(stopCtx, wc.info.ContainerID)
		cancel()
	}
	for _, id := range pending {
		stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		p.terminate(stopCtx, id)
		cancel()
		stopped++
	}
	if stopped > 0 {
		log.Info().Int("stopped", stopped).Msg("warm pool: drained")
	}
}

// waitTimeout waits for wg up to timeout, reporting whether it completed.
func waitTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (p *WarmPool) terminate(ctx context.Context, containerID string) {
	if p.stop != nil {
		p.stop(ctx, containerID)
		return
	}
	if p.orch == nil {
		// A pool built without an orchestrator (unit tests) has no runtime to
		// call; the container is the caller's fixture. Production always sets
		// orch in NewWarmPool, so this never hides a container in the wild.
		return
	}
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := p.orch.TerminateSession(tctx, containerID); err != nil {
		log.Warn().Err(err).Str("container_id", containerID).Msg("warm pool: terminate")
	}
}

// provisionWarm: start, wait for the stream, resolve addresses; a never-ready container is torn down here.
func (p *WarmPool) provisionWarm(ctx context.Context, params WarmPoolParams) (*SessionInfo, error) {
	info, err := p.orch.CreateSession(ctx, CreateSessionParams{
		SessionID:    params.SessionID,
		UserID:       "warm-pool",
		TargetURL:    "about:blank",
		Resolution:   params.Resolution,
		BrowserType:  "chromium",
		BrowserTheme: params.Theme,
		StreamMode:   params.StreamMode,
		FrameRate:    params.FPS,
		// isola sessions always run with CDP capture on (the handler forces
		// this for the transport); baking it in keeps claims identical.
		NetworkMonitor:       true,
		NetworkLogMaxEntries: p.networkLogMaxEntries,
		IntelEnabled:         p.intelEnabled,
		IntelCAPEM:           p.intelCAPEM,
		Timezone:             p.timezone,
		Locale:               p.locale,
		DockerImage:          params.Image,
		DNSMode:              p.dnsMode,
		DNSServers:           p.dnsServers,
		// Tor is baked at creation: the entrypoint starts the daemon as its own
		// UID and scopes direct egress away from the browser, so a Tor session
		// can only ever be served by a container created for it.
		TorEnabled: params.Mode.tor,
		TorCountry: params.Mode.country,
		// Label swap: CreateSession stamps isola.session_id; warm containers
		// must carry isola.warm instead so the orphan sweep ignores it.
		warmLabel: true,
	})
	if err != nil {
		// A failed create may still have created the (warm-labeled) container; remove it by name.
		p.removeFailedContainer("isola-" + params.SessionID[:12])
		return nil, err
	}
	// From here the container exists on the host: record it so a shutdown
	// landing mid-fill (the browser boot can take tens of seconds) still
	// reaches it.
	p.markPending(info.ContainerID)
	defer p.clearPending(info.ContainerID)
	// Inside the pool's fill budget, not on a user's request path: the browser
	// boot (streamer, Xvfb, Electron, first paint) has to finish before the
	// container is worth claiming. A Tor container also blocks on its circuit
	// gate, so it gets the same extra budget a cold Tor launch gets.
	streamBudget := 30 * time.Second
	if params.Mode.tor {
		// Same order as streamReadyTimeout: the circuit gate (45s unpinned,
		// 60s pinned) runs before the streamer starts.
		streamBudget = 75 * time.Second
		if params.Mode.country != "" {
			streamBudget = 90 * time.Second
		}
	}
	if err := p.orch.WaitForStream(ctx, info.ContainerID, info.Host, info.Port, params.StreamMode, "chromium", streamBudget); err != nil {
		_ = p.orch.TerminateSession(context.Background(), info.ContainerID)
		return nil, fmt.Errorf("warm container stream not ready: %w", err)
	}
	return info, nil
}

// removeFailedContainer: retry over a bounded window — a busy daemon can materialize
// the container ~30 s after the client gave up, and the warm label hides it from the sweep.
func (p *WarmPool) removeFailedContainer(name string) {
	go func() {
		for _, wait := range []time.Duration{0, 5 * time.Second, 10 * time.Second} {
			if wait > 0 {
				time.Sleep(wait)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			p.orch.RemoveContainerByName(ctx, name)
			cancel()
		}
	}()
}

// probeWithRetry probes a container twice before concluding it is unusable.
func (p *WarmPool) probeWithRetry(ctx context.Context, info *SessionInfo) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = p.prober(probeCtx, info)
		cancel()
		if err == nil {
			p.clearMiss(info.ContainerID)
			return nil
		}
		if attempt == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
	return err
}

// probeWarm: ask the stream endpoint itself. `docker inspect <64-hex-id>` is
// unreliable on Podman's compat API; a false negative here costs a real container.
// When the host cannot dial the stream but the container's own loopback answers,
// the failure is classified as ErrPublishUnreachable so the pool keeps it.
func (p *WarmPool) probeWarm(ctx context.Context, info *SessionInfo) error {
	ready, err := p.orch.waitForIsolaReady(info.Host, info.Port)
	if err == nil && ready {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("stream healthz not ready")
	}
	if p.orch != nil && info.ContainerID != "" &&
		containerStreamAliveProbe(ctx, p.orch.host, info.ContainerID) {
		return &PublishUnreachableError{
			ContainerID:   info.ContainerID,
			PublishedAddr: net.JoinHostPort(info.Host, strconv.Itoa(info.Port)),
			InternalAlive: true,
			Err:           err,
		}
	}
	return err
}
