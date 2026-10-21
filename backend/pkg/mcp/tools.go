package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dm0xa/isola/backend/pkg/graphopts"
	"github.com/dm0xa/isola/backend/pkg/models"
)

type Service struct {
	deps Deps

	// Resource-graph limits, overridable in tests. Zero means the package
	// defaults.
	graphTimeout  time.Duration
	graphMaxBytes int64
}

// graphLimits resolves the resource-graph deadline and payload cap.
func (s *Service) graphLimits() (time.Duration, int64) {
	timeout, maxBytes := s.graphTimeout, s.graphMaxBytes
	if timeout <= 0 {
		timeout = graphRenderTimeout
	}
	if maxBytes <= 0 {
		maxBytes = maxGraphBytes
	}
	return timeout, maxBytes
}

// byteLimitLabel renders a byte limit for an error message.
func byteLimitLabel(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

func textResult(s string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: s}},
	}, nil, nil
}

func parseUUID(s string) (uuid.UUID, error) {
	return uuid.Parse(strings.TrimSpace(s))
}

type sessionIDParams struct {
	SessionID string `json:"session_id" jsonschema:"Isola session UUID"`
}

type createSessionParams struct {
	URL          string `json:"url" jsonschema:"URL to open in the isolated browser"`
	Tor          bool   `json:"tor,omitempty" jsonschema:"Route the session through Tor (SOCKS5). The browser gets no unproxied egress; the first load waits for a live circuit. Default false."`
	TorCountry   string `json:"tor_country,omitempty" jsonschema:"Optional Tor exit country or countries to pin to, as ISO 3166-1 alpha-2 codes (e.g. 'se' or 'se,no'). Requires tor=true. Omit to use the server default, or any exit."`
	WaitReady    *bool  `json:"wait_ready,omitempty" jsonschema:"Wait for CDP/snapshot readiness before returning (default true)"`
	WaitDocument *bool  `json:"wait_document,omitempty" jsonschema:"After CDP is ready, wait for a main-document network entry (default true)"`
}

// toRequest maps the MCP arguments onto the API's create-session request.
func (p *createSessionParams) toRequest() models.CreateSessionRequest {
	return models.CreateSessionRequest{
		URL:            strings.TrimSpace(p.URL),
		TorEnabled:     p.Tor,
		TorCountry:     strings.TrimSpace(p.TorCountry),
		NetworkMonitor: true,
	}
}

type navigateParams struct {
	SessionID    string `json:"session_id"`
	URL          string `json:"url"`
	WaitDocument *bool  `json:"wait_document,omitempty" jsonschema:"Wait for a Document network entry after navigation (default true)"`
}

type getNetworkParams struct {
	SessionID    string `json:"session_id"`
	Type         string `json:"type,omitempty" jsonschema:"Filter by resource type, e.g. Document or Script"`
	HostContains string `json:"host_contains,omitempty" jsonschema:"Filter URLs whose host contains this substring"`
	Limit        int    `json:"limit,omitempty" jsonschema:"Max entries to return after filtering"`
}

type getLinksParams struct {
	SessionID    string `json:"session_id"`
	Limit        int    `json:"limit,omitempty" jsonschema:"Max links to return (default 200, max 1000)"`
	HostContains string `json:"host_contains,omitempty" jsonschema:"Only return links whose URL contains this substring"`
}

type clickParams struct {
	SessionID string `json:"session_id"`
	Ref       string `json:"ref" jsonschema:"Element ref from the latest snapshot"`
}

type typeParams struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
	Ref       string `json:"ref,omitempty" jsonschema:"Optional element ref to focus before typing"`
}

type screenshotParams struct {
	SessionID string `json:"session_id"`
	Format    string `json:"format,omitempty" jsonschema:"png or jpeg (default png)"`
	Quality   int    `json:"quality,omitempty" jsonschema:"JPEG quality 1-100 (default 85)"`
}

