package main

import (
	"strconv"
	"strings"
)

// Security-header posture: a compact, deterministic assessment of the
// protections a document declares. It is computed once, in Go, because both
// the viewer panel and the MCP tools report it — two implementations would
// drift. Nothing here sends a request: it only reads response headers already
// captured over CDP.

const postureMaxValue = 200

// postureCheck is one header family in the report.
type postureCheck struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
	Value   string `json:"value,omitempty"`
	Note    string `json:"note,omitempty"`
}

// postureReport is the assessment of one document response.
type postureReport struct {
	Score int    `json:"score"`
	Total int    `json:"total"`
	Grade string `json:"grade"`
	// TLSState is the CDP securityState of the response: "secure", "info",
	// "neutral", "insecure", "insecure-broken" or "unknown". Empty when the
	// capture predates the field.
	TLSState string `json:"tls_state,omitempty"`
	// Note explains a grade cap or a transport warning.
	Note   string         `json:"note,omitempty"`
	Checks []postureCheck `json:"checks"`
}

// CDP Network.SecurityState values. Chromium launched with
// --ignore-certificate-errors reports "insecure" for a chain it accepted but
// did not trust, which is exactly what the headers alone cannot show.
const (
	tlsStateSecure         = "secure"
	tlsStateInfo           = "info"
	tlsStateNeutral        = "neutral"
	tlsStateInsecure       = "insecure"
	tlsStateInsecureBroken = "insecure-broken"
)

// postureTotal is the number of families assessed.
const postureTotal = 7

// assessPosture grades the seven families that matter for a visited page. Names
// are stable identifiers (MCP consumers key on them); notes are advisory.
//
// tlsState is the CDP securityState of the response. Header posture describes
// what a server declares; the transport decides whether those declarations came
// from that server at all. A response served without TLS, or over a chain
// Chromium only accepted because certificate errors were ignored, therefore
// caps the grade — otherwise a page rendered through an untrusted proxy could
// be reported as an A.
func assessPosture(h map[string]string, tlsState string) *postureReport {
	get := func(name string) string { return strings.TrimSpace(h[strings.ToLower(name)]) }

	checks := make([]postureCheck, 0, postureTotal)

	// 1. HSTS.
	hsts := get("strict-transport-security")
	check := postureCheck{Name: "hsts", Present: hsts != "", Value: clipHeaderValue(hsts)}
	if hsts != "" {
		if age, ok := hstsMaxAge(hsts); ok && age < 15552000 {
			check.Note = "max-age below 180 days"
		}
		if !strings.Contains(strings.ToLower(hsts), "includesubdomains") {
			if check.Note == "" {
				check.Note = "includeSubDomains absent"
			} else {
				check.Note += "; includeSubDomains absent"
			}
		}
	}
	checks = append(checks, check)

	// 2. CSP.
	csp := get("content-security-policy")
	check = postureCheck{Name: "csp", Present: csp != "", Value: clipHeaderValue(csp)}
	if csp != "" {
		lower := strings.ToLower(csp)
		var weak []string
		if strings.Contains(lower, "unsafe-inline") {
			weak = append(weak, "unsafe-inline")
		}
		if strings.Contains(lower, "unsafe-eval") {
			weak = append(weak, "unsafe-eval")
		}
		if len(weak) > 0 {
			check.Note = "allows " + strings.Join(weak, ", ")
		}
	}
	checks = append(checks, check)

	// 3. Frame protection: X-Frame-Options or a CSP frame-ancestors directive
	// are equivalent for this purpose — CSP is the stronger of the two.
	xfo := get("x-frame-options")
	frameAncestors := strings.Contains(strings.ToLower(csp), "frame-ancestors")
	check = postureCheck{Name: "frame", Present: xfo != "" || frameAncestors}
	switch {
	case frameAncestors:
		check.Value = "csp frame-ancestors"
	case xfo != "":
		check.Value = clipHeaderValue(xfo)
	}
	checks = append(checks, check)

	// 4. XCTO — only nosniff is the useful value.
	xcto := get("x-content-type-options")
	check = postureCheck{Name: "nosniff", Present: strings.EqualFold(xcto, "nosniff"), Value: clipHeaderValue(xcto)}
	if xcto != "" && !strings.EqualFold(xcto, "nosniff") {
		check.Note = "value is not nosniff"
	}
	checks = append(checks, check)

	// 5. Referrer-Policy.
	ref := get("referrer-policy")
	checks = append(checks, postureCheck{Name: "referrer", Present: ref != "", Value: clipHeaderValue(ref)})

	// 6. Permissions-Policy.
	perm := get("permissions-policy")
	checks = append(checks, postureCheck{Name: "permissions", Present: perm != "", Value: clipHeaderValue(perm)})

	// 7. Cross-origin isolation: either half is progress, both is the goal.
	coop := get("cross-origin-opener-policy")
	coep := get("cross-origin-embedder-policy")
	check = postureCheck{Name: "cross_origin", Present: coop != "" || coep != ""}
	switch {
	case coop != "" && coep != "":
		check.Value = "COOP + COEP"
	case coop != "":
		check.Value = "COOP"
	case coep != "":
		check.Value = "COEP"
		if check.Note == "" {
			check.Note = "COOP absent"
		}
	}
	checks = append(checks, check)

	score := 0
	for _, c := range checks {
		if c.Present {
			score++
		}
	}
	report := &postureReport{
		Score:  score,
		Total:  postureTotal,
		Grade:  postureGrade(score),
		Checks: checks,
	}
	applyTransportVerdict(report, tlsState)
	return report
}

