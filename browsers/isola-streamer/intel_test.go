package main

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMurmur3KnownVector(t *testing.T) {
	if got := murmur3_32([]byte("hello"), 0); got != 613153351 {
		t.Fatalf("murmur3_32(\"hello\") = %d, want 613153351", got)
	}
	if got := murmur3_32(nil, 0); got != 0 {
		t.Fatalf("murmur3_32(nil) = %d, want 0", got)
	}
}

func TestFaviconMMH3IsDeterministic(t *testing.T) {
	icon := []byte("\x00\x00\x01\x00fake-icon-bytes")
	a := faviconMMH3(icon)
	b := faviconMMH3(icon)
	if a != b {
		t.Fatalf("faviconMMH3 is not deterministic: %d vs %d", a, b)
	}
	if a == faviconMMH3([]byte("different")) {
		t.Fatal("different inputs produced the same hash")
	}
}

func TestNormalizeIntelHost(t *testing.T) {
	if got, err := normalizeIntelHost(" Example.COM. "); err != nil || got != "example.com" {
		t.Fatalf("got %q err %v", got, err)
	}
	for _, bad := range []string{"https://example.com", "example.com:443", "localhost", "a b.com", "exa/mple.com"} {
		if _, err := normalizeIntelHost(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestParseChecks(t *testing.T) {
	def, err := parseChecks("", true)
	if err != nil || len(def) != len(intelDefaultChecks) {
		t.Fatalf("defaults = %v err %v", def, err)
	}
	all, err := parseChecks("all", true)
	if err != nil || len(all) != len(intelChecks) {
		t.Fatalf("all = %v err %v", all, err)
	}
	// The provider checks are gone: they must not parse at all any more.
	for _, gone := range []string{"rdap", "ct", "wayback"} {
		if _, err := parseChecks(gone, true); err == nil {
			t.Fatalf("%q must be rejected: recon no longer contacts providers", gone)
		}
	}
	if _, err := parseChecks("nope", true); err == nil {
		t.Fatal("unknown check must be rejected")
	}
	if got, err := parseChecks("", false); err != nil || got != nil {
		t.Fatalf("no host should mean no checks, got %v err %v", got, err)
	}
}

func TestParseIPList(t *testing.T) {
	got, err := parseIPList("8.8.8.8, 1.1.1.1")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v err %v", got, err)
	}
	if _, err := parseIPList("8.8.8.8,nope"); err == nil {
		t.Fatal("invalid IP must be rejected")
	}
}

// withTLSServer swaps the probe client for one that trusts a fixture server and
// hands the caller the host:port to use.
func withTLSServer(t *testing.T, h http.Handler, fn func(host string)) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	defer srv.Close()
	old := intelHTTPClient
	intelHTTPClient = &http.Client{Transport: srv.Client().Transport, Timeout: 5 * time.Second}
	defer func() { intelHTTPClient = old }()
	fn(strings.TrimPrefix(srv.URL, "https://"))
}

func TestProbeRobots(t *testing.T) {
	withTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /admin\nAllow: /admin/public\nSitemap: https://example.com/sitemap.xml\nCrawl-delay: 5\n"))
	}), func(host string) {
		res := probeRobots(host)
		if res.Status != "ok" {
			t.Fatalf("status = %s (%s)", res.Status, res.Error)
		}
		data := res.Data.(map[string]any)
		if len(data["disallow"].([]string)) != 1 {
			t.Fatalf("disallow = %v", data["disallow"])
		}
		if len(data["sitemaps"].([]string)) != 1 || data["crawl_delay"] != "5" {
			t.Fatalf("sitemaps/crawl-delay = %v/%v", data["sitemaps"], data["crawl_delay"])
		}
	})
}

func TestProbeRobotsMissingIsEmptyNotError(t *testing.T) {
	withTLSServer(t, http.NotFoundHandler(), func(host string) {
		res := probeRobots(host)
		if res.Status != "empty" {
			t.Fatalf("status = %s, want empty", res.Status)
		}
	})
}

func TestProbeSitemap(t *testing.T) {
	withTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><urlset><url><loc>https://example.com/a</loc></url><url><loc>https://example.com/b</loc></url></urlset>`))
	}), func(host string) {
		res := probeSitemap(host)
		if res.Status != "ok" {
			t.Fatalf("status = %s", res.Status)
		}
		if got := res.Data.(map[string]any)["count"]; got != 2 {
			t.Fatalf("count = %v", got)
		}
	})
}

func TestProbeSecurityTXTParsesContacts(t *testing.T) {
	withTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/security.txt" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("Contact: mailto:security@example.com\nContact: https://example.com/report\nExpires: 2027-01-01T00:00:00Z\nPolicy: https://example.com/policy\n"))
	}), func(host string) {
		res := probeSecurityTXT(host)
		if res.Status != "ok" {
			t.Fatalf("status = %s (%s)", res.Status, res.Error)
		}
		data := res.Data.(map[string]any)
		if len(data["contacts"].([]string)) != 2 {
			t.Fatalf("contacts = %v", data["contacts"])
		}
		if data["fields"].(map[string]string)["expires"] != "2027-01-01T00:00:00Z" {
			t.Fatalf("fields = %v", data["fields"])
		}
	})
}