type resourceGraphParams struct {
	SessionID  string `json:"session_id" jsonschema:"Isola session UUID"`
	Theme      string `json:"theme,omitempty" jsonschema:"Image theme: dark (default) or light"`
	Direction  string `json:"direction,omitempty" jsonschema:"Flow direction: horizontal (default) or vertical"`
	Expand     string `json:"expand,omitempty" jsonschema:"origin (default: the opened host's type groups), all (every group and resource), or collapsed (hosts only)"`
	Types      string `json:"types,omitempty" jsonschema:"Comma-separated resource types to keep: document,script,stylesheet,image,font,xhr,media,other"`
	FailedOnly bool   `json:"failed_only,omitempty" jsonschema:"Only draw failed requests"`
	Scale      int    `json:"scale,omitempty" jsonschema:"Raster scale 1-4 (default 2)"`
}

func (s *Service) toolCreateSession(ctx context.Context, _ *mcp.CallToolRequest, p *createSessionParams) (*mcp.CallToolResult, any, error) {
	if p == nil || strings.TrimSpace(p.URL) == "" {
		return nil, nil, fmt.Errorf("url is required")
	}
	body, _ := json.Marshal(p.toRequest())
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	s.deps.API.CreateSession(w, req)
	if w.Code != http.StatusAccepted {
		return nil, nil, fmt.Errorf("create session failed (%d): %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	var resp models.CreateSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		return nil, nil, err
	}
	if err := s.waitSessionRunning(ctx, resp.ID); err != nil {
		return nil, nil, err
	}
	waitReady := true
	if p.WaitReady != nil {
		waitReady = *p.WaitReady
	}
	automationReady := false
	documentSeen := false
	if waitReady {
		if err := s.waitAutomationReady(ctx, resp.ID); err != nil {
			return nil, nil, err
		}
		automationReady = true
		waitDoc := true
		if p.WaitDocument != nil {
			waitDoc = *p.WaitDocument
		}
		if waitDoc {
			documentSeen = s.waitForDocumentNetwork(ctx, resp.ID, 25*time.Second)
		}
	}
	result := map[string]any{
		"session_id":       resp.ID,
		"url":              strings.TrimSpace(p.URL),
		"tor_enabled":      p.Tor,
		"automation_ready": automationReady,
		"document_seen":    documentSeen,
	}
	if p.Tor {
		// Report the effective pin (server default applied) rather than only what
		// was passed, so an agent can confirm what the session actually uses.
		torCountry := p.TorCountry
		if id, perr := parseUUID(resp.ID); perr == nil {
			if sess, gerr := s.deps.Store.Sessions.GetSession(ctx, id); gerr == nil && sess != nil {
				torCountry = sess.TorCountry
			}
		}
		result["tor_country"] = torCountry
	}
	if automationReady {
		if inspect, err := s.sessionInspectMap(ctx, resp.ID); err == nil {
			result["inspect"] = inspect
		}
	}
	out, _ := json.Marshal(result)
	return textResult(string(out))
}

func (s *Service) toolNavigate(ctx context.Context, _ *mcp.CallToolRequest, p *navigateParams) (*mcp.CallToolResult, any, error) {
	if p == nil || p.SessionID == "" || p.URL == "" {
		return nil, nil, fmt.Errorf("session_id and url are required")
	}
	if err := s.ensureSession(ctx, p.SessionID); err != nil {
		return nil, nil, err
	}
	if err := s.navigateStreamer(ctx, p.SessionID, p.URL); err != nil {
		return nil, nil, err
	}
	waitDoc := true
	if p.WaitDocument != nil {
		waitDoc = *p.WaitDocument
	}
	result := map[string]any{"status": "ok", "url": strings.TrimSpace(p.URL)}
	if waitDoc {
		result["document_seen"] = s.waitForDocumentNetwork(ctx, p.SessionID, 25*time.Second)
		if inspect, err := s.sessionInspectMap(ctx, p.SessionID); err == nil {
			result["inspect"] = inspect
		}
	}
	out, _ := json.Marshal(result)
	return textResult(string(out))
}

func (s *Service) toolSnapshot(ctx context.Context, _ *mcp.CallToolRequest, p *sessionIDParams) (*mcp.CallToolResult, any, error) {
	if p == nil || p.SessionID == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	if err := s.ensureSession(ctx, p.SessionID); err != nil {
		return nil, nil, err
	}
	body, _, err := s.streamerGET(ctx, p.SessionID, "/snapshot")
	if err != nil {
		return nil, nil, err
	}
	return textResult(string(body))
}

func (s *Service) toolClick(ctx context.Context, _ *mcp.CallToolRequest, p *clickParams) (*mcp.CallToolResult, any, error) {
	if p == nil || p.SessionID == "" || p.Ref == "" {
		return nil, nil, fmt.Errorf("session_id and ref are required")
	}
	if err := s.ensureSession(ctx, p.SessionID); err != nil {
		return nil, nil, err
	}
	_, _, err := s.streamerPOST(ctx, p.SessionID, "/click", map[string]string{"ref": p.Ref})
	if err != nil {
		return nil, nil, err
	}
	return textResult("ok")
}

func (s *Service) toolType(ctx context.Context, _ *mcp.CallToolRequest, p *typeParams) (*mcp.CallToolResult, any, error) {
	if p == nil || p.SessionID == "" || p.Text == "" {
		return nil, nil, fmt.Errorf("session_id and text are required")
	}
	if err := s.ensureSession(ctx, p.SessionID); err != nil {
		return nil, nil, err
	}
	payload := map[string]string{"text": p.Text}
	if p.Ref != "" {
		payload["ref"] = p.Ref
	}
	_, _, err := s.streamerPOST(ctx, p.SessionID, "/type", payload)
	if err != nil {
		return nil, nil, err
	}
	return textResult("ok")
}

func (s *Service) toolInspectSession(ctx context.Context, _ *mcp.CallToolRequest, p *sessionIDParams) (*mcp.CallToolResult, any, error) {
	if p == nil || p.SessionID == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	if err := s.ensureSession(ctx, p.SessionID); err != nil {
		return nil, nil, err
	}
	snapBody, _, err := s.streamerGET(ctx, p.SessionID, "/snapshot")
	if err != nil {
		return nil, nil, err
	}
	netBody, _, err := s.streamerGET(ctx, p.SessionID, "/network")
	if err != nil {
		return nil, nil, err
	}
	out := buildInspectSummary(snapBody, netBody)
	b, _ := json.Marshal(out)
	return textResult(string(b))
}

func buildInspectSummary(snapBody, netBody []byte) map[string]any {
	summary := map[string]any{
		"page_title": "",
		"nodes":      0,
		"network": map[string]any{
			"entry_count": 0,
			"documents":   []map[string]any{},
		},
	}
	var snap struct {
		Nodes []struct {
			Role string `json:"role"`
			Name string `json:"name"`
		} `json:"nodes"`
	}
	if json.Unmarshal(snapBody, &snap) == nil {
		summary["nodes"] = len(snap.Nodes)
		for _, n := range snap.Nodes {
			if n.Role == "RootWebArea" && n.Name != "" {
				summary["page_title"] = n.Name
				break
			}
		}
	}
	var net struct {
		Entries []struct {
			URL    string `json:"url"`
			Status int    `json:"status"`
			Type   string `json:"type"`
			Method string `json:"method"`
		} `json:"entries"`
	}
	if json.Unmarshal(netBody, &net) == nil {
		summary["network"] = map[string]any{"entry_count": len(net.Entries)}
		docs := make([]map[string]any, 0, 4)
		for _, e := range net.Entries {
			if e.Type != "Document" {
				continue
			}
			docs = append(docs, map[string]any{
				"url": e.URL, "status": e.Status, "method": e.Method,
			})
			if len(docs) >= 5 {
				break
			}
		}
		summary["network"] = map[string]any{
			"entry_count": len(net.Entries),
			"documents":   docs,
		}
	}
	return summary
}

func (s *Service) toolScreenshot(ctx context.Context, _ *mcp.CallToolRequest, p *screenshotParams) (*mcp.CallToolResult, any, error) {
	if p == nil || p.SessionID == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	if err := s.ensureSession(ctx, p.SessionID); err != nil {
		return nil, nil, err
	}
	q := url.Values{}
	format := strings.ToLower(strings.TrimSpace(p.Format))
	if format != "" {
		q.Set("format", format)
	}
	if p.Quality > 0 {
		q.Set("quality", fmt.Sprintf("%d", p.Quality))
	}
	path := "/screenshot"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	body, _, err := s.streamerGET(ctx, p.SessionID, path)
	if err != nil {
		return nil, nil, err
	}
	return textResult(string(body))
}

const (
	// graphRenderTimeout matches the REST proxy budget (pkg/api): the streamer
	// does the same work whichever path asks for it.
	graphRenderTimeout = 45 * time.Second
	// maxGraphBytes caps the PNG embedded in one JSON-RPC response. The SDK
	// base64-encodes it, so 8 MiB becomes ~10.7 MiB on the wire and is held by
	// both the control plane and the agent.
	maxGraphBytes = 8 << 20
)

func (s *Service) toolResourceGraph(ctx context.Context, _ *mcp.CallToolRequest, p *resourceGraphParams) (*mcp.CallToolResult, any, error) {
	if p == nil || strings.TrimSpace(p.SessionID) == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	if err := s.ensureSession(ctx, p.SessionID); err != nil {
		return nil, nil, err
	}

	scale := p.Scale
	if scale == 0 {
		scale = graphopts.DefaultScale
	}
	if scale < graphopts.MinScale || scale > graphopts.MaxScale {
		return nil, nil, fmt.Errorf("scale must be %d-%d", graphopts.MinScale, graphopts.MaxScale)
	}

	q := url.Values{}
	set := func(key, value string) {
		if v := strings.TrimSpace(value); v != "" {
			q.Set(key, v)
		}
	}
	set("theme", p.Theme)
	set("direction", p.Direction)
	set("expand", p.Expand)
	set("type", p.Types)
	if p.FailedOnly {
		q.Set("failed", "1")
	}
	q.Set("scale", strconv.Itoa(scale))

	opts, err := graphopts.Parse(q)
	if err != nil {
		return nil, nil, err
	}
	graphPath := "/graph.png?" + opts.Query().Encode()

	timeout, maxBytes := s.graphLimits()
	body, _, err := s.streamerGETLimited(ctx, p.SessionID, graphPath, timeout, maxBytes)
	if err != nil {
		if errors.Is(err, ErrResponseTooLarge) {
			return nil, nil, fmt.Errorf("resource graph exceeds the %s limit; retry with scale=1 or expand=collapsed", byteLimitLabel(maxBytes))
		}
		return nil, nil, err
	}

	// The PNG travels as an image block so a client can render it; the text
	// block carries the metadata a client without image support can quote.
	meta := map[string]any{
		"format": "png",
		"bytes":  len(body),
		"theme":  opts.Theme,
	}
	if cfg, err := png.DecodeConfig(bytes.NewReader(body)); err == nil {
		meta["width"] = cfg.Width
		meta["height"] = cfg.Height
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(metaJSON)},
			&mcp.ImageContent{Data: body, MIMEType: "image/png"},
		},
	}, nil, nil
}

