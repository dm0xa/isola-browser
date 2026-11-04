// Real-site frame-rate regression test for a sandbox session.
//
// A session has two frame rates: the capture rate the streamer encodes at
// (ISOLA_FPS) and the rate the runner's compositor is actually told to produce.
// The runner learns about viewers only through the streamer's
// {"cmd":"capture","on":true|false} control message, and missing one of those
// transitions leaves the active tab composing at BACKGROUND_FPS — "capture
// active" in every log line and metric, a page that looks frozen, and a viewer
// stuck at ~1 fps. The unit suites cannot see that failure: each policy
// decision is correct in isolation while the wiring between the ctrl socket,
// the rate and the compositor is what breaks.
//
// This test boots the real sandbox image against a real, neutral site (GitHub
// by default), attaches a real stream WS viewer and measures the rate the page
// actually composes at via requestAnimationFrame over CDP. The idle reading
// proves the background rate applies, the attached reading proves the capture
// rate does, and the readings after a viewer reconnect, a capture re-send, a
// live resize and a detach prove the rate follows every one of those
// transitions in both directions.
//
// Opt-in because it needs Docker, a built sandbox image and network access:
//
//	ISOLA_FPS_E2E=1 go test -run TestViewerFrameRateOnRealSite -v .
//
// Overrides: ISOLA_FPS_E2E_IMAGE (default isola-browser:electron),
// ISOLA_FPS_E2E_URL (default https://github.com/), ISOLA_FPS_E2E_RESOLUTION
// (default 1280x720, lowered on small CI runners), ISOLA_FPS_E2E_KEEP=1 keeps
// the container for inspection.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	fpsE2EEnv    = "ISOLA_FPS_E2E"
	fpsE2EImage  = "ISOLA_FPS_E2E_IMAGE"
	fpsE2EURL    = "ISOLA_FPS_E2E_URL"
	fpsE2ERes    = "ISOLA_FPS_E2E_RESOLUTION"
	fpsE2EDefRes = "1280x720"
	fpsE2EFPS    = 25
)

// The background rate is BACKGROUND_FPS=1; the margin tolerates a keepalive
// capture nudge without masking a real capture-rate composition.
const fpsE2EIdleMaxRPS = 6

// The attached reading must be close to ISOLA_FPS, not merely above the
// background rate: a latch at 2 fps would pass a "> idle" assertion. The margin
// is half the capture rate because a shared CI runner rasterises and encodes on
// the same cores; the failure this catches sits at ~1-2 fps.
const fpsE2EActiveMinRatio = 0.5

