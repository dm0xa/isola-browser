package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dm0xa/isola/backend/pkg/config"
)

func getConfigBody(t *testing.T, cfg *config.Config) map[string]any {
	t.Helper()
	h, _ := newCapacityTestHandler(t, cfg)
	w := httptest.NewRecorder()
	h.GetConfig(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/config: %v", err)
	}
	return body
}

// TestGetConfigReportsNetworkLogMaxEntries: the viewer trims its capture
// buffers to the same window the streamer keeps.
func TestGetConfigReportsNetworkLogMaxEntries(t *testing.T) {
	body := getConfigBody(t, &config.Config{
		BrowserType:          "chromium",
		Browsers:             map[string]config.BrowserEntry{"chromium": {Enabled: true}},
		NetworkLogMaxEntries: 2500,
	})

	got, ok := body["network_log_max_entries"].(float64)
	if !ok {
		t.Fatalf("network_log_max_entries missing or not a number: %v", body["network_log_max_entries"])
	}
	if got != 2500 {
		t.Fatalf("network_log_max_entries = %v, want 2500", got)
	}
}

// TestGetConfigOmitsUnsetNetworkLogMaxEntries: an unset window is omitted so
// the client falls back to its default instead of trimming to zero.
func TestGetConfigOmitsUnsetNetworkLogMaxEntries(t *testing.T) {
	body := getConfigBody(t, &config.Config{})

	if _, ok := body["network_log_max_entries"]; ok {
		t.Fatalf("network_log_max_entries must be omitted when unset: %v", body["network_log_max_entries"])
	}
}
