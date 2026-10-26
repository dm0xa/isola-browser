package main

import (
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Recon: on-demand intelligence about the hosts a session observed. Every check
// is a single bounded GET from inside the sandbox (so it egresses exactly like
// the browser, including Tor when the session is routed through it), cached for
// the session and never triggered automatically. Passive DNS enrichment and the
// active probes share one endpoint because MCP exposes both.

const (
	intelMaxBodyBytes = 2 << 20 // 2 MiB per response
	intelMaxIPs       = 32
	intelMaxChecks    = 5
	intelProbeTimeout = 8 * time.Second
	intelMaxRedirects = 3
	intelUserAgent    = "isola-recon/1.0 (+https://github.com/dm0xa/isola-browser)"
)

// intelChecks is the allowlist. Every check fetches the target itself: recon
// never sends the investigated hostname to a provider. The only outside lookups
// are the configured resolver and the Team Cymru DNS TXT query used for ASN.
var intelChecks = map[string]bool{
	"robots":      true,
	"sitemap":     true,
	"securitytxt": true,
	"wellknown":   true,
	"favicon":     true,
}

var intelDefaultChecks = []string{"robots", "securitytxt", "sitemap"}

// intelEnabled is the operator kill switch (INTEL_PROBES). Read per request:
// recon calls are rare, and a session must never start with an egress path the
// deployment did not intend.
func intelEnabled() bool {
	v := strings.TrimSpace(os.Getenv("INTEL_PROBES"))
	return v == "1" || strings.EqualFold(v, "true")
}

// isTorSession reports whether the orchestrator launched this container with
// Tor enabled. The entrypoint enforces the firewall from the same variable, so
// the two cannot disagree about whether direct egress exists.
func isTorSession() bool {
	v := strings.TrimSpace(os.Getenv("TOR_ENABLED"))
	return v == "1" || strings.EqualFold(v, "true")
}

// Tor endpoints used when the session is routed through Tor. HTTP goes out
// through the SOCKS proxy (which also resolves names remotely); DNS goes to
// Tor's DNSPort, so a lookup cannot leak past the circuit.
const (
	torSocksURL = "socks5://127.0.0.1:9050"
	torDNSAddr  = "127.0.0.1:5353"
)

// intelRootCAs adds an operator-supplied CA to the system pool. TLS-inspecting
// networks (a corporate proxy terminating connections) present a chain the
// image does not trust; without this every probe fails with "certificate
// signed by unknown authority". Verification is never disabled — the operator
// installs the CA instead.
func intelRootCAs() *x509.CertPool {
	pem := strings.TrimSpace(os.Getenv("INTEL_CA_PEM"))
	if pem == "" {
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM([]byte(pem)) {
		log.Printf("intel: INTEL_CA_PEM contains no usable certificate; using the system pool")
		return nil
	}
	return pool
}

// newIntelTransport builds the transport for recon requests. In a Tor session
// it dials through the Tor SOCKS proxy: the container firewall drops direct
// exits, and a proxied dialect gives the proxy the hostname so name resolution
// happens inside the circuit too.
func newIntelTransport(tor bool) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if tor {
		if u, err := url.Parse(torSocksURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	if pool := intelRootCAs(); pool != nil {
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{}
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return tr
}

// newIntelResolver returns the resolver recon uses. In a Tor session it sends
// DNS to Tor's DNSPort (which answers A/AAAA/PTR inside the circuit; TXT is not
// supported there, so ASN lookups are reported as unavailable).
func newIntelResolver(tor bool) *net.Resolver {
	if !tor {
		return net.DefaultResolver
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "udp", torDNSAddr)
		},
	}
}

// DNS lookups are injectable so the intel tests never touch the network.
var intelResolver = newIntelResolver(isTorSession())

var (
	lookupHostFn = func(host string) ([]string, error) {
		return intelResolver.LookupHost(context.Background(), host)
	}
	lookupCNAMEFn = func(host string) (string, error) {
		return intelResolver.LookupCNAME(context.Background(), host)
	}
	lookupAddrFn = func(addr string) ([]string, error) {
		return intelResolver.LookupAddr(context.Background(), addr)
	}
	lookupTXTFn = func(name string) ([]string, error) {
		return intelResolver.LookupTXT(context.Background(), name)
	}
)

// intelHTTPClient is shared and bounded: one timeout for the whole check, a
// hard redirect count, and no cross-registrable-domain hops. That last rule is
// for *target* probes — a robots.txt fetch must not silently become a request
// to an unrelated third party.
var intelHTTPClient = &http.Client{
	Timeout:   intelProbeTimeout,
	Transport: newIntelTransport(isTorSession()),
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if err := intelRedirectGuard(req, via); err != nil {
			return err
		}
		if len(via) > 0 {
			from := via[0].URL.Hostname()
			to := req.URL.Hostname()
			if !strings.EqualFold(registrableDomain(from), registrableDomain(to)) {
				return errors.New("cross-domain redirect")
			}
		}
		return nil
	},
}

// intelRedirectGuard bounds redirect depth and keeps every hop on http/https.
func intelRedirectGuard(req *http.Request, via []*http.Request) error {
	if len(via) >= intelMaxRedirects {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
		return errors.New("unsupported scheme")
	}
	return nil
}

// hostIntel is the DNS/ASN view of one host or IP.
type hostIntel struct {
	Host         string            `json:"host,omitempty"`
	IPs          []string          `json:"ips,omitempty"`
	CNAME        string            `json:"cname,omitempty"`
	PTR          map[string]string `json:"ptr,omitempty"`
	ASN          []*asnInfo        `json:"asn,omitempty"`
	ResolveError string            `json:"resolve_error,omitempty"`
	Private      bool              `json:"private,omitempty"`
	// Notes explain what was deliberately not looked up (Tor, operator switch).
	Notes []string `json:"notes,omitempty"`
}

// asnInfo is one IP's network ownership, resolved over DNS (Team Cymru).
type asnInfo struct {
	IP       string `json:"ip"`
	ASN      int    `json:"asn,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	Country  string `json:"country,omitempty"`
	Registry string `json:"registry,omitempty"`
	Org      string `json:"org,omitempty"`
}

// probeResult is one active check. Status is always present so a consumer can
// tell "nothing configured" (empty) from "could not ask" (unreachable/error).
type probeResult struct {
	Check      string `json:"check"`
	URL        string `json:"url,omitempty"`
	Status     string `json:"status"` // ok | empty | unreachable | error | skipped
	HTTPStatus int    `json:"http_status,omitempty"`
	Error      string `json:"error,omitempty"`
	Data       any    `json:"data,omitempty"`
	Cached     bool   `json:"cached,omitempty"`
}

// intelResponse is the /intel payload.
type intelResponse struct {
	Host        string                  `json:"host,omitempty"`
	DNS         map[string]*hostIntel   `json:"dns,omitempty"`
	Probes      map[string]*probeResult `json:"probes,omitempty"`
	GeneratedAt int64                   `json:"generated_at"`
}

// intelCache keys results for the session's lifetime. The container dies with
// the session, so "forever" is exactly as long as the process lives.
var (
	intelCacheMu sync.Mutex
	intelCache   = map[string]*probeResult{}
)

func resetIntelCacheForTest() {
	intelCacheMu.Lock()
	intelCache = map[string]*probeResult{}
	intelCacheMu.Unlock()
}

func cachedProbe(key string) (*probeResult, bool) {
	intelCacheMu.Lock()
	defer intelCacheMu.Unlock()
	r, ok := intelCache[key]
	if !ok {
		return nil, false
	}
	cp := *r
	cp.Cached = true
	return &cp, true
}

func storeProbe(key string, r *probeResult) {
	intelCacheMu.Lock()
	if len(intelCache) < 2000 {
		cp := *r
		cp.Cached = false
		intelCache[key] = &cp
	}
	intelCacheMu.Unlock()
}

// intelHandler serves GET /intel.
//
//	host=<hostname>        the site to inspect (required for probes)
//	ips=<a,b,c>            enrich these IPs instead of resolving host
//	checks=<robots,...>    which active checks to run (default three same-origin)
//	mode=dns|probe|all     explicit mode; otherwise inferred
func intelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !intelEnabled() {
		writeIntelJSON(w, http.StatusForbidden, map[string]any{"error": "intel_disabled_by_operator"})
		return
	}

	q := r.URL.Query()
	host, err := normalizeIntelHost(q.Get("host"))
	if err != nil {
		writeIntelJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_host", "detail": err.Error()})
		return
	}
	ips, err := parseIPList(q.Get("ips"))
	if err != nil {
		writeIntelJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_ip", "detail": err.Error()})
		return
	}
	checks, err := parseChecks(q.Get("checks"), host != "")
	if err != nil {
		writeIntelJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_checks", "detail": err.Error()})
		return
	}

	mode := strings.ToLower(strings.TrimSpace(q.Get("mode")))
	switch mode {
	case "", "dns", "probe", "all":
	default:
		writeIntelJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_mode"})
		return
	}
	if mode == "" {
		switch {
		case len(checks) > 0:
			mode = "probe"
		default:
			mode = "all"
		}
	}
	if host == "" && len(ips) == 0 {
		writeIntelJSON(w, http.StatusBadRequest, map[string]any{"error": "host_or_ips_required"})
		return
	}

	resp := &intelResponse{Host: host, GeneratedAt: time.Now().UnixMilli()}

	if mode == "dns" || mode == "all" {
		resp.DNS = map[string]*hostIntel{}
		if host != "" {
			resp.DNS[host] = resolveHostIntel(host)
		}
		for _, ip := range ips {
			resp.DNS[ip] = resolveIPIntel(ip)
		}
	}

	if (mode == "probe" || mode == "all") && host != "" {
		// A probe target must be public: the egress firewall already blocks
		// private ranges, but refusing early is cheaper and explicit.
		if private, why := hostIsPrivate(host); private {
			writeIntelJSON(w, http.StatusBadRequest, map[string]any{
				"error": "probe_target_not_public", "detail": why,
			})
			return
		}
		resp.Probes = runProbes(host, checks)
	}

	writeIntelJSON(w, http.StatusOK, resp)
}

func writeIntelJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// normalizeIntelHost validates and canonicalises a hostname. URLs and ports are
// rejected rather than silently parsed: the caller must pass a host.
func normalizeIntelHost(raw string) (string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	raw = strings.TrimSuffix(raw, ".")
	if raw == "" {
		return "", nil
	}
	if len(raw) > 253 {
		return "", errors.New("hostname too long")
	}
	if strings.ContainsAny(raw, " /\\:@?#") {
		return "", errors.New("not a hostname")
	}
	for _, r := range raw {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '.' && r != '-' && r != '_' {
			return "", errors.New("not a hostname")
		}
	}
	if !strings.Contains(raw, ".") {
		return "", errors.New("hostname must contain a dot")
	}
	return raw, nil
}

func parseIPList(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > intelMaxIPs {
		return nil, fmt.Errorf("at most %d addresses", intelMaxIPs)
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		ip := net.ParseIP(strings.TrimSpace(p))
		if ip == nil {
			return nil, fmt.Errorf("%q is not an IP address", p)
		}
		out = append(out, ip.String())
	}
	return out, nil
}

// parseChecks resolves the requested check list. An empty value means the
// conservative default; "all" expands to every allowlisted check.
func parseChecks(raw string, haveHost bool) ([]string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		if !haveHost {
			return nil, nil
		}
		out := make([]string, len(intelDefaultChecks))
		copy(out, intelDefaultChecks)
		return out, nil
	}
	if raw == "all" {
		out := make([]string, 0, len(intelChecks))
		for name := range intelChecks {
			out = append(out, name)
		}
		// Deterministic order keeps the JSON stable for tests and agents.
		sortStrings(out)
		return out, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(intelChecks))
	for _, p := range strings.Split(raw, ",") {
		name := strings.TrimSpace(p)
		if name == "" {
			continue
		}
		if !intelChecks[name] {
			return nil, fmt.Errorf("unknown check %q", name)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) > intelMaxChecks {
		return nil, fmt.Errorf("at most %d checks", intelMaxChecks)
	}
	sortStrings(out)
	return out, nil
}

func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// hostIsPrivate resolves a hostname and reports whether it points anywhere the
// sandbox must not be asked to reach.
func hostIsPrivate(host string) (bool, string) {
	ips, err := lookupHostFn(host)
	if err != nil || len(ips) == 0 {
		return false, ""
	}
	for _, raw := range ips {
		ip := net.ParseIP(raw)
		if ip == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return true, host + " resolves to " + raw
		}
	}
	return false, ""
}

// resolveHostIntel collects addresses, the CNAME chain and per-IP ownership.
func resolveHostIntel(host string) *hostIntel {
	out := &hostIntel{Host: host}
	ips, err := lookupHostFn(host)
	if err != nil {
		out.ResolveError = err.Error()
	}
	if len(ips) > intelMaxIPs {
		ips = ips[:intelMaxIPs]
	}
	out.IPs = ips
	if cname, err := lookupCNAMEFn(host); err == nil {
		cname = strings.TrimSuffix(cname, ".")
		if !strings.EqualFold(cname, host) {
			out.CNAME = cname
		}
	}
	enrichIPs(out, ips)
	applyIntelNotes(out)
	return out
}

func resolveIPIntel(ip string) *hostIntel {
	parsed := net.ParseIP(ip)
	out := &hostIntel{Host: ip, IPs: []string{ip}}
	if parsed != nil && (parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsLinkLocalUnicast()) {
		out.Private = true
	}
	enrichIPs(out, []string{ip})
	applyIntelNotes(out)
	return out
}

// applyIntelNotes records the lookups that were deliberately not performed, so
// an empty ASN list is never mistaken for "no data available".
func applyIntelNotes(out *hostIntel) {
	if isTorSession() {
		out.Notes = append(out.Notes, "resolved through Tor (DNSPort)")
		if len(out.ASN) == 0 && len(out.IPs) > 0 && !out.Private {
			out.Notes = append(out.Notes, "ASN/org lookup unavailable through Tor (DNSPort answers A/AAAA/PTR, not TXT)")
		}
	}
}

// enrichIPs fills PTR and ASN for each address, skipping IP literals that are
// not public (their PTR is noise, and ASN lookups would leak the private range).
func enrichIPs(out *hostIntel, ips []string) {
	for _, raw := range ips {
		ip := net.ParseIP(raw)
		if ip == nil {
			continue
		}
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			out.Private = true
			continue
		}
		if names, err := lookupAddrFn(raw); err == nil && len(names) > 0 {
			if out.PTR == nil {
				out.PTR = map[string]string{}
			}
			out.PTR[raw] = strings.TrimSuffix(names[0], ".")
		}
		if info := asnForIP(raw); info != nil {
			out.ASN = append(out.ASN, info)
		}
	}
}

// asnForIP asks Team Cymru over DNS TXT — the one ownership source that works
// inside the sandbox's allowlist (WHOIS on port 43 is blocked).
func asnForIP(ip string) *asnInfo {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return nil
	}
	octets := strings.Split(parsed.To4().String(), ".")
	for i, j := 0, len(octets)-1; i < j; i, j = i+1, j-1 {
		octets[i], octets[j] = octets[j], octets[i]
	}
	query := strings.Join(octets, ".") + ".origin.asn.cymru.com"
	txts, err := lookupTXTFn(query)
	if err != nil || len(txts) == 0 {
		return nil
	}
	fields := strings.Split(txts[0], "|")
	if len(fields) < 3 {
		return nil
	}
	info := &asnInfo{IP: ip}
	if n, err := strconv.Atoi(strings.TrimSpace(strings.Fields(fields[0])[0])); err == nil {
		info.ASN = n
	}
	info.Prefix = strings.TrimSpace(fields[1])
	info.Country = strings.TrimSpace(fields[2])
	if len(fields) > 3 {
		info.Registry = strings.TrimSpace(fields[3])
	}
	if info.ASN > 0 {
		info.Org = asnOrg(info.ASN)
	}
	return info
}

func asnOrg(asn int) string {
	txts, err := lookupTXTFn("AS" + strconv.Itoa(asn) + ".asn.cymru.com")
	if err != nil || len(txts) == 0 {
		return ""
	}
	fields := strings.Split(txts[0], "|")
	if len(fields) < 5 {
		return ""
	}
	return strings.TrimSpace(fields[len(fields)-1])
}

// runProbes executes the requested checks, in order, using the session cache.
// Every check fetches the host being investigated, never a provider.
func runProbes(host string, checks []string) map[string]*probeResult {
	out := make(map[string]*probeResult, len(checks))
	for _, check := range checks {
		key := host + "|" + check
		if cached, ok := cachedProbe(key); ok {
			out[check] = cached
			continue
		}
		var res *probeResult
		switch check {
		case "robots":
			res = probeRobots(host)
		case "sitemap":
			res = probeSitemap(host)
		case "securitytxt":
			res = probeSecurityTXT(host)
		case "wellknown":
			res = probeWellKnown(host)
		case "favicon":
			res = probeFavicon(host)
		default:
			res = &probeResult{Check: check, Status: "skipped", Error: "unknown check"}
		}
		storeProbe(key, res)
		out[check] = res
	}
	return out
}

// fetchText performs the bounded GET shared by the target probes.
func fetchText(rawURL string) ([]byte, int, error) {
	return fetchTextVia(intelHTTPClient, rawURL)
}

func fetchTextVia(client *http.Client, rawURL string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", intelUserAgent)
	req.Header.Set("Accept", "*/*")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, intelMaxBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func probeRobots(host string) *probeResult {
	rawURL := "https://" + host + "/robots.txt"
	body, status, err := fetchText(rawURL)
	res := &probeResult{Check: "robots", URL: rawURL, HTTPStatus: status}
	if err != nil {
		res.Status = "unreachable"
		res.Error = err.Error()
		return res
	}
	if status == http.StatusNotFound {
		res.Status = "empty"
		return res
	}
	if status < 200 || status >= 300 {
		res.Status = "error"
		res.Error = fmt.Sprintf("HTTP %d", status)
		return res
	}

	agents := []string{}
	disallow := []string{}
	allow := []string{}
	sitemaps := []string{}
	crawlDelay := ""
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		if val == "" && key != "disallow" && key != "allow" {
			continue
		}
		switch key {
		case "user-agent":
			agents = appendCapped(agents, val, 50)
		case "disallow":
			disallow = appendCapped(disallow, val, 200)
		case "allow":
			allow = appendCapped(allow, val, 200)
		case "sitemap":
			sitemaps = appendCapped(sitemaps, val, 20)
		case "crawl-delay":
			crawlDelay = val
		}
	}
	res.Status = "ok"
	res.Data = map[string]any{
		"user_agents": agents,
		"disallow":    disallow,
		"allow":       allow,
		"sitemaps":    sitemaps,
		"crawl_delay": crawlDelay,
		"bytes":       len(body),
	}
	return res
}

var sitemapLocRe = regexp.MustCompile(`(?is)<loc>\s*([^<]+?)\s*</loc>`)

func probeSitemap(host string) *probeResult {
	rawURL := "https://" + host + "/sitemap.xml"
	body, status, err := fetchText(rawURL)
	res := &probeResult{Check: "sitemap", URL: rawURL, HTTPStatus: status}
	if err != nil {
		res.Status = "unreachable"
		res.Error = err.Error()
		return res
	}
	if status == http.StatusNotFound {
		res.Status = "empty"
		return res
	}
	if status < 200 || status >= 300 {
		res.Status = "error"
		res.Error = fmt.Sprintf("HTTP %d", status)
		return res
	}
	matches := sitemapLocRe.FindAllSubmatch(body, 200)
	urls := make([]string, 0, len(matches))
	for _, m := range matches {
		urls = append(urls, strings.TrimSpace(string(m[1])))
	}
	res.Status = "ok"
	res.Data = map[string]any{"urls": urls, "count": len(urls), "bytes": len(body)}
	return res
}

func probeSecurityTXT(host string) *probeResult {
	rawURL := "https://" + host + "/.well-known/security.txt"
	body, status, err := fetchText(rawURL)
	if err != nil {
		return &probeResult{Check: "securitytxt", URL: rawURL, Status: "unreachable", Error: err.Error()}
	}
	if status == http.StatusNotFound {
		// The legacy location is still the specified fallback.
		alt := "https://" + host + "/security.txt"
		if body2, status2, err2 := fetchText(alt); err2 == nil && status2 >= 200 && status2 < 300 {
			body, status, err = body2, status2, nil
			rawURL = alt
		}
	}
	res := &probeResult{Check: "securitytxt", URL: rawURL, HTTPStatus: status}
	if err != nil {
		res.Status = "unreachable"
		res.Error = err.Error()
		return res
	}
	if status == http.StatusNotFound {
		res.Status = "empty"
		return res
	}
	if status < 200 || status >= 300 {
		res.Status = "error"
		res.Error = fmt.Sprintf("HTTP %d", status)
		return res
	}

	contacts := []string{}
	fieldsOut := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		if val == "" {
			continue
		}
		switch key {
		case "contact":
			contacts = appendCapped(contacts, val, 10)
		case "expires", "policy", "canonical", "acknowledgments", "preferred-languages", "hiring":
			if _, exists := fieldsOut[key]; !exists {
				fieldsOut[key] = val
			}
		}
	}
	res.Status = "ok"
	res.Data = map[string]any{"contacts": contacts, "fields": fieldsOut, "bytes": len(body)}
	return res
}

func probeWellKnown(host string) *probeResult {
	rawURL := "https://" + host + "/.well-known/change-password"
	req, err := http.NewRequest(http.MethodHead, rawURL, nil)
	if err != nil {
		return &probeResult{Check: "wellknown", URL: rawURL, Status: "error", Error: err.Error()}
	}
	req.Header.Set("User-Agent", intelUserAgent)
	resp, err := intelHTTPClient.Do(req)
	res := &probeResult{Check: "wellknown", URL: rawURL}
	if err != nil {
		res.Status = "unreachable"
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.HTTPStatus = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		res.Status = "ok"
		res.Data = map[string]any{"change_password": true}
	} else {
		res.Status = "empty"
		res.Data = map[string]any{"change_password": false}
	}
	return res
}

func probeFavicon(host string) *probeResult {
	rawURL := "https://" + host + "/favicon.ico"
	body, status, err := fetchText(rawURL)
	res := &probeResult{Check: "favicon", URL: rawURL, HTTPStatus: status}
	if err != nil {
		res.Status = "unreachable"
		res.Error = err.Error()
		return res
	}
	if status == http.StatusNotFound || len(body) == 0 {
		res.Status = "empty"
		return res
	}
	if status < 200 || status >= 300 {
		res.Status = "error"
		res.Error = fmt.Sprintf("HTTP %d", status)
		return res
	}
	sum := sha1.Sum(body)
	res.Status = "ok"
	res.Data = map[string]any{
		"mmh3":  faviconMMH3(body),
		"sha1":  hex.EncodeToString(sum[:]),
		"bytes": len(body),
	}
	return res
}

func appendCapped(list []string, v string, max int) []string {
	if v == "" || len(list) >= max {
		return list
	}
	return append(list, v)
}

func containsFold(list []string, needle string) bool {
	for _, v := range list {
		if strings.EqualFold(v, needle) {
			return true
		}
	}
	return false
}

// faviconMMH3 reproduces Shodan's favicon hash: base64 with 76-character lines
// and a trailing newline, hashed with MurmurHash3 x86_32 (seed 0). It is the
// standard pivot for "what else runs this favicon".
func faviconMMH3(body []byte) int32 {
	enc := base64.StdEncoding.EncodeToString(body)
	var b strings.Builder
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteByte('\n')
		enc = enc[76:]
	}
	b.WriteString(enc)
	b.WriteByte('\n')
	return int32(murmur3_32([]byte(b.String()), 0))
}

// murmur3_32 is MurmurHash3 x86_32.
func murmur3_32(data []byte, seed uint32) uint32 {
	const (
		c1 = 0xcc9e2d51
		c2 = 0x1b873593
	)
	h1 := seed
	nblocks := len(data) / 4
	for i := 0; i < nblocks; i++ {
		k1 := uint32(data[i*4]) | uint32(data[i*4+1])<<8 |
			uint32(data[i*4+2])<<16 | uint32(data[i*4+3])<<24
		k1 *= c1
		k1 = (k1 << 15) | (k1 >> 17)
		k1 *= c2
		h1 ^= k1
		h1 = (h1 << 13) | (h1 >> 19)
		h1 = h1*5 + 0xe6546b64
	}
	var k1 uint32
	tail := data[nblocks*4:]
	switch len(tail) {
	case 3:
		k1 ^= uint32(tail[2]) << 16
		fallthrough
	case 2:
		k1 ^= uint32(tail[1]) << 8
		fallthrough
	case 1:
		k1 ^= uint32(tail[0])
		k1 *= c1
		k1 = (k1 << 15) | (k1 >> 17)
		k1 *= c2
		h1 ^= k1
	}
	h1 ^= uint32(len(data))
	h1 ^= h1 >> 16
	h1 *= 0x85ebca6b
	h1 ^= h1 >> 13
	h1 *= 0xc2b2ae35
	h1 ^= h1 >> 16
	return h1
}
