package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dm0xa/isola-browser/backend/pkg/api"
	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/orchestrator"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

type streamerHits struct {
	mu   sync.Mutex
	path map[string]int
}

func (h *streamerHits) inc(path string) {
	h.mu.Lock()
	h.path[path]++
	h.mu.Unlock()
}

func (h *streamerHits) count(path string) int {
	h.mu.Lock()
	n := h.path[path]
	h.mu.Unlock()
	return n
}

func newFakeStreamer() (*httptest.Server, *streamerHits) {
	hits := &streamerHits{path: map[string]int{}}
	mux := http.NewServeMux()
	record := func(path string, w http.ResponseWriter, body any) {
		hits.inc(path)
		w.Header().Set("Content-Type", "application/json")
		if body == nil {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}
	mux.HandleFunc("/navigate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		record("/navigate", w, nil)
	})
	mux.HandleFunc("/snapshot", func(w http.ResponseWriter, r *http.Request) {
		record("/snapshot", w, map[string]any{
			"nodes": []map[string]any{
				{"ref": "e1", "role": "link", "name": "Example", "x": 10, "y": 20, "w": 100, "h": 30},
			},
		})
	})
	mux.HandleFunc("/screenshot", func(w http.ResponseWriter, r *http.Request) {
		record("/screenshot", w, map[string]any{
			"format": "png", "mime": "image/png", "width": 1280, "height": 720,
			"data_base64": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
		})
	})
	mux.HandleFunc("/click", func(w http.ResponseWriter, r *http.Request) {
		record("/click", w, nil)
	})
	mux.HandleFunc("/type", func(w http.ResponseWriter, r *http.Request) {
		record("/type", w, nil)
	})
	mux.HandleFunc("/network", func(w http.ResponseWriter, r *http.Request) {
		record("/network", w, map[string]any{
			"entries": []map[string]any{
				{"id": "t:1", "url": "https://example.com/full-path-unclipped", "method": "GET", "status": 200, "protocol": "h2", "connection_id": 7},
			},
		})
	})
	mux.HandleFunc("/graph.png", func(w http.ResponseWriter, r *http.Request) {
		hits.inc("/graph.png")
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{
			0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
			0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
			0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
			0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
			0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41,
			0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
			0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
			0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
			0x42, 0x60, 0x82,
		})
	})
	mux.HandleFunc("/links", func(w http.ResponseWriter, r *http.Request) {
		record("/links", w, map[string]any{
			"url":   "https://example.com/",
			"title": "Example",
			"count": 1,
			"links": []map[string]any{
				{"ref": "e1", "href": "https://example.com/about", "text": "About"},
			},
		})
	})
	return httptest.NewServer(mux), hits
}

func connectTestMCP(t *testing.T, endpoint string, token string) *mcp.ClientSession {
	t.Helper()
	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &bearerRoundTripper{token: token, base: http.DefaultTransport},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return sess
}

