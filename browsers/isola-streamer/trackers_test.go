package main

import "testing"

func TestRegistrableDomain(t *testing.T) {
	cases := map[string]string{
		"example.com":        "example.com",
		"www.example.com":    "example.com",
		"a.b.example.com":    "example.com",
		"shop.example.co.uk": "example.co.uk",
		"example.co.uk":      "example.co.uk",
		"cdn.example.com.se": "example.com.se",
		"EXAMPLE.COM.":       "example.com",
		"93.184.216.34":      "93.184.216.34",
		"2001:db8::1":        "2001:db8::1",
		"localhost":          "localhost",
		"":                   "",
	}
	for in, want := range cases {
		if got := registrableDomain(in); got != want {
			t.Errorf("registrableDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrackerForIsLabelAware(t *testing.T) {
	if got := trackerFor("www.google-analytics.com"); got != "Google Analytics" {
		t.Fatalf("google-analytics = %q", got)
	}
	if got := trackerFor("notsegment.com"); got != "" {
		t.Fatalf("notsegment.com must not match segment.com, got %q", got)
	}
	if got := trackerFor("cdn.segment.com"); got != "Segment" {
		t.Fatalf("cdn.segment.com = %q", got)
	}
}

func TestTrackerForURLMatchesPixelPaths(t *testing.T) {
	got := trackerForURL("https://www.facebook.com/tr?id=1&ev=PageView", "www.facebook.com")
	if got != "Meta Pixel" {
		t.Fatalf("facebook /tr = %q, want Meta Pixel", got)
	}
	if got := trackerForURL("https://www.facebook.com/some/page", "www.facebook.com"); got != "" {
		t.Fatalf("non-pixel facebook path matched %q", got)
	}
}

func TestCookieFingerprint(t *testing.T) {
	if got := cookieFingerprint("_ga"); got != "Google Analytics" {
		t.Fatalf("_ga = %q", got)
	}
	if got := cookieFingerprint("PHPSESSID"); got != "PHP" {
		t.Fatalf("PHPSESSID = %q", got)
	}
	if got := cookieFingerprint("unknown_cookie"); got != "" {
		t.Fatalf("unknown cookie matched %q", got)
	}
}

func TestIsBeaconType(t *testing.T) {
	for _, typ := range []string{"Ping", "ping", "Beacon"} {
		if !isBeaconType(typ) {
			t.Errorf("%q must be a beacon type", typ)
		}
	}
	if isBeaconType("Document") {
		t.Error("Document is not a beacon")
	}
}
