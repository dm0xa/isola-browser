package mcp

import (
	"github.com/dm0xa/isola-browser/backend/pkg/api"
	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

// Deps wires the MCP server into the existing backend.
type Deps struct {
	API   *api.Handler
	Store *store.Store
	Cache *store.MemCache
	Cfg   *config.Config
}
