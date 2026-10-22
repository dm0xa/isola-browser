package main

import (
	"bytes"
	"context"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func sampleEntries() []netDumpEntry {
	entries := []netDumpEntry{
		{URL: "https://duckduckgo.com/", Type: "Document", Status: 200},
	}
	for _, res := range []struct{ url, typ string }{
		{"https://duckduckgo.com/app.js", "Script"},
		{"https://duckduckgo.com/app.css", "Stylesheet"},
		{"https://duckduckgo.com/logo.png", "Image"},
	} {
		entries = append(entries, netDumpEntry{URL: res.url, Type: res.typ, Status: 200})
	}
	entries = append(entries,
		netDumpEntry{URL: "https://cdn.example.net/lib.js", Type: "Script", Status: 200},
		netDumpEntry{URL: "https://cdn.example.net/broken.woff2", Type: "Font", Status: 404, Failed: true},
	)
	return entries
}

func TestGraphModelAggregatesHostsAndOrigin(t *testing.T) {
	m := buildGraphModel(sampleEntries(), "", nil)

	if m.Origin != "duckduckgo.com" {
		t.Fatalf("origin = %q, want duckduckgo.com", m.Origin)
	}
	var hosts, resources, failed int
	for _, n := range m.Nodes {
		switch n.Kind {
		case "host":
			hosts++
		case "resource":
			resources++
			if n.Failed {
				failed++
			}
		case "root":
			if n.Count != len(sampleEntries()) {
				t.Fatalf("root count = %d, want %d", n.Count, len(sampleEntries()))
			}
		}
	}
	if hosts != 2 {
		t.Fatalf("hosts = %d, want 2", hosts)
	}
	if resources != 6 {
		t.Fatalf("resources = %d, want 6 (the document counts too)", resources)
	}
	if failed != 1 {
		t.Fatalf("failed = %d, want 1", failed)
	}
}

func TestGraphModelClassifiesResources(t *testing.T) {
	m := buildGraphModel(sampleEntries(), "", nil)
	classes := map[string]resClass{}
	for _, n := range m.Nodes {
		if n.Kind == "resource" {
			classes[n.Label] = n.Class
		}
	}
	if classes["app.js"] != classScript {
		t.Fatalf("app.js class = %q, want script", classes["app.js"])
	}
	if classes["app.css"] != classStylesheet {
		t.Fatalf("app.css class = %q, want stylesheet", classes["app.css"])
	}
	if classes["logo.png"] != classImage {
		t.Fatalf("logo.png class = %q, want image", classes["logo.png"])
	}
	if classes["broken.woff2"] != classFont {
		t.Fatalf("broken.woff2 class = %q, want font", classes["broken.woff2"])
	}
}

func TestExpandOriginShowsGroupsAndExpandAllListsResources(t *testing.T) {
	m := buildGraphModel(sampleEntries(), "", nil)

	summary := expandModel(m, expandOrigin)
	var groups, res int
	for _, n := range summary.Nodes {
		switch n.Kind {
		case "group":
			groups++
		case "resource":
			res++
		}
	}
	if groups == 0 {
		t.Fatal("origin expansion produced no type groups")
	}
	if res != 0 {
		t.Fatalf("origin expansion should keep groups collapsed, got %d resources", res)
	}

	full := expandModel(m, expandAll)
	groups, res = 0, 0
	for _, n := range full.Nodes {
		switch n.Kind {
		case "group":
			groups++
		case "resource":
			res++
		}
	}
	if groups == 0 || res != 6 {
		t.Fatalf("expand=all groups=%d resources=%d, want >0 and 6", groups, res)
	}

	collapsed := expandModel(m, expandCollapsed)
	for _, n := range collapsed.Nodes {
		if n.Kind == "group" || n.Kind == "resource" {
			t.Fatalf("collapsed graph leaks a %s node", n.Kind)
		}
	}
}

func TestLayoutKeepsNodesInsideTotals(t *testing.T) {
	for _, dir := range []layoutDirection{dirHorizontal, dirVertical} {
		m := expandModel(buildGraphModel(sampleEntries(), "", nil), expandAll)
		l := layoutModel(m, 900, 600, dir)
		if len(l.Nodes) == 0 {
			t.Fatalf("%s: empty layout", dir)
		}
		for _, n := range l.Nodes {
			if n.X < 0 || n.Y < 0 {
				t.Fatalf("%s: %s at negative position (%v,%v)", dir, n.Node.ID, n.X, n.Y)
			}
			if n.X+n.W > l.TotalW+0.01 || n.Y+n.H > l.TotalH+0.01 {
				t.Fatalf("%s: %s outside %.0fx%.0f at (%.0f,%.0f)+%.0fx%.0f",
					dir, n.Node.ID, l.TotalW, l.TotalH, n.X, n.Y, n.W, n.H)
			}
		}
	}
}

func TestRenderGraphPNGThemesDiffer(t *testing.T) {
	now := time.Date(2026, 10, 21, 10, 45, 0, 0, time.UTC)

	dark, err := renderGraphPNG(context.Background(), sampleEntries(), graphOptions{
		Theme: "dark", Direction: dirHorizontal, Expand: expandAll,
		Scale: 1, Width: 900, Height: 600, Caption: true,
	}, now)
	if err != nil {
		t.Fatalf("dark render: %v", err)
	}
	light, err := renderGraphPNG(context.Background(), sampleEntries(), graphOptions{
		Theme: "light", Direction: dirHorizontal, Expand: expandAll,
		Scale: 1, Width: 900, Height: 600, Caption: true,
	}, now)
	if err != nil {
		t.Fatalf("light render: %v", err)
	}

	if dark.Bounds().Dx() <= 0 || dark.Bounds().Dy() <= 0 {
		t.Fatal("dark render has empty bounds")
	}
	if dark.Bounds() != light.Bounds() {
		t.Fatalf("theme changed the frame: %v vs %v", dark.Bounds(), light.Bounds())
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, dark); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	if buf.Len() < 1000 {
		t.Fatalf("encoded png is suspiciously small (%d bytes)", buf.Len())
	}
}

func TestRenderGraphHonoursTypeFilterAndFailedOnly(t *testing.T) {
	opts := graphOptions{
		Theme: "dark", Direction: dirHorizontal, Expand: expandAll,
		Scale: 1, Width: 900, Height: 600,
		Types: []resClass{classFont}, FailedOnly: true,
	}
	m := buildGraphModel(sampleEntries(), "", opts.Types)
	if got := graphStatsOf(m); got.Resources != 1 || got.Failed != 1 {
		t.Fatalf("stats = %+v, want one failing font resource", got)
	}
	img, err := renderGraphPNG(context.Background(), sampleEntries(), opts, time.Now())
	if err != nil {
		t.Fatalf("filtered render: %v", err)
	}
	if img.Bounds().Dx() <= 0 {
		t.Fatal("filtered render has empty bounds")
	}
}

func TestParseGraphOptions(t *testing.T) {
	defaults, err := parseGraphOptions(map[string][]string{})
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if defaults.Theme != "dark" || defaults.Scale != 2 || !defaults.Caption || defaults.Expand != expandOrigin {
		t.Fatalf("defaults = %+v", defaults)
	}

	opts, err := parseGraphOptions(map[string][]string{
		"theme":     {"light"},
		"direction": {"vertical"},
		"expand":    {"all"},
		"type":      {"script, font"},
		"failed":    {"1"},
		"scale":     {"3"},
		"caption":   {"0"},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if opts.Theme != "light" || opts.Direction != dirVertical || opts.Expand != expandAll {
		t.Fatalf("parsed = %+v", opts)
	}
	if len(opts.Types) != 2 || opts.Types[0] != classScript || opts.Types[1] != classFont {
		t.Fatalf("types = %v", opts.Types)
	}
	if !opts.FailedOnly || opts.Scale != 3 || opts.Caption {
		t.Fatalf("parsed flags = %+v", opts)
	}

	bad := []map[string][]string{
		{"theme": {"neon"}},
		{"direction": {"diagonal"}},
		{"expand": {"everything"}},
		{"type": {"banana"}},
		{"scale": {"9"}},
		{"caption": {"maybe"}},
	}
	for _, q := range bad {
		if _, err := parseGraphOptions(q); err == nil {
			t.Fatalf("expected an error for %v", q)
		}
	}
}

func TestGraphHandlerServesPNGAndValidates(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/graph.png?theme=light&expand=all&scale=1", nil)
	rec := httptest.NewRecorder()
	graphHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content type = %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control = %q", rec.Header().Get("Cache-Control"))
	}
	img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	if img.Bounds().Dx() <= 0 || img.Bounds().Dy() <= 0 {
		t.Fatal("empty image")
	}

	bad := httptest.NewRecorder()
	graphHandler(bad, httptest.NewRequest(http.MethodGet, "/graph.png?theme=neon", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad theme status = %d", bad.Code)
	}

	post := httptest.NewRecorder()
	graphHandler(post, httptest.NewRequest(http.MethodPost, "/graph.png", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", post.Code)
	}
}

// TestExpandGroupCappedAtPreview: expand=all is a summary, not a dump. An
// uncapped group made the canvas (and the render) grow with the capture.
func TestExpandGroupCappedAtPreview(t *testing.T) {
	entries := []netDumpEntry{{URL: "https://example.com/", Type: "Document"}}
	for i := 0; i < goGroupPreview+8; i++ {
		entries = append(entries, netDumpEntry{
			URL:  "https://cdn.example.com/asset-" + strconv.Itoa(i) + ".js",
			Type: "Script",
		})
	}
	full := expandModel(buildGraphModel(entries, "", nil), expandAll)

	resources, more := 0, 0
	for _, n := range full.Nodes {
		switch n.Kind {
		case "resource":
			if n.Class == classScript {
				resources++
			}
		case "more":
			more++
			if n.Count != 8 {
				t.Fatalf("more row count = %d, want 8", n.Count)
			}
		}
	}
	if resources != goGroupPreview {
		t.Fatalf("expanded script resources = %d, want %d", resources, goGroupPreview)
	}
	if more != 1 {
		t.Fatalf("more rows = %d, want 1", more)
	}
}

// TestRenderGraphFitsRequestedFrame: width/height bound the raster, so a large
// capture cannot allocate an arbitrarily large image.
func TestRenderGraphFitsRequestedFrame(t *testing.T) {
	entries := []netDumpEntry{{URL: "https://example.com/", Type: "Document"}}
	for i := 0; i < 120; i++ {
		entries = append(entries, netDumpEntry{
			URL:  "https://host" + strconv.Itoa(i%12) + ".example.com/file-" + strconv.Itoa(i) + ".js",
			Type: "Script",
		})
	}
	opts := graphOptions{
		Theme: "dark", Direction: dirHorizontal, Expand: expandAll,
		Scale: 4, Width: 640, Height: 480, Caption: true,
	}
	img, err := renderGraphPNG(context.Background(), entries, opts, time.Now())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	b := img.Bounds()
	if float64(b.Dx()) > opts.Width*opts.Scale+1 || float64(b.Dy()) > opts.Height*opts.Scale+1 {
		t.Fatalf("image %v exceeds the requested %gx%g frame at scale %g", b, opts.Width, opts.Height, opts.Scale)
	}
	if b.Dx()*b.Dy() > 24_000_000 {
		t.Fatalf("image %v exceeds the pixel budget", b)
	}
}

func TestRenderGraphStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := renderGraphPNG(ctx, sampleEntries(), graphOptions{
		Theme: "dark", Direction: dirHorizontal, Expand: expandAll,
		Scale: 1, Width: 900, Height: 600,
	}, time.Now())
	if err == nil {
		t.Fatal("cancelled render returned nil error")
	}
}

func TestOriginOfPrefersDocumentHost(t *testing.T) {
	entries := []netDumpEntry{
		{URL: "https://cdn.example.net/lib.js", Type: "Script"},
		{URL: "https://page.example.se/", Type: "Document"},
	}
	if got := originOf(entries); got != "page.example.se" {
		t.Fatalf("originOf = %q, want page.example.se", got)
	}
}

// A real capture interleaves third-party scripts with the page's own polls.
// The trigger scan used to attribute the page to the last ad host, closing a
// loop that parked the ad host at depth 0 and pushed the opened page right.
func TestLayoutKeepsOriginFirst(t *testing.T) {
	entries := []netDumpEntry{
		{URL: "https://www.unibet.se/", Type: "Document"},
		{URL: "https://www.unibet.se/app.js", Type: "Script"},
		{URL: "https://ads.googlesyndication.example/tag.js", Type: "Script"},
		{URL: "https://connect.facebook.example/sdk.js", Type: "Script"},
		{URL: "https://lo.liveperson.example/lp.js", Type: "Script"},
		{URL: "https://www.unibet.se/api/poll", Type: "XHR"},
	}
	m := buildGraphModel(entries, "www.unibet.se", nil)
	for _, e := range m.Edges {
		if e.To == "host:www.unibet.se" && e.From != rootNodeID {
			t.Fatalf("%s -> %s: inferred trigger edge back into the opened page", e.From, e.To)
		}
	}

	lay := layoutModel(m, 1280, 720, dirHorizontal)
	origin, ok := layoutNodeByID(lay.Nodes, "host:www.unibet.se")
	if !ok {
		t.Fatal("origin node missing from the layout")
	}
	if origin.Depth != 1 {
		t.Fatalf("origin depth = %d, want 1", origin.Depth)
	}
	for _, n := range lay.Nodes {
		if n.Node.Kind != "host" || n.Node.ID == origin.Node.ID {
			continue
		}
		if n.X <= origin.X {
			t.Fatalf("%s at x=%.0f is not right of the origin at x=%.0f", n.Node.ID, n.X, origin.X)
		}
	}
}

// Hosts whose trigger chain loops (a's script → b, b's script → a) used to
// settle on depth 0 and appear in the entry column, left of the opened page.
func TestLayoutCondensesTriggerCycles(t *testing.T) {
	entries := []netDumpEntry{
		{URL: "https://example.com/", Type: "Document"},
		{URL: "https://example.com/app.js", Type: "Script"},
		{URL: "https://a.tracker.example/t.js", Type: "Script"},
		{URL: "https://b.tracker.example/b.js", Type: "Script"},
		{URL: "https://example.com/poll", Type: "XHR"},
		{URL: "https://a.tracker.example/t2.js", Type: "Script"},
	}
	m := buildGraphModel(entries, "example.com", nil)
	lay := layoutModel(m, 1280, 720, dirHorizontal)
	origin, ok := layoutNodeByID(lay.Nodes, "host:example.com")
	if !ok {
		t.Fatal("origin node missing from the layout")
	}
	if origin.Depth != 1 {
		t.Fatalf("origin depth = %d, want 1", origin.Depth)
	}
	for _, n := range lay.Nodes {
		if n.Node.Kind == "root" || n.Node.ID == origin.Node.ID {
			continue
		}
		if n.Depth <= 1 {
			t.Fatalf("%s at depth %d, want > 1", n.Node.ID, n.Depth)
		}
		if n.X <= origin.X {
			t.Fatalf("%s at x=%.0f is not right of the origin at x=%.0f", n.Node.ID, n.X, origin.X)
		}
	}
}

func layoutNodeByID(nodes []layoutNode, id string) (layoutNode, bool) {
	for _, n := range nodes {
		if n.Node.ID == id {
			return n, true
		}
	}
	return layoutNode{}, false
}

// BenchmarkRenderGraphPNG measures the full build+layout+paint path. The
// per-shape rasteriser this replaced took seconds to minutes on the same input.
func BenchmarkRenderGraphPNG(b *testing.B) {
	entries := []netDumpEntry{{URL: "https://example.com/", Type: "Document"}}
	for i := 0; i < 200; i++ {
		entries = append(entries, netDumpEntry{
			URL:  "https://host" + strconv.Itoa(i%10) + ".example.com/file-" + strconv.Itoa(i) + ".js",
			Type: "Script",
		})
	}
	opts := graphOptions{
		Theme: "dark", Direction: dirHorizontal, Expand: expandAll,
		Scale: 2, Width: 1400, Height: 900, Caption: true,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := renderGraphPNG(context.Background(), entries, opts, time.Now()); err != nil {
			b.Fatal(err)
		}
	}
}
