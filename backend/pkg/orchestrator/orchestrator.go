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

	"github.com/dm0xa/isola/backend/pkg/config"
	"github.com/dm0xa/isola/backend/pkg/models"
	"github.com/dm0xa/isola/backend/pkg/store"
	"github.com/google/uuid"
)

// SessionCleanupFunc runs after a container is stopped during background termination.
type SessionCleanupFunc func(ctx context.Context, sess *models.Session)

// Orchestrator manages Docker containers for browser sessions via the docker CLI.
type Orchestrator struct {
	host             string
	onSessionCleanup SessionCleanupFunc

	proxyIPOnce sync.Once
	proxyIP     string // this process's IP on browser-net, passed to containers

	// dnsProxyEnabled is set once at startup when a DoT/DoH forwarding server
	// is listening on the browser-net IP. When false, sessions fall back to
	// plaintext `--dns` resolvers even if the config asks for encryption.
	dnsProxyEnabled bool
	// dnsFallbackWarnOnce logs the encryption downgrade once per process.
	dnsFallbackWarnOnce sync.Once

	// containerMisses counts consecutive health probes that reported a
	// session's container as not running, keyed by session ID. It exists so a
	// single false negative (a container mid-restart, a runtime hiccup) does
	// not finalize a live session.
	containerMisses sync.Map // sessionID -> *atomic.Int64
}

// containerMissLimit is how many consecutive probes must report a container
// as not running before the session is finalized.
const containerMissLimit = 3

// healthProbeTimeout bounds one `docker inspect` health probe. The probe runs on
// the app context, which lives for the whole process, so without a deadline of
// its own a wedged daemon (or a paused VM) leaves the probe blocked inside
// CombinedOutput and the health-checker goroutine never checks another session.
// On expiry the probe returns an inconclusive failure — empty output plus a
// non-nil error — which the loop deliberately skips rather than counting.
const healthProbeTimeout = 10 * time.Second

// runtimeUnreachable reports whether the output of a failed `docker inspect`
// describes a runtime the CLI could not reach, as opposed to the runtime
// answering that the container is not there.
//
// The exit code cannot be used to tell the two apart — both are non-zero — and
// neither can a single "not found" message: the docker CLI in front of the
// podman compat API answers a missing hex ID with an image-name normalization
// error rather than "no such object". So we look for the connectivity
// signatures instead: if the daemon itself complained, the container is not
// usable; if the CLI could not reach the daemon, we have learned nothing.
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

// containerAbsent reports whether the output of a failed `docker inspect`
// positively says *this container is not there*, as opposed to a failure that
// says nothing about it at all.
//
// This is the inverse of runtimeUnreachable and it exists because a miss is
// destructive: containerMissLimit consecutive ones finalize the session. Only
// an answer that names the container's absence may be allowed to count, so a
// probe that failed for an unmodelled reason — empty output, a message we do
// not recognise, a CLI killed by a timeout — is treated as inconclusive and
// skipped instead of being read as "the container is gone".
//
// Both runtimes' real removal messages live here: podman's `Error: no such
// object`, the daemon's `No such container`, and the podman-compat API's
// image-name normalization error (it reads the bare hex ID as an image name).
// Dropping that last one would make a genuinely removed container look
// inconclusive and leak its session on that runtime.
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
			log.Warn().Err(err).Msg("could not resolve backend browser-net IP; session service ports stay open to the bridge")
			return
		}
		ip := strings.TrimSpace(string(out))
		if net.ParseIP(ip) != nil {
			o.proxyIP = ip
			log.Info().Str("proxy_ip", ip).Msg("session service ports will be restricted to this proxy")
		}
	})
	return o.proxyIP
}

