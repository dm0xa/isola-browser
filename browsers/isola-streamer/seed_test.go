package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// seedCDPCommand is one command the fake debugger received.
type seedCDPCommand struct {
	path   string
	method string
	params map[string]any
}

// fakeSeedCDP serves a browser endpoint (/json/version + /devtools/browser) and
// one page target (/json/list + /p/page), recording every command. Answers are
// enough for seed: location.origin, location.hostname, storage scripts, and the
// init-script registration.
func fakeSeedCDP(t *testing.T) (*httptest.Server, *[]seedCDPCommand, *sync.Mutex) {
	t.Helper()
	up := websocket.Upgrader{}
	var mu sync.Mutex
	var cmds []seedCDPCommand

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json/version":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"webSocketDebuggerUrl": "ws://" + r.Host + "/devtools/browser",
			})
			return
		case "/json/list":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"type":                 "page",
				"url":                  "https://example.com/",
				"webSocketDebuggerUrl": "ws://" + r.Host + "/p/page",
			}})
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req struct {
				ID     int            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(raw, &req) != nil || req.ID == 0 {
				continue
			}
			mu.Lock()
			cmds = append(cmds, seedCDPCommand{path: r.URL.Path, method: req.Method, params: req.Params})
			mu.Unlock()

			result := map[string]any{}
			switch req.Method {
			case "Runtime.evaluate":
				expr, _ := req.Params["expression"].(string)
				val := "ok"
				switch {
				case expr == "location.origin":
					val = "https://example.com"
				case expr == "location.hostname":
					val = "example.com"
				}
				result = map[string]any{"result": map[string]any{"type": "string", "value": val}}
			case "Page.addScriptToEvaluateOnNewDocument":
				result = map[string]any{"identifier": "script-1"}
			}
			_ = conn.WriteJSON(map[string]any{"id": req.ID, "result": result})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &cmds, &mu
}

// withSeedCDP installs the fake as the page client and returns the recorded
// commands. The browser-level one-shot client finds the same server through
// CDP_URL.
func withSeedCDP(t *testing.T) (*httptest.Server, func() []seedCDPCommand) {
	t.Helper()
	srv, cmds, mu := fakeSeedCDP(t)
	t.Setenv("CDP_URL", srv.URL)

	// Init-script bookkeeping is package state; start every test clean.
	seedScriptsMu.Lock()
	seedScripts = map[string][]string{}
	seedSessionItems = map[string]map[string]string{}
	seedScriptsMu.Unlock()

	c := newCDPClient()
	c.baseURL = srv.URL
	prev := cdpClient
	cdpClient = c
	t.Cleanup(func() { cdpClient = prev })
	go c.Start()
	waitForAttached(t, c, "https://example.com/")

	return srv, func() []seedCDPCommand {
		mu.Lock()
		defer mu.Unlock()
		out := make([]seedCDPCommand, len(*cmds))
		copy(out, *cmds)
		return out
	}
}

func findCommand(cmds []seedCDPCommand, method string) (seedCDPCommand, bool) {
	for _, c := range cmds {
		if c.method == method {
			return c, true
		}
	}
	return seedCDPCommand{}, false
}