func TestViewerFrameRateOnRealSite(t *testing.T) {
	if os.Getenv(fpsE2EEnv) != "1" {
		t.Skipf("set %s=1 to run the real-site frame-rate test (needs Docker + network)", fpsE2EEnv)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("docker not on PATH: %v", err)
	}
	image := envOrE2E(fpsE2EImage, "isola-browser:electron")
	site := envOrE2E(fpsE2EURL, "https://github.com/")
	resolution := envOrE2E(fpsE2ERes, fpsE2EDefRes)
	minActive := float64(fpsE2EFPS) * fpsE2EActiveMinRatio

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	name := fmt.Sprintf("isola-fps-e2e-%d", time.Now().UnixNano())
	runE2E(ctx, t, "docker", fpsE2EDockerRunArgs(image, name, site, resolution)...)
	if os.Getenv("ISOLA_FPS_E2E_KEEP") == "1" {
		t.Logf("keeping container %s", name)
	} else {
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	}

	streamPort := waitE2EPort(ctx, t, name, "5900/tcp")
	cdpPort := waitE2EPort(ctx, t, name, "9223/tcp")
	waitE2EStreamer(t, ctx, streamPort)
	page := dialE2EPage(t, ctx, cdpPort, site)
	waitE2EPageLoaded(t, page)
	time.Sleep(400 * time.Millisecond) // let the startup capture settle

	// 1. Nobody attached: the runner must compose at the background rate.
	idle := page.rafRate(t, 1500*time.Millisecond)
	if idle > fpsE2EIdleMaxRPS { // one retry: the load capture can still be settling
		time.Sleep(time.Second)
		idle = page.rafRate(t, 1500*time.Millisecond)
	}
	if idle > fpsE2EIdleMaxRPS {
		dumpE2ELogs(t, name)
		t.Fatalf("idle compositor ran at %.1f fps with no viewer attached, want <= %d (background rate not applied)",
			idle, fpsE2EIdleMaxRPS)
	}
	t.Logf("idle (no viewer): %.1f fps", idle)

	// 2. A real viewer attached to a real site: the rate must jump to the
	// capture rate while the page repaints.
	viewer := dialE2EViewer(t, ctx, streamPort)
	defer viewer.close()
	time.Sleep(1500 * time.Millisecond) // streamer gate -> runner "capture on"

	active := expectE2ERate(t, name, page, minActive, "with a viewer attached", 2500*time.Millisecond)
	m := viewer.bestSample()
	if m.CaptureFPS != fpsE2EFPS {
		t.Fatalf("streamer capture_fps = %d, want %d", m.CaptureFPS, fpsE2EFPS)
	}
	if m.EncodeFPS < int(minActive) {
		t.Fatalf("encoder delivered %d fps while a viewer scrolled a live site, want >= %d (activity_mode=%q, changes/s=%.1f)",
			m.EncodeFPS, int(minActive), m.ActivityMode, m.ChangesPerSec)
	}
	if m.ChangesPerSec <= 0 {
		t.Fatalf("page reported %.1f changes/s while scrolling: the rate reading is meaningless without a repainting page",
			m.ChangesPerSec)
	}
	t.Logf("active (viewer attached): %.1f fps; streamer encode_fps=%d changes/s=%.1f activity=%s",
		active, m.EncodeFPS, m.ChangesPerSec, m.ActivityMode)

	// 3. Viewer reconnect: the streamer re-sends "capture on" on every ctrl
	// reconnect, so the rate must survive the churn.
	viewer.close()
	viewer = dialE2EViewer(t, ctx, streamPort)
	time.Sleep(1500 * time.Millisecond)
	expectE2ERate(t, name, page, minActive, "after a viewer reconnect", 2000*time.Millisecond)

	// 4. A forced repeated "capture on" — the message the streamer sends when
	// its ctrl socket reconnects while capture is already on. It must be a
	// no-op for the rate, never a pause or a drop to the background value.
	forceE2ECaptureOn(t, name)
	time.Sleep(700 * time.Millisecond)
	expectE2ERate(t, name, page, minActive, "after a repeated capture-on", 2000*time.Millisecond)

	// 5. A live resize rebuilds the surface; the rate must survive it.
	resizeE2E(t, streamPort, 1024, 596)
	time.Sleep(time.Second)
	expectE2ERate(t, name, page, minActive, "after a live resize", 2000*time.Millisecond)

	// 6. Detach: the rate must fall back to the background value, or a session
	// burns CPU composing a page nobody watches.
	viewer.close()
	time.Sleep(2500 * time.Millisecond)
	if got := page.rafRate(t, 1500*time.Millisecond); got > fpsE2EIdleMaxRPS {
		dumpE2ELogs(t, name)
		t.Fatalf("compositor kept running at %.1f fps after the last viewer left, want <= %d",
			got, fpsE2EIdleMaxRPS)
	}
}

// expectE2ERate asserts the compositor holds at least want fps, retrying once:
// an encoder that was just replaced (viewer reconnect, resize) takes a moment
// to refill, while a compositor latched at the background value never recovers
// on its own — which is exactly the failure this test exists for.
func expectE2ERate(t *testing.T, name string, page *e2ePage, want float64, when string, window time.Duration) float64 {
	t.Helper()
	got := page.rafRate(t, window)
	if got >= want {
		t.Logf("%s: %.1f fps", when, got)
		return got
	}
	time.Sleep(800 * time.Millisecond)
	got = page.rafRate(t, window)
	if got < want {
		dumpE2ELogs(t, name)
		t.Fatalf("compositor ran at %.1f fps %s, want >= %.1f", got, when, want)
	}
	t.Logf("%s: %.1f fps", when, got)
	return got
}

