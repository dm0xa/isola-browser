package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
)

func seedHostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func TestSeedRequestURL(t *testing.T) {
	t.Parallel()
	if got := seedRequestURL("127.0.0.1", 5900); got != "http://127.0.0.1:5900/seed" {
		t.Fatalf("seedRequestURL = %q", got)
	}
}

func TestApplySeedForwardsPayloadAndDecodesResult(t *testing.T) {
	t.Parallel()

	var gotPath, gotMethod string
	var gotSeed models.SessionSeed
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotSeed); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cookies":2,"headers":1,"storage":3,"reloaded":true}`))
	}))
	defer srv.Close()

	host, port := seedHostPort(t, srv.URL)
	o, err := New("unix:///nonexistent/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	seed := &models.SessionSeed{
		Cookies: []models.SeedCookie{{Name: "sid", Value: "abc", Domain: "example.com"}},
		Headers: []models.SeedHeader{{Name: "X-Api-Key", Value: "k"}},
		Storage: []models.SeedStorage{{Origin: "https://example.com", Items: map[string]string{"k": "v"}}},
		Reload:  true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := o.ApplySeed(ctx, host, port, seed)
	if err != nil {
		t.Fatalf("ApplySeed: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/seed" {
		t.Fatalf("upstream = %s %s, want POST /seed", gotMethod, gotPath)
	}
	if len(gotSeed.Cookies) != 1 || gotSeed.Cookies[0].Domain != "example.com" || !gotSeed.Reload {
		t.Fatalf("upstream seed = %+v, want the payload verbatim", gotSeed)
	}
	if res != (models.SeedResult{Cookies: 2, Headers: 1, Storage: 3, Reloaded: true}) {
		t.Fatalf("result = %+v", res)
	}
}

func TestApplySeedMapsUpstreamFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "page origin does not match: page is on https://a.example", http.StatusConflict)
	}))
	defer srv.Close()

	host, port := seedHostPort(t, srv.URL)
	o, err := New("unix:///nonexistent/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	_, err = o.ApplySeed(context.Background(), host, port, &models.SessionSeed{Reload: true})
	var seedErr *SeedError
	if !errors.As(err, &seedErr) {
		t.Fatalf("err = %v, want *SeedError", err)
	}
	if seedErr.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", seedErr.Status)
	}
	if seedErr.Message != "page origin does not match: page is on https://a.example" {
		t.Fatalf("message = %q", seedErr.Message)
	}
}

func TestApplySeedWithoutAddressIsConflict(t *testing.T) {
	t.Parallel()

	o, err := New("unix:///nonexistent/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	_, err = o.ApplySeed(context.Background(), "", 0, &models.SessionSeed{Reload: true})
	var seedErr *SeedError
	if !errors.As(err, &seedErr) || seedErr.Status != http.StatusConflict {
		t.Fatalf("err = %v, want a 409 SeedError", err)
	}
}
