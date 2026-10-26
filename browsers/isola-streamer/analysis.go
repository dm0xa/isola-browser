package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// Read-only analysis endpoints over the captured traffic. Both exist because
// the viewer panel and the MCP tools must report the same numbers: the panel
// renders what these return, and the tools proxy them unchanged.

// postureEntry is one document's assessment.
type postureEntry struct {
	ID      string            `json:"id"`
	URL     string            `json:"url"`
	Status  int               `json:"status"`
	Tab     int               `json:"tab"`
	Posture *postureReport    `json:"posture"`
	TLS     *netTLS           `json:"tls,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// postureHandler serves GET /posture:
//
//	?host=<substring>  filter by document host
//	?headers=0         omit the raw header map (posture grade only)
func postureHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("host")))
	withHeaders := r.URL.Query().Get("headers") != "0"

	netMu.Lock()
	entries := make([]postureEntry, 0, 8)
	for _, id := range netOrder {
		req := netRequests[id]
		if req == nil || req.posture == nil {
			continue
		}
		if host != "" && !strings.Contains(strings.ToLower(req.url), host) {
			continue
		}
		entry := postureEntry{
			ID:      req.id,
			URL:     req.url,
			Status:  req.status,
			Tab:     req.tab,
			Posture: req.posture,
			TLS:     req.tls,
		}
		if withHeaders {
			entry.Headers = req.headers
		}
		entries = append(entries, entry)
	}
	netMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"entries": entries,
		"total":   len(entries),
	})
}

// trackerHost is one aggregated host row.
type trackerHost struct {
	Host     string   `json:"host"`
	Domain   string   `json:"domain"`
	Requests int      `json:"requests"`
	Bytes    int64    `json:"bytes"`
	Tracker  string   `json:"tracker,omitempty"`
	IPs      []string `json:"ips,omitempty"`
}

// cookieFingerprintRow labels a cookie family without exposing values.
type cookieFingerprintRow struct {
	Name       string `json:"name"`
	Domain     string `json:"domain"`
	Product    string `json:"product"`
	ThirdParty bool   `json:"third_party"`
}

// trackerReport is the aggregate the panel's Trackers tab and the
// get_trackers MCP tool both render.
type trackerReport struct {
	Origin             string                 `json:"origin,omitempty"`
	Requests           int                    `json:"requests"`
	FirstPartyRequests int                    `json:"first_party_requests"`
	ThirdPartyRequests int                    `json:"third_party_requests"`
	FirstPartyHosts    int                    `json:"first_party_hosts"`
	ThirdPartyHosts    int                    `json:"third_party_hosts"`
	Beacons            int                    `json:"beacons"`
	Trackers           []trackerHost          `json:"trackers"`
	ThirdParty         []trackerHost          `json:"third_party"`
	Cookies            []cookieFingerprintRow `json:"cookie_fingerprints"`
}

const trackerDefaultLimit = 100

// trackersHandler serves GET /trackers with an optional ?limit= per list.
func trackersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := trackerDefaultLimit
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n := atoiPositive(v); n > 0 {
			limit = n
		}
	}

	report := buildTrackerReport(limit)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(report)
}

func buildTrackerReport(limit int) *trackerReport {
	type agg struct {
		host  trackerHost
		ipSet map[string]bool
	}

	report := &trackerReport{
		Trackers:   []trackerHost{},
		ThirdParty: []trackerHost{},
		Cookies:    []cookieFingerprintRow{},
	}

	netMu.Lock()
	// The origin is the most recently seen document host: that is the page the
	// user is actually looking at, and third-party status is relative to it.
	for i := len(netOrder) - 1; i >= 0; i-- {
		req := netRequests[netOrder[i]]
		if req != nil && strings.EqualFold(req.typ, "Document") && req.originHost != "" {
			report.Origin = req.originHost
			break
		}
	}
	if report.Origin == "" {
		for _, origin := range netOriginByTab {
			report.Origin = origin
			break
		}
	}

	byHost := map[string]*agg{}
	for _, id := range netOrder {
		req := netRequests[id]
		if req == nil || req.failed || req.status == 0 {
			continue
		}
		host := hostOfURL(req.url)
		if host == "" {
			continue
		}
		third, tracker := classifyEntry(req)
		report.Requests++
		if third {
			report.ThirdPartyRequests++
		} else {
			report.FirstPartyRequests++
		}
		if isBeaconType(req.typ) {
			report.Beacons++
		}
		a := byHost[host]
		if a == nil {
			a = &agg{
				host: trackerHost{
					Host:    host,
					Domain:  registrableDomain(host),
					Tracker: tracker,
				},
				ipSet: map[string]bool{},
			}
			byHost[host] = a
		}
		a.host.Requests++
		a.host.Bytes += req.bytes
		if tracker != "" && a.host.Tracker == "" {
			a.host.Tracker = tracker
		}
		if req.ip != "" {
			a.ipSet[req.ip] = true
		}
	}
	netMu.Unlock()

	originDomain := registrableDomain(report.Origin)
	for _, a := range byHost {
		third := originDomain != "" && a.host.Domain != originDomain
		if third {
			report.ThirdPartyHosts++
		} else {
			report.FirstPartyHosts++
		}
		if len(a.ipSet) > 0 {
			a.host.IPs = sortedKeys(a.ipSet)
		}
		if a.host.Tracker != "" {
			report.Trackers = append(report.Trackers, a.host)
		}
		if third {
			report.ThirdParty = append(report.ThirdParty, a.host)
		}
	}
	byRequests := func(list []trackerHost) {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Requests != list[j].Requests {
				return list[i].Requests > list[j].Requests
			}
			return list[i].Host < list[j].Host
		})
	}
	byRequests(report.Trackers)
	byRequests(report.ThirdParty)
	report.Trackers = capTrackerHosts(report.Trackers, limit)
	report.ThirdParty = capTrackerHosts(report.ThirdParty, limit)

	report.Cookies = cookieFingerprintRows(originDomain)
	return report
}

func capTrackerHosts(in []trackerHost, limit int) []trackerHost {
	if limit > 0 && len(in) > limit {
		return in[:limit]
	}
	return in
}

// cookieFingerprintRows labels every captured cookie by family, flagging those
// whose domain is outside the page's registrable domain (cross-site cookies).
func cookieFingerprintRows(originDomain string) []cookieFingerprintRow {
	cookieMu.Lock()
	entries := make([]cookieEntry, len(cookieBuffer))
	copy(entries, cookieBuffer)
	cookieMu.Unlock()

	out := make([]cookieFingerprintRow, 0, len(entries))
	for _, e := range entries {
		product := cookieFingerprint(e.Name)
		if product == "" {
			continue
		}
		domain := strings.TrimPrefix(strings.ToLower(e.Domain), ".")
		out = append(out, cookieFingerprintRow{
			Name:       e.Name,
			Domain:     domain,
			Product:    product,
			ThirdParty: originDomain != "" && domain != "" && registrableDomain(domain) != originDomain,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Product != out[j].Product {
			return out[i].Product < out[j].Product
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// atoiPositive parses a positive integer, returning 0 for anything else.
func atoiPositive(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
		if n > 1_000_000 {
			return 0
		}
	}
	return n
}
