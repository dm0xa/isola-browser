package main

import "testing"

func TestPostureFullSetScoresSeventhGradeA(t *testing.T) {
	h := map[string]string{
		"strict-transport-security":    "max-age=31536000; includeSubDomains",
		"content-security-policy":      "default-src 'self'; frame-ancestors 'none'",
		"x-content-type-options":       "nosniff",
		"referrer-policy":              "strict-origin-when-cross-origin",
		"permissions-policy":           "geolocation=()",
		"cross-origin-opener-policy":   "same-origin",
		"cross-origin-embedder-policy": "require-corp",
	}
	r := assessPosture(h, "secure")
	if r.Score != 7 || r.Total != 7 {
		t.Fatalf("score = %d/%d, want 7/7", r.Score, r.Total)
	}
	if r.Grade != "A" {
		t.Fatalf("grade = %q, want A", r.Grade)
	}
}

func TestPostureEmptyIsF(t *testing.T) {
	r := assessPosture(nil, "secure")
	if r.Score != 0 || r.Grade != "F" {
		t.Fatalf("score = %d grade = %q, want 0/F", r.Score, r.Grade)
	}
	if len(r.Checks) != postureTotal {
		t.Fatalf("checks = %d, want %d", len(r.Checks), postureTotal)
	}
	for _, c := range r.Checks {
		if c.Present {
			t.Fatalf("%s reported present with no headers", c.Name)
		}
	}
}

func TestPostureHSTSNotesShortMaxAgeAndMissingSubdomains(t *testing.T) {
	r := assessPosture(map[string]string{"strict-transport-security": "max-age=300"}, "secure")
	var hsts postureCheck
	for _, c := range r.Checks {
		if c.Name == "hsts" {
			hsts = c
		}
	}
	if !hsts.Present {
		t.Fatal("hsts must count as present")
	}
	if hsts.Note == "" {
		t.Fatal("short max-age and missing includeSubDomains must be noted")
	}
}

func TestPostureFrameAncestorsCountsWithoutXFO(t *testing.T) {
	r := assessPosture(map[string]string{
		"content-security-policy": "frame-ancestors 'self'",
	}, "secure")
	for _, c := range r.Checks {
		if c.Name == "frame" {
			if !c.Present {
				t.Fatal("CSP frame-ancestors must satisfy frame protection")
			}
			if c.Value != "csp frame-ancestors" {
				t.Fatalf("value = %q", c.Value)
			}
		}
	}
}

func TestPostureXCTORequiresNosniff(t *testing.T) {
	r := assessPosture(map[string]string{"x-content-type-options": "sniff"}, "secure")
	for _, c := range r.Checks {
		if c.Name == "nosniff" {
			if c.Present {
				t.Fatal("a non-nosniff XCTO must not count")
			}
			if c.Note == "" {
				t.Fatal("the wrong value must be explained")
			}
		}
	}
}

func TestPostureGradeBands(t *testing.T) {
	cases := map[int]string{7: "A", 6: "A", 5: "B", 4: "C", 3: "C", 2: "D", 1: "D", 0: "F"}
	for score, want := range cases {
		if got := postureGrade(score); got != want {
			t.Errorf("postureGrade(%d) = %q, want %q", score, got, want)
		}
	}
}

// fullHeaders is the set that scores 7/7, used to show what the transport
// verdict does to an otherwise perfect grade.
func fullHeaders() map[string]string {
	return map[string]string{
		"strict-transport-security":    "max-age=31536000; includeSubDomains",
		"content-security-policy":      "default-src 'self'; frame-ancestors 'none'",
		"x-content-type-options":       "nosniff",
		"referrer-policy":              "strict-origin-when-cross-origin",
		"permissions-policy":           "geolocation=()",
		"cross-origin-opener-policy":   "same-origin",
		"cross-origin-embedder-policy": "require-corp",
	}
}

func TestPostureUntrustedTLSStateCapsGrade(t *testing.T) {
	// Chromium was launched with --ignore-certificate-errors, so the response
	// arrived but its chain was never trusted. The headers may be genuine; we
	// cannot claim they are, so the grade is capped.
	r := assessPosture(fullHeaders(), tlsStateInsecure)
	if r.Score != 7 || r.Total != 7 {
		t.Fatalf("score = %d/%d, want 7/7", r.Score, r.Total)
	}
	if r.Grade != "C" {
		t.Fatalf("grade = %q, want C (capped from A)", r.Grade)
	}
	if r.TLSState != tlsStateInsecure {
		t.Fatalf("tls_state = %q, want %q", r.TLSState, tlsStateInsecure)
	}
	if r.Note == "" {
		t.Fatal("a capped grade must explain itself")
	}
}

func TestPostureBrokenTLSCapsGrade(t *testing.T) {
	r := assessPosture(fullHeaders(), tlsStateInsecureBroken)
	if r.Grade != "C" {
		t.Fatalf("grade = %q, want C", r.Grade)
	}
}

func TestPosturePlainHTTPCapsGrade(t *testing.T) {
	r := assessPosture(fullHeaders(), tlsStateNeutral)
	if r.Grade != "C" {
		t.Fatalf("grade = %q, want C for a non-TLS response", r.Grade)
	}
	if r.TLSState != tlsStateNeutral {
		t.Fatalf("tls_state = %q", r.TLSState)
	}
}

func TestPostureCapNeverRaisesAGrade(t *testing.T) {
	// 0/7 is F; an untrusted transport must not improve it to C.
	r := assessPosture(nil, tlsStateInsecure)
	if r.Grade != "F" {
		t.Fatalf("grade = %q, want F", r.Grade)
	}
}

func TestPostureTrustedAndUnknownKeepGrade(t *testing.T) {
	for _, state := range []string{tlsStateSecure, "", "unknown"} {
		r := assessPosture(fullHeaders(), state)
		if r.Grade != "A" {
			t.Errorf("state %q: grade = %q, want A", state, r.Grade)
		}
		if r.Note != "" {
			t.Errorf("state %q: unexpected note %q", state, r.Note)
		}
	}
}

func TestPostureInfoNotesWithoutCapping(t *testing.T) {
	r := assessPosture(fullHeaders(), tlsStateInfo)
	if r.Grade != "A" {
		t.Fatalf("grade = %q, want A (info is not a failure)", r.Grade)
	}
	if r.Note == "" || r.TLSState != tlsStateInfo {
		t.Fatalf("info must be surfaced: tls_state=%q note=%q", r.TLSState, r.Note)
	}
}
