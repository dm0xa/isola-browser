// Package app wires and runs the isola backend HTTP server.
//
// Both entry points share this bootstrap and differ only in the options they
// pass: cmd/server has no embedded UI and no session persistence, cmd/single-user
// embeds the SPA and persists session routing state across restarts.
package app

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/dm0xa/isola/backend/pkg/api"
	"github.com/dm0xa/isola/backend/pkg/config"
	"github.com/dm0xa/isola/backend/pkg/mcp"
	isomw "github.com/dm0xa/isola/backend/pkg/middleware"
	"github.com/dm0xa/isola/backend/pkg/modulehost"
	"github.com/dm0xa/isola/backend/pkg/orchestrator"
	"github.com/dm0xa/isola/backend/pkg/registry"
	"github.com/dm0xa/isola/backend/pkg/store"
	"github.com/dm0xa/isola/backend/pkg/version"
	"github.com/dm0xa/isola/backend/pkg/wsproxy"
)

// Options selects the parts that differ between the entry points.
type Options struct {
	// StartupMessage is logged once the logger is configured.
	StartupMessage string
	// FrontendFS, when non-nil, serves the embedded SPA for non-API routes.
	FrontendFS fs.FS
	// SnapshotPath, when non-empty, persists and restores session routing
	// state across restarts.
	SnapshotPath string
}

