package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
	"github.com/google/uuid"
)

// SessionCleanupFunc runs after a container is stopped during background termination.
type SessionCleanupFunc func(ctx context.Context, sess *models.Session)

// Orchestrator manages Docker containers for browser sessions via the docker CLI.
type Orchestrator struct {
	host             string
	onSessionCleanup SessionCleanupFunc
	// preStopHook runs before the container is stopped, while the streamer is
	// still reachable. Resource capture uses it for its final pull.
	preStopHook func(ctx context.Context, sessionID string)

	proxyIPOnce sync.Once
	proxyIP     string // this process's IP on browser-net, passed to containers

	// dnsProxyEnabled is set once at startup when a DoT/DoH forwarding server
	// is listening on the browser-net IP. When false, sessions fall back to
	// plaintext `--dns` resolvers even if the config asks for encryption.
	dnsProxyEnabled bool
	// dnsFallbackWarnOnce logs the encryption downgrade once per process.
	dnsFallbackWarnOnce sync.Once

	// containerMisses: consecutive not-running probes per session; a single false negative must not finalize.
	containerMisses sync.Map // sessionID -> *atomic.Int64

	// reaping serializes FinalizeIfContainerGone per session: both relay
	// channels can fail at the same instant, and two finalizations would stop
	// the container twice and write the terminate audit twice.
	reaping sync.Map // sessionID -> struct{}
}

// containerMissLimit is how many consecutive probes must report a container
// as not running before the session is finalized.
const containerMissLimit = 3

// healthProbeTimeout: without it a wedged daemon blocks the health goroutine forever;
// expiry yields an inconclusive probe the loop skips.
const healthProbeTimeout = 10 * time.Second

