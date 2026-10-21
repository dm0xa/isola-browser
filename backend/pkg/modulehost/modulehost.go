package modulehost

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/dm0xa/isola-browser/backend/pkg/modulesdk"
	"github.com/dm0xa/isola-browser/backend/pkg/registry"
)

// Options configures the host.
type Options struct {
	// Dir is the modules root: one package subdirectory per module.
	Dir string
	// DSN is injected to modules whose manifest requires db (ISOLA_DB_DSN).
	DSN string
	// RedisURL is injected to modules whose manifest requires redis.
	RedisURL string
}

// Host supervises all discovered modules for one backend process.
type Host struct {
	opts    Options
	runtime string // per-host dir for module sockets (0700) — package dirs may be read-only
	mu      sync.Mutex
	modules []*Module
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// createSocketDir makes the host-owned directory that holds module sockets.
//
// AF_UNIX socket paths are length-capped (104 bytes on macOS, 108 on Linux), so
// a long $TMPDIR — macOS per-user temp dirs are long, CI and container temp
// dirs longer — can push <tmp>/isola-*/<module>.sock past the limit and make
// every bind fail with EINVAL. Prefer the short, conventional /tmp when it is
// writable, falling back to the system temp dir.
func createSocketDir() (string, error) {
	var firstErr error
	for _, base := range []string{"/tmp", os.TempDir()} {
		if base == "" {
			continue
		}
		dir, err := os.MkdirTemp(base, "isola-")
		if err == nil {
			return dir, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return "", firstErr
}

// Start discovers and launches every enabled module under opts.Dir and
// bridges reported capabilities into the plugin registry. A single failing
// module is logged and skipped — it never takes the backend down.
func Start(ctx context.Context, opts Options) (*Host, error) {
	found, err := Discover(opts.Dir)
	if err != nil {
		return nil, err
	}

	hctx, cancel := context.WithCancel(ctx)
	h := &Host{opts: opts, cancel: cancel}

	// Sockets live in a host-owned runtime dir: module packages may be
	// mounted read-only (the integrity guarantee is worth keeping), and a
	// world-writable socket next to the binary would be a tamper vector.
	// createSocketDir keeps the path short — AF_UNIX paths are length-capped.
	runtimeDir, err := createSocketDir()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("modulehost: runtime dir: %w", err)
	}
	h.runtime = runtimeDir
	// Socket names are per-module inside a 0700 dir owned by the backend user.
	_ = os.Chmod(runtimeDir, 0o700)

	for _, d := range found {
		name := d.Manifest.Module
		if !d.Enabled {
			log.Info().Str("module", name).Msg("modulehost: disabled — skipping")
			continue
		}
		if !d.BinFound {
			log.Warn().Str("module", name).Str("bin", d.Manifest.Backend.Bin).
				Msg("modulehost: binary missing — skipping (reinstall the module)")
			continue
		}
		if d.Manifest.API != "" && d.Manifest.API != modulesdk.APIVersion {
			log.Warn().Str("module", name).Str("api", d.Manifest.API).
				Msg("modulehost: unsupported manifest api — skipping")
			continue
		}

		m := &Module{man: d.Manifest, dir: d.Dir, opts: opts, socket: filepath.Join(runtimeDir, name+".sock")}
		if err := m.launch(hctx); err != nil {
			log.Error().Str("module", name).Err(err).Msg("modulehost: launch failed — module disabled for this run")
			m.stop()
			continue
		}

		var info modulesdk.Info
		if err := m.clientSnapshot().get(hctx, "/module/info", &info); err != nil {
			log.Error().Str("module", name).Err(err).Msg("modulehost: info fetch failed — module disabled for this run")
			m.stop()
			continue
		}
		adapters{m}.registerInRegistry(info)
		// The manifest's frontend entry is declarative: the chunk is served
		// and advertised even when the module's SDK server doesn't declare
		// a frontend capability — a UI must not require a backend hook.
		if d.Manifest.Frontend.Entry != "" {
			registry.RegisterFrontendPlugin(frontendAdapter{m})
		}

		h.mu.Lock()
		h.modules = append(h.modules, m)
		h.mu.Unlock()

		h.wg.Add(1)
		go m.supervise(hctx, &h.wg)
	}

	h.wg.Add(1)
	go h.healthLoop(hctx)

	log.Info().Int("modules", len(h.modules)).Str("dir", opts.Dir).Msg("modulehost: started")
	return h, nil
}

// Stop terminates all module processes and the supervision loops.
func (h *Host) Stop() {
	h.cancel()
	h.mu.Lock()
	mods := append([]*Module(nil), h.modules...)
	runtime := h.runtime
	h.mu.Unlock()
	for _, m := range mods {
		m.stop()
	}
	h.wg.Wait()
	if runtime != "" {
		_ = os.RemoveAll(runtime)
	}
}

// healthLoop periodically probes each module; a hung process (answers nothing
// for 3s) is killed so its supervise loop restarts it with backoff.
func (h *Host) healthLoop(ctx context.Context) {
	defer h.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		h.mu.Lock()
		mods := append([]*Module(nil), h.modules...)
		h.mu.Unlock()
		for _, m := range mods {
			if err := m.health(ctx); err != nil {
				log.Warn().Str("module", m.man.Module).Err(err).
					Msg("modulehost: health probe failed — killing for supervised restart")
				m.kill()
			}
		}
	}
}

// Statuses reports the current module fleet for the admin API.
func (h *Host) Statuses(ctx context.Context) []Status {
	h.mu.Lock()
	mods := append([]*Module(nil), h.modules...)
	h.mu.Unlock()
	out := make([]Status, 0, len(mods))
	for _, m := range mods {
		s := m.statusSnapshot()
		s.Enabled = true
		s.Healthy = m.health(ctx) == nil
		out = append(out, s)
	}
	return out
}

// FrontendHandler serves installed module frontend chunks at
// /modules/{name}/frontend.js so the SPA can dynamic-import them.
func (h *Host) FrontendHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/modules/")
		parts := strings.Split(rest, "/")
		if len(parts) != 2 || parts[1] != "frontend.js" || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		dir, entry, ok := h.chunkFor(parts[0])
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(dir, entry))
	}
}