// applyTransportVerdict records the transport state and caps the grade when the
// response was not served over a trusted connection. An unknown or absent state
// leaves the header-only grade untouched, so older captures keep their verdict.
func applyTransportVerdict(r *postureReport, state string) {
	state = strings.ToLower(strings.TrimSpace(state))
	switch state {
	case "":
		return
	case tlsStateInsecure, tlsStateInsecureBroken:
		r.Grade = capGrade(r.Grade, "C")
		r.Note = "certificate chain was not trusted (Chromium accepts certificate errors); grade capped"
	case tlsStateNeutral:
		r.Grade = capGrade(r.Grade, "C")
		r.Note = "response was not served over TLS; grade capped"
	case tlsStateInfo:
		r.Note = "TLS accepted with warnings"
	}
	r.TLSState = state
}

// postureGrade maps a score to a letter. The bands are deliberately blunt: the
// panel and agents need an at-a-glance verdict, not a decimal.
func postureGrade(score int) string {
	switch {
	case score >= 6:
		return "A"
	case score == 5:
		return "B"
	case score >= 3:
		return "C"
	case score >= 1:
		return "D"
	default:
		return "F"
	}
}

// capGrade returns g unless it is better than max, in which case it is lowered
// to max. A grade that is already worse than max is returned unchanged.
func capGrade(g, max string) string {
	if gradeRank(g) < gradeRank(max) {
		return max
	}
	return g
}

// gradeRank orders the letter grades; a lower number is a better grade.
func gradeRank(grade string) int {
	switch grade {
	case "A":
		return 0
	case "B":
		return 1
	case "C":
		return 2
	case "D":
		return 3
	default:
		return 4
	}
}

// hstsMaxAge extracts max-age from a Strict-Transport-Security value.
func hstsMaxAge(v string) (int64, bool) {
	for _, part := range strings.Split(v, ";") {
		key, val, ok := strings.Cut(part, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "max-age") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// clipHeaderValue keeps a header value short enough for the wire. The full
// value stays available in the headers view and the /network dump.
func clipHeaderValue(v string) string {
	if len(v) <= postureMaxValue {
		return v
	}
	return v[:postureMaxValue]
}