// New creates a new Orchestrator.
func New(dockerHost string) (*Orchestrator, error) {
	log.Info().Str("docker_host", dockerHost).Msg("orchestrator initialized")
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
	TorCountry     string
	DNSMode        string   // "plain" | "tls" | "https" — how containers resolve names
	DNSServers     []string // plaintext resolvers passed via `docker run --dns` (empty = runtime embedded resolver)
	NetworkMonitor bool     // isola: stream CDP network events to the viewer
	// NetworkLogMaxEntries is the capture window handed to the streamer
	// (NET_LOG_MAX_ENTRIES). 0 leaves the streamer's own default in place.
	NetworkLogMaxEntries int    // isola: CDP network entries retained per session
	DockerImage          string // Docker image name (e.g. isola-browser:electron)
	// warmLabel stamps the container with isola.warm=1 instead of a session
	// label. Warm-pool containers are the pool's property: the orphan sweep
	// filters on isola.session_id and must never see them. No code path may
	// set this outside the warm pool.
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

// containerLabels returns the --label pairs for a container. Exactly one of
// the two label sets is ever applied:
//
//   - session containers get isola.session_id, which the orphan sweep filters
//     on and the health checker/reapers correlate with the DB row;
//   - warm-pool containers get isola.warm instead and must NEVER carry
//     isola.session_id — the sweep would read that placeholder id as an
//     untracked session and terminate the container, including after it has
//     been claimed and is serving a user.
//
// Kept as a pure function because getting it wrong is silent and destructive.
func containerLabels(sessionID, userID string, warm bool) []string {
	if warm {
		return []string{
			"--label", "isola.warm=1",
			"--label", "isola.warm_id=" + sessionID,
		}
	}
	return []string{
		"--label", "isola.session_id=" + sessionID,
		"--label", "isola.user_id=" + userID,
	}
}

// torSessionArgs returns the `-e` environment arguments that route a session
// through Tor. Exit-country pinning is optional: an empty country means "any
// exit". The country list is re-validated here even though the API already
// normalized it — a caller that asked for a pinned exit must not silently egress
// from a wider set, so an unusable value is an error, not a downgrade.
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

// dnsSessionArgs returns the DNS-related `docker run` arguments for a session.
// In the encrypted modes (tls/https) it points the container's resolver at the
// local forwarder (proxyIP); otherwise it passes the plaintext servers.
// fellBack reports that an encrypted mode could not use the forwarder, so the
// caller can warn once instead of on every session.
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

// dnsArgs builds the `--dns` flag pairs for a session container. Blank entries
// are dropped: `docker run --dns ""` is rejected outright, so a stray empty
// YAML/env entry must not turn into a failed session. An empty result leaves the
// container on the runtime's embedded resolver (Docker 127.0.0.11 / Podman
// gateway), which is the documented opt-out.
//
// Kept as a pure function so the flag shape can be tested without Docker.
func dnsArgs(servers []string) []string {
	args := make([]string, 0, len(servers)*2)
	for _, server := range servers {
		if server = strings.TrimSpace(server); server != "" {
			args = append(args, "--dns", server)
		}
	}
	return args
}

// CreateSession starts a new browser-node container.
func (o *Orchestrator) CreateSession(ctx context.Context, params CreateSessionParams) (*SessionInfo, error) {
	name := "isola-" + params.SessionID[:12]
	log.Info().Str("session_id", params.SessionID).Str("url", params.TargetURL).Msg("creating container")

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
		log.Info().Msg("GPU: ISOLA_GPU=1 and /dev/dri/renderD128 present, passing through for HW encoding")
	}
	cmdArgs = append(cmdArgs, containerLabels(params.SessionID, params.UserID, params.warmLabel)...)
	if params.PinnedHost != "" && params.PinnedIP != "" {
		cmdArgs = append(cmdArgs, "--add-host", params.PinnedHost+":"+params.PinnedIP)
	}

	// DNS. The encrypted modes route the container's resolver at this process's
	// browser-net address, where the DoT/DoH forwarder listens; the plaintext
	// hop is then confined to the browser-net bridge. Falls back to plaintext
	// resolvers when the forwarder is not running.
	dnsCmdArgs, dnsFellBack := dnsSessionArgs(params.DNSMode, o.dnsProxyEnabled, o.proxyIPOnBrowserNet(), params.DNSServers)
	if dnsFellBack {
		o.dnsFallbackWarnOnce.Do(func() {
			log.Warn().Str("mode", params.DNSMode).Msg("DNS proxy unavailable; falling back to plaintext resolvers for this deployment")
		})
	}
	cmdArgs = append(cmdArgs, dnsCmdArgs...)

	// ── Seccomp (VULN-5) ──
	// Docker's default seccomp profile is always active unless overridden. The
	// bundled /etc/isola/seccomp.json is stricter than the default, but it is
	// not applied by default because the Chromium/ffmpeg runtime needs
	// syscalls it does not yet allow (splice, mknod, chmod, unshare, ...).
	// Operators who have validated the profile for their image can opt in via
	// ISOLA_SECCOMP_PROFILE=/etc/isola/seccomp.json.
	if p := strings.TrimSpace(os.Getenv("ISOLA_SECCOMP_PROFILE")); p != "" {
		if _, err := os.Stat(p); err == nil {
			cmdArgs = append(cmdArgs, "--security-opt", "seccomp="+p)
			log.Info().Str("profile", p).Msg("applying custom seccomp profile")
		} else {
			log.Warn().Str("profile", p).Msg("ISOLA_SECCOMP_PROFILE not found; using Docker default seccomp")
		}
	}

	// ── MAC confinement ──
	// The entrypoint refuses to start a session unless an enforcing LSM
	// (AppArmor docker-default or SELinux) confines it. Session containers do
	// not inherit this process's environment, so forward the operator's opt-out
	// (ISOLA_REQUIRE_MAC=0) for it to take effect.
	if v := strings.TrimSpace(os.Getenv("ISOLA_REQUIRE_MAC")); v != "" {
		cmdArgs = append(cmdArgs, "-e", "ISOLA_REQUIRE_MAC="+v)
	}

	if params.NetworkMonitor {
		cmdArgs = append(cmdArgs, "-e", "NET_MONITOR=1")
	}
	if params.NetworkLogMaxEntries > 0 {
		cmdArgs = append(cmdArgs, "-e", fmt.Sprintf("NET_LOG_MAX_ENTRIES=%d", params.NetworkLogMaxEntries))
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
		log.Error().Err(err).Str("output", sanitized).Str("session_id", params.SessionID).Msg("failed to create container")
		return nil, fmt.Errorf("docker run: %w: %s", err, sanitized)
	}

	containerID := strings.TrimSpace(string(out))

	// Prefer published host ports (backend on host, macOS/Podman VM scenario
	// where host has no route to the bridge subnet). Fall back to the
	// container's browser-net IP for the docker-compose deployment where
	// isola runs alongside browsers on the same bridge. A mapping in
	// `docker inspect` alone is not proof of reachability: published ports
	// bind the host's loopback, which a containerized backend cannot dial —
	// so verify with a real connection before committing to that path.
	host := ""
	port := 0
	cdpPort := 0
	if hp59, errHP := o.getHostPort(ctx, containerID, "5900/tcp"); errHP == nil && hp59 > 0 {
		if hp9223, errCDP := o.getHostPort(ctx, containerID, "9223/tcp"); errCDP == nil && hp9223 > 0 {
			if hostReachable(ctx, "127.0.0.1", hp59) {
				host = "127.0.0.1"
				port = hp59
				cdpPort = hp9223
				log.Info().Str("container_id", containerID).
					Int("stream_port", port).Int("cdp_port", cdpPort).
					Msg("container ready, using published host ports")
			} else {
				log.Info().Str("container_id", containerID).
					Int("stream_port", hp59).
					Msg("published host port unreachable from this backend, falling back to browser-net address")
			}
		}
	}
	if host == "" {
		ip, err := o.GetContainerIP(ctx, containerID)
		if err != nil {
			log.Error().Err(err).Str("container_id", containerID).Msg("failed to get container IP")
			return nil, err
		}
		host = ip
		port = 5900
		cdpPort = 9223
		log.Info().Str("container_id", containerID).Str("ip", host).Msg("container ready, using browser-net address")
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
func (o *Orchestrator) FinalizeRunningSession(ctx context.Context, st *store.Store, cache *store.MemCache, sess *models.Session) {
	if sess == nil {
		return
	}
	sid := sess.ID.String()
	log.Info().Str("session_id", sid).Msg("finalizing session termination")

	if sess.ContainerID != "" {
		if err := o.TerminateSession(ctx, sess.ContainerID); err != nil {
			log.Warn().Err(err).Str("container_id", sess.ContainerID).Msg("finalize: container stop failed")
		}
	}

	cache.Del(ctx, sessionCacheKeys(sid)...)

	if o.onSessionCleanup != nil {
		o.onSessionCleanup(ctx, sess)
	}

	if err := st.Sessions.TerminateSession(ctx, sess.ID); err != nil {
		log.Warn().Err(err).Str("session_id", sid).Msg("finalize: db terminate failed")
	}
}

// TerminateSession stops and removes a browser-node container.
func (o *Orchestrator) TerminateSession(ctx context.Context, containerID string) error {
	log.Info().Str("container_id", containerID).Msg("terminating container")

	cmd := o.dockerCmd(ctx, "stop", "-t", "3", containerID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Warn().Err(err).Str("output", string(out)).Str("container_id", containerID).Msg("docker stop failed, force-removing")
	}

	// Force remove to ensure cleanup (safe even if already removed via --rm)
	rmCmd := o.dockerCmd(ctx, "rm", "-f", containerID)
	rmCmd.CombinedOutput()

	return nil
}

// ResizeSessionDisplay asks isola-streamer to apply a capture resize.
//
// It talks to the streamer's HTTP port directly, like the audio toggle: the
// sandbox image is minimal and ships no curl, so shelling out inside the
// container is not an option.
func (o *Orchestrator) ResizeSessionDisplay(ctx context.Context, containerHost string, containerPort, width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("invalid resize target: %dx%d", width, height)
	}
	if containerHost == "" || containerPort == 0 {
		return fmt.Errorf("streamer address unknown")
	}

	log.Info().Str("container_host", containerHost).Int("port", containerPort).Int("width", width).Int("height", height).Msg("resize: notifying isola-streamer")
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
	log.Info().Str("container_host", containerHost).Msg("resize: isola resize ok")
	return nil
}

// WaitForStream waits until the isola-streamer health endpoint is ready.
func (o *Orchestrator) WaitForStream(ctx context.Context, host string, port int, streamMode string, browserType string, timeout time.Duration) error {
	_ = streamMode
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	deadline := time.Now().Add(timeout)
	retryInterval := 200 * time.Millisecond
	const maxRetries = 60

	for attempt := 0; ; attempt++ {
		if time.Now().After(deadline) || attempt >= maxRetries {
			return fmt.Errorf("stream not ready at %s after %v (%d attempts)", addr, timeout, attempt)
		}

		ready, err := o.waitForIsolaReady(host, port)
		if err == nil && ready {
			log.Info().Str("addr", addr).Str("browser_type", browserType).Msg("isola stream is ready")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryInterval):
		}
		retryInterval *= 2
		if retryInterval > 1*time.Second {
			retryInterval = 1 * time.Second
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

// StartReaper periodically terminates expired sessions.
func (o *Orchestrator) StartReaper(ctx context.Context, st *store.Store, cache *store.MemCache, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("reaper stopping")
			return
		case <-ticker.C:
			sessions, err := st.Sessions.ListExpiredRunningSessions(ctx)
			if err != nil {
				log.Error().Err(err).Msg("reaper: failed to list expired sessions")
				continue
			}
			for _, sess := range sessions {
				log.Info().Str("session_id", sess.ID.String()).Msg("reaper: terminating expired session")
				s := sess
				o.FinalizeRunningSession(ctx, st, cache, &s)
			}
		}
	}
}

// StartIdleReaper terminates sessions that have been inactive for the given duration.
func (o *Orchestrator) StartIdleReaper(ctx context.Context, st *store.Store, cache *store.MemCache, idleTimeout time.Duration, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("idle reaper stopping")
			return
		case <-ticker.C:
			sessions, err := st.Sessions.ListIdleSessions(ctx, idleTimeout)
			if err != nil {
				log.Error().Err(err).Msg("idle reaper: failed to list idle sessions")
				continue
			}
			for _, sess := range sessions {
				log.Info().Str("session_id", sess.ID.String()).Msg("idle reaper: terminating idle session")
				s := sess
				o.FinalizeRunningSession(ctx, st, cache, &s)
			}
		}
	}
}