// runtimeUnreachable: the daemon itself was unreachable — the probe learned nothing
// (exit codes and "not found" strings cannot tell the two apart).
func runtimeUnreachable(output string) bool {
	s := strings.ToLower(output)
	for _, sig := range []string{
		"failed to connect to the docker api",
		"cannot connect to the docker daemon",
		"is the docker daemon running",
		"connection refused",
		"connection reset",
		"context deadline exceeded",
		"i/o timeout",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// containerAbsent: the probe positively named this container's absence. A miss is
// destructive, so anything unrecognized is inconclusive. All three runtimes' removal
// messages live here — dropping one leaks its session.
func containerAbsent(output string) bool {
	s := strings.ToLower(output)
	for _, sig := range []string{
		"no such container",
		"no such object",
		"normalizing image",
		"64-byte hexadecimal",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// noteContainerMiss increments the consecutive "not running" count for a
// session and returns the new value.
func (o *Orchestrator) noteContainerMiss(sessionID string) int {
	v, _ := o.containerMisses.LoadOrStore(sessionID, new(atomic.Int64))
	return int(v.(*atomic.Int64).Add(1))
}

// proxyIPOnBrowserNet resolves the backend's own address on the browser-net
// bridge so session containers can firewall their service ports to the proxy
// only. Runs once; empty result means "unknown" and containers stay permissive.
func (o *Orchestrator) proxyIPOnBrowserNet() string {
	o.proxyIPOnce.Do(func() {
		// Only a container process can hold a browser-net address; the host-install inspect cost 152 ms and can't succeed.
		if !runningInContainer() {
			return
		}
		hostname, err := os.Hostname()
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := o.dockerCmd(ctx, "inspect", "-f",
			"{{(index .NetworkSettings.Networks \"browser-net\").IPAddress}}", hostname,
		).Output()
		if err != nil {
			log.Warn().Err(err).Msg("browser-net: resolve")
			return
		}
		ip := strings.TrimSpace(string(out))
		if net.ParseIP(ip) != nil {
			o.proxyIP = ip
			log.Info().Str("ip", ip).Msg("ports: proxy-only")
		}
	})
	return o.proxyIP
}

// runningInContainer reports whether this process appears to run inside a
// container. Docker and Podman both leave a marker file at the root; a host
// install has neither.
func runningInContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

// New creates a new Orchestrator.
func New(dockerHost string) (*Orchestrator, error) {
	log.Info().Str("docker_host", dockerHost).Msg("orchestrator: init")
	return &Orchestrator{host: dockerHost}, nil
}

// BrowserNetIP exposes the orchestrator's own address on the browser-net
// bridge (empty when unknown). Used to bind the DNS forwarding server.
func (o *Orchestrator) BrowserNetIP() string { return o.proxyIPOnBrowserNet() }

// EnableDNSProxy marks the DoT/DoH forwarding server as available so sessions
// point their resolver at this process. Must be called before serving traffic;
// it is not safe to toggle at runtime.
func (o *Orchestrator) EnableDNSProxy() { o.dnsProxyEnabled = true }

// dockerCmd creates an exec.CommandContext for the docker CLI with the configured host.
func (o *Orchestrator) dockerCmd(ctx context.Context, args ...string) *exec.Cmd {
	if o.host != "" && o.host != "unix:///var/run/docker.sock" {
		args = append([]string{"-H", o.host}, args...)
	}
	return exec.CommandContext(ctx, "docker", args...)
}

// CreateSessionParams holds parameters for session creation.
type CreateSessionParams struct {
	SessionID     string
	UserID        string
	TargetURL     string
	Resolution    string
	BrowserType   string
	PinnedHost    string
	PinnedIP      string
	BrowserTheme  string
	BrowserFlags  string
	StreamMode    string // always "isola"
	AudioEnabled  bool   // enable Opus audio capture
	BandwidthKbps int    // encoder bitrate cap (kbps); 0 = resolution default
	FrameRate     int    // capture framerate (default 30)
	TorEnabled    bool
	// TorCountry pins Tor exits to these ISO 3166-1 alpha-2 countries; only
	// meaningful when TorEnabled. Empty allows any exit.
	TorCountry string
	// Timezone / Locale are the page-visible identity values baked into the
	// container. The streamer applies them over CDP at target creation; the
	// container's own TZ follows Timezone. Empty = image defaults (UTC, en-US).
	Timezone       string
	Locale         string
	DNSMode        string   // "plain" | "tls" | "https" — how containers resolve names
	DNSServers     []string // plaintext resolvers passed via `docker run --dns` (empty = runtime embedded resolver)
	NetworkMonitor bool     // isola: stream CDP network events to the viewer
	// NetworkLogMaxEntries is the capture window handed to the streamer
	// (NET_LOG_MAX_ENTRIES). 0 leaves the streamer's own default in place.
	NetworkLogMaxEntries int // isola: CDP network entries retained per session
	// IntelEnabled exposes the streamer's /intel recon endpoint (INTEL_PROBES).
	// No traffic leaves the sandbox until a caller names a host, and recon only
	// ever talks to that host (plus the resolver).
	IntelEnabled bool // isola: on-demand recon probes / DNS enrichment
	// IntelCAPEM is an operator-supplied CA bundle passed to the streamer
	// (INTEL_CA_PEM) so recon can verify certificates behind a TLS-inspecting
	// proxy. Empty leaves the image's system pool alone.
	IntelCAPEM  string
	DockerImage string // Docker image name (e.g. isola-browser:electron)
	// isola.warm=1 instead of a session label: the orphan sweep must never see warm containers.
	warmLabel bool
}

// SessionInfo holds information about a created session container.
type SessionInfo struct {
	SessionID   string
	ContainerID string
	Host        string
	Port        int
	CdpPort     int
}

// dangerousBrowserFlags are flags that would disable security sandboxing or
// enable dangerous browser features. Validated at container creation time.
var dangerousBrowserFlags = []string{
	"--disable-setuid-sandbox",
	"--disable-web-security",
	"--allow-running-insecure-content",
	"--disable-features=VizDisplayCompositor",
	"--disable-gpu-sandbox",
	"--single-process",
	"--in-process-gpu",
	"--remote-debugging-port",
	"--remote-debugging-address",
}

// validateBrowserFlags checks that browser flags don't contain dangerous patterns.
func validateBrowserFlags(flags string) error {
	if flags == "" {
		return nil
	}
	for _, dangerous := range dangerousBrowserFlags {
		if strings.Contains(flags, dangerous) {
			return fmt.Errorf("dangerous browser flag rejected: %s", dangerous)
		}
	}
	return nil
}

// stackLabel: operator-visible marker (`docker ps --filter label=isola.stack`); never stands in for the lifecycle labels.
const stackLabel = "isola.stack=1"

// containerLabels: exactly one of isola.session_id / isola.warm — a warm container
// carrying session_id is destroyed by the sweep. Pure function; getting it wrong is silent.
func containerLabels(sessionID, userID string, warm bool) []string {
	if warm {
		return []string{
			"--label", stackLabel,
			"--label", "isola.warm=1",
			"--label", "isola.warm_id=" + sessionID,
		}
	}
	return []string{
		"--label", stackLabel,
		"--label", "isola.session_id=" + sessionID,
		"--label", "isola.user_id=" + userID,
	}
}

// torSessionArgs: the exit country is re-validated here — an unusable pin is an error, not a downgrade.
func torSessionArgs(enabled bool, exitCountry string) ([]string, error) {
	if !enabled {
		return nil, nil
	}
	args := []string{"-e", "TOR_ENABLED=true"}
	if strings.TrimSpace(exitCountry) == "" {
		return args, nil
	}
	country, err := config.NormalizeTorCountry(exitCountry)
	if err != nil {
		return nil, err
	}
	if country != "" {
		args = append(args, "-e", "TOR_EXIT_COUNTRY="+country)
	}
	return args, nil
}

// dnsSessionArgs: encrypted modes point the resolver at the local forwarder (proxyIP);
// fellBack lets the caller warn once.
func dnsSessionArgs(mode string, proxyEnabled bool, proxyIP string, servers []string) (args []string, fellBack bool) {
	encrypted := mode == "tls" || mode == "https"
	if encrypted && proxyEnabled && proxyIP != "" {
		return []string{"--dns", proxyIP, "-e", "ISOLA_DNS_PROXY_IP=" + proxyIP}, false
	}
	if encrypted {
		fellBack = true
	}
	return dnsArgs(servers), fellBack
}

// dnsArgs: blank entries dropped (docker rejects `--dns ""`); empty result = the runtime's embedded resolver. Pure function.
func dnsArgs(servers []string) []string {
	args := make([]string, 0, len(servers)*2)
	for _, server := range servers {
		if server = strings.TrimSpace(server); server != "" {
			args = append(args, "--dns", server)
		}
	}
	return args
}

// identitySessionArgs: only non-empty values are passed, so a session with no
// declared identity keeps the image's UTC / en-US untouched. TZ moves the
// container clock; ISOLA_SESSION_TZ / ISOLA_SESSION_LOCALE feed the streamer's
// CDP overrides, which are what the page actually reads. Pure function.
func identitySessionArgs(timezone, locale string) []string {
	args := make([]string, 0, 6)
	if timezone = strings.TrimSpace(timezone); timezone != "" {
		args = append(args, "-e", "TZ="+timezone, "-e", "ISOLA_SESSION_TZ="+timezone)
	}
	if locale = strings.TrimSpace(locale); locale != "" {
		args = append(args, "-e", "ISOLA_SESSION_LOCALE="+locale)
	}
	return args
}

// CreateSession starts a new browser-node container.
func (o *Orchestrator) CreateSession(ctx context.Context, params CreateSessionParams) (*SessionInfo, error) {
	name := "isola-" + params.SessionID[:12]
	log.Info().Str("session_id", params.SessionID).Str("url", params.TargetURL).Msg("container: create")

	// Validate browser flags at container creation (defense-in-depth beyond admin API)
	if err := validateBrowserFlags(params.BrowserFlags); err != nil {
		return nil, fmt.Errorf("invalid browser flags: %w", err)
	}

	image := params.DockerImage
	if image == "" {
		image = config.DefaultBrowserImage
	}

	cmdArgs := []string{"run", "-d", "--rm",
		"--name", name,
		"-e", "START_URL=" + params.TargetURL,
		"-e", "SESSION_ID=" + params.SessionID,
		"-e", "RESOLUTION=" + params.Resolution,
		"-e", "BROWSER_THEME=" + params.BrowserTheme,
		"-e", "BROWSER_FLAGS=" + params.BrowserFlags,
		"-e", "ISOLA_PROXY_IP=" + o.proxyIPOnBrowserNet(),
		"--network", "browser-net",
	}
	cmdArgs = append(cmdArgs, identitySessionArgs(params.Timezone, params.Locale)...)
	gpuPassthrough := os.Getenv("ISOLA_GPU") == "1"
	if gpuPassthrough {
		if _, err := os.Stat("/dev/dri/renderD128"); err != nil {
			gpuPassthrough = false
		}
	}
	cmdArgs = append(cmdArgs, sessionContainerSecurityArgs(SessionContainerSecurityParams{
		Resolution:     params.Resolution,
		GPUPassthrough: gpuPassthrough,
	})...)
	if gpuPassthrough {
		log.Info().Msg("gpu passthrough")
	}
	cmdArgs = append(cmdArgs, containerLabels(params.SessionID, params.UserID, params.warmLabel)...)
	if params.PinnedHost != "" && params.PinnedIP != "" {
		cmdArgs = append(cmdArgs, "--add-host", params.PinnedHost+":"+params.PinnedIP)
	}

	// Encrypted DNS modes route the resolver at this process on browser-net;
	// plaintext fallback when the forwarder is not running.
	dnsCmdArgs, dnsFellBack := dnsSessionArgs(params.DNSMode, o.dnsProxyEnabled, o.proxyIPOnBrowserNet(), params.DNSServers)
	if dnsFellBack {
		o.dnsFallbackWarnOnce.Do(func() {
			log.Warn().Str("mode", params.DNSMode).Msg("dns: plaintext fallback")
		})
	}
	cmdArgs = append(cmdArgs, dnsCmdArgs...)

	// Seccomp (VULN-5): the bundled stricter profile is opt-in — the Chromium/ffmpeg runtime needs syscalls it does not allow yet.
	if p := strings.TrimSpace(os.Getenv("ISOLA_SECCOMP_PROFILE")); p != "" {
		if _, err := os.Stat(p); err == nil {
			cmdArgs = append(cmdArgs, "--security-opt", "seccomp="+p)
			log.Info().Str("profile", p).Msg("seccomp: profile")
		} else {
			log.Warn().Str("profile", p).Msg("seccomp: default")
		}
	}

	// MAC confinement: forward the operator's ISOLA_REQUIRE_MAC=0 opt-out to the entrypoint's gate.
	if v := strings.TrimSpace(os.Getenv("ISOLA_REQUIRE_MAC")); v != "" {
		cmdArgs = append(cmdArgs, "-e", "ISOLA_REQUIRE_MAC="+v)
	}

	if params.NetworkMonitor {
		cmdArgs = append(cmdArgs, "-e", "NET_MONITOR=1")
	}
	if params.NetworkLogMaxEntries > 0 {
		cmdArgs = append(cmdArgs, "-e", fmt.Sprintf("NET_LOG_MAX_ENTRIES=%d", params.NetworkLogMaxEntries))
	}
	if params.IntelEnabled {
		cmdArgs = append(cmdArgs, "-e", "INTEL_PROBES=1")
	}
	if params.IntelCAPEM != "" {
		cmdArgs = append(cmdArgs, "-e", "INTEL_CA_PEM="+params.IntelCAPEM)
	}

	// Route traffic through Tor if enabled, optionally pinned to exit
	// countries. An unusable country value fails the launch rather than
	// silently widening the exit set the caller asked for.
	torArgs, torErr := torSessionArgs(params.TorEnabled, params.TorCountry)
	if torErr != nil {
		return nil, fmt.Errorf("invalid tor exit country: %w", torErr)
	}
	cmdArgs = append(cmdArgs, torArgs...)

	cmdArgs = append(cmdArgs, "-e", "STREAM_MODE=isola")
	fps := params.FrameRate
	if fps <= 0 {
		fps = 30
	}
	cmdArgs = append(cmdArgs, "-e", fmt.Sprintf("ISOLA_FPS=%d", fps))
	bw := params.BandwidthKbps
	if bw <= 0 {
		bw = isolaBitrateKbps(params.Resolution)
	}
	cmdArgs = append(cmdArgs, "-e", fmt.Sprintf("ISOLA_BITRATE_KBPS=%d", bw))

	// Set audio capture flag for isola-streamer
	audioFlag := "false"
	if params.AudioEnabled {
		audioFlag = "true"
	}
	cmdArgs = append(cmdArgs, "-e", "ENABLE_AUDIO="+audioFlag)

	// Chrome DevTools Protocol — accessed via WS proxy only (no port publishing).

	cmdArgs = append(cmdArgs, image)

	cmd := o.dockerCmd(ctx, cmdArgs...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		sanitized := sanitizeOutput(string(out))
		log.Error().Err(err).Str("output", sanitized).Str("session_id", params.SessionID).Msg("container: create")
		return nil, fmt.Errorf("docker run: %w: %s", err, sanitized)
	}

	containerID := strings.TrimSpace(string(out))

	// Published host ports first (backend on host); a mapping in inspect is not
	// proof of reachability, so verify with a real connection before committing.
	host := ""
	port := 0
	cdpPort := 0
	hp59, hp9223, containerIP, endpointErr := o.containerEndpoints(ctx, containerID)
	if endpointErr != nil {
		log.Warn().Err(endpointErr).Str("container_id", containerID).
			Msg("endpoint: discover")
	}
	if endpointErr == nil && hp59 > 0 && hp9223 > 0 {
		if hostReachable(ctx, "127.0.0.1", hp59) {
			host = "127.0.0.1"
			port = hp59
			cdpPort = hp9223
			log.Info().Str("container_id", containerID).
				Int("stream_port", port).Int("cdp_port", cdpPort).
				Msg("container ready")
		} else {
			log.Info().Str("container_id", containerID).
				Int("stream_port", hp59).
				Msg("published port unreachable, browser-net")
		}
	}
	if host == "" {
		if containerIP == "" {
			return nil, fmt.Errorf("no IP found for container %s", containerID)
		}
		host = containerIP
		port = 5900
		cdpPort = 9223
		log.Info().Str("container_id", containerID).Str("ip", host).Msg("container ready")
	}

	return &SessionInfo{
		SessionID:   params.SessionID,
		ContainerID: containerID,
		Host:        host,
		Port:        port,
		CdpPort:     cdpPort,
	}, nil
}

// SetSessionCleanup registers a hook for expiry/idle/health-check terminations.
// Ping probes the docker daemon with a trivial command. Used by admin
// health checks; cheap enough for periodic polling.
func (o *Orchestrator) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := o.dockerCmd(ctx, "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return fmt.Errorf("docker daemon unreachable: %w", err)
	}
	_ = out
	return nil
}

func (o *Orchestrator) SetSessionCleanup(fn SessionCleanupFunc) {
	o.onSessionCleanup = fn
}

// SetPreStopHook installs a callback that runs before a session's container is
// stopped, while the streamer is still reachable. Optional.
func (o *Orchestrator) SetPreStopHook(fn func(ctx context.Context, sessionID string)) {
	o.preStopHook = fn
}

// sessionCacheKeys lists every per-session cache entry. FinalizeRunningSession
// and CleanupOrphanContainers both clear this set: a key added to only one of
// them survives the other teardown path and leaks for the life of the cache.
func sessionCacheKeys(sid string) []string {
	return []string{
		"session:" + sid,
		"session:" + sid + ":transport",
		"session:" + sid + ":cdp",
		"session:" + sid + ":control_token",
		"session:" + sid + ":view_password",
		"viewer_count:" + sid,
	}
}

// FinalizeRunningSession stops the container, clears cache keys, runs hooks, and marks terminated.
func (o *Orchestrator) FinalizeRunningSession(ctx context.Context, st *store.MemStore, cache *store.MemCache, sess *models.Session) {
	if sess == nil {
		return
	}
	sid := sess.ID.String()
	log.Info().Str("session_id", sid).Msg("session: terminate")

	// The container is still alive here: capture (or another consumer) gets its
	// final chance to drain state from the streamer.
	if o.preStopHook != nil {
		o.preStopHook(ctx, sid)
	}

	if sess.ContainerID != "" {
		if err := o.TerminateSession(ctx, sess.ContainerID); err != nil {
			log.Warn().Err(err).Str("container_id", sess.ContainerID).Msg("finalize: stop")
		}
	}

	cache.Del(ctx, sessionCacheKeys(sid)...)

	if o.onSessionCleanup != nil {
		o.onSessionCleanup(ctx, sess)
	}

	if err := st.TerminateSession(ctx, sess.ID); err != nil {
		log.Warn().Err(err).Str("session_id", sid).Msg("finalize: db")
	}
}

// FinalizeIfContainerGone: the relay's refused dial is ground truth now, the
// health checker needs rounds to agree. The probe still gates the verdict —
// a runtime that cannot answer opens no verdict.
func (o *Orchestrator) FinalizeIfContainerGone(ctx context.Context, st *store.MemStore, cache *store.MemCache, sessionID uuid.UUID) bool {
	sid := sessionID.String()
	if _, busy := o.reaping.LoadOrStore(sid, struct{}{}); busy {
		return false
	}
	defer o.reaping.Delete(sid)

	sess, err := st.GetSession(ctx, sessionID)
	if err != nil || sess == nil || sess.Status != models.SessionStatusRunning || sess.ContainerID == "" {
		return false
	}

	probeCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	out, inspectErr := o.dockerCmd(probeCtx, "inspect", "--format", "{{.State.Running}}", sess.ContainerID).CombinedOutput()
	cancel()

	if inspectErr == nil && strings.TrimSpace(string(out)) == "true" {
		// The container is up, so the streamer endpoint is what failed. That is
		// the health checker's call, not the relay's.
		return false
	}
	if inspectErr != nil && !containerAbsent(string(out)) {
		// Inconclusive: the runtime never answered about this container.
		return false
	}

	log.Warn().Str("session_id", sid).Str("container_id", sess.ContainerID).
		Msg("relay: refused, finalizing")
	o.containerMisses.Delete(sid)
	s := *sess
	o.FinalizeRunningSession(ctx, st, cache, &s)
	return true
}

// TerminateSession stops and removes a browser-node container.
func (o *Orchestrator) TerminateSession(ctx context.Context, containerID string) error {
	// The reapers call this with the process-lifetime context, and `docker stop`
	// has no deadline of its own: a paused VM or a wedged daemon would park that
	// reaper goroutine forever and stall every session queued behind it.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	log.Info().Str("container_id", containerID).Msg("container: terminate")

	cmd := o.dockerCmd(ctx, "stop", "-t", "3", containerID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Warn().Err(err).Str("output", string(out)).Str("container_id", containerID).Msg("docker stop: force remove")
	} else {
		// Containers run with --rm, so a successful stop already removed it.
		// `rm -f` on a vanished container is safe but costs another CLI spawn
		// (~35 ms measured) on every session teardown.
		return nil
	}

	// Force remove to ensure cleanup (safe even if already removed via --rm)
	rmCmd := o.dockerCmd(ctx, "rm", "-f", containerID)
	rmCmd.CombinedOutput()

	return nil
}

// ResizeSessionDisplay talks to the streamer's HTTP port: the sandbox image ships no curl.
func (o *Orchestrator) ResizeSessionDisplay(ctx context.Context, containerHost string, containerPort, width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("invalid resize target: %dx%d", width, height)
	}
	if containerHost == "" || containerPort == 0 {
		return fmt.Errorf("streamer address unknown")
	}

	url := fmt.Sprintf("http://%s/resize?width=%d&height=%d",
		net.JoinHostPort(containerHost, fmt.Sprintf("%d", containerPort)), width, height)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("resize returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// WaitForStream waits until the isola-streamer health endpoint is ready.
func (o *Orchestrator) WaitForStream(ctx context.Context, host string, port int, streamMode string, browserType string, timeout time.Duration) error {
	_ = streamMode
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	deadline := time.Now().Add(timeout)
	// A fixed 100 ms poll: the streamer binds its health endpoint in ~10 ms once
	// it starts, so the old 200ms→1s backoff only added quantization delay
	// (measured median 140 ms per launch) to the create path.
	const retryInterval = 100 * time.Millisecond

	for attempt := 1; ; attempt++ {
		if time.Now().After(deadline) {
			return fmt.Errorf("stream not ready at %s after %v (%d attempts)", addr, timeout, attempt)
		}

		ready, err := o.waitForIsolaReady(host, port)
		if err == nil && ready {
			log.Debug().Str("addr", addr).Str("browser_type", browserType).Msg("stream ready")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryInterval):
		}
	}
}

func (o *Orchestrator) waitForIsolaReady(host string, port int) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, fmt.Sprintf("%d", port))), nil)
	if err != nil {
		return false, err
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK, nil
}

