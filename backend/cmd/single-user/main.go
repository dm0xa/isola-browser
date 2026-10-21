package main

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"

	"github.com/dm0xa/isola-browser/backend/internal/app"
)

//go:embed frontend-dist/*
var frontendFiles embed.FS

func main() {
	opts := app.Options{
		StartupMessage: "starting isola single-user",
		// Session persistence: without it every restart wiped all sessions and
		// open viewers looped in "Reconnecting...". Sandbox containers survive
		// restarts, so restoring the routing state lets viewers resume.
		SnapshotPath: filepath.Join(envOr("ISOLA_DATA_DIR", "/var/lib/isola"), "sessions.json"),
	}

	// Serve the embedded SPA when the build produced one; otherwise the server
	// runs API-only.
	if frontendFS, err := fs.Sub(frontendFiles, "frontend-dist"); err == nil && frontendFS != nil {
		opts.FrontendFS = frontendFS
	} else {
		log.Warn().Msg("frontend not embedded — API-only mode")
	}

	if err := app.Run(opts); err != nil {
		log.Fatal().Err(err).Msg("server failed")
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
