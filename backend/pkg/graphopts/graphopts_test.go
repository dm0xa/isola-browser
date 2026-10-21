package graphopts

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseDefaults(t *testing.T) {
	opts, err := Parse(url.Values{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Options{
		Theme: "dark", Direction: "horizontal", Expand: "origin",
		Caption: true, Scale: DefaultScale, Width: DefaultWidth, Height: DefaultHeight,
	}
	if opts.Theme != want.Theme || opts.Direction != want.Direction || opts.Expand != want.Expand {
		t.Fatalf("modes = %+v", opts)
	}
	if opts.Scale != want.Scale || opts.Width != want.Width || opts.Height != want.Height {
		t.Fatalf("numeric defaults = scale %d %dx%d", opts.Scale, opts.Width, opts.Height)
	}
	if !opts.Caption || opts.Failed {
		t.Fatalf("booleans = caption %v failed %v", opts.Caption, opts.Failed)
	}
}

func TestParseAcceptsOptions(t *testing.T) {
	q := url.Values{
		"theme":     {"light"},
		"direction": {"VERTICAL"},
		"expand":    {"all"},
		"type":      {"script, stylesheet"},
		"failed":    {"1"},
		"scale":     {"4"},
		"width":     {"8000"},
		"height":    {"320"},
		"origin":    {"example.com"},
		"caption":   {"0"},
	}
	opts, err := Parse(q)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if opts.Theme != "light" || opts.Direction != "vertical" || opts.Expand != "all" {
		t.Fatalf("modes = %+v", opts)
	}
	if strings.Join(opts.Types, ",") != "script,stylesheet" {
		t.Fatalf("types = %v", opts.Types)
	}
	if !opts.Failed || opts.Caption {
		t.Fatalf("booleans = failed %v caption %v", opts.Failed, opts.Caption)
	}
	if opts.Scale != 4 || opts.Width != 8000 || opts.Height != 320 || opts.Origin != "example.com" {
		t.Fatalf("values = %+v", opts)
	}
}

// TestParseRejects mirrors the streamer's validation wording: a rejection here
// must not reach the renderer, but the reason the client sees is the same.
func TestParseRejects(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{"theme=neon", "theme must be dark or light"},
		{"direction=up", "direction must be horizontal or vertical"},
		{"expand=some", "expand must be origin, all or collapsed"},
		{"type=beacon", `unknown resource type "beacon"`},
		{"failed=maybe", "failed must be 0 or 1"},
		{"scale=0", "scale must be 1-4"},
		{"scale=5", "scale must be 1-4"},
		{"scale=abc", "scale must be 1-4"},
		{"width=319", "width must be 320-8000"},
		{"height=8001", "height must be 320-8000"},
		{"caption=2", "caption must be 0 or 1"},
		{"origin=" + strings.Repeat("a", MaxOriginLen+1), "origin must be at most 255 characters"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(q); err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestQueryCanonicalAndRoundTrips(t *testing.T) {
	in := url.Values{
		"theme":  {"light"},
		"expand": {"all"},
		"type":   {"script,image"},
		"scale":  {"3"},
		"origin": {"example.com"},
		// Unknown parameters must not be forwarded.
		"bogus": {"1"},
	}
	opts, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out := opts.Query()
	if out.Get("bogus") != "" {
		t.Fatal("unknown parameter was forwarded")
	}
	if out.Get("theme") != "light" || out.Get("expand") != "all" || out.Get("scale") != "3" {
		t.Fatalf("canonical query = %s", out.Encode())
	}
	if out.Get("direction") != "horizontal" || out.Get("width") != "1400" || out.Get("height") != "900" {
		t.Fatalf("defaults not materialised: %s", out.Encode())
	}

	again, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse canonical query: %v", err)
	}
	if again.Theme != opts.Theme || again.Expand != opts.Expand || again.Scale != opts.Scale ||
		strings.Join(again.Types, ",") != strings.Join(opts.Types, ",") {
		t.Fatalf("round trip changed options: %+v -> %+v", opts, again)
	}
}
