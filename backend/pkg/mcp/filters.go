package mcp

import (
	"encoding/json"
	"strings"
)

// Field-presence predicates for the tools that projection-filter the network
// log (get_headers, get_redirects). Entry selection itself lives in
// pkg/netfilter so the REST endpoint and these tools cannot drift.

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
