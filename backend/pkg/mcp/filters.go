package mcp

import (
	"encoding/json"
	"net/url"
	"strings"
)

// One filter implementation serves every tool that reads the captured network
// log. It used to live twice (get_network vs get_headers/get_redirects) with
// `host_contains` meaning "hostname" in one and "anywhere in the URL" in the
// other, which made the same argument mean different things to an agent.

// entryMeta is the subset of a captured entry the filters look at. The entry
// itself travels as json.RawMessage so numbers and nested objects are never
// re-encoded.
type entryMeta struct {
	URL    string `json:"url"`
	Method string `json:"method"`
	Status int    `json:"status"`
	Type   string `json:"type"`

	// Host is derived from URL, never decoded, and is what host_contains
	// matches. Empty when the URL cannot be parsed.
	Host string `json:"-"`
}

// filterEntries selects entries while preserving their exact JSON. Callers may
// mutate `fields` to drop parts of an accepted entry.
func filterEntries(body []byte, limit int, keep func(entryMeta, map[string]json.RawMessage) bool) ([]byte, error) {
	var parsed struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, err
	}
	out := make([]json.RawMessage, 0, len(parsed.Entries))
	for _, raw := range parsed.Entries {
		var meta entryMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			continue
		}
		meta.Host = hostOf(meta.URL)
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

// hostOf returns the lowercased hostname of a captured URL.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

// needle normalizes a user-supplied substring filter. Matching is
// case-insensitive, so both sides are lowered once.
func needle(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// matchesHost implements host_contains: the entry's hostname contains needle.
// Unparseable URLs fall back to a whole-URL test so a captured entry is never
// silently unreachable.
func matchesHost(meta entryMeta, want string) bool {
	if want == "" {
		return true
	}
	if meta.Host != "" {
		return strings.Contains(meta.Host, want)
	}
	return strings.Contains(strings.ToLower(meta.URL), want)
}

// matchesURL implements url_contains: the full URL contains needle, which is
// what a path, query or scheme filter needs.
func matchesURL(meta entryMeta, want string) bool {
	return want == "" || strings.Contains(strings.ToLower(meta.URL), want)
}

func hasNonEmptyObject(fields map[string]json.RawMessage, key string) bool {
	raw, ok := fields[key]
	if !ok {
		return false
	}
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "{}" && s != "null"
}

func hasNonEmptyArray(fields map[string]json.RawMessage, key string) bool {
	raw, ok := fields[key]
	if !ok {
		return false
	}
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "[]" && s != "null"
}
