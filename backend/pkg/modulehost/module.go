package modulehost

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/dm0xa/isola-browser/backend/pkg/modulesdk"
)

// Module is one launched module process plus its RPC client.
type Module struct {
	mu      sync.Mutex
	man     Manifest
	dir     string
	socket  string
	client  *rpcClient
	cmd     *exec.Cmd
	running bool

	restarts  int
	lastError string
	startedAt time.Time

	opts Options
}

// launch spawns the module binary and waits for its contract endpoint.
// The socket lives at a fixed path inside the package dir so restarts are
// transparent to the RPC client.
func (m *Module) launch(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Socket path is chosen by the host (runtime dir, not the package dir —
	// packages are often mounted read-only; the integrity guarantee stays).
	_ = os.Remove(m.socket)

	cmd := exec.CommandContext(ctx, filepath.Join(m.dir, m.man.Backend.Bin))
	cmd.Env = append(os.Environ(), modulesdk.EnvSocket+"="+m.socket)
	if m.opts.DSN != "" && m.man.Requires.DB {
		cmd.Env = append(cmd.Env, "ISOLA_DB_DSN="+m.opts.DSN)
	}
	if m.opts.RedisURL != "" && m.man.Requires.Redis {
		cmd.Env = append(cmd.Env, "ISOLA_REDIS_URL="+m.opts.RedisURL)
	}
	// Module package dir is the working dir so relative asset paths resolve.
	cmd.Dir = m.dir
	cmd.Stdout = logWriter{module: m.man.Module}
	cmd.Stderr = logWriter{module: m.man.Module}

	if err := cmd.Start(); err != nil {
		m.lastError = err.Error()
		return fmt.Errorf("modulehost: start %s: %w", m.man.Module, err)
	}
	m.cmd = cmd
	m.running = true
	m.startedAt = time.Now()

	// Wait for the contract endpoint (socket bind + info doc), bounded.
	client := newRPCClient(m.socket)
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var info modulesdk.Info
		if lastErr = client.get(ctx, "/module/info", &info); lastErr == nil {
			if info.Name != "" && info.API == modulesdk.APIVersion {
				m.client = client
				// Prefer the runtime subpath when the manifest omits it.
				if m.man.SubPath == "" {
					m.man.SubPath = info.SubPath
				}
				log.Info().Str("module", info.Name).Str("version", info.Version).
					Strs("implements", info.Implements).Msg("modulehost: module ready")
				return nil
			}
			lastErr = fmt.Errorf("contract mismatch: api=%q name=%q", info.API, info.Name)
		}
		time.Sleep(150 * time.Millisecond)
	}
	// Failed to come up — kill and report.
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	m.running = false
	m.lastError = lastErr.Error()
	return fmt.Errorf("modulehost: %s did not become ready: %w", m.man.Module, lastErr)
}

// supervise restarts the module with exponential backoff when its process
// exits for any reason other than host shutdown. Registry adapters keep
// pointing at this Module, so restarts are invisible to callers.
func (m *Module) supervise(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	backoff := time.Second
	for {
		waitErr := m.wait()
		select {
		case <-ctx.Done():
			return
		default:
		}

		m.mu.Lock()
		m.restarts++
		m.lastError = fmt.Sprintf("process exited: %v", waitErr)
		m.running = false
		m.mu.Unlock()
		log.Warn().Str("module", m.man.Module).Err(waitErr).
			Int("restarts", m.restarts).Msg("modulehost: module exited — restarting")

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}

		if err := m.launch(ctx); err != nil {
			log.Error().Str("module", m.man.Module).Err(err).Msg("modulehost: relaunch failed")
			continue
		}
		backoff = time.Second
	}
}

func (m *Module) wait() error {
	m.mu.Lock()
	cmd := m.cmd
	m.mu.Unlock()
	if cmd == nil {
		return nil
	}
	return cmd.Wait()
}

// health pings /module/info; used by the host's periodic health loop.
func (m *Module) health(ctx context.Context) error {
	client := m.clientSnapshot()
	if client == nil {
		return fmt.Errorf("not launched")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var info modulesdk.Info
	return client.get(ctx, "/module/info", &info)
}

func (m *Module) clientSnapshot() *rpcClient {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.client
}

// kill terminates the process so the supervise loop restarts it (health
// recovery path — the process is hung, not exiting).
func (m *Module) kill() {
	m.mu.Lock()
	cmd, running := m.cmd, m.running
	m.mu.Unlock()
	if cmd != nil && running && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// stop terminates the process (host shutdown path).
func (m *Module) stop() {
	m.mu.Lock()
	cmd, running := m.cmd, m.running
	m.mu.Unlock()
	if cmd != nil && running && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	_ = os.Remove(filepath.Join(m.dir, "module.sock"))
}

// Status is the admin-facing module health record.
type Status struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Enabled   bool   `json:"enabled"`
	Running   bool   `json:"running"`
	Healthy   bool   `json:"healthy"`
	Restarts  int    `json:"restarts"`
	UptimeSec int64  `json:"uptime_sec"`
	LastError string `json:"last_error,omitempty"`
	Feature   string `json:"feature,omitempty"`
}

// statusSnapshot builds the Status record (health probe done by caller).
func (m *Module) statusSnapshot() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{
		Name:      m.man.Module,
		Version:   m.man.Version,
		Feature:   m.man.Feature,
		Restarts:  m.restarts,
		LastError: m.lastError,
		Running:   m.running,
		UptimeSec: int64(time.Since(m.startedAt).Seconds()),
	}
}

// logWriter prefixes module stdout/stderr with the module name.
type logWriter struct{ module string }

func (w logWriter) Write(p []byte) (int, error) {
	log.Info().Str("module", w.module).Msg(string(trimNewline(p)))
	return len(p), nil
}

func trimNewline(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// socketPath exposes the UDS path for the WS pipe (single consumer, no lock
// needed after launch sets it before ServeWS is registered).
func (m *Module) socketPath() string { return m.socket }

// dialModule dials the module socket raw — used by the WS byte-pipe.
func (m *Module) dialModule(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", m.socket)
}
