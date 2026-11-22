package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dm0xa/isola-browser/backend/pkg/api"
	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/orchestrator"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

func TestSetSessionDataPayload(t *testing.T) {
	t.Parallel()

	body := (&setSessionDataParams{
		Cookies:        []models.SeedCookie{{Name: "sid", Value: "abc"}},
		Headers:        []models.SeedHeader{{Name: "X-Api-Key", Value: "k"}},
		LocalStorage:   map[string]string{"token": "t"},
		SessionStorage: map[string]string{"step": "2"},
		Reload:         true,
	}).payload()

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var generic struct {
		Cookies []models.SeedCookie `json:"cookies"`
		Headers []models.SeedHeader `json:"headers"`
		Storage []struct {
			Kind  string            `json:"kind"`
			Items map[string]string `json:"items"`
		} `json:"storage"`
		Reload bool `json:"reload"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if len(generic.Cookies) != 1 || generic.Cookies[0].Name != "sid" {
		t.Fatalf("cookies = %+v, want the one cookie verbatim", generic.Cookies)
	}
	if len(generic.Headers) != 1 || generic.Headers[0].Name != "X-Api-Key" {
		t.Fatalf("headers = %+v, want the one header verbatim", generic.Headers)
	}
	if len(generic.Storage) != 2 {
		t.Fatalf("storage = %+v, want local + session entries", generic.Storage)
	}
	// Storage carries no origin: the streamer resolves it to the live page.
	for _, st := range generic.Storage {
		if st.Kind != "local" && st.Kind != "session" {
			t.Fatalf("storage kind = %q, want local or session", st.Kind)
		}
	}
	if !generic.Reload {
		t.Fatal("reload flag dropped")
	}

	// An empty payload must not become an empty /seed round trip.
	if got := (&setSessionDataParams{SessionID: "x"}).payload(); len(got.Cookies) != 0 || len(got.Headers) != 0 || len(got.Storage) != 0 || got.Reload {
		t.Fatalf("empty params payload = %+v, want empty", got)
	}
}

// A cookie without a domain is bound to the session target, exactly like the
// REST surface, instead of being rejected by the streamer.
func TestSetSessionDataFillsCookieDefaults(t *testing.T) {
	t.Parallel()

	seed := (&setSessionDataParams{
		SessionID: "x",
		Cookies:   []models.SeedCookie{{Name: "sid", Value: "abc"}},
	}).payload()
	if err := seed.FillDefaults("https://example.com/some/page"); err != nil {
		t.Fatalf("FillDefaults: %v", err)
	}
	if seed.Cookies[0].URL != "https://example.com" {
		t.Fatalf("cookie url = %q, want the session target origin", seed.Cookies[0].URL)
	}
}

func TestMCPAdvertisesSetSessionData(t *testing.T) {
	const token = "mcp-seed-test-token-32-characters!"
	mem := store.NewMemStore()
	t.Cleanup(func() { mem.Close() })
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
	r := chi.NewRouter()
	Mount(r, cfg, Deps{API: api.New(mem, cache, orch, cfg), Store: mem, Cache: cache, Cfg: cfg})
	srv := httptest.NewServer(r)
	defer srv.Close()

	sess := connectTestMCP(t, srv.URL+"/mcp", token)
	defer sess.Close()

	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, candidate := range tools.Tools {
		if candidate.Name == "set_session_data" {
			tool = candidate
			break
		}
	}
	if tool == nil {
		t.Fatal("set_session_data tool not registered")
	}
	b, err := json.Marshal(tool)
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
	for _, want := range []string{"session_id", "cookies", "headers", "local_storage", "session_storage", "reload"} {
		if _, ok := generic.InputSchema.Properties[want]; !ok {
			t.Fatalf("set_session_data schema lacks %q: %s", want, b)
		}
	}
}