// AudioToggle toggles Opus audio capture on/off via the isola-streamer /audio endpoint.
func (o *Orchestrator) AudioToggle(ctx context.Context, containerHost string, containerPort int, enabled bool) error {
	body, err := json.Marshal(map[string]bool{"enabled": enabled})
	if err != nil {
		return err
	}

	url := fmt.Sprintf("http://%s/audio", net.JoinHostPort(containerHost, fmt.Sprintf("%d", containerPort)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("audio toggle returned status %d", resp.StatusCode)
	}
	return nil
}

// sanitizeOutput replaces sensitive values in docker command output.
func sanitizeOutput(output string, passwords ...string) string {
	for _, password := range passwords {
		if password == "" {
			continue
		}
		output = strings.ReplaceAll(output, password, "***")
	}
	return output
}
func (o *Orchestrator) GetContainerIP(ctx context.Context, containerID string) (string, error) {
	cmd := o.dockerCmd(ctx, "inspect", "-f", "{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}", containerID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker inspect: %w: %s", err, string(out))
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("no IP found for container %s", containerID)
	}
	return ip, nil
}

// hostReachable reports whether a TCP connection to host:port succeeds within
// a short deadline. Used to validate that a discovered address (published host
// port) is actually dialable from this process before the session commits to it.
func hostReachable(ctx context.Context, host string, port int) bool {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (o *Orchestrator) getHostPort(ctx context.Context, containerID, portKey string) (int, error) {
	tpl := fmt.Sprintf("{{(index (index .NetworkSettings.Ports %q) 0).HostPort}}", portKey)
	cmd := o.dockerCmd(ctx, "inspect", "-f", tpl, containerID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("docker inspect host port %s: %w: %s", portKey, err, strings.TrimSpace(string(out)))
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return 0, fmt.Errorf("no host port mapping for %s on %s", portKey, containerID)
	}
	p, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parse host port %s for %s: %w", raw, containerID, err)
	}
	return p, nil
}

// containerEndpoints reads published ports + browser-net IP in one inspect
// (cold create used to spawn three); `|` separators keep absent fields aligned.
func (o *Orchestrator) containerEndpoints(ctx context.Context, containerID string) (streamPort, cdpPort int, ip string, err error) {
	const tpl = `{{with index .NetworkSettings.Ports "5900/tcp"}}{{if .}}{{(index . 0).HostPort}}{{end}}{{end}}|` +
		`{{with index .NetworkSettings.Ports "9223/tcp"}}{{if .}}{{(index . 0).HostPort}}{{end}}{{end}}|` +
		`{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}`
	cmd := o.dockerCmd(ctx, "inspect", "-f", tpl, containerID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, 0, "", fmt.Errorf("docker inspect endpoints: %w: %s", err, strings.TrimSpace(string(out)))
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(parts) != 3 {
		return 0, 0, "", fmt.Errorf("docker inspect endpoints: unexpected output %q", strings.TrimSpace(string(out)))
	}
	streamPort, _ = strconv.Atoi(strings.TrimSpace(parts[0]))
	cdpPort, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
	return streamPort, cdpPort, strings.TrimSpace(parts[2]), nil
}

// StartReaper periodically terminates expired sessions.
func (o *Orchestrator) StartReaper(ctx context.Context, st *store.MemStore, cache *store.MemCache, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("reaper: stop")
			return
		case <-ticker.C:
			sessions, err := st.ListExpiredRunningSessions(ctx)
			if err != nil {
				log.Warn().Err(err).Msg("reaper: list")
				continue
			}
			for _, sess := range sessions {
				log.Info().Str("session_id", sess.ID.String()).Msg("reaper: expired")
				s := sess
				o.FinalizeRunningSession(ctx, st, cache, &s)
			}
		}
	}
}

