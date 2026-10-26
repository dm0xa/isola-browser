package main

import "testing"

func TestNormalizeHeadersLowercasesAndDropsSetCookie(t *testing.T) {
	out := normalizeHeaders(map[string]string{
		"Server":     "nginx",
		"SET-COOKIE": "sid=secret",
		" X-Test ":   "ok",
	})
	if _, ok := out["set-cookie"]; ok {
		t.Fatal("set-cookie must never be stored in the headers map")
	}
	if out["server"] != "nginx" || out["x-test"] != "ok" {
		t.Fatalf("unexpected map: %v", out)
	}
}

func TestNormalizeHeadersCapsCountAndBytes(t *testing.T) {
	in := map[string]string{}
	for i := 0; i < netHeaderMaxCount+20; i++ {
		in["h"+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	if got := len(normalizeHeaders(in)); got > netHeaderMaxCount {
		t.Fatalf("header count = %d, want <= %d", got, netHeaderMaxCount)
	}

	big := map[string]string{"a": string(make([]byte, netHeaderMaxBytes+10))}
	if out := normalizeHeaders(big); out != nil {
		t.Fatalf("oversized single header must be dropped, got %d bytes", len(out["a"]))
	}
}

func TestMergeHeaderTextParsesRawBlock(t *testing.T) {
	out := mergeHeaderText(nil, "HTTP/2 200\r\ncontent-type: text/html\r\nserver: nginx\r\nset-cookie: sid=secret\r\n")
	if out["content-type"] != "text/html" || out["server"] != "nginx" {
		t.Fatalf("unexpected map: %v", out)
	}
	if _, ok := out["set-cookie"]; ok {
		t.Fatal("set-cookie must be dropped from the raw block too")
	}
	if _, ok := out["http/2 200"]; ok {
		t.Fatal("the status line must be skipped")
	}
}

func TestMergeHeaderTextJoinsDuplicates(t *testing.T) {
	out := mergeHeaderText(nil, "vary: Accept\r\nvary: Accept-Encoding\r\n")
	if out["vary"] != "Accept, Accept-Encoding" {
		t.Fatalf("vary = %q", out["vary"])
	}
}

func TestHeadersWanted(t *testing.T) {
	for _, typ := range []string{"Document", "XHR", "Fetch", "Script", "Stylesheet"} {
		if !headersWanted(typ) {
			t.Errorf("%s must keep headers", typ)
		}
	}
	for _, typ := range []string{"Image", "Font", "Media", ""} {
		if headersWanted(typ) {
			t.Errorf("%s must not keep headers", typ)
		}
	}
}

func TestHostOfURL(t *testing.T) {
	if got := hostOfURL("https://Example.COM:8443/a"); got != "example.com" {
		t.Fatalf("host = %q", got)
	}
	if got := hostOfURL("data:text/plain,hi"); got != "" {
		t.Fatalf("data URL host = %q", got)
	}
}