// fpsE2EDockerRunArgs mirrors the orchestrator's session container hardening
// (backend/pkg/orchestrator/security_args.go): caps dropped and re-added,
// read-only rootfs, no-new-privileges, loopback-only published ports.
// ISOLA_REQUIRE_MAC=0 skips the LSM gate, which the host answers with
// AppArmor/SELinux and a test runner on a bare Docker host may not have.
func fpsE2EDockerRunArgs(image, name, site, resolution string) []string {
	return []string{
		"run", "-d", "--rm", "--name", name,
		"-p", "127.0.0.1::5900",
		"-p", "127.0.0.1::9223",
		"--cap-drop", "ALL",
		"--cap-add", "SETUID", "--cap-add", "SETGID", "--cap-add", "CHOWN",
		"--cap-add", "DAC_OVERRIDE", "--cap-add", "KILL",
		"--cap-add", "NET_BIND_SERVICE", "--cap-add", "NET_ADMIN",
		"--security-opt", "no-new-privileges",
		"--memory", "2048m", "--memory-swap", "2048m",
		"--pids-limit", "512",
		"--read-only",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=256m",
		"--tmpfs", "/var/run:rw,noexec,nosuid,nodev,size=64m",
		"--tmpfs", "/run:rw,noexec,nosuid,nodev,size=64m",
		"--tmpfs", "/home/isolauser:rw,noexec,nosuid,nodev,size=512m",
		"--tmpfs", "/dev/shm:rw,noexec,nosuid,nodev,size=256m",
		"-e", "START_URL=" + site,
		"-e", "RESOLUTION=" + resolution,
		"-e", "ISOLA_FPS=" + strconv.Itoa(fpsE2EFPS),
		"-e", "ISOLA_BITRATE_KBPS=4000",
		"-e", "STREAM_MODE=isola",
		"-e", "ISOLA_REQUIRE_MAC=0",
		image,
	}
}

