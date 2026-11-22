package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
)

// Live custom data injection. The streamer owns validation and the CDP work;
// this tool forwards the same /seed contract the REST API exposes, with one
// ergonomic shortcut: storage is a plain key/value map for the current page's
// origin (the streamer resolves "nowhere in particular" to where the page is).

type setSessionDataParams struct {
	SessionID      string              `json:"session_id"`
	Cookies        []models.SeedCookie `json:"cookies,omitempty" jsonschema:"Cookies to set on the live page. Omit domain and the session target's host is used. Fields: name, value, domain, path, secure, http_only, same_site (strict|lax|none), expires (unix seconds)."`
	Headers        []models.SeedHeader `json:"headers,omitempty" jsonschema:"Extra request headers for subsequent requests from the active tab. domains (optional) limits each to matching hosts."`
	LocalStorage   map[string]string   `json:"local_storage,omitempty" jsonschema:"Key/value pairs written to localStorage on the page's current origin. Survives reloads and new tabs."`
	SessionStorage map[string]string   `json:"session_storage,omitempty" jsonschema:"Key/value pairs written to sessionStorage on the page's current origin. Lost when the tab closes."`
	Reload         bool                `json:"reload,omitempty" jsonschema:"Reload the page after applying, so cookie-dependent SPA boot and header changes take effect in this call (default false)"`
}

// payload builds the /seed body. Storage entries carry no origin: the streamer
// applies them to whatever origin the page is on.
func (p *setSessionDataParams) payload() models.SessionSeed {
	var seed models.SessionSeed
	seed.Cookies = p.Cookies
	seed.Headers = p.Headers
	if len(p.LocalStorage) > 0 {
		seed.Storage = append(seed.Storage, models.SeedStorage{Kind: "local", Items: p.LocalStorage})
	}
	if len(p.SessionStorage) > 0 {
		seed.Storage = append(seed.Storage, models.SeedStorage{Kind: "session", Items: p.SessionStorage})
	}
	seed.Reload = p.Reload
	return seed
}

func (s *Service) toolSetSessionData(ctx context.Context, _ *mcp.CallToolRequest, p *setSessionDataParams) (*mcp.CallToolResult, any, error) {
	if p == nil || strings.TrimSpace(p.SessionID) == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	sess, err := s.runningSession(ctx, p.SessionID)
	if err != nil {
		return nil, nil, err
	}
	seed := p.payload()
	// Same defaults as POST /api/sessions/{id}/seed: a cookie without a domain
	// belongs to the session's target host. Without this the streamer rejects
	// it, because only the control plane knows the launch URL.
	if err := seed.FillDefaults(sess.TargetURL); err != nil {
		return nil, nil, err
	}
	raw, _, err := s.streamerPOST(ctx, p.SessionID, "/seed", seed)
	if err != nil {
		return nil, nil, err
	}
	var res models.SeedResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, nil, fmt.Errorf("unexpected /seed response: %w", err)
	}
	out, _ := json.Marshal(map[string]any{
		"status":   "ok",
		"cookies":  res.Cookies,
		"headers":  res.Headers,
		"storage":  res.Storage,
		"reloaded": res.Reloaded,
	})
	return textResult(string(out))
}
