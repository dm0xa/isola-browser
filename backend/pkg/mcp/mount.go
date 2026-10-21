package mcp

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog/log"

	"github.com/dm0xa/isola-browser/backend/pkg/config"
	isomw "github.com/dm0xa/isola-browser/backend/pkg/middleware"
	"github.com/dm0xa/isola-browser/backend/pkg/version"
)

// Mount registers the Streamable HTTP MCP endpoint when enabled in config.
func Mount(r chi.Router, cfg *config.Config, deps Deps) {
	if cfg == nil || !cfg.MCPEnabled {
		return
	}
	if len(cfg.MCPTokens) == 0 {
		log.Error().Msg("mcp.enabled=true but mcp.token is empty — /mcp not mounted")
		return
	}
	expectedTokens := cfg.MCPTokens

	svc := &Service{deps: deps}
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "isola",
		Version: version.String(),
	}, nil)
	registerTools(server, svc)

	mcpHandler := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server {
		return server
	}, nil)

	r.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !isomw.CheckBearerAny(req, expectedTokens) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, req)
	}))
	log.Info().Msg("MCP server mounted at POST /mcp (Bearer token required)")
}