func TestNormalizeSeed(t *testing.T) {
	t.Parallel()

	valid, err := normalizeSeed(seedPayload{
		Cookies: []seedCookie{
			{Name: "sid", Value: "v", URL: "https://example.com/app", Secure: true, SameSite: "none", HTTPOnly: true},
			{Name: "host", Value: "1", Domain: ".Example.COM"},
		},
		Headers: []seedHeader{{Name: "X-Api-Key", Value: "k", Domains: []string{"Example.com"}}},
		Storage: []seedStorage{{Origin: "https://example.com/anything", Items: map[string]string{"k": "v"}}},
	})
	if err != nil {
		t.Fatalf("valid payload rejected: %v", err)
	}
	got := valid.Cookies[0]
	if got.Domain != "example.com" || got.Path != "/app" || got.SameSite != "None" {
		t.Fatalf("url cookie normalized to %+v, want domain example.com path /app sameSite None", got)
	}
	if valid.Cookies[1].Domain != "example.com" || valid.Cookies[1].Path != "/" {
		t.Fatalf("domain cookie normalized to %+v, want example.com /", valid.Cookies[1])
	}
	if valid.Headers[0].Domains[0] != "example.com" {
		t.Fatalf("header domain = %q, want lowercased", valid.Headers[0].Domains[0])
	}
	if valid.Storage[0].Kind != "local" || valid.Storage[0].Origin != "https://example.com" {
		t.Fatalf("storage normalized to %+v, want local https://example.com", valid.Storage[0])
	}

	cases := []struct {
		name string
		in   seedPayload
	}{
		{"cookie without domain or url", seedPayload{Cookies: []seedCookie{{Name: "a", Value: "b"}}}},
		{"cookie domain with a path", seedPayload{Cookies: []seedCookie{{Name: "a", Value: "v", Domain: "example.com/x"}}}},
		{"cookie with domain and url", seedPayload{Cookies: []seedCookie{{Name: "a", Value: "v", Domain: "example.com", URL: "https://other.example/x"}}}},
		{"cookie value with newline", seedPayload{Cookies: []seedCookie{{Name: "a", Value: "b\r\nX: y", Domain: "example.com"}}}},
		{"cookie name with separator", seedPayload{Cookies: []seedCookie{{Name: "a;b", Value: "v", Domain: "example.com"}}}},
		{"same_site none without secure", seedPayload{Cookies: []seedCookie{{Name: "a", Value: "v", Domain: "example.com", SameSite: "none"}}}},
		{"unknown same_site", seedPayload{Cookies: []seedCookie{{Name: "a", Value: "v", Domain: "example.com", SameSite: "loose"}}}},
		{"path without slash", seedPayload{Cookies: []seedCookie{{Name: "a", Value: "v", Domain: "example.com", Path: "app"}}}},
		{"header name with space", seedPayload{Headers: []seedHeader{{Name: "X Api", Value: "v"}}}},
		{"header value with newline", seedPayload{Headers: []seedHeader{{Name: "X-A", Value: "v\r\nX-B: c"}}}},
		{"unknown storage kind", seedPayload{Storage: []seedStorage{{Kind: "indexed", Origin: "https://example.com", Items: map[string]string{"k": "v"}}}}},
		{"storage with empty items", seedPayload{Storage: []seedStorage{{Origin: "https://example.com"}}}},
		{"too many cookies", seedPayload{Cookies: func() []seedCookie {
			out := make([]seedCookie, seedMaxCookies+1)
			for i := range out {
				out[i] = seedCookie{Name: "c", Value: "v", Domain: "example.com"}
			}
			return out
		}()}},
	}
	for _, tc := range cases {
		if _, err := normalizeSeed(tc.in); err == nil {
			t.Errorf("%s: accepted, want error", tc.name)
		}
	}
}

func TestNormalizeSeedAcceptsSingleLabelHost(t *testing.T) {
	t.Parallel()
	got, err := normalizeSeed(seedPayload{
		Cookies: []seedCookie{{Name: "sid", Value: "v", URL: "http://seed-echo:8099/"}},
		Headers: []seedHeader{{Name: "X-A", Value: "v", Domains: []string{"intranet"}}},
	})
	if err != nil {
		t.Fatalf("single-label host rejected: %v", err)
	}
	if got.Cookies[0].Domain != "seed-echo" {
		t.Fatalf("cookie domain = %q, want seed-echo", got.Cookies[0].Domain)
	}
	if got.Headers[0].Domains[0] != "intranet" {
		t.Fatalf("header domain = %q, want intranet", got.Headers[0].Domains[0])
	}
}

