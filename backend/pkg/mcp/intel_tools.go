package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Intelligence tools. The analysis itself lives in the streamer (one
// implementation shared with the viewer panel); these tools validate params,
// call the streamer and shape the reply for an agent. Every new OSINT surface
// must land here as a named tool — a panel-only feature is a bug.

type getHeadersParams struct {
	SessionID     string `json:"session_id"`
	HostContains  string `json:"host_contains,omitempty" jsonschema:"Only entries whose host contains this substring"`
	URLContains   string `json:"url_contains,omitempty" jsonschema:"Only entries whose URL contains this substring"`
	DocumentsOnly *bool  `json:"documents_only,omitempty" jsonschema:"Only document responses (default true)"`
	Type          string `json:"type,omitempty" jsonschema:"Filter by resource type, e.g. Document or XHR"`
	Limit         int    `json:"limit,omitempty" jsonschema:"Max entries (default 50)"`
	IncludeTLS    *bool  `json:"include_tls,omitempty" jsonschema:"Include the TLS/certificate block (default true)"`
}

type getPostureParams struct {
	SessionID string `json:"session_id"`
	Host      string `json:"host,omitempty" jsonschema:"Limit to documents whose URL contains this substring"`
}

type getRedirectsParams struct {
	SessionID     string `json:"session_id"`
	HostContains  string `json:"host_contains,omitempty" jsonschema:"Only chains whose host contains this substring"`
	URLContains   string `json:"url_contains,omitempty" jsonschema:"Only chains whose URL contains this substring"`
	DocumentsOnly *bool  `json:"documents_only,omitempty" jsonschema:"Only document requests (default true)"`
	Limit         int    `json:"limit,omitempty" jsonschema:"Max chains (default 20)"`
}

type getTrackersParams struct {
	SessionID      string `json:"session_id"`
	ThirdPartyOnly bool   `json:"third_party_only,omitempty" jsonschema:"Report only hosts outside the page's registrable domain"`
	IncludeBeacons *bool  `json:"include_beacons,omitempty" jsonschema:"Include the beacon count (default true)"`
	IncludeCookies *bool  `json:"include_cookies,omitempty" jsonschema:"Include cookie-family fingerprints (default true)"`
	Limit          int    `json:"limit,omitempty" jsonschema:"Max rows per list (default 100)"`
}

type hostIntelParams struct {
	SessionID    string `json:"session_id"`
	Host         string `json:"host,omitempty" jsonschema:"Host to inspect. Omit to use the most recently captured document host."`
	IncludeCNAME *bool  `json:"include_cname,omitempty" jsonschema:"Include the CNAME chain (default true)"`
}

type probeHostParams struct {
	SessionID string `json:"session_id"`
	Host      string `json:"host,omitempty" jsonschema:"Host to probe. Omit to use the most recently captured document host."`
	Checks    string `json:"checks,omitempty" jsonschema:"Comma list: robots, securitytxt, sitemap, wellknown, favicon. Default robots,securitytxt,sitemap. Every check fetches the host itself; none contacts a provider."`
}

// toolGetHeaders returns captured response headers (and TLS) per request.
func (s *Service) toolGetHeaders(ctx context.Context, _ *mcp.CallToolRequest, p *getHeadersParams) (*mcp.CallToolResult, any, error) {
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

	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	documentsOnly := p.DocumentsOnly == nil || *p.DocumentsOnly
	includeTLS := p.IncludeTLS == nil || *p.IncludeTLS
	hostNeedle := needle(p.HostContains)
	urlNeedle := needle(p.URLContains)
	typ := strings.TrimSpace(p.Type)

	out, err := filterEntries(body, limit, func(meta entryMeta, fields map[string]json.RawMessage) bool {
		if !hasNonEmptyObject(fields, "headers") {
			return false
		}
		if documentsOnly && !strings.EqualFold(meta.Type, "Document") {
			return false
		}
		if typ != "" && !strings.EqualFold(meta.Type, typ) {
			return false
		}
		if !matchesHost(meta, hostNeedle) || !matchesURL(meta, urlNeedle) {
			return false
		}
		if !includeTLS {
			delete(fields, "tls")
		}
		return true
	})
	if err != nil {
		return nil, nil, err
	}
	return textResult(string(out))
}

// toolGetPosture returns the security-header assessment for captured documents.
func (s *Service) toolGetPosture(ctx context.Context, _ *mcp.CallToolRequest, p *getPostureParams) (*mcp.CallToolResult, any, error) {
	if p == nil || p.SessionID == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	if err := s.ensureSession(ctx, p.SessionID); err != nil {
		return nil, nil, err
	}
	path := "/posture"
	if host := strings.TrimSpace(p.Host); host != "" {
		path += "?host=" + url.QueryEscape(host)
	}
	body, _, err := s.streamerGET(ctx, p.SessionID, path)
	if err != nil {
		return nil, nil, err
	}
	return textResult(string(body))
}

// toolGetRedirects returns the redirect chain of each request that had one.
func (s *Service) toolGetRedirects(ctx context.Context, _ *mcp.CallToolRequest, p *getRedirectsParams) (*mcp.CallToolResult, any, error) {
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
	limit := p.Limit
	if limit <= 0 {
		limit = 20
	}
	documentsOnly := p.DocumentsOnly == nil || *p.DocumentsOnly
	hostNeedle := needle(p.HostContains)
	urlNeedle := needle(p.URLContains)

	out, err := filterEntries(body, limit, func(meta entryMeta, fields map[string]json.RawMessage) bool {
		if !hasNonEmptyArray(fields, "redirects") {
			return false
		}
		if documentsOnly && !strings.EqualFold(meta.Type, "Document") {
			return false
		}
		return matchesHost(meta, hostNeedle) && matchesURL(meta, urlNeedle)
	})
	if err != nil {
		return nil, nil, err
	}
	return textResult(string(out))
}

// toolGetTrackers returns the third-party / tracker inventory.
func (s *Service) toolGetTrackers(ctx context.Context, _ *mcp.CallToolRequest, p *getTrackersParams) (*mcp.CallToolResult, any, error) {
	if p == nil || p.SessionID == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	if err := s.ensureSession(ctx, p.SessionID); err != nil {
		return nil, nil, err
	}
	path := "/trackers"
	if p.Limit > 0 {
		path += "?limit=" + url.QueryEscape(fmt.Sprintf("%d", p.Limit))
	}
	body, _, err := s.streamerGET(ctx, p.SessionID, path)
	if err != nil {
		return nil, nil, err
	}

	var report map[string]json.RawMessage
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, nil, err
	}
	if p.IncludeBeacons != nil && !*p.IncludeBeacons {
		delete(report, "beacons")
	}
	if p.IncludeCookies != nil && !*p.IncludeCookies {
		delete(report, "cookie_fingerprints")
	}
	if p.ThirdPartyOnly {
		if tp, ok := report["third_party"]; ok {
			report["trackers"] = tp
		}
	}
	shaped, err := json.Marshal(report)
	if err != nil {
		return nil, nil, err
	}
	return textResult(string(shaped))
}