// StartHealthChecker periodically probes running containers.
func (o *Orchestrator) StartHealthChecker(ctx context.Context, st *store.Store, cache *store.MemCache, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("health checker stopping")
			return
		case <-ticker.C:
			sessions, err := st.Sessions.ListRunningSessions(ctx)
			if err != nil {
				log.Error().Err(err).Msg("health checker: failed to list running sessions")
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
					// The CLI never got an answer out of the runtime: socket
					// hiccup, paused VM, daemon restarting. That says nothing
					// about this particular container, so skip the round and
					// re-probe on the next tick instead of finalizing.
					log.Warn().Err(inspectErr).Str("session_id", sess.ID.String()).
						Str("container_id", sess.ContainerID).
						Msg("health checker: runtime unreachable, skipping")
					continue
				}
				if running {
					o.containerMisses.Delete(sess.ID.String())
					continue
				}
				// Everything below is a *failed* probe, and a miss is destructive:
				// containerMissLimit consecutive ones finalize the session. So a miss
				// may only be counted on a positive answer about this container — the
				// runtime said it is stopped (the CLI succeeded and printed false), or
				// it said the container is not there (containerAbsent). Any other
				// failure has told us nothing about the container, and counting it
				// finalizes sessions whose container the daemon still reports as Up.
				//
				// The trade is deliberate: an unrecognised failure leaves a genuinely
				// dead session's row to the expiry and idle reapers, which is bounded
				// by the session TTL, instead of destroying a live sandbox. A row that
				// outlives its container for a few minutes costs a slot; a false
				// finalize costs the user their work.
				if inspectErr != nil && !containerAbsent(string(out)) {
					log.Warn().Err(inspectErr).Str("session_id", sess.ID.String()).
						Str("container_id", sess.ContainerID).
						Str("output", strings.TrimSpace(string(out))).
						Msg("health checker: inconclusive probe, skipping")
					continue
				}
				// The runtime answered, and not with "running": the container is
				// stopped or gone. Require consecutive confirmations so one false
				// negative does not finalize a live session.
				misses := o.noteContainerMiss(sess.ID.String())
				if misses < containerMissLimit {
					log.Warn().Str("session_id", sess.ID.String()).
						Str("container_id", sess.ContainerID).Int("misses", misses).
						Msg("health checker: container not running (confirming)")
					continue
				}
				log.Warn().Str("session_id", sess.ID.String()).Str("container_id", sess.ContainerID).Msg("container dead, cleaning up")
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
		Msg("startup reconcile: container runtime unreachable")
	return false
}

