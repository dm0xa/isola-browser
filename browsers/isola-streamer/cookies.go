package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
)

// Cookie capture: taps Network.responseReceivedExtraInfo for Set-Cookie
// lines. Parsed attributes are broadcast on the session-owner input WS
// (same channel as network/console). Enabled when NET_MONITOR=1.

type cookieEntry struct {
	Domain   string `json:"d"`
	Name     string `json:"n"`
	Value    string `json:"v"`
	Path     string `json:"p"`
	SameSite string `json:"ss"`
	Expires  string `json:"exp"`
	MaxAge   int    `json:"ma"`
	HTTPOnly bool   `json:"h"`
	Secure   bool   `json:"s"`
}

var (
	cookieMu     sync.Mutex
	cookieBuffer []cookieEntry
)

const cookieMaxEntries = 200

func cookieTap(params json.RawMessage) {
	var p struct {
		Headers        map[string]string `json:"headers"`
		HeadersText    string            `json:"headersText"`
		BlockedCookies []struct {
			CookieLine string `json:"cookieLine"`
		} `json:"blockedCookies"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}

	var lines []string
	for k, v := range p.Headers {
		if strings.EqualFold(k, "set-cookie") {
			lines = append(lines, splitSetCookieHeader(v)...)
		}
	}
	lines = append(lines, setCookieLinesFromHeaderText(p.HeadersText)...)
	for _, bc := range p.BlockedCookies {
		if strings.TrimSpace(bc.CookieLine) != "" {
			lines = append(lines, bc.CookieLine)
		}
	}

	for _, line := range lines {
		entry, ok := parseSetCookieMeta(line)
		if !ok {
			continue
		}
		rememberCookie(entry)
	}
}

func rememberCookie(entry cookieEntry) {
	cookieMu.Lock()
	updated := false
	for i, existing := range cookieBuffer {
		if existing.Domain == entry.Domain && existing.Name == entry.Name {
			cookieBuffer[i] = entry
			updated = true
			break
		}
	}
	if !updated {
		cookieBuffer = append(cookieBuffer, entry)
		if len(cookieBuffer) > cookieMaxEntries {
			cookieBuffer = cookieBuffer[1:]
		}
	}
	cookieMu.Unlock()

	broadcastToInputWS(cookieWireMsg(entry))
}

func cookieWireMsg(entry cookieEntry) map[string]any {
	msg := map[string]any{
		"type": "cookie",
		"d":    entry.Domain,
		"n":    entry.Name,
		"h":    entry.HTTPOnly,
		"s":    entry.Secure,
	}
	if entry.Value != "" {
		msg["v"] = entry.Value
	}
	if entry.Path != "" {
		msg["p"] = entry.Path
	}
	if entry.SameSite != "" {
		msg["ss"] = entry.SameSite
	}
	if entry.Expires != "" {
		msg["exp"] = entry.Expires
	}
	if entry.MaxAge != 0 {
		msg["ma"] = entry.MaxAge
	}
	if fp := cookieFingerprint(entry.Name); fp != "" {
		// Product family label, computed here so the panel and the trackers
		// aggregate never keep two copies of the catalogue.
		msg["fp"] = fp
	}
	return msg
}

func parseSetCookieMeta(line string) (cookieEntry, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return cookieEntry{}, false
	}
	if i := strings.IndexByte(line, ':'); i >= 0 && strings.EqualFold(strings.TrimSpace(line[:i]), "Set-Cookie") {
		line = strings.TrimSpace(line[i+1:])
	}
	parts := strings.Split(line, ";")
	nv := strings.TrimSpace(parts[0])
	name, value, found := strings.Cut(nv, "=")
	name = strings.TrimSpace(name)
	if !found || name == "" {
		return cookieEntry{}, false
	}
	entry := cookieEntry{Name: name, Value: strings.TrimSpace(value)}
	for _, raw := range parts[1:] {
		attr := strings.TrimSpace(raw)
		if attr == "" {
			continue
		}
		key, val, hasVal := strings.Cut(attr, "=")
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "domain":
			if hasVal {
				entry.Domain = strings.TrimSpace(val)
			}
		case "path":
			if hasVal {
				entry.Path = strings.TrimSpace(val)
			}
		case "expires":
			if hasVal {
				entry.Expires = strings.TrimSpace(val)
			}
		case "max-age":
			if hasVal {
				if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
					entry.MaxAge = n
				}
			}
		case "samesite":
			if hasVal {
				entry.SameSite = strings.TrimSpace(val)
			} else {
				entry.SameSite = "Lax"
			}
		case "secure":
			entry.Secure = true
		case "httponly":
			entry.HTTPOnly = true
		}
	}
	return entry, true
}

func splitSetCookieHeader(v string) []string {
	v = strings.ReplaceAll(v, "\r\n", "\n")
	var out []string
	for _, part := range strings.Split(v, "\n") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func setCookieLinesFromHeaderText(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if len(line) >= 11 && strings.EqualFold(line[:11], "set-cookie:") {
			out = append(out, strings.TrimSpace(line[11:]))
		}
	}
	return out
}

// replayCookiesTo sends the cookie buffer to a newly attached viewer.
func replayCookiesTo(fn func(map[string]any)) {
	cookieMu.Lock()
	entries := make([]cookieEntry, len(cookieBuffer))
	copy(entries, cookieBuffer)
	cookieMu.Unlock()
	for _, e := range entries {
		fn(cookieWireMsg(e))
	}
}

// isCookieEventType filters CDP events for the cookie tap.
func isCookieEventType(method string) bool {
	return method == "Network.responseReceivedExtraInfo"
}