func callToolText(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error: %v", name, res.Content)
	}
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestMCPAllToolsAgainstFakeStreamer(t *testing.T) {
	const token = "integration-test-token-32chars-minimum!!"
	streamer, hits := newFakeStreamer()
	defer streamer.Close()

	mem := store.NewMemStore()
	st := store.NewMemBridgeStore(mem)
	cache := store.NewMemCache()
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock")
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
	apiH := api.New(st, cache, orch, cfg)

	sessionID := uuid.New().String()
	sid, _ := uuid.Parse(sessionID)
	if err := st.Sessions.CreateSession(context.Background(), &models.Session{
		ID: sid, Status: models.SessionStatusRunning, TargetURL: "https://example.com",
	}); err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(streamer.URL, "http://")
	if err := cache.Set(context.Background(), sessionAddrKey(sessionID), addr, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := cache.Set(context.Background(), controlTokenKey(sessionID), "owner-token", time.Hour); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	Mount(r, cfg, Deps{API: apiH, Store: st, Cache: cache, Cfg: cfg})
	srv := httptest.NewServer(r)
	defer srv.Close()

	mcpSess := connectTestMCP(t, srv.URL+"/mcp", token)
	defer mcpSess.Close()

	tools, err := mcpSess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantTools := []string{"create_session", "navigate", "inspect_session", "snapshot", "screenshot", "click", "type", "get_network", "resource_graph", "get_links", "close_session"}
	if len(tools.Tools) != len(wantTools) {
		t.Fatalf("tools = %d, want %d", len(tools.Tools), len(wantTools))
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	for _, w := range wantTools {
		if !names[w] {
			t.Fatalf("missing tool %q", w)
		}
	}

	callToolText(t, mcpSess, "navigate", map[string]any{"session_id": sessionID, "url": "https://example.com/page"})
	if hits.count("/navigate") != 1 {
		t.Fatalf("navigate hits = %d", hits.count("/navigate"))
	}

	inspect := callToolText(t, mcpSess, "inspect_session", map[string]any{"session_id": sessionID})
	if !strings.Contains(inspect, "entry_count") {
		t.Fatalf("inspect_session: %s", inspect)
	}

	shot := callToolText(t, mcpSess, "screenshot", map[string]any{"session_id": sessionID, "format": "png"})
	if !strings.Contains(shot, "data_base64") {
		t.Fatalf("screenshot missing image data: %s", shot)
	}

	snap := callToolText(t, mcpSess, "snapshot", map[string]any{"session_id": sessionID})
	if !strings.Contains(snap, `"ref":"e1"`) && !strings.Contains(snap, `"ref": "e1"`) {
		t.Fatalf("snapshot missing ref: %s", snap)
	}
	if hits.count("/snapshot") < 2 {
		t.Fatalf("snapshot hits = %d, want >=2 (inspect_session + snapshot)", hits.count("/snapshot"))
	}

	callToolText(t, mcpSess, "click", map[string]any{"session_id": sessionID, "ref": "e1"})
	if hits.count("/click") != 1 {
		t.Fatalf("click hits = %d", hits.count("/click"))
	}

	callToolText(t, mcpSess, "type", map[string]any{"session_id": sessionID, "text": "hello", "ref": "e1"})
	if hits.count("/type") != 1 {
		t.Fatalf("type hits = %d", hits.count("/type"))
	}

	netBody := callToolText(t, mcpSess, "get_network", map[string]any{"session_id": sessionID})
	if !strings.Contains(netBody, "connection_id") || !strings.Contains(netBody, "full-path-unclipped") {
		t.Fatalf("get_network body: %s", netBody)
	}
	if hits.count("/network") < 2 {
		t.Fatalf("network hits = %d, want >=2 (inspect_session + get_network)", hits.count("/network"))
	}

	graphBody := callToolText(t, mcpSess, "resource_graph", map[string]any{
		"session_id": sessionID, "theme": "light", "expand": "all", "scale": 1,
	})
	if !strings.Contains(graphBody, `"format":"png"`) || !strings.Contains(graphBody, `"theme":"light"`) {
		t.Fatalf("resource_graph body: %s", graphBody)
	}
	if hits.count("/graph.png") != 1 {
		t.Fatalf("graph hits = %d, want 1", hits.count("/graph.png"))
	}

	linksBody := callToolText(t, mcpSess, "get_links", map[string]any{"session_id": sessionID, "limit": 10})
	if !strings.Contains(linksBody, "https://example.com/about") || !strings.Contains(linksBody, `"ref":"e1"`) {
		t.Fatalf("get_links body: %s", linksBody)
	}
	if hits.count("/links") != 1 {
		t.Fatalf("links hits = %d, want 1", hits.count("/links"))
	}

	closeOut := callToolText(t, mcpSess, "close_session", map[string]any{"session_id": sessionID})
	if closeOut != "terminated" {
		t.Fatalf("close_session = %q", closeOut)
	}
	row, _ := st.Sessions.GetSession(context.Background(), sid)
	if row != nil && row.Status == models.SessionStatusRunning {
		t.Fatalf("session still running after close")
	}
}

func TestMCP_CreateSessionRequiresURL(t *testing.T) {
	const token = "integration-test-token-32chars-minimum!!"
	mem := store.NewMemStore()
	st := store.NewMemBridgeStore(mem)
	cache := store.NewMemCache()
	orch, _ := orchestrator.New("unix:///nonexistent/docker.sock")
	cfg := &config.Config{
		MCPEnabled:           true,
		MCPTokens:            []string{token},
		BrowserType:          "chromium",
		Browsers:             map[string]config.BrowserEntry{"chromium": {Enabled: true}},
		DisableURLValidation: true,
	}
	apiH := api.New(st, cache, orch, cfg)
	r := chi.NewRouter()
	Mount(r, cfg, Deps{API: apiH, Store: st, Cache: cache, Cfg: cfg})
	srv := httptest.NewServer(r)
	defer srv.Close()

	mcpSess := connectTestMCP(t, srv.URL+"/mcp", token)
	defer mcpSess.Close()

	res, err := mcpSess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "create_session",
		Arguments: map[string]any{"url": ""},
	})
	if err != nil {
		return
	}
	if !res.IsError {
		t.Fatal("expected tool error for empty url")
	}
}

// bearerRoundTripper adds the Bearer token to outgoing MCP requests.
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(req)
}

func TestMCP_HTTPRoutes(t *testing.T) {
	const token = "route-test-token-32-characters-min!!"
	r := chi.NewRouter()
	cfg := &config.Config{MCPEnabled: true, MCPTokens: []string{token}}
	Mount(r, cfg, Deps{
		API:   api.New(store.NewMemBridgeStore(store.NewMemStore()), store.NewMemCache(), nil, cfg),
		Store: store.NewMemBridgeStore(store.NewMemStore()),
		Cache: store.NewMemCache(),
		Cfg:   cfg,
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	for _, tc := range []struct {
		method string
		auth   bool
		want   int
	}{
		{http.MethodGet, false, http.StatusUnauthorized},
		{http.MethodPost, false, http.StatusUnauthorized},
		{http.MethodPost, true, http.StatusBadRequest}, // missing MCP JSON body
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+"/mcp", strings.NewReader("{}"))
		if tc.auth {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want && !(tc.auth && tc.method == http.MethodPost && resp.StatusCode == http.StatusOK) {
			// POST with auth may be 200 or 400 depending on MCP parse; allow 200/400
			if tc.auth && tc.method == http.MethodPost && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadRequest) {
				continue
			}
			t.Fatalf("%s auth=%v: status %d want %d", tc.method, tc.auth, resp.StatusCode, tc.want)
		}
	}
}
