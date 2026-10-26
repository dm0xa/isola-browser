package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeEntries(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var parsed struct {
		Entries []map[string]any `json:"entries"`
		Count   int              `json:"count"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode filtered body: %v (%s)", err, body)
	}
	if parsed.Count != len(parsed.Entries) {
		t.Fatalf("count = %d, entries = %d", parsed.Count, len(parsed.Entries))
	}
	return parsed.Entries
}

func urlsOf(entries []map[string]any) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		s, _ := e["url"].(string)
		out = append(out, s)
	}
	return out
}

func TestFilterEntriesHostContainsMatchesHostnameOnly(t *testing.T) {
	t.Parallel()
	body := []byte(`{"entries":[
		{"url":"https://www.example.com/","type":"Document","status":403,"connection_id":7},
		{"url":"https://evil.test/?ref=example.com","type":"Script","status":200},
		{"url":"https://cdn.other.example/x.js","type":"Script","status":200}
	]}`)

	out, err := filterEntries(body, 0, func(meta entryMeta, _ map[string]json.RawMessage) bool {
		return matchesHost(meta, needle("example.com"))
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := decodeEntries(t, out)
	if len(entries) != 1 || urlsOf(entries)[0] != "https://www.example.com/" {
		t.Fatalf("host_contains must match the hostname, not the whole URL: %v", urlsOf(entries))
	}
	// Field preservation is load-bearing: a filtered read must not become a
	// four-field projection of the entry.
	if _, ok := entries[0]["connection_id"]; !ok {
		t.Fatalf("filtered entry dropped fields: %v", entries[0])
	}
}

func TestFilterEntriesURLContainsMatchesPath(t *testing.T) {
	t.Parallel()
	body := []byte(`{"entries":[
		{"url":"https://www.example.com/","type":"Document","status":200},
		{"url":"https://evil.test/?ref=example.com","type":"Script","status":200}
	]}`)

	out, err := filterEntries(body, 0, func(meta entryMeta, _ map[string]json.RawMessage) bool {
		return matchesURL(meta, "example.com")
	})
	if err != nil {
		t.Fatal(err)
	}
	if entries := decodeEntries(t, out); len(entries) != 2 {
		t.Fatalf("url_contains must see the whole URL: %v", urlsOf(entries))
	}
}

func TestFilterEntriesTypeAndLimit(t *testing.T) {
	t.Parallel()
	body := []byte(`{"entries":[
		{"url":"https://example.com/","type":"Document","status":200},
		{"url":"https://example.com/a.js","type":"Script","status":200},
		{"url":"https://example.com/b.js","type":"Script","status":200}
	]}`)

	out, err := filterEntries(body, 1, func(meta entryMeta, _ map[string]json.RawMessage) bool {
		return strings.EqualFold(meta.Type, "Script")
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := decodeEntries(t, out)
	if len(entries) != 1 || urlsOf(entries)[0] != "https://example.com/a.js" {
		t.Fatalf("limit must apply after filtering: %v", urlsOf(entries))
	}
}

func TestFilterEntriesCallbackCanDropFields(t *testing.T) {
	t.Parallel()
	body := []byte(`{"entries":[{"url":"https://example.com/","type":"Document","tls":{"protocol":"TLS 1.3"}}]}`)

	out, err := filterEntries(body, 0, func(_ entryMeta, fields map[string]json.RawMessage) bool {
		delete(fields, "tls")
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := decodeEntries(t, out)
	if _, ok := entries[0]["tls"]; ok {
		t.Fatalf("callback deletion ignored: %v", entries[0])
	}
}

func TestHostOf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ url, want string }{
		{"https://WWW.Example.COM/a?b=1", "www.example.com"},
		{"http://93.184.216.34:8080/x", "93.184.216.34:8080"},
		{"not a url", ""},
		{"", ""},
	} {
		if got := hostOf(tc.url); got != tc.want {
			t.Fatalf("hostOf(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}