// StartIdleReaper terminates sessions that have been inactive for the given duration.
func (o *Orchestrator) StartIdleReaper(ctx context.Context, st *store.MemStore, cache *store.MemCache, idleTimeout time.Duration, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("idle reaper: stop")
			return
		case <-ticker.C:
			sessions, err := st.ListIdleSessions(ctx, idleTimeout)
			if err != nil {
				log.Warn().Err(err).Msg("idle reaper: list")
				continue
			}
			for _, sess := range sessions {
				log.Info().Str("session_id", sess.ID.String()).Msg("idle reaper: idle")
				s := sess
				o.FinalizeRunningSession(ctx, st, cache, &s)
			}
		}
	}
}

// StartHealthChecker periodically probes running containers.
func (o *Orchestrator) StartHealthChecker(ctx context.Context, st *store.MemStore, cache *store.MemCache, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("health: stop")
			return
		case <-ticker.C:
			sessions, err := st.ListRunningSessions(ctx)
			if err != nil {
				log.Warn().Err(err).Msg("health: list")
				continue
			}
			for _, sess := range sessions {
				if sess.ContainerID == "" {
					continue
				}

				// Check if container is still running via docker inspect, with its own
				// deadline so a hung daemon cannot stall the whole checker.
				probeCtx, cancelProbe := context.WithTimeout(ctx, healthProbeTimeout)
				cmd := o.dockerCmd(probeCtx, "inspect", "--format", "{{.State.Running}}", sess.ContainerID)
				out, inspectErr := cmd.CombinedOutput()
				cancelProbe()
				running := inspectErr == nil && strings.TrimSpace(string(out)) == "true"

				if !running && runtimeUnreachable(string(out)) {
					// Runtime unreachable: skip the round, re-probe next tick.
					log.Warn().Err(inspectErr).Str("session_id", sess.ID.String()).
						Str("container_id", sess.ContainerID).
						Msg("health: runtime unreachable")
					continue
				}
				if running {
					o.containerMisses.Delete(sess.ID.String())
					continue
				}
				// A miss is destructive: count only a positive answer (stopped
				// or containerAbsent); unrecognized failures go to the TTL reapers.
				if inspectErr != nil && !containerAbsent(string(out)) {
					log.Warn().Err(inspectErr).Str("session_id", sess.ID.String()).
						Str("container_id", sess.ContainerID).
						Str("output", strings.TrimSpace(string(out))).
						Msg("health: inconclusive probe")
					continue
				}
				// The runtime answered, and not with "running": the container is
				// stopped or gone. Require consecutive confirmations so one false
				// negative does not finalize a live session.
				misses := o.noteContainerMiss(sess.ID.String())
				if misses < containerMissLimit {
					log.Debug().Str("session_id", sess.ID.String()).
						Str("container_id", sess.ContainerID).Int("misses", misses).
						Msg("health: down, confirming")
					continue
				}
				log.Warn().Str("session_id", sess.ID.String()).Str("container_id", sess.ContainerID).Msg("health: container dead")
				o.containerMisses.Delete(sess.ID.String())
				s := sess
				o.FinalizeRunningSession(ctx, st, cache, &s)
			}
		}
	}
}