// toolHostIntel returns IPs, PTR, ASN/org/country and the CNAME chain for a
// host (or for the most recent document host when none is named).
func (s *Service) toolHostIntel(ctx context.Context, _ *mcp.CallToolRequest, p *hostIntelParams) (*mcp.CallToolResult, any, error) {
	if p == nil || p.SessionID == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	if err := s.ensureSession(ctx, p.SessionID); err != nil {
		return nil, nil, err
	}
	host, err := s.resolveIntelHost(ctx, p.SessionID, p.Host)
	if err != nil {
		return nil, nil, err
	}
	q := url.Values{}
	q.Set("mode", "dns")
	q.Set("host", host)

	body, code, err := s.streamerGET(ctx, p.SessionID, "/intel?"+q.Encode())
	if err != nil {
		return nil, nil, intelToolError(code, body, err)
	}
	if p.IncludeCNAME != nil && !*p.IncludeCNAME {
		body = stripIntelField(body, "dns", "cname")
	}
	return textResult(string(body))
}

// toolProbeHost runs the explicit recon checks. It never runs on navigation:
// the caller names the host and thereby opts in to the outbound requests.
func (s *Service) toolProbeHost(ctx context.Context, _ *mcp.CallToolRequest, p *probeHostParams) (*mcp.CallToolResult, any, error) {
	if p == nil || p.SessionID == "" {
		return nil, nil, fmt.Errorf("session_id is required")
	}
	if err := s.ensureSession(ctx, p.SessionID); err != nil {
		return nil, nil, err
	}
	host, err := s.resolveIntelHost(ctx, p.SessionID, p.Host)
	if err != nil {
		return nil, nil, err
	}
	q := url.Values{}
	q.Set("mode", "probe")
	q.Set("host", host)
	if checks := strings.TrimSpace(p.Checks); checks != "" {
		q.Set("checks", checks)
	}

	body, code, err := s.streamerGET(ctx, p.SessionID, "/intel?"+q.Encode())
	if err != nil {
		return nil, nil, intelToolError(code, body, err)
	}
	return textResult(string(body))
}