func (s *Service) toolGetNetwork(ctx context.Context, _ *mcp.CallToolRequest, p *getNetworkParams) (*mcp.CallToolResult, any, error) {
	if p == nil || p.SessionID == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	if err := s.ensureSession(ctx, p.SessionID); err != nil {
		return nil, nil, err
	}
	body, _, err := s.streamerGET(ctx, p.SessionID, "/network")
	if err != nil {
		return nil, nil, err
	}
	if p.Type != "" || p.HostContains != "" || p.Limit > 0 {
		body, err = filterNetworkBody(body, p.Type, p.HostContains, p.Limit)
		if err != nil {
			return nil, nil, err
		}
	}
	return textResult(string(body))
}

func (s *Service) toolGetLinks(ctx context.Context, _ *mcp.CallToolRequest, p *getLinksParams) (*mcp.CallToolResult, any, error) {
	if p == nil || p.SessionID == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	if err := s.ensureSession(ctx, p.SessionID); err != nil {
		return nil, nil, err
	}
	q := url.Values{}
	if p.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", p.Limit))
	}
	if hc := strings.TrimSpace(p.HostContains); hc != "" {
		q.Set("host_contains", hc)
	}
	path := "/links"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	body, code, err := s.streamerGET(ctx, p.SessionID, path)
	if err != nil {
		if code == http.StatusNotFound {
			return nil, nil, fmt.Errorf("get_links is not supported by this streamer image (update the browser node)")
		}
		return nil, nil, err
	}
	return textResult(string(body))
}