func TestProbeFaviconHashesBytes(t *testing.T) {
	icon := []byte("icon-bytes")
	withTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/favicon.ico" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(icon)
	}), func(host string) {
		res := probeFavicon(host)
		if res.Status != "ok" {
			t.Fatalf("status = %s (%s)", res.Status, res.Error)
		}
		data := res.Data.(map[string]any)
		if data["mmh3"] != faviconMMH3(icon) {
			t.Fatalf("mmh3 = %v", data["mmh3"])
		}
		if data["sha1"] == "" {
			t.Fatal("sha1 missing")
		}
	})
}
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestIntelTransportUsesTorSocksProxy(t *testing.T) {
	tr := newIntelTransport(true)
	req, err := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := tr.Proxy(req)
	if err != nil || u == nil {
		t.Fatalf("Tor transport proxy = %v, %v; want a SOCKS proxy", u, err)
	}
	if u.Scheme != "socks5" || u.Host != "127.0.0.1:9050" {
		t.Fatalf("proxy = %s, want socks5://127.0.0.1:9050", u)
	}
}
func TestIntelRootCAsAddsOperatorCA(t *testing.T) {
	t.Setenv("INTEL_CA_PEM", "not-a-certificate")
	if pool := intelRootCAs(); pool != nil {
		t.Fatal("an unusable PEM must fall back to the system pool")
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	leaf := srv.Certificate()
	t.Setenv("INTEL_CA_PEM", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})))

	pool := intelRootCAs()
	if pool == nil {
		t.Fatal("operator CA was not loaded")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "example.com"}); err != nil {
		t.Fatalf("operator CA did not verify the certificate: %v", err)
	}
	tr := newIntelTransport(false)
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("transport is not using the operator CA pool")
	}
}

func TestHostIsPrivate(t *testing.T) {
	old := lookupHostFn
	defer func() { lookupHostFn = old }()

	lookupHostFn = func(string) ([]string, error) { return []string{"10.0.0.5"}, nil }
	if private, _ := hostIsPrivate("internal.example"); !private {
		t.Fatal("RFC1918 target must be refused")
	}
	lookupHostFn = func(string) ([]string, error) { return []string{"93.184.216.34"}, nil }
	if private, _ := hostIsPrivate("example.com"); private {
		t.Fatal("public target wrongly refused")
	}
}

func TestResolveHostIntel(t *testing.T) {
	oldHost, oldAddr, oldTXT, oldCNAME := lookupHostFn, lookupAddrFn, lookupTXTFn, lookupCNAMEFn
	defer func() {
		lookupHostFn, lookupAddrFn, lookupTXTFn, lookupCNAMEFn = oldHost, oldAddr, oldTXT, oldCNAME
	}()

	lookupHostFn = func(string) ([]string, error) { return []string{"8.8.8.8"}, nil }
	lookupCNAMEFn = func(string) (string, error) { return "edge.dns.google.", nil }
	lookupAddrFn = func(string) ([]string, error) { return []string{"dns.google."}, nil }
	lookupTXTFn = func(name string) ([]string, error) {
		switch name {
		case "8.8.8.8.origin.asn.cymru.com":
			return []string{"15169 | 8.8.8.0/24 | US | arin | 1992-12-01"}, nil
		case "AS15169.asn.cymru.com":
			return []string{"15169 | US | arin | 2000-03-30 | GOOGLE, US"}, nil
		}
		return nil, nil
	}

	// 8.8.8.8 reversed is 8.8.8.8, so the same query string is asserted above.
	got := resolveHostIntel("dns.google")
	if len(got.IPs) != 1 || got.IPs[0] != "8.8.8.8" {
		t.Fatalf("ips = %v", got.IPs)
	}
	if got.CNAME != "edge.dns.google" {
		t.Fatalf("cname = %q", got.CNAME)
	}
	if got.PTR["8.8.8.8"] != "dns.google" {
		t.Fatalf("ptr = %v", got.PTR)
	}
	if len(got.ASN) != 1 || got.ASN[0].ASN != 15169 || got.ASN[0].Org != "GOOGLE, US" {
		t.Fatalf("asn = %+v", got.ASN)
	}
}

func TestIntelHandler(t *testing.T) {
	t.Setenv("INTEL_PROBES", "")

	req := httptest.NewRequest(http.MethodGet, "/intel?mode=dns&host=example.com", nil)
	w := httptest.NewRecorder()
	intelHandler(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled status = %d, want 403", w.Code)
	}

	t.Setenv("INTEL_PROBES", "1")
	oldHost := lookupHostFn
	defer func() { lookupHostFn = oldHost }()
	lookupHostFn = func(string) ([]string, error) { return []string{"93.184.216.34"}, nil }

	w = httptest.NewRecorder()
	intelHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var body intelResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.DNS["example.com"] == nil {
		t.Fatalf("dns = %+v", body.DNS)
	}

	bad := httptest.NewRequest(http.MethodGet, "/intel?mode=dns&host=bad_host!", nil)
	w = httptest.NewRecorder()
	intelHandler(w, bad)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid host status = %d, want 400", w.Code)
	}

	// Probe targets on private addresses are refused before any request.
	lookupHostFn = func(string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	priv := httptest.NewRequest(http.MethodGet, "/intel?mode=probe&host=internal.example&checks=robots", nil)
	w = httptest.NewRecorder()
	intelHandler(w, priv)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("private target status = %d: %s", w.Code, w.Body.String())
	}
}
