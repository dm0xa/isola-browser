package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestIdentitySessionArgs pins the container env contract: a declared identity
// reaches the sandbox as TZ (container clock) plus the streamer's CDP inputs,
// and an undeclared one adds nothing, so the image's UTC / en-US are untouched.
func TestIdentitySessionArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		timezone string
		locale   string
		want     []string
	}{
		{name: "nothing declared yields no args", want: []string{}},
		{name: "blank values yield no args", timezone: "  ", locale: " ", want: []string{}},
		{
			name:     "both are passed",
			timezone: "Europe/Stockholm",
			locale:   "sv-SE",
			want: []string{
				"-e", "TZ=Europe/Stockholm",
				"-e", "ISOLA_SESSION_TZ=Europe/Stockholm",
				"-e", "ISOLA_SESSION_LOCALE=sv-SE",
			},
		},
		{
			name:     "timezone alone",
			timezone: "Asia/Tokyo",
			want:     []string{"-e", "TZ=Asia/Tokyo", "-e", "ISOLA_SESSION_TZ=Asia/Tokyo"},
		},
		{
			name:   "locale alone",
			locale: "en-GB",
			want:   []string{"-e", "ISOLA_SESSION_LOCALE=en-GB"},
		},
		{
			name:     "values are trimmed",
			timezone: " UTC ",
			locale:   " en-GB ",
			want: []string{
				"-e", "TZ=UTC",
				"-e", "ISOLA_SESSION_TZ=UTC",
				"-e", "ISOLA_SESSION_LOCALE=en-GB",
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := identitySessionArgs(tt.timezone, tt.locale)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("identitySessionArgs(%q, %q) = %v, want %v", tt.timezone, tt.locale, got, tt.want)
			}
		})
	}
}

func TestIdentityRequestURL(t *testing.T) {
	t.Parallel()
	if got := identityRequestURL("127.0.0.1", 5900); got != "http://127.0.0.1:5900/identity" {
		t.Fatalf("identityRequestURL = %q", got)
	}
}

func TestApplyIdentityPostsTheDeclaredValues(t *testing.T) {
	t.Parallel()

	var gotPath, gotMethod string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"timezone":"Europe/Stockholm","locale":"sv-SE","targets":1}`))
	}))
	defer srv.Close()

	host, port := seedHostPort(t, srv.URL)
	o, err := New("unix:///nonexistent/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := o.ApplyIdentity(ctx, host, port, "Europe/Stockholm", "sv-SE"); err != nil {
		t.Fatalf("ApplyIdentity: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/identity" {
		t.Fatalf("upstream = %s %s, want POST /identity", gotMethod, gotPath)
	}
	if gotBody["timezone"] != "Europe/Stockholm" || gotBody["locale"] != "sv-SE" {
		t.Fatalf("upstream body = %v, want the declared identity verbatim", gotBody)
	}
}

func TestApplyIdentitySurfacesUpstreamFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid timezone", http.StatusBadRequest)
	}))
	defer srv.Close()

	host, port := seedHostPort(t, srv.URL)
	o, err := New("unix:///nonexistent/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	err = o.ApplyIdentity(context.Background(), host, port, "Europe/Atlantis", "sv-SE")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v, want a status-bearing failure", err)
	}
	if err := o.ApplyIdentity(context.Background(), "", 0, "UTC", "en-US"); err == nil {
		t.Fatal("missing streamer address must be an error")
	}
}
