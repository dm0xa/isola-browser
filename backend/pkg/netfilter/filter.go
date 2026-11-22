// Package netfilter selects entries from a captured network log without
// re-encoding the values it keeps. The REST endpoint and the MCP tools read the
// same streamer document, so they share one implementation: `host_contains`
// must never mean "hostname" to one caller and "anywhere in the URL" to the
// other.
package netfilter

import (
	"encoding/json"
	"net/url"
	"strings"
)

// EntryMeta is the subset of a captured entry the filters look at. The entry
// itself travels as json.RawMessage so numbers and nested objects are never
// re-encoded.
type EntryMeta struct {
	URL    string `json:"url"`
	Method string `json:"method"`
	Status int    `json:"status"`
	Type   string `json:"type"`

	// Host is derived from URL, never decoded, and is what HostContains
	// matches. Empty when the URL cannot be parsed.
	Host string `json:"-"`
}

// FilterEntries selects entries while preserving their exact JSON. Callers may
// mutate `fields` to drop parts of an accepted entry.
func FilterEntries(body []byte, limit int, keep func(EntryMeta, map[string]json.RawMessage) bool) ([]byte, error) {
	var parsed struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, err
	}
	out := make([]json.RawMessage, 0, len(parsed.Entries))
	for _, raw := range parsed.Entries {
		var meta EntryMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			continue
		}
		meta.Host = HostOf(meta.URL)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			continue
		}
		if !keep(meta, fields) {
			continue
		}
		// Field values stay json.RawMessage, so re-encoding preserves every
		// value verbatim (numbers included); only the callback's deletions change.
		encoded, err := json.Marshal(fields)
		if err != nil {
			continue
		}
		out = append(out, encoded)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return json.Marshal(map[string]any{"entries": out, "count": len(out)})
}

// HostOf returns the lowercased hostname of a captured URL.
func HostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

// Needle normalizes a user-supplied substring filter. Matching is
// case-insensitive, so both sides are lowered once.
func Needle(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// MatchesHost implements host_contains: the entry's hostname contains want.
// Unparseable URLs fall back to a whole-URL test so a captured entry is never
// silently unreachable.
func MatchesHost(meta EntryMeta, want string) bool {
	if want == "" {
		return true
	}
	if meta.Host != "" {
		return strings.Contains(meta.Host, want)
	}
	return strings.Contains(strings.ToLower(meta.URL), want)
}

// MatchesURL implements url_contains: the full URL contains want, which is
// what a path, query or scheme filter needs.
func MatchesURL(meta EntryMeta, want string) bool {
	return want == "" || strings.Contains(strings.ToLower(meta.URL), want)
}
