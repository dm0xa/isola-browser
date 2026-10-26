package mcp

import (
	"encoding/json"
	"net/url"
	"strings"
)

type networkEntry struct {
	URL    string `json:"url"`
	Method string `json:"method"`
	Status int    `json:"status"`
	Type   string `json:"type"`
}

// filterNetworkBody keeps the original entry JSON and only decides membership.
// It used to re-marshal a four-field struct, which silently stripped every
// field added later (ip, bytes, headers, redirects, tracker labels) whenever a
// caller passed type/host/limit.
func filterNetworkBody(body []byte, entryType, hostContains string, limit int) ([]byte, error) {
	var parsed struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, err
	}
	filtered := make([]json.RawMessage, 0, len(parsed.Entries))
	for _, raw := range parsed.Entries {
		var e networkEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			continue
		}
		if entryType != "" && !strings.EqualFold(e.Type, entryType) {
			continue
		}
		if hostContains != "" && !hostMatches(e.URL, hostContains) {
			continue
		}
		filtered = append(filtered, raw)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	out, err := json.Marshal(map[string]any{"entries": filtered})
	if err != nil {
		return body, err
	}
	return out, nil
}

func hostMatches(rawURL, hostContains string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return strings.Contains(strings.ToLower(rawURL), strings.ToLower(hostContains))
	}
	host := strings.ToLower(u.Host)
	needle := strings.ToLower(strings.TrimSpace(hostContains))
	return strings.Contains(host, needle)
}
