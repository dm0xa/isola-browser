package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/models"
)

// TestCreateSessionTorCountry pins the Tor exit-country contract on create:
// a malformed code is a client error, a country is only stored when Tor is on,
// and a per-request value overrides the server default (config.TorExitCountry).
func TestCreateSessionTorCountry(t *testing.T) {
	tests := []struct {
		name        string
		tor         bool
		country     string
		cfgDefault  string
		wantStatus  int
		wantCountry string
	}{
		{name: "invalid code is rejected", tor: true, country: "swe", wantStatus: http.StatusBadRequest},
		{name: "country without tor is dropped", tor: false, country: "se", wantStatus: http.StatusAccepted, wantCountry: ""},
		{name: "country is stored and normalized", tor: true, country: "SE", wantStatus: http.StatusAccepted, wantCountry: "se"},
		{name: "server default applies when omitted", tor: true, cfgDefault: "no", wantStatus: http.StatusAccepted, wantCountry: "no"},
		{name: "request overrides server default", tor: true, country: "se", cfgDefault: "no", wantStatus: http.StatusAccepted, wantCountry: "se"},
		{name: "no tor and no default stays empty", tor: false, wantStatus: http.StatusAccepted, wantCountry: ""},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				BrowserType:          "chromium",
				Browsers:             map[string]config.BrowserEntry{"chromium": {Enabled: true}},
				Resolutions:          []config.OptionEntry{{Value: "1280x720", Label: "720p"}},
				Resolution:           "1280x720",
				ResolutionMax:        "1920x1080",
				DisableURLValidation: true,
				TorExitCountry:       tt.cfgDefault,
			}
			h, st := newCapacityTestHandler(t, cfg)

			body, _ := json.Marshal(models.CreateSessionRequest{
				URL:        "https://example.com",
				TorEnabled: tt.tor,
				TorCountry: tt.country,
			})
			req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body))
			w := httptest.NewRecorder()
			h.CreateSession(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantStatus != http.StatusAccepted {
				return
			}

			var resp models.CreateSessionResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			id, err := uuid.Parse(resp.ID)
			if err != nil {
				t.Fatal(err)
			}
			sess, err := st.Sessions.GetSession(context.Background(), id)
			if err != nil || sess == nil {
				t.Fatalf("session row missing (err=%v)", err)
			}
			if sess.TorCountry != tt.wantCountry {
				t.Fatalf("stored TorCountry = %q, want %q", sess.TorCountry, tt.wantCountry)
			}
		})
	}
}

// TestGetConfigExposesTorExitCountry: the default is published so a UI can
// prefill it, and omitted entirely when unset.
func TestGetConfigExposesTorExitCountry(t *testing.T) {
	t.Parallel()

	fetch := func(t *testing.T, country string) (string, bool) {
		t.Helper()
		h, _ := newCapacityTestHandler(t, &config.Config{
			BrowserType:    "chromium",
			Browsers:       map[string]config.BrowserEntry{"chromium": {Enabled: true}},
			TorExitCountry: country,
		})
		w := httptest.NewRecorder()
		h.GetConfig(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET /api/config = %d: %s", w.Code, w.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		v, ok := resp["tor_exit_country"].(string)
		return v, ok
	}

	if got, ok := fetch(t, "se,no"); !ok || got != "se,no" {
		t.Fatalf("tor_exit_country = %q (present=%v), want \"se,no\"", got, ok)
	}
	if _, ok := fetch(t, ""); ok {
		t.Fatal("tor_exit_country must be omitted when no default is configured")
	}
}