type labeledContainer struct {
	ID        string
	SessionID string
}

// dockerReachable reports whether the container runtime answers at all, as
// opposed to answering "no such container". The two are indistinguishable by
// exit code alone, which is why responses must go through runtimeUnreachable.
func (o *Orchestrator) dockerReachable(ctx context.Context) bool {
	out, err := o.dockerCmd(ctx, "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err == nil {
		return true
	}
	log.Warn().Err(err).Str("output", strings.TrimSpace(string(out))).
		Msg("reconcile: runtime unreachable")
	return false
}

// ReconcileRestoredSessions acts on the first answer at startup: a restored row
// predates this process and occupies a slot. If the runtime is unreachable, leave it.
func (o *Orchestrator) ReconcileRestoredSessions(ctx context.Context, st *store.MemStore, cache *store.MemCache) {
	if st == nil {
		return
	}

	sessions, err := st.ListActiveSessions(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("reconcile: list")
		return
	}
	if len(sessions) == 0 {
		return
	}

	if !o.dockerReachable(ctx) {
		log.Warn().Int("sessions", len(sessions)).
			Msg("reconcile: runtime unreachable, kept")
		return
	}

	dropped := 0
	for _, sess := range sessions {
		if sess.ContainerID != "" {
			out, inspectErr := o.dockerCmd(ctx, "inspect", "--format", "{{.State.Running}}", sess.ContainerID).CombinedOutput()
			if inspectErr == nil && strings.TrimSpace(string(out)) == "true" {
				continue
			}
		}
		// Not running or never provisioned: nothing can serve. Orphans go to CleanupOrphanContainers.
		s := sess
		log.Info().Str("session_id", s.ID.String()).Str("status", s.Status).
			Str("container_id", s.ContainerID).
			Msg("reconcile: dropping, no container")
		o.FinalizeRunningSession(ctx, st, cache, &s)
		dropped++
	}

	log.Info().Int("checked", len(sessions)).Int("dropped", dropped).
		Msg("reconcile: done")
}

