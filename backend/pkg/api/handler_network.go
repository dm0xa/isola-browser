package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dm0xa/isola-browser/backend/pkg/netfilter"
)

// Network-log proxy. The capture lives in the streamer (inside the sandbox);
// this endpoint is the REST twin of MCP get_network, so a control-plane client
// can collect the same entries — headers, TLS verdicts, redirects and tracker
// labels are fields of each entry — without an MCP connection. Filtering uses
// pkg/netfilter, the implementation the MCP tools share, so both surfaces
// select the same entries for the same query.

const (
	networkUpstreamTimeout  = 15 * time.Second
	networkMaxResponseBytes = 8 << 20
	networkMaxLimit         = 1000
	networkMaxTypeLen       = 64
	networkMaxHostLen       = 253
	networkMaxURLLen        = 2048
)

// GetSessionNetwork handles GET /api/sessions/{id}/network.
//
// Query: type, host_contains, url_contains, limit. Everything is validated here
// and the document is buffered before the status line, so a client can always
// trust a 200 body to be complete and untruncated.
func (h *Handler) GetSessionNetwork(w http.ResponseWriter, r *http.Request) {
	session, ok := h.ownedActiveSession(w, r)
	if !ok {
		return
	}

	typeFilter, hostNeedle, urlNeedle, limit, err := parseNetworkQuery(r)
	if err != nil {
		respondErrorMessage(w, http.StatusBadRequest, "invalid_network_query", err.Error())
		return
	}

	addr, ok := h.sessionContainerAddr(w, r, session)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), networkUpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/network", nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			log.Printf("network: upstream timed out after %v", networkUpstreamTimeout)
			respondError(w, http.StatusGatewayTimeout, "network_failed")
			return
		}
		log.Printf("network: streamer request: %v", err)
		respondError(w, http.StatusBadGateway, "network_failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		reason, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Printf("network: streamer returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(reason)))
		respondError(w, http.StatusBadGateway, "network_failed")
		return
	}

	var body bytes.Buffer
	n, err := io.Copy(&body, io.LimitReader(resp.Body, networkMaxResponseBytes+1))
	if err != nil {
		log.Printf("network: response read: %v", err)
		respondError(w, http.StatusBadGateway, "network_failed")
		return
	}
	if n > networkMaxResponseBytes {
		respondErrorMessage(w, http.StatusBadGateway, "network_failed",
			"network log exceeds the 8 MiB limit; retry with limit=200 or a type/host_contains/url_contains filter")
		return
	}

	out := body.Bytes()
	if typeFilter != "" || hostNeedle != "" || urlNeedle != "" || limit > 0 {
		out, err = netfilter.FilterEntriesRaw(out, limit, func(meta netfilter.EntryMeta) bool {
			if typeFilter != "" && !strings.EqualFold(meta.Type, typeFilter) {
				return false
			}
			return netfilter.MatchesHost(meta, hostNeedle) && netfilter.MatchesURL(meta, urlNeedle)
		})
		if err != nil {
			log.Printf("network: filter: %v", err)
			respondError(w, http.StatusBadGateway, "network_failed")
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	_, _ = w.Write(out)
}

// parseNetworkQuery validates the REST query the same way MCP get_network
// validates its arguments: filters are bounded so a captured document can never
// be amplified into unbounded work, and an unusable value is rejected instead
// of silently ignored.
func parseNetworkQuery(r *http.Request) (typeFilter, hostNeedle, urlNeedle string, limit int, err error) {
	q := r.URL.Query()

	typeFilter = strings.TrimSpace(q.Get("type"))
	if len(typeFilter) > networkMaxTypeLen {
		return "", "", "", 0, errors.New("type is too long")
	}

	host := strings.TrimSpace(q.Get("host_contains"))
	if len(host) > networkMaxHostLen {
		return "", "", "", 0, errors.New("host_contains is too long")
	}
	hostNeedle = netfilter.Needle(host)

	rawURL := strings.TrimSpace(q.Get("url_contains"))
	if len(rawURL) > networkMaxURLLen {
		return "", "", "", 0, errors.New("url_contains is too long")
	}
	urlNeedle = netfilter.Needle(rawURL)

	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > networkMaxLimit {
			return "", "", "", 0, errors.New("limit must be an integer between 1 and 1000")
		}
	}
	return typeFilter, hostNeedle, urlNeedle, limit, nil
}