func (h *Host) chunkFor(name string) (dir, entry string, ok bool) {
	if strings.Contains(name, "..") || strings.Contains(name, "/") {
		return "", "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.modules {
		if m.man.Module != name || m.man.Frontend.Entry == "" {
			continue
		}
		// The manifest is data, not code: never let its `entry` escape the
		// module directory (e.g. "../../../etc/passwd") when served.
		if !withinDir(m.dir, m.man.Frontend.Entry) {
			log.Warn().Str("module", name).Str("entry", m.man.Frontend.Entry).
				Msg("module frontend entry escapes its directory; refusing to serve")
			continue
		}
		return m.dir, m.man.Frontend.Entry, true
	}
	return "", "", false
}

// withinDir reports whether rel resolves to a path inside dir.
func withinDir(dir, rel string) bool {
	if filepath.IsAbs(rel) {
		return false
	}
	r, err := filepath.Rel(dir, filepath.Join(dir, rel))
	if err != nil {
		return false
	}
	return r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

// SetEnabled writes or removes the .disabled marker for a module package.
// Takes effect on restart (boot-time enable/disable semantics).
func SetEnabled(opts Options, module string, enabled bool) error {
	if strings.Contains(module, "..") || strings.Contains(module, "/") {
		return os.ErrInvalid
	}
	marker := filepath.Join(opts.Dir, module, disabledMarker)
	if enabled {
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(marker, []byte("disabled by administrator\n"), 0o644)
}

// Enabled reports whether a module package is currently marked enabled.
func Enabled(opts Options, module string) bool {
	if strings.Contains(module, "..") || strings.Contains(module, "/") {
		return false
	}
	_, err := os.Stat(filepath.Join(opts.Dir, module, disabledMarker))
	return os.IsNotExist(err)
}