func (s *Service) toolCloseSession(ctx context.Context, _ *mcp.CallToolRequest, p *sessionIDParams) (*mcp.CallToolResult, any, error) {
	if p == nil || p.SessionID == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	token, err := s.deps.Cache.Get(ctx, controlTokenKey(p.SessionID))
	if err != nil || token == "" {
		return nil, nil, fmt.Errorf("session control token not found")
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", p.SessionID)
	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/"+p.SessionID, nil)
	req.Header.Set("X-Session-Token", token)
	req = req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.deps.API.DeleteSession(w, req)
	if w.Code != http.StatusOK {
		return nil, nil, fmt.Errorf("close session failed (%d): %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	return textResult("terminated")
}

func (s *Service) ensureSession(ctx context.Context, sessionID string) error {
	id, err := parseUUID(sessionID)
	if err != nil {
		return fmt.Errorf("invalid session_id")
	}
	sess, err := s.deps.Store.Sessions.GetSession(ctx, id)
	if err != nil || sess == nil {
		return fmt.Errorf("session not found")
	}
	if sess.Status != models.SessionStatusRunning {
		return fmt.Errorf("session is not running (status=%s)", sess.Status)
	}
	return nil
}

func registerTools(server *mcp.Server, svc *Service) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_session",
		Description: "Start an isolated browser session at url. Set tor=true to route all session traffic through Tor, optionally pinning exit countries with tor_country (e.g. 'se'). Returns session_id and waits for CDP readiness by default (wait_ready). Use inspect_session or snapshot next.",
	}, svc.toolCreateSession)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "navigate",
		Description: "Navigate the active tab to a URL (retries until the runner ctrl socket is connected)",
	}, svc.toolNavigate)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "inspect_session",
		Description: "Compact page summary: title from accessibility tree, node count, and recent document requests from the network log",
	}, svc.toolInspectSession)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "snapshot",
		Description: "Accessibility tree with refs for click/type (not a visual image)",
	}, svc.toolSnapshot)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "screenshot",
		Description: "Visual PNG or JPEG of the active tab via CDP Page.captureScreenshot (returns data_base64 + dimensions)",
	}, svc.toolScreenshot)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "click",
		Description: "Click an element by ref from the latest snapshot",
	}, svc.toolClick)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "type",
		Description: "Type text; optional ref to focus first",
	}, svc.toolType)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_network",
		Description: "Network log (optional type, host_contains, limit filters). Full streamer window when unfiltered.",
	}, svc.toolGetNetwork)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "resource_graph",
		Description: "Render the captured network as a resource-graph PNG (hosts → resource types → files) and return it as an image block. theme=dark|light, direction=horizontal|vertical, expand=origin|all|collapsed, types=<comma list>, failed_only, scale=1-4.",
	}, svc.toolResourceGraph)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_links",
		Description: "List the links (href + text) on the active page so the agent can navigate. Optional limit and host_contains filter; returns the page url and title too.",
	}, svc.toolGetLinks)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "close_session",
		Description: "Terminate the session and destroy its container",
	}, svc.toolCloseSession)
}