// Run bootstraps and serves until the process receives SIGINT/SIGTERM.
func Run(opts Options) error {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	switch cfg.LogLevel {
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "warn":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}
	if opts.StartupMessage != "" {
		log.Info().Str("version", version.String()).Msg(opts.StartupMessage)
	}

	// Background context for module supervision and background jobs.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Out-of-process modules — strictly opt-in. With ISOLA_MODULES_DIR unset
	// the binary runs with no sockets, no child processes and no extra routes.
	var moduleHost *modulehost.Host
	if modulesDir := cfg.ModulesDir; modulesDir != "" {
		mh, err := modulehost.Start(ctx, modulehost.Options{
			Dir:      modulesDir,
			DSN:      cfg.DatabaseURL,
			RedisURL: cfg.RedisURL,
		})
		if err != nil {
			log.Error().Err(err).Msg("modulehost: start failed — continuing without modules")
		} else {
			moduleHost = mh
			defer mh.Stop()
		}
	}

	// In-memory session store and cache — no Postgres, no Redis.
	memStore := store.NewMemStore()
	defer memStore.Close()
	st := store.NewMemBridgeStore(memStore)
	cache := store.NewMemCache()

	// Restore session routing so viewers reconnect seamlessly after a restart.
	if opts.SnapshotPath != "" {
		if err := store.LoadSnapshot(opts.SnapshotPath, memStore, cache); err != nil {
			log.Error().Err(err).Msg("failed to load session snapshot (starting clean)")
		}
	}

	orch, err := orchestrator.New(cfg.DockerHost)
	if err != nil {
		return fmt.Errorf("create docker client: %w", err)
	}

	// Encrypted DNS (DNS-over-TLS / DNS-over-HTTPS) for session containers. On
	// failure this logs and continues: sessions fall back to plaintext resolvers.
	if err := orch.SetupDNSProxy(ctx, cfg.DNSMode, cfg.DNSServers); err != nil {
		log.Error().Err(err).Msg("DNS forwarder unavailable; sessions use plaintext resolvers")
	}

	// A snapshot can predate this process. Drop restored sessions the runtime no
	// longer backs before serving traffic: otherwise they hold admission slots
	// against ISOLA_MAX_SESSIONS (until the health checker converges, or the
	// store reaper clears a stuck provisioning row) and answer reconnects for a
	// session that cannot render.
	orch.ReconcileRestoredSessions(ctx, st, cache)

	proxy := wsproxy.New(cache, st, orch)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(isomw.TrustedRealIP)
	// RedactingLogger replaces chi's middleware.Logger: the WS handshake
	// carries the control token in its query string, which must not be logged.
	r.Use(isomw.RedactingLogger)
	r.Use(middleware.Recoverer)
	// CORS_ORIGIN may list several origins (comma/whitespace separated); go-chi/cors
	// does not split it, so expand it here. X-Session-Token must be allowed or a
	// cross-origin session GET/PATCH fails its preflight.
	// CORSOrigins never returns an empty list: an unusable CORS_ORIGIN yields a
	// non-matching origin so cross-origin access is denied, not opened. Warn so
	// the misconfiguration is visible at startup.
	if len(config.Origins(cfg.CORSOrigin)) == 0 {
		log.Warn().Str("cors_origin", cfg.CORSOrigin).
			Msg("CORS_ORIGIN has no usable origin — cross-origin requests will be denied")
	}
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   cfg.CORSOrigins(),
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-Session-Token"},
		AllowCredentials: true,
		MaxAge:           300,
	}))
	r.Use(isomw.SecurityHeaders)

	// Rate limiting: 120 mutating requests/minute per client IP. Session
	// creation launches containers, so an unbounded API is a DoS vector.
	// GET/HEAD are exempt so viewer polling is not throttled.
	if cfg.RateLimitingEnabled {
		r.Use(isomw.NewRateLimiter(120, time.Minute).MiddlewareMutating)
		log.Info().Msg("rate limiting enabled (120 mutating req/min per IP)")
	}

	// Health endpoints (no timeout — they complete in microseconds).
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := memStore.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("store unavailable"))
			return
		}
		if err := cache.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("cache unavailable"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	apiHandler := api.New(st, cache, orch, cfg)
	registry.RegisterDefaultAuditLogger()
	apiHandler.SetExt(&api.HandlerExt{
		ValidateSession:  registry.BuildSessionValidator(),
		OnSessionReady:   registry.BuildOnSessionReady(),
		OnSessionDeleted: registry.BuildOnSessionDeleted(),
		PluginConfig:     registry.BuildPluginConfig(),
	})
	orch.SetSessionCleanup(apiHandler.OnBackgroundSessionEnd)

	// Warm pool: pre-started containers so POST /api/sessions activates
	// instantly when enabled. Runs only when explicitly configured.
	var warmPool *orchestrator.WarmPool
	if cfg.WarmPoolEnabled {
		warmPool = orchestrator.NewWarmPool(orch, cfg)
		warmPool.SetActiveContainers(func(ctx context.Context) (map[string]bool, error) {
			sessions, err := st.Sessions.ListActiveSessions(ctx)
			if err != nil {
				return nil, err
			}
			set := make(map[string]bool, len(sessions))
			for _, sess := range sessions {
				if sess.ContainerID != "" {
					set[sess.ContainerID] = true
				}
			}
			return set, nil
		})
		apiHandler.SetWarmPool(warmPool)
		go warmPool.Start(ctx, 30*time.Second)
	} else {
		log.Info().Msg("warm pool disabled (warm_pool.enabled=false)")
	}

	// API routes — 60s timeout for REST calls only (not WebSocket).
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(60 * time.Second))
		apiHandler.RegisterRoutes(r)
	})
	mcp.Mount(r, cfg, mcp.Deps{API: apiHandler, Store: st, Cache: cache, Cfg: cfg})

	// WebSocket proxy route (no timeout — long-lived connection).
	r.HandleFunc("/ws/{sessionId}", proxy.HandleWebSocket)

	// Module frontend chunks (only when module support is active).
	if moduleHost != nil {
		r.Handle("/modules/*", moduleHost.FrontendHandler())
	}

	// Dynamic WebSocket sub-routes from plugins (no timeout).
	for _, p := range registry.WebSocketPlugins() {
		subPath := p.SubPath()
		handler := p.ServeWS
		r.HandleFunc("/ws/{sessionId}/"+subPath, func(w http.ResponseWriter, r *http.Request) {
			sid := chi.URLParam(r, "sessionId")
			// Plugin sub-routes stream session data, so they must enforce the
			// same token capabilities as the main relay — a bare session id is
			// never sufficient (docs/security.md).
			canView, _, authErr := proxy.AuthorizeWS(r, sid)
			if authErr != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !canView {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			handler(w, r, sid)
		})
	}

	// Embedded SPA (single-user build only).
	if opts.FrontendFS != nil {
		frontendHandler := http.FileServer(http.FS(opts.FrontendFS))
		// http.FileServer over an embed.FS sends no validators (no Last-Modified,
		// no ETag) and no Cache-Control, so the browser has no way to know the app
		// shell changed and can keep running the previous bundle — a frontend fix
		// then never reaches the user. Vite content-hashes everything under
		// /assets, so those are safe to pin; the shell must be revalidated.
		serveFrontend := func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			frontendHandler.ServeHTTP(w, r)
		}
		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			// SPA fallback: serve index.html for non-API routes.
			r.URL.Path = "/"
			serveFrontend(w, r)
		})
		r.Get("/assets/*", serveFrontend)
		r.Get("/fonts/*", serveFrontend)
		r.Get("/favicon.svg", serveFrontend)
		r.Get("/index.html", serveFrontend)
	}

	// Background jobs (ctx created at the top for module supervision).
	if opts.SnapshotPath != "" {
		go store.StartSnapshotSaver(ctx, opts.SnapshotPath, memStore, cache, 10*time.Second)
	}
	go orch.StartReaper(ctx, st, cache, 10*time.Second)
	go orch.StartHealthChecker(ctx, st, cache, 15*time.Second)
	go orch.StartIdleReaper(ctx, st, cache,
		time.Duration(cfg.InactivityMin)*time.Minute,
		10*time.Second)
	go orch.StartOrphanCleaner(ctx, st, cache, 60*time.Second)
	go proxy.StartActivityFlusher(ctx, 10*time.Second)

	addr := fmt.Sprintf(":%s", cfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Info().Msg("shutting down...")
		cancel()
		// Stop warm containers before the process exits, otherwise they are
		// left behind and the next start has to sweep them and refill from
		// scratch. Bounded so a slow runtime cannot hang shutdown.
		if warmPool != nil {
			drainCtx, cancelDrain := context.WithTimeout(context.Background(), 30*time.Second)
			warmPool.Drain(drainCtx)
			cancelDrain()
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info().Str("addr", addr).Msg("server listening")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
