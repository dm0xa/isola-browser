package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
)

// Recon proxy. The lookups and probes run in the streamer (inside the sandbox,
// behind the egress firewall), and the control plane never performs them. This
// handler validates the query, forwards it and streams the reply back so the
// viewer and MCP see exactly the same document.

const (
	intelUpstreamTimeout  = 12 * time.Second
	intelMaxResponseBytes = 4 << 20
	intelMaxHostLen       = 253
	intelMaxIPs           = 32
	intelMaxChecks        = 5
)

// intelAllowedChecks mirrors the streamer allowlist. Keeping the list here stops
// an unknown value from reaching the container at all. Every check fetches the
// host being investigated; none of them contacts a provider.
var intelAllowedChecks = map[string]bool{
	"robots":      true,
	"sitemap":     true,
	"securitytxt": true,
	"wellknown":   true,
	"favicon":     true,
}

// GetSessionIntel handles GET /api/sessions/{id}/intel.
//
// Query: host, ips, mode=dns|probe|all, checks=a,b,c. Everything is validated
// here; the streamer only ever sees a canonical query.
func (h *Handler) GetSessionIntel(w http.ResponseWriter, r *http.Request) {
	session, ok := h.resolveSession(w, r)
	if !ok {
		return
	}

	switch session.Status {
	case models.SessionStatusRunning:
		if !h.authorizeOwner(w, r, session) {
			return
		}
	case models.SessionStatusProvisioning:
		if !h.authorizeOwner(w, r, session) {
			return
		}
		respondError(w, http.StatusConflict, "container_not_ready")
		return
	default:
		respondError(w, http.StatusNotFound, "session_not_found")
		return
	}

	query, err := validateIntelQuery(r.URL.Query())
	if err != nil {
		respondErrorMessage(w, http.StatusBadRequest, "invalid_intel_query", err.Error())
		return
	}

	addr, err := h.cache.Get(r.Context(), "session:"+session.ID.String())
	if err != nil || addr == "" {
		respondError(w, http.StatusConflict, "container_not_ready")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), intelUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/intel?"+query.Encode(), nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			log.Printf("intel: upstream timed out after %v", intelUpstreamTimeout)
			respondError(w, http.StatusGatewayTimeout, "intel_failed")
			return
		}
		log.Printf("intel: streamer request: %v", err)
		respondError(w, http.StatusBadGateway, "intel_failed")
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, io.LimitReader(resp.Body, intelMaxResponseBytes)); err != nil {
		log.Printf("intel: response copy: %v", err)
	}
}

// validateIntelQuery rejects unknown keys and re-emits a canonical query. A
// request must name a host or at least one IP; probing always needs a host.
func validateIntelQuery(in url.Values) (url.Values, error) {
	out := url.Values{}
	for key := range in {
		switch key {
		case "host", "ips", "mode", "checks":
		default:
			return nil, errors.New("unknown query parameter: " + key)
		}
	}

	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.Get("host")), "."))
	if host != "" {
		if err := validateIntelHost(host); err != nil {
			return nil, err
		}
		out.Set("host", host)
	}

	mode := strings.ToLower(strings.TrimSpace(in.Get("mode")))
	switch mode {
	case "", "dns", "probe", "all":
		if mode != "" {
			out.Set("mode", mode)
		}
	default:
		return nil, errors.New("mode must be dns, probe or all")
	}

	if raw := strings.TrimSpace(in.Get("ips")); raw != "" {
		parts := strings.Split(raw, ",")
		if len(parts) > intelMaxIPs {
			return nil, errors.New("too many IP addresses")
		}
		canonical := make([]string, 0, len(parts))
		for _, p := range parts {
			ip := net.ParseIP(strings.TrimSpace(p))
			if ip == nil {
				return nil, errors.New("invalid IP address: " + p)
			}
			canonical = append(canonical, ip.String())
		}
		out.Set("ips", strings.Join(canonical, ","))
	}

	if raw := strings.ToLower(strings.TrimSpace(in.Get("checks"))); raw != "" && raw != "all" {
		parts := strings.Split(raw, ",")
		if len(parts) > intelMaxChecks {
			return nil, errors.New("too many checks")
		}
		seen := map[string]bool{}
		canonical := make([]string, 0, len(parts))
		for _, p := range parts {
			name := strings.TrimSpace(p)
			if name == "" {
				continue
			}
			if !intelAllowedChecks[name] {
				return nil, errors.New("unknown check: " + name)
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			canonical = append(canonical, name)
		}
		if len(canonical) > 0 {
			out.Set("checks", strings.Join(canonical, ","))
		}
	} else if raw == "all" {
		out.Set("checks", "all")
	}

	if host == "" && out.Get("ips") == "" {
		return nil, errors.New("host or ips is required")
	}
	if mode == "probe" && host == "" {
		return nil, errors.New("probe mode requires a host")
	}

	return out, nil
}

// validateIntelHost accepts a bare hostname only. URLs, ports and credentials
// are rejected rather than parsed: the caller must say what it is probing.
func validateIntelHost(host string) error {
	if len(host) > intelMaxHostLen {
		return errors.New("hostname too long")
	}
	if strings.ContainsAny(host, " /\\:@?#") {
		return errors.New("host must be a bare hostname")
	}
	for _, r := range host {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			continue
		}
		return errors.New("host must be a bare hostname")
	}
	if !strings.Contains(host, ".") {
		return errors.New("host must contain a dot")
	}
	return nil
}