// ReconcileRestoredSessions finalizes restored sessions that no container backs
// any more. It runs once at startup, before traffic is served.
//
// The periodic health checker reaches the same verdict, but it deliberately
// demands containerMissLimit consecutive misses before acting, so a single
// false negative cannot kill a live session. A row restored from the snapshot
// deserves none of that benefit of the doubt: it predates this process, and
// until it is finalized it occupies a slot against ISOLA_MAX_SESSIONS (admission
// counts provisioning + running sessions). So this acts on the first answer —
// the mirror image of CleanupOrphanContainers, which removes the opposite
// leftover (containers with no session row).
//
// Conservative in exactly one direction: if the runtime cannot be reached at
// all, nothing can be verified and every restored row is left alone for the
// health checker and the store reaper.
func (o *Orchestrator) ReconcileRestoredSessions(ctx context.Context, st *store.Store, cache *store.MemCache) {
	if st == nil || st.Sessions == nil {
		return
	}

	sessions, err := st.Sessions.ListActiveSessions(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("startup reconcile: list active sessions failed")
		return
	}
	if len(sessions) == 0 {
		return
	}

	if !o.dockerReachable(ctx) {
		log.Warn().Int("sessions", len(sessions)).
			Msg("startup reconcile: runtime unreachable, leaving restored sessions alone")
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
		// Either the runtime answered and the container is not running, or the row
		// never recorded a container — the process died during provisioning. Both
		// leave nothing that can serve. A container that outlived its row is swept
		// by CleanupOrphanContainers, because no session row names it.
		s := sess
		log.Info().Str("session_id", s.ID.String()).Str("status", s.Status).
			Str("container_id", s.ContainerID).
			Msg("startup reconcile: dropping restored session with no live container")
		o.FinalizeRunningSession(ctx, st, cache, &s)
		dropped++
	}

	log.Info().Int("checked", len(sessions)).Int("dropped", dropped).
		Msg("startup reconcile: restored sessions checked")
}