func TestApplySeedSendsCDPCommands(t *testing.T) {
	_, commands := withSeedCDP(t)

	res, err := applySeed(seedPayload{
		Cookies: []seedCookie{{Name: "sid", Value: "abc", Domain: "example.com", Path: "/", Secure: true, HTTPOnly: true, SameSite: "Lax", Expires: 1767225600}},
		Headers: []seedHeader{
			{Name: "X-Api-Key", Value: "k", Domains: []string{"example.com"}},
			{Name: "X-Other", Value: "no", Domains: []string{"other.example"}},
		},
		Storage: []seedStorage{
			{Kind: "local", Origin: "https://example.com", Items: map[string]string{"token": "t"}},
			{Kind: "session", Origin: "https://example.com", Items: map[string]string{"step": "2"}},
		},
		Reload: true,
	})
	if err != nil {
		t.Fatalf("applySeed: %v", err)
	}
	if res != (seedResult{Cookies: 1, Headers: 1, Storage: 2, Reloaded: true}) {
		t.Fatalf("result = %+v, want 1 cookie / 1 header / 2 storage / reloaded", res)
	}

	cmds := commands()
	cookieCmd, ok := findCommand(cmds, "Storage.setCookies")
	if !ok {
		t.Fatal("no Storage.setCookies command (browser-level connection missing)")
	}
	if cookieCmd.path != "/devtools/browser" {
		t.Fatalf("Storage.setCookies went to %q, want the browser target", cookieCmd.path)
	}
	cookies, _ := cookieCmd.params["cookies"].([]any)
	if len(cookies) != 1 {
		t.Fatalf("cookies param = %v, want one", cookieCmd.params["cookies"])
	}
	c0, _ := cookies[0].(map[string]any)
	if c0["domain"] != "example.com" || c0["path"] != "/" || c0["sameSite"] != "Lax" || c0["secure"] != true || c0["httpOnly"] != true {
		t.Fatalf("cookie params = %v, want normalized attributes", c0)
	}

	headerCmd, ok := findCommand(cmds, "Network.setExtraHTTPHeaders")
	if !ok {
		t.Fatal("no Network.setExtraHTTPHeaders command")
	}
	headers, _ := headerCmd.params["headers"].(map[string]any)
	if _, ok := headers["X-Api-Key"]; !ok {
		t.Fatalf("headers = %v, want X-Api-Key for example.com", headers)
	}
	if _, ok := headers["X-Other"]; ok {
		t.Fatalf("headers = %v, must not carry a header scoped to another host", headers)
	}

	if _, ok := findCommand(cmds, "Page.addScriptToEvaluateOnNewDocument"); !ok {
		t.Fatal("no init script registered for session storage")
	}
	if _, ok := findCommand(cmds, "Page.reload"); !ok {
		t.Fatal("reload requested but Page.reload not sent")
	}
}

func TestApplySeedOriginMismatch(t *testing.T) {
	withSeedCDP(t)

	_, err := applySeed(seedPayload{Storage: []seedStorage{{
		Origin: "https://other.example", Items: map[string]string{"k": "v"},
	}}})
	if err == nil || !strings.Contains(err.Error(), "page origin does not match") {
		t.Fatalf("err = %v, want an origin mismatch", err)
	}
}

