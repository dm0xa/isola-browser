package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
)

// POST /identity — change the page-visible identity (IANA timezone, BCP-47
// locale) of a live session.
//
// The container env (ISOLA_SESSION_TZ / ISOLA_SESSION_LOCALE) is read once at
// boot, so a warm-pool container is baked with the pool's identity. The backend
// calls this right after a claim to give it the session's requested identity
// without a cold start. The same call also works on a running session, where
// the next page and every attached target pick the new values up immediately.
//
// Same trust boundary as /seed, /navigate and /resize: loopback-published, with
// the backend's session token as the authentication boundary.

// activeIdentity is the running identity client, published at startup so the
// route can reach every attached target.
var activeIdentity *identityClient

const identityMaxBodyBytes = 4 << 10

var (
	// Deliberately permissive: the backend validates with the tz database and
	// a BCP-47 parser; this only keeps obvious junk out of the CDP call.
	identityTimezoneRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+\-/]*$`)
	identityLocaleRe   = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
)

func handleIdentity(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, identityMaxBodyBytes+1))
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}
	if len(body) > identityMaxBodyBytes {
		http.Error(w, fmt.Sprintf("payload exceeds %d bytes", identityMaxBodyBytes), http.StatusRequestEntityTooLarge)
		return
	}
	var payload struct {
		Timezone string `json:"timezone"`
		Locale   string `json:"locale"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		http.Error(w, "invalid json: "+truncateForLog(err.Error()), http.StatusBadRequest)
		return
	}
	if dec.More() {
		http.Error(w, "invalid json: trailing data", http.StatusBadRequest)
		return
	}
	timezone := strings.TrimSpace(payload.Timezone)
	locale := strings.TrimSpace(payload.Locale)
	if timezone != "" && !identityTimezoneRe.MatchString(timezone) {
		http.Error(w, "invalid timezone", http.StatusBadRequest)
		return
	}
	if locale != "" && !identityLocaleRe.MatchString(locale) {
		http.Error(w, "invalid locale", http.StatusBadRequest)
		return
	}
	if activeIdentity == nil {
		http.Error(w, "identity unavailable", http.StatusServiceUnavailable)
		return
	}
	targets, err := activeIdentity.SetIdentity(timezone, locale)
	if err != nil {
		log.Printf("identity: set %q/%q: %v", timezone, locale, err)
		http.Error(w, truncateForLog(err.Error()), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"timezone": timezone,
		"locale":   locale,
		"targets":  targets,
	})
}
