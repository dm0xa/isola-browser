package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dm0xa/isola-browser/backend/pkg/api"
	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/orchestrator"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

// TestCreateSessionParamsToRequest pins that the MCP `tor` argument reaches the
// API request as TorEnabled. The backend routes TorEnabled end to end
// (session record → orchestrator TOR_ENABLED env → container), so the only gap
// this guards is the MCP mapping: dropping it silently disabled Tor for every
// session created through MCP.
func TestCreateSessionParamsToRequest(t *testing.T) {
	t.Parallel()

	on := createSessionParams{URL: " https://example.com ", Tor: true, TorCountry: " se "}
	req := on.toRequest()
	if !req.TorEnabled {
		t.Fatal("tor=true must set TorEnabled on the request")
	}
	if req.URL != "https://example.com" {
		t.Fatalf("url = %q, want trimmed", req.URL)
	}
	if req.TorCountry != "se" {
		t.Fatalf("tor_country = %q, want trimmed 'se'", req.TorCountry)
	}
	if !req.NetworkMonitor {
		t.Fatal("network_monitor must stay enabled for MCP-created sessions")
	}

	off := createSessionParams{URL: "https://example.com"}
	if off.toRequest().TorEnabled {
		t.Fatal("omitting tor must leave TorEnabled false")
	}
	if off.toRequest().TorCountry != "" {
		t.Fatal("omitting tor_country must leave it empty")
	}
}

// TestMCPCreateSessionAdvertisesTor guards the tool schema: a client can only
// turn Tor on if `tor` is listed among create_session's arguments.
func TestMCPCreateSessionAdvertisesTor(t *testing.T) {
	const token = "mcp-tor-test-token-32-characters!!"
	mem := store.NewMemStore()
	t.Cleanup(func() { mem.Close() })
	st := store.NewMemBridgeStore(mem)
	cache := store.NewMemCache()
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock") // lazy — no docker needed
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		MCPEnabled:           true,
		MCPTokens:            []string{token},
		BrowserType:          "chromium",
		Browsers:             map[string]config.BrowserEntry{"chromium": {Enabled: true}},
		DisableURLValidation: true,
	}

	r := chi.NewRouter()
	Mount(r, cfg, Deps{API: api.New(st, cache, orch, cfg), Store: st, Cache: cache, Cfg: cfg})
	srv := httptest.NewServer(r)
	defer srv.Close()

	sess := connectTestMCP(t, srv.URL+"/mcp", token)
	defer sess.Close()

	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var create *mcp.Tool
	for _, tool := range tools.Tools {
		if tool.Name == "create_session" {
			create = tool
			break
		}
	}
	if create == nil {
		t.Fatal("create_session tool not registered")
	}

	b, err := json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	var generic struct {
		InputSchema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"inputSchema"`
	}
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatal(err)
	}
	if _, ok := generic.InputSchema.Properties["tor"]; !ok {
		t.Fatalf("create_session schema lacks a `tor` property: %s", b)
	}
	if _, ok := generic.InputSchema.Properties["tor_country"]; !ok {
		t.Fatalf("create_session schema lacks a `tor_country` property: %s", b)
	}
}

// TestMCPCreateSessionRequestBypassesAPITokenGate guards the wiring between the
// MCP tool and the REST handler: create_session must not run into
// ISOLA_API_TOKEN, because the caller already authenticated on /mcp.
func TestMCPCreateSessionRequestBypassesAPITokenGate(t *testing.T) {
	const mcpToken = "mcp-gate-test-token-32-characters!!"
	const apiToken = "create-gate-token-32-characters-min!!"

	mem := store.NewMemStore()
	t.Cleanup(func() { mem.Close() })
	st := store.NewMemBridgeStore(mem)
	cache := store.NewMemCache()
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		MCPEnabled:           true,
		MCPTokens:            []string{mcpToken},
		APITokens:            []string{apiToken},
		BrowserType:          "chromium",
		Browsers:             map[string]config.BrowserEntry{"chromium": {Enabled: true}},
		DisableURLValidation: true,
	}
	h := api.New(st, cache, orch, cfg)

	params := createSessionParams{URL: "https://example.com"}
	body, err := json.Marshal(params.toRequest())
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.CreateSession(w, newCreateSessionRequest(context.Background(), body))
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("MCP create_session blocked by the api.token gate: %d %s", w.Code, w.Body.String())
	}
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
}