func envOrE2E(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func runE2E(ctx context.Context, t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func dumpE2ELogs(t *testing.T, name string) {
	t.Helper()
	out, _ := exec.Command("docker", "logs", name).CombinedOutput()
	lines := strings.Split(string(out), "\n")
	if len(lines) > 60 {
		lines = lines[len(lines)-60:]
	}
	t.Logf("last container logs:\n%s", strings.Join(lines, "\n"))
}

// waitE2EPort resolves a published port. docker run assigns it before the
// listener is up, so this only retries the (instant) inspection call.
func waitE2EPort(ctx context.Context, t *testing.T, name, containerPort string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.CommandContext(ctx, "docker", "port", name, containerPort).CombinedOutput()
		if err == nil {
			if f := strings.Fields(strings.TrimSpace(string(out))); len(f) > 0 {
				if _, port, splitErr := net.SplitHostPort(f[len(f)-1]); splitErr == nil && port != "" {
					return port
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx done resolving %s/%s", name, containerPort)
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("no published port for %s/%s", name, containerPort)
	return ""
}

func waitE2EStreamer(t *testing.T, ctx context.Context, port string) {
	t.Helper()
	url := "http://127.0.0.1:" + port + "/healthz"
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx done waiting for the streamer")
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("streamer did not answer %s within 90s", url)
}

// e2ePage is a second CDP client on the session's page target. The streamer
// owns its own CDP connection; Chromium serves both.
type e2ePage struct {
	conn *websocket.Conn
	mu   sync.Mutex
	next int
}

type e2eTarget struct {
	Type              string `json:"type"`
	URL               string `json:"url"`
	WebSocketDebugger string `json:"webSocketDebuggerUrl"`
}

func dialE2EPage(t *testing.T, ctx context.Context, cdpPort, site string) *e2ePage {
	t.Helper()
	wantHost, err := hostOfE2E(site)
	if err != nil {
		t.Fatalf("bad %s=%q: %v", fpsE2EURL, site, err)
	}
	var wsURL string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if resp, gerr := http.Get("http://127.0.0.1:" + cdpPort + "/json/list"); gerr == nil {
			var targets []e2eTarget
			derr := json.NewDecoder(resp.Body).Decode(&targets)
			resp.Body.Close()
			if derr == nil {
				for _, c := range targets {
					if c.Type == "page" && c.WebSocketDebugger != "" && strings.Contains(c.URL, wantHost) {
						wsURL = c.WebSocketDebugger
					}
				}
			}
		}
		if wsURL != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx done waiting for the page target")
		case <-time.After(500 * time.Millisecond):
		}
	}
	if wsURL == "" {
		t.Fatalf("no page target for %q on CDP port %s", wantHost, cdpPort)
	}
	// The target's URL names the container's own :9222; rewrite the authority
	// onto the published port.
	if i := strings.Index(wsURL, "/devtools/"); i >= 0 {
		wsURL = "ws://127.0.0.1:" + cdpPort + wsURL[i:]
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial CDP %s: %v", wsURL, err)
	}
	page := &e2ePage{conn: conn}
	t.Cleanup(func() { conn.Close() })
	return page
}

func hostOfE2E(raw string) (string, error) {
	i := strings.Index(raw, "://")
	if i < 0 {
		return "", fmt.Errorf("not an absolute url")
	}
	host := raw[i+3:]
	if j := strings.IndexAny(host, "/?#"); j >= 0 {
		host = host[:j]
	}
	if host == "" {
		return "", fmt.Errorf("no host")
	}
	return host, nil
}

func waitE2EPageLoaded(t *testing.T, page *e2ePage) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		v, err := page.eval(t, `document.readyState`, 10*time.Second)
		if err == nil && v == "complete" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("page never reached readyState=complete")
}

// eval runs one expression in the page and returns its value as a string.
func (p *e2ePage) eval(t *testing.T, expr string, timeout time.Duration) (string, error) {
	t.Helper()
	p.mu.Lock()
	p.next++
	id := p.next
	p.mu.Unlock()

	if err := p.conn.WriteJSON(map[string]any{
		"id":     id,
		"method": "Runtime.evaluate",
		"params": map[string]any{"expression": expr, "awaitPromise": true, "returnByValue": true},
	}); err != nil {
		return "", err
	}
	_ = p.conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		var reply struct {
			ID     int `json:"id"`
			Result struct {
				Result struct {
					Value any `json:"value"`
				} `json:"result"`
				ExceptionDetails *struct {
					Text string `json:"text"`
				} `json:"exceptionDetails"`
			} `json:"result"`
		}
		if err := p.conn.ReadJSON(&reply); err != nil {
			return "", err
		}
		if reply.ID != id {
			continue
		}
		if reply.Result.ExceptionDetails != nil {
			return "", fmt.Errorf("page exception: %s", reply.Result.ExceptionDetails.Text)
		}
		switch v := reply.Result.Result.Value.(type) {
		case string:
			return v, nil
		case nil:
			return "", nil
		default:
			b, merr := json.Marshal(v)
			return string(b), merr
		}
	}
}

// rafRate measures how many frames the page's compositor actually produced
// over window while scrolling the real site so the surface genuinely changes.
// requestAnimationFrame is driven by the compositor, so a tab stuck at the
// background rate reports ~1/s however busy the page's own timers are.
func (p *e2ePage) rafRate(t *testing.T, window time.Duration) float64 {
	t.Helper()
	// One retry: the probe can land while a navigation replaces the document
	// (document.scrollingElement is null for a moment), which is a measurement
	// artefact, not a frame-rate finding.
	for attempt := 0; ; attempt++ {
		rate, raw, err := p.rafOnce(t, window)
		if err == nil && raw.Secs > 0 {
			return rate
		}
		if attempt > 0 {
			t.Fatalf("rAF probe failed twice (%v, raw=%+v)", err, raw)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

type rafSample struct {
	Frames  int     `json:"frames"`
	Secs    float64 `json:"secs"`
	ScrollY int     `json:"y"`
	Err     string  `json:"err"`
}

func (p *e2ePage) rafOnce(t *testing.T, window time.Duration) (float64, rafSample, error) {
	t.Helper()
	expr := fmt.Sprintf(`(async () => {
		const t0 = performance.now();
		let frames = 0;
		let err = '';
		await new Promise((resolve) => {
			(function tick() {
				frames++;
				try {
					const el = document.scrollingElement || document.body;
					const max = el ? Math.max(0, el.scrollHeight - window.innerHeight) : 0;
					if (max > 0) window.scrollTo(0, (window.scrollY + 40) %% max);
				} catch (e) { err = String(e); }
				if (performance.now() - t0 < %d) requestAnimationFrame(tick); else resolve();
			})();
		});
		const secs = (performance.now() - t0) / 1000;
		return JSON.stringify({frames, secs, y: window.scrollY, err});
	})()`, window.Milliseconds())
	raw, err := p.eval(t, expr, window+20*time.Second)
	if err != nil {
		return 0, rafSample{}, err
	}
	var got rafSample
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		return 0, rafSample{}, fmt.Errorf("probe reply %q: %w", raw, err)
	}
	if got.Secs <= 0 {
		return 0, got, fmt.Errorf("probe reported %.3fs", got.Secs)
	}
	rate := float64(got.Frames) / got.Secs
	t.Logf("rAF probe: %.1f fps (%d frames in %.3fs, scrollY=%d %s)", rate, got.Frames, got.Secs, got.ScrollY, got.Err)
	return rate, got, nil
}

type e2eMetrics struct {
	CaptureFPS    int     `json:"capture_fps"`
	EncodeFPS     int     `json:"encode_fps"`
	ChangesPerSec float64 `json:"changes_per_sec"`
	ActivityMode  string  `json:"activity_mode"`
}

// e2eViewer is a real stream WS viewer speaking the frontend's protocol, so the
// streamer's producer capture gate behaves exactly as it does for a user.
type e2eViewer struct {
	conn   *websocket.Conn
	once   sync.Once
	mu     sync.Mutex
	best   e2eMetrics
	closed chan struct{}
}

func dialE2EViewer(t *testing.T, ctx context.Context, streamPort string) *e2eViewer {
	t.Helper()
	wsURL := "ws://127.0.0.1:" + streamPort + "/?channel=isola"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial viewer %s: %v", wsURL, err)
	}
	v := &e2eViewer{conn: conn, closed: make(chan struct{})}
	t.Cleanup(v.close)
	// Drain: an unread stream WS applies backpressure and would stall the
	// session's encoder. Keep the strongest metrics sample seen.
	go func() {
		for {
			mt, data, rerr := conn.ReadMessage()
			if rerr != nil {
				return
			}
			if mt != websocket.TextMessage || len(data) == 0 || data[0] != '{' {
				continue
			}
			var msg struct {
				Type string `json:"type"`
				e2eMetrics
			}
			if json.Unmarshal(data, &msg) != nil || msg.Type != msgTypeMetrics {
				continue
			}
			v.mu.Lock()
			if msg.EncodeFPS >= v.best.EncodeFPS {
				v.best = msg.e2eMetrics
			}
			v.mu.Unlock()
		}
	}()
	return v
}

// bestSample returns the strongest metrics sample this viewer observed: the
// encoder only reaches the capture rate on ticks where the page changed, so a
// single arbitrary second can under-report a healthy stream.
func (v *e2eViewer) bestSample() e2eMetrics {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.best
}

func (v *e2eViewer) close() {
	v.once.Do(func() {
		close(v.closed)
		_ = v.conn.Close()
	})
}

// forceE2ECaptureOn re-sends {"cmd":"capture","on":true} on the runner's ctrl
// socket from inside the container: the same message the streamer writes on
// every ctrl reconnect, and the one the runner used to ignore.
func forceE2ECaptureOn(t *testing.T, name string) {
	t.Helper()
	out, err := exec.Command("docker", "exec", name, "sh", "-c",
		`printf '{"cmd":"capture","on":true}\n' | socat - UNIX-CONNECT:/run/isola/ctrl.sock`).CombinedOutput()
	if err != nil {
		t.Fatalf("forced capture-on re-send: %v\n%s", err, out)
	}
}

func resizeE2E(t *testing.T, streamPort string, w, h int) {
	t.Helper()
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%s/resize?width=%d&height=%d", streamPort, w, h), "application/json", nil)
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("resize returned %d", resp.StatusCode)
	}
}