// resolveIntelHost validates an explicit host and falls back to the most recent
// captured document, so an agent can probe "the page I just opened".
func (s *Service) resolveIntelHost(ctx context.Context, sessionID, raw string) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if host != "" {
		if strings.ContainsAny(host, " /\\:@?#") || !strings.Contains(host, ".") {
			return "", fmt.Errorf("host must be a bare hostname such as example.com")
		}
		return host, nil
	}
	return s.latestDocumentHost(ctx, sessionID)
}

// latestDocumentHost reads the newest document URL from /posture. It returns a
// helpful error rather than silently probing nothing.
func (s *Service) latestDocumentHost(ctx context.Context, sessionID string) (string, error) {
	body, _, err := s.streamerGET(ctx, sessionID, "/posture?headers=0")
	if err != nil {
		return "", err
	}
	var doc struct {
		Entries []struct {
			URL string `json:"url"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("could not determine the page host: %w", err)
	}
	for i := len(doc.Entries) - 1; i >= 0; i-- {
		u, err := url.Parse(doc.Entries[i].URL)
		if err != nil || u.Hostname() == "" {
			continue
		}
		return strings.ToLower(u.Hostname()), nil
	}
	return "", fmt.Errorf("no document captured yet; pass host explicitly")
}

// intelToolError turns the streamer's error body into an actionable message.
func intelToolError(code int, body []byte, fallback error) error {
	var e struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(body, &e)
	switch code {
	case http.StatusForbidden:
		return fmt.Errorf("intel is disabled by the operator (set ISOLA_INTEL_ENABLED=true)")
	case http.StatusBadRequest:
		if e.Detail != "" {
			return fmt.Errorf("invalid intel request: %s", e.Detail)
		}
		if e.Error != "" {
			return fmt.Errorf("invalid intel request: %s", e.Error)
		}
	}
	return fallback
}

// mergePostureSummary adds the newest document's grade to inspect_session.
func mergePostureSummary(summary map[string]any, body []byte) {
	var doc struct {
		Entries []struct {
			Posture struct {
				Score int    `json:"score"`
				Total int    `json:"total"`
				Grade string `json:"grade"`
			} `json:"posture"`
		} `json:"entries"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return
	}
	for i := len(doc.Entries) - 1; i >= 0; i-- {
		p := doc.Entries[i].Posture
		if p.Total > 0 {
			summary["posture"] = map[string]any{"score": p.Score, "total": p.Total, "grade": p.Grade}
			return
		}
	}
}

// mergeTrackerSummary adds the third-party/tracker counts to inspect_session.
func mergeTrackerSummary(summary map[string]any, body []byte) {
	var rep struct {
		ThirdPartyHosts int `json:"third_party_hosts"`
		Beacons         int `json:"beacons"`
		Trackers        []struct {
			Host string `json:"host"`
		} `json:"trackers"`
	}
	if json.Unmarshal(body, &rep) != nil {
		return
	}
	summary["third_party_hosts"] = rep.ThirdPartyHosts
	summary["tracker_count"] = len(rep.Trackers)
	summary["beacons"] = rep.Beacons
}

// stripIntelField removes a nested field from the /intel DNS block.
func stripIntelField(body []byte, block, field string) []byte {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	rawBlock, ok := doc[block]
	if !ok {
		return body
	}
	var entries map[string]map[string]json.RawMessage
	if err := json.Unmarshal(rawBlock, &entries); err != nil {
		return body
	}
	for _, entry := range entries {
		delete(entry, field)
	}
	shapedBlock, err := json.Marshal(entries)
	if err != nil {
		return body
	}
	doc[block] = shapedBlock
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}