// listLabeledSessionContainers returns isola session containers with their session_id label.
func (o *Orchestrator) listLabeledSessionContainers(ctx context.Context) []labeledContainer {
	cmd := o.dockerCmd(ctx, "ps", "-a",
		"--filter", "label=isola.session_id",
		"--format", "{{.ID}}\t{{.Label \"isola.session_id\"}}\t{{.Label \"isola.warm\"}}",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Warn().Err(err).Str("output", string(out)).Msg("sweep: docker ps")
		return nil
	}
	return parseLabeledContainers(string(out))
}

// parseLabeledContainers: an absent warm label prints a trailing tab — trim the whole output and the last field is lost.
func parseLabeledContainers(out string) []labeledContainer {
	var containers []labeledContainer
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 || parts[0] == "" {
			continue
		}
		// Belt and braces: never let a labeling mistake make the sweep kill a possibly-serving container.
		if len(parts) == 3 && strings.TrimSpace(parts[2]) != "" {
			continue
		}
		containers = append(containers, labeledContainer{ID: parts[0], SessionID: parts[1]})
	}
	return containers
}

// listWarmContainers returns the IDs of containers labeled as warm-pool
// members. Warm containers carry no isola.session_id label, so they are
// invisible to the orphan sweep; the pool is their only lifecycle owner.
func (o *Orchestrator) listWarmContainers(ctx context.Context) []string {
	cmd := o.dockerCmd(ctx, "ps", "-a",
		"--filter", "label=isola.warm=1",
		"--format", "{{.ID}}",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Warn().Err(err).Str("output", string(out)).Msg("warm pool: docker ps")
		return nil
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// RemoveContainerByName: a failed `docker run` may still have created the
// container; the warm label hides it from the sweep, so remove it by name.
func (o *Orchestrator) RemoveContainerByName(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := o.dockerCmd(ctx, "rm", "-f", name).CombinedOutput()
	if err != nil {
		log.Debug().Err(err).Str("name", name).
			Str("output", strings.TrimSpace(string(out))).
			Msg("rm by name (likely uncreated)")
		return
	}
	log.Info().Str("name", name).Msg("rm orphan by name")
}

// CleanupOrphanContainers removes session containers that are not tracked as active.
func (o *Orchestrator) CleanupOrphanContainers(ctx context.Context, st *store.MemStore, cache *store.MemCache) {
	active := make(map[string]bool)
	sessions, err := st.ListActiveSessions(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("sweep: list")
		return
	}
	for _, sess := range sessions {
		active[sess.ID.String()] = true
		if sess.ContainerID != "" {
			active[sess.ContainerID] = true
		}
	}

	for _, c := range o.listLabeledSessionContainers(ctx) {
		if c.SessionID != "" && active[c.SessionID] {
			continue
		}
		if active[c.ID] {
			continue
		}
		log.Info().Str("container_id", c.ID).Str("session_id", c.SessionID).Msg("sweep: removing untracked")
		_ = o.TerminateSession(ctx, c.ID)
		if c.SessionID != "" {
			if id, err := uuid.Parse(c.SessionID); err == nil {
				_ = st.TerminateSession(ctx, id)
				cache.Del(ctx, sessionCacheKeys(c.SessionID)...)
			}
		}
	}
}

// StartOrphanCleaner periodically removes containers not tied to an active session.
func (o *Orchestrator) StartOrphanCleaner(ctx context.Context, st *store.MemStore, cache *store.MemCache, interval time.Duration) {
	o.CleanupOrphanContainers(ctx, st, cache)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("sweep: stop")
			return
		case <-ticker.C:
			o.CleanupOrphanContainers(ctx, st, cache)
		}
	}
}

