package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/models"
)

// TestCreateSessionIdentity pins the timezone/locale contract on create: a
// malformed value is a 400 with a stable code, valid values are normalized and
// stored, and a per-request value overrides the deployment default
// (session.timezone / session.locale).
func TestCreateSessionIdentity(t *testing.T) {
	tests := []struct {
		name       string
		timezone   string
		locale     string
		cfgTZ      string
		cfgLocale  string
		wantStatus int
		wantCode   string
		wantTZ     string
		wantLocale string
	}{
		{name: "no identity and no defaults stays unset", wantStatus: http.StatusAccepted},
		{
			name:       "request values are normalized and stored",
			timezone:   "europe/stockholm",
			locale:     "sv-se",
			wantStatus: http.StatusAccepted,
			wantTZ:     "Europe/Stockholm",
			wantLocale: "sv-SE",
		},
		{
			name:       "deployment defaults apply when omitted",
			cfgTZ:      "Asia/Tokyo",
			cfgLocale:  "ja-JP",
			wantStatus: http.StatusAccepted,
			wantTZ:     "Asia/Tokyo",
			wantLocale: "ja-JP",
		},
		{
			name:       "request overrides deployment defaults",
			timezone:   "Europe/Berlin",
			locale:     "de-DE",
			cfgTZ:      "Asia/Tokyo",
			cfgLocale:  "ja-JP",
			wantStatus: http.StatusAccepted,
			wantTZ:     "Europe/Berlin",
			wantLocale: "de-DE",
		},
		{
			name:       "unknown timezone is rejected",
			timezone:   "Europe/Atlantis",
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_timezone",
		},
		{
			name:       "host-local timezone is rejected",
			timezone:   "Local",
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_timezone",
		},
		{
			name:       "malformed locale is rejected",
			locale:     "sv_SE",
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_locale",
		},
		{
			name:       "one-letter language subtag is rejected",
			locale:     "e-US",
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_locale",
		},
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
				SessionTimezone:      tt.cfgTZ,
				SessionLocale:        tt.cfgLocale,
			}
			h, st := newCapacityTestHandler(t, cfg)

			body, _ := json.Marshal(models.CreateSessionRequest{
				URL:      "https://example.com",
				Timezone: tt.timezone,
				Locale:   tt.locale,
			})
			req := httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body))
			w := httptest.NewRecorder()
			h.CreateSession(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantStatus != http.StatusAccepted {
				var resp models.ErrorResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("error body: %v (%s)", err, w.Body.String())
				}
				if resp.Error.Code != tt.wantCode {
					t.Fatalf("error code = %q, want %q", resp.Error.Code, tt.wantCode)
				}
				if !strings.Contains(strings.ToLower(resp.Error.Message), "timezone") &&
					!strings.Contains(strings.ToLower(resp.Error.Message), "locale") {
					t.Fatalf("error message = %q, want it to name the invalid field", resp.Error.Message)
				}
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
			sess, err := st.GetSession(context.Background(), id)
			if err != nil || sess == nil {
				t.Fatalf("session row missing (err=%v)", err)
			}
			if sess.Timezone != tt.wantTZ {
				t.Fatalf("stored Timezone = %q, want %q", sess.Timezone, tt.wantTZ)
			}
			if sess.Locale != tt.wantLocale {
				t.Fatalf("stored Locale = %q, want %q", sess.Locale, tt.wantLocale)
			}
		})
	}
}

// TestWarmIdentityMatches pins the admission gate: a warm container bakes its
// timezone/locale at creation, so a request that resolves to a different pair
// must cold-start rather than receive a container whose pages report the wrong
// zone and language.
func TestWarmIdentityMatches(t *testing.T) {
	tests := []struct {
		name                      string
		cfgTZ, cfgLocale, tz, loc string
		want                      bool
	}{
		{name: "both unset claims", want: true},
		{name: "same deployment default claims", cfgTZ: "Asia/Tokyo", cfgLocale: "ja-JP", tz: "Asia/Tokyo", loc: "ja-JP", want: true},
		{name: "different timezone cold starts", cfgTZ: "Asia/Tokyo", cfgLocale: "ja-JP", tz: "Europe/Berlin", loc: "ja-JP", want: false},
		{name: "different locale cold starts", cfgTZ: "Asia/Tokyo", cfgLocale: "ja-JP", tz: "Asia/Tokyo", loc: "de-DE", want: false},
		{name: "defaults versus unset cold starts", cfgTZ: "Asia/Tokyo", cfgLocale: "ja-JP", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := warmIdentityMatches(tt.cfgTZ, tt.cfgLocale, tt.tz, tt.loc); got != tt.want {
				t.Fatalf("warmIdentityMatches(%q, %q, %q, %q) = %v, want %v",
					tt.cfgTZ, tt.cfgLocale, tt.tz, tt.loc, got, tt.want)
			}
		})
	}
}