func TestHandleSeedContract(t *testing.T) {
	// Validation and method handling happen before any CDP work.
	rec := httptest.NewRecorder()
	handleSeed(rec, httptest.NewRequest(http.MethodGet, "/seed", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /seed = %d, want 405", rec.Code)
	}

	rec = httptest.NewRecorder()
	handleSeed(rec, httptest.NewRequest(http.MethodPost, "/seed", strings.NewReader(`{"cookies":[{"name":"a","value":"b","httpOnly":true,"domain":"example.com"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handleSeed(rec, httptest.NewRequest(http.MethodPost, "/seed", strings.NewReader(`{"cookies":[{"name":"a","value":"b"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing domain: status = %d, want 400", rec.Code)
	}

	// A payload that asks for nothing must not answer 200 with zero counts.
	withSeedCDP(t)
	rec = httptest.NewRecorder()
	handleSeed(rec, httptest.NewRequest(http.MethodPost, "/seed", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "nothing to apply") {
		t.Fatalf("empty payload: status = %d (%s), want 400 nothing to apply", rec.Code, rec.Body.String())
	}

	// A reload on its own is a legitimate call.
	rec = httptest.NewRecorder()
	handleSeed(rec, httptest.NewRequest(http.MethodPost, "/seed", strings.NewReader(`{"reload":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("reload-only: status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	prev := cdpClient
	cdpClient = nil
	t.Cleanup(func() { cdpClient = prev })
	rec = httptest.NewRecorder()
	handleSeed(rec, httptest.NewRequest(http.MethodPost, "/seed", strings.NewReader(`{"reload":true}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no cdp: status = %d, want 503", rec.Code)
	}
}

func TestNormalizeSeedRejectsReservedCookiesAndDuplicateHeaders(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   seedPayload
		want string
	}{
		{
			name: "host-prefixed cookie is a silent no-op as a domain cookie",
			in:   seedPayload{Cookies: []seedCookie{{Name: "__Host-sid", Value: "v", Domain: "example.com"}}},
			want: "__Host-",
		},
		{
			name: "secure-prefixed cookie is a silent no-op as a domain cookie",
			in:   seedPayload{Cookies: []seedCookie{{Name: "__Secure-sid", Value: "v", Domain: "example.com"}}},
			want: "__Secure-",
		},
		{
			name: "cookie name with a space is not a token",
			in:   seedPayload{Cookies: []seedCookie{{Name: "my cookie", Value: "v", Domain: "example.com"}}},
			want: "invalid name",
		},
		{
			name: "duplicate header names would collapse in the CDP map",
			in: seedPayload{Headers: []seedHeader{
				{Name: "X-Api-Key", Value: "one"},
				{Name: "x-api-key", Value: "two"},
			}},
			want: "duplicate name",
		},
	}
	for _, tc := range cases {
		_, err := normalizeSeed(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// A second call for the same origin must add its keys to the sessionStorage
// init script, not replace the earlier call's keys with its own.
func TestApplyStorageSessionItemsAccumulate(t *testing.T) {
	_, commands := withSeedCDP(t)

	for _, entry := range []seedStorage{
		{Kind: "session", Origin: "https://example.com", Items: map[string]string{"one": "1"}},
		{Kind: "session", Origin: "https://example.com", Items: map[string]string{"two": "2"}},
	} {
		if _, err := applyStorage([]seedStorage{entry}, "https://example.com"); err != nil {
			t.Fatalf("applyStorage: %v", err)
		}
	}

	var scripts []string
	for _, c := range commands() {
		if c.method != "Page.addScriptToEvaluateOnNewDocument" {
			continue
		}
		if src, ok := c.params["source"].(string); ok {
			scripts = append(scripts, src)
		}
	}
	if len(scripts) != 2 {
		t.Fatalf("init scripts = %d, want one per call", len(scripts))
	}
	last := scripts[len(scripts)-1]
	if !strings.Contains(last, `"one"`) || !strings.Contains(last, `"two"`) {
		t.Fatalf("second script = %s, want both keys", last)
	}
}

// Seeding a second origin must not remove the first origin's sessionStorage
// init scripts; re-seeding one origin replaces only its own.
func TestRegisterSeedScriptsIsPerOrigin(t *testing.T) {
	_, commands := withSeedCDP(t)

	seedScriptsMu.Lock()
	seedScripts = map[string][]string{}
	seedScriptsMu.Unlock()
	t.Cleanup(func() {
		seedScriptsMu.Lock()
		seedScripts = map[string][]string{}
		seedScriptsMu.Unlock()
	})

	if err := registerSeedScripts("https://a.example", []string{"a"}); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if err := registerSeedScripts("https://b.example", []string{"b"}); err != nil {
		t.Fatalf("register b: %v", err)
	}
	if _, ok := findCommand(commands(), "Page.removeScriptToEvaluateOnNewDocument"); ok {
		t.Fatal("seeding a second origin removed the first origin's init script")
	}

	if err := registerSeedScripts("https://a.example", []string{"a2"}); err != nil {
		t.Fatalf("re-register a: %v", err)
	}
	if _, ok := findCommand(commands(), "Page.removeScriptToEvaluateOnNewDocument"); !ok {
		t.Fatal("re-seeding one origin did not replace its own init script")
	}
}

func TestHandleSeedRejectsOversizeAndTrailingJSON(t *testing.T) {
	prev := cdpClient
	cdpClient = nil
	t.Cleanup(func() { cdpClient = prev })

	oversize := `{"cookies":[{"name":"a","value":"` + strings.Repeat("x", seedMaxBodyBytes) + `"}]}`
	rec := httptest.NewRecorder()
	handleSeed(rec, httptest.NewRequest(http.MethodPost, "/seed", strings.NewReader(oversize)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: status = %d, want 413", rec.Code)
	}

	rec = httptest.NewRecorder()
	handleSeed(rec, httptest.NewRequest(http.MethodPost, "/seed", strings.NewReader(`{"reload":true}{"reload":true}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing data: status = %d, want 400", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("error Cache-Control = %q, want no-store on every response", got)
	}
}

func TestHandleSeedApplies(t *testing.T) {
	withSeedCDP(t)

	body := `{"cookies":[{"name":"sid","value":"abc","domain":"example.com"}],"reload":true}`
	rec := httptest.NewRecorder()
	handleSeed(rec, httptest.NewRequest(http.MethodPost, "/seed", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Cache-Control"); ct != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", ct)
	}
	var res seedResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("response not json: %v", err)
	}
	if res.Cookies != 1 || !res.Reloaded {
		t.Fatalf("result = %+v, want 1 cookie and reloaded", res)
	}
	// The response must describe counts, never the values.
	if strings.Contains(rec.Body.String(), "abc") {
		t.Fatalf("response leaked a cookie value: %s", rec.Body.String())
	}
}