// isolaBitrateKbps: linear in pixel area, 6 Mbps @ 720p; ABR steps down from it.
// shmBudgetMB: one BGRA frame × frames-in-flight + headroom, floored at the
// proven 1080p value and capped by the container memory limit.
const (
	shmFramesInFlight = 8
	shmHeadroomMB     = 256
	shmMinMB          = 512
	shmMaxMB          = 1024
)

func shmBudgetMB(resolution string) int {
	parts := strings.SplitN(resolution, "x", 2)
	if len(parts) != 2 {
		return shmMinMB
	}
	w, errW := strconv.Atoi(parts[0])
	h, errH := strconv.Atoi(parts[1])
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return shmMinMB
	}
	frameMB := float64(w*h*4) / (1024 * 1024)
	mb := int(math.Ceil(frameMB*shmFramesInFlight)) + shmHeadroomMB
	if mb < shmMinMB {
		mb = shmMinMB
	}
	if mb > shmMaxMB {
		mb = shmMaxMB
	}
	return mb
}

func isolaBitrateKbps(resolution string) int {
	parts := strings.SplitN(resolution, "x", 2)
	if len(parts) != 2 {
		return 6000
	}
	w, errW := strconv.Atoi(parts[0])
	h, errH := strconv.Atoi(parts[1])
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return 6000
	}
	// Linear in pixel area (6 Mbps @ 720p) so a Retina-class capture keeps the
	// same per-pixel quality. 60 Mbps guards only against a malformed
	// resolution; the container's ISOLA_MAX_PIXELS budget caps real requests.
	pixels := w * h
	base := 1280 * 720
	kbps := (6000 * pixels) / base
	if kbps < 2000 {
		kbps = 2000
	}
	if kbps > 60000 {
		kbps = 60000
	}
	return kbps
}