// listLabeledSessionContainers returns isola session containers with their session_id label.
func (o *Orchestrator) listLabeledSessionContainers(ctx context.Context) []labeledContainer {
	cmd := o.dockerCmd(ctx, "ps", "-a",
		"--filter", "label=isola.session_id",
		"--format", "{{.ID}}\t{{.Label \"isola.session_id\"}}\t{{.Label \"isola.warm\"}}",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Warn().Err(err).Str("output", string(out)).Msg("orphan sweep: docker ps failed")
		return nil
	}
	var containers []labeledContainer
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || parts[0] == "" {
			continue
		}
		// Belt and braces: warm-pool containers are the pool's property. A
		// claimed one is already covered by the active-container check below,
		// but a labeling mistake must never make the sweep kill a container
		// that may be serving a user.
		if strings.TrimSpace(parts[2]) != "" {
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
		log.Warn().Err(err).Str("output", string(out)).Msg("warm pool: docker ps failed")
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

// RemoveContainerByName force-removes a container by its deterministic name
// and ignores the not-found case.
//
// Needed because a failed `docker run` can still have created the container:
// if the CLI dies after the daemon accepted the request (signal, timeout,
// constrained host), only the client sees an error while the container runs
// on. The container carries the warm label, so the orphan sweep will not
// collect it — it has to be removed by name.
func (o *Orchestrator) RemoveContainerByName(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := o.dockerCmd(ctx, "rm", "-f", name).CombinedOutput()
	if err != nil {
		log.Debug().Err(err).Str("name", name).
			Str("output", strings.TrimSpace(string(out))).
			Msg("container cleanup by name (likely never created)")
		return
	}
	log.Info().Str("name", name).Msg("removed container left by a failed create")
}

// CleanupOrphanContainers removes session containers that are not tracked as active.
func (o *Orchestrator) CleanupOrphanContainers(ctx context.Context, st *store.Store, cache *store.MemCache) {
	active := make(map[string]bool)
	sessions, err := st.Sessions.ListActiveSessions(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("orphan sweep: list active sessions failed")
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
		log.Info().Str("container_id", c.ID).Str("session_id", c.SessionID).Msg("orphan sweep: removing untracked container")
		_ = o.TerminateSession(ctx, c.ID)
		if c.SessionID != "" {
			if id, err := uuid.Parse(c.SessionID); err == nil {
				_ = st.Sessions.TerminateSession(ctx, id)
				cache.Del(ctx, sessionCacheKeys(c.SessionID)...)
			}
		}
	}
}

// StartOrphanCleaner periodically removes containers not tied to an active session.
func (o *Orchestrator) StartOrphanCleaner(ctx context.Context, st *store.Store, cache *store.MemCache, interval time.Duration) {
	o.CleanupOrphanContainers(ctx, st, cache)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("orphan cleaner stopping")
			return
		case <-ticker.C:
			o.CleanupOrphanContainers(ctx, st, cache)
		}
	}
}

// isolaBitrateKbps scales encoder bitrate linearly with pixel area
// (6 Mbps @ 720p). The viewer's ABR ladder steps down from this base.
// shmBudgetMB sizes a session's /dev/shm tmpfs for the capture it will carry.
//
// One BGRA frame at the clamped capture size is the unit that matters (8.3 MB
// at 1920x1080), and the offscreen compositor keeps several copy-output
// bitmaps in flight while raster tiles, shared bitmaps and V8 need room of
// their own — hence a frame count plus headroom rather than a round number.
// The floor keeps today's proven value at 1080p, and the ceiling keeps the
// budget inside the container's memory limit (default 2g — see
// ISOLA_SESSION_MEMORY_MB).
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
