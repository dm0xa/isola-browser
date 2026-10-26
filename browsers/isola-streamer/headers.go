package main

import (
	"net/url"
	"strings"
)

// Response headers: normalisation, caps and the TLS summary carried beside
// them. Headers are captured for every response but stored only for the
// resource types where they carry signal — see headersWanted.

const (
	// netHeaderMaxCount bounds one response's header map. A page can answer
	// with dozens of diagnostic headers; the cap keeps the dump predictable.
	netHeaderMaxCount = 48
	// netHeaderMaxBytes bounds the stored key+value bytes per response.
	netHeaderMaxBytes = 4096
	// netHeaderEntriesDefault bounds how many entries in the capture window keep
	// headers at all. Entries beyond it lose theirs (oldest first) so a
	// long-lived session cannot pin memory with headers alone.
	netHeaderEntriesDefault = 250
)

// netHeaderEntriesLimit is the effective bound; a variable so tests can shrink
// the window without allocating hundreds of entries.
var netHeaderEntriesLimit = netHeaderEntriesDefault

// headersWanted reports whether a resource type is worth storing headers for.
// Images, fonts and media carry no useful header signal beyond what the row
// already shows.
func headersWanted(typ string) bool {
	switch typ {
	case "Document", "XHR", "Fetch", "Script", "Stylesheet", "Manifest":
		return true
	default:
		return false
	}
}

// normalizeHeaders lowercases keys, enforces the count/size caps and drops
// Set-Cookie: the Cookies tab is the single owner of cookie data, and a second
// copy in the headers view would leak the same secret twice.
func normalizeHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	bytes := 0
	for k, v := range in {
		key := strings.ToLower(strings.TrimSpace(k))
		if key == "" || key == "set-cookie" {
			continue
		}
		if len(out) >= netHeaderMaxCount {
			break
		}
		v = strings.TrimSpace(v)
		if bytes+len(key)+len(v) > netHeaderMaxBytes {
			continue
		}
		out[key] = v
		bytes += len(key) + len(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergeHeaders folds new headers into an existing map without exceeding the
// caps. Later values win: ExtraInfo is the raw wire form and is authoritative
// over the filtered set Chromium exposes on responseReceived.
func mergeHeaders(dst, add map[string]string) map[string]string {
	if len(add) == 0 {
		return dst
	}
	if dst == nil {
		return normalizeHeaders(add)
	}
	bytes := 0
	for k, v := range dst {
		bytes += len(k) + len(v)
	}
	for k, v := range add {
		key := strings.ToLower(strings.TrimSpace(k))
		if key == "" || key == "set-cookie" {
			continue
		}
		v = strings.TrimSpace(v)
		if _, exists := dst[key]; !exists {
			if len(dst) >= netHeaderMaxCount {
				continue
			}
			if bytes+len(key)+len(v) > netHeaderMaxBytes {
				continue
			}
			bytes += len(key) + len(v)
		}
		dst[key] = v
	}
	return dst
}

// mergeHeaderText parses a raw header block (as delivered by
// Network.responseReceivedExtraInfo) into the map. The status line and blank
// lines are skipped, and duplicate keys are joined with ", ".
func mergeHeaderText(dst map[string]string, text string) map[string]string {
	if text == "" {
		return dst
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		i := strings.IndexByte(line, ':')
		if i <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:i]))
		if key == "" || key == "set-cookie" || strings.HasPrefix(key, "http/") {
			continue
		}
		val := strings.TrimSpace(line[i+1:])
		if dst == nil {
			dst = map[string]string{}
		}
		if prev, ok := dst[key]; ok && prev != "" {
			if prev != val {
				dst[key] = prev + ", " + val
			}
			continue
		}
		dst[key] = val
	}
	if len(dst) == 0 {
		return nil
	}
	if len(dst) > netHeaderMaxCount {
		return normalizeHeaders(dst)
	}
	return dst
}

// hostOfURL extracts the hostname from a captured URL. Invalid URLs (data:,
// malformed) yield "".
func hostOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
