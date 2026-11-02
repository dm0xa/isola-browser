package main

import (
	"fmt"
	"math"
	"net/url"
	"path"
	"sort"
	"strings"
)

// Resource graph, server side: the same summary-first model the viewer draws
// client-side (frontend/src/components/ResourceGraph.tsx), rendered here so an
// API or MCP caller can ask for a PNG without a browser. Node kinds, group ids,
// class names and layout constants are deliberately kept in step with the
// frontend — a change on one side is a change on both.

type resClass string

const (
	classDocument   resClass = "document"
	classScript     resClass = "script"
	classStylesheet resClass = "stylesheet"
	classImage      resClass = "image"
	classFont       resClass = "font"
	classXHR        resClass = "xhr"
	classMedia      resClass = "media"
	classOther      resClass = "other"
)

var classOrder = []resClass{
	classDocument, classScript, classStylesheet, classImage,
	classFont, classXHR, classMedia, classOther,
}

var classShort = map[resClass]string{
	classDocument: "DOC", classScript: "JS", classStylesheet: "CSS",
	classImage: "IMG", classFont: "FNT", classXHR: "XHR",
	classMedia: "MED", classOther: "MISC",
}

var className = map[resClass]string{
	classDocument: "Document", classScript: "Script", classStylesheet: "Stylesheet",
	classImage: "Image", classFont: "Font", classXHR: "XHR",
	classMedia: "Media", classOther: "Other",
}

func parseClass(s string) (resClass, bool) {
	for _, c := range classOrder {
		if string(c) == s || strings.ToLower(className[c]) == s {
			return c, true
		}
	}
	return "", false
}

// classifyResource mirrors frontend/src/lib/resourceType.ts.
func classifyResource(t string, f urlFacts) resClass {
	typ := strings.ToLower(strings.TrimSpace(t))
	if typ == "" {
		switch f.Ext {
		case "js", "mjs":
			return classScript
		case "css":
			return classStylesheet
		case "png", "jpg", "jpeg", "gif", "webp", "svg", "ico", "avif":
			return classImage
		case "woff", "woff2", "ttf", "otf", "eot":
			return classFont
		case "mp4", "webm", "ogg", "mp3", "wav", "flac":
			return classMedia
		}
		return classOther
	}
	switch {
	case strings.Contains(typ, "document") || typ == "text/html" ||
		strings.Contains(typ, "html") || strings.Contains(typ, "main_frame") ||
		strings.Contains(typ, "sub_frame"):
		return classDocument
	case strings.Contains(typ, "script") || strings.Contains(typ, "javascript"):
		return classScript
	case strings.Contains(typ, "stylesheet") || strings.Contains(typ, "css"):
		return classStylesheet
	case strings.Contains(typ, "image") || strings.Contains(typ, "img"):
		return classImage
	case strings.Contains(typ, "font") || strings.Contains(typ, "woff") || strings.Contains(typ, "ttf") || strings.Contains(typ, "otf"):
		return classFont
	case strings.Contains(typ, "media") || strings.Contains(typ, "video") || strings.Contains(typ, "audio"):
		return classMedia
	case strings.Contains(typ, "xhr") || strings.Contains(typ, "fetch") || strings.Contains(typ, "json") ||
		strings.Contains(typ, "websocket") || strings.Contains(typ, "ajax"):
		return classXHR
	}
	return classOther
}

var riskyTLDs = map[string]bool{
	"tk": true, "xyz": true, "top": true, "gq": true, "cf": true,
	"ml": true, "pw": true, "cc": true, "su": true,
}

// urlFacts is one parse of a request URL: everything the model needs to know
// about it. The frontend does the same in analyze(), so a URL is parsed once
// for its host, row label, type and warning flag instead of four times.
type urlFacts struct {
	Host    string
	Label   string
	Ext     string
	Flagged bool
}

func parseRequestURL(raw string) urlFacts {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		short := raw
		if len(short) > 60 {
			short = short[:60]
		}
		return urlFacts{Host: short, Label: short}
	}
	host := u.Hostname()
	label := host
	parts := strings.Split(u.Path, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" {
			label = parts[i]
			break
		}
	}
	facts := urlFacts{
		Host:  host,
		Label: label,
		Ext:   strings.ToLower(strings.TrimPrefix(path.Ext(u.Path), ".")),
	}
	// Warning flags: cleartext, none-standard port, throwaway TLD.
	facts.Flagged = u.Scheme == "http" ||
		(u.Port() != "" && u.Port() != "80" && u.Port() != "443")
	if labels := strings.Split(host, "."); len(labels) > 1 && riskyTLDs[labels[len(labels)-1]] {
		facts.Flagged = true
	}
	return facts
}

type graphNode struct {
	ID          string
	Kind        string // root | host | group | resource
	Label       string
	Host        string
	Class       resClass
	Count       int
	ResourceCnt int
	FailedCnt   int
	FlaggedCnt  int
	Failed      bool
	Flagged     bool
	TS          int64
	Origin      bool
	// Page marks a top-level page the capture visited (a main-frame document
	// host). Page cards sit in the first column in Seq (visit) order; every
	// other host hangs off the page that loaded it.
	Page bool
	Seq  int
}

type graphEdge struct {
	From   string
	To     string
	Direct bool
}

type graphModel struct {
	Nodes  []graphNode
	Edges  []graphEdge
	Origin string
}

const rootNodeID = "root:request"

// buildGraphModel aggregates the captured requests into hosts, type groups and
// resources. Port of buildGraph().
func buildGraphModel(entries []netDumpEntry, pinnedOrigin string, types []resClass) graphModel {
	nodes := map[string]*graphNode{}
	var order []string
	edges := map[string]*graphEdge{}
	var edgeOrder []string

	addEdge := func(from, to string, direct bool) {
		id := from + "->" + to
		if _, ok := edges[id]; ok {
			return
		}
		edges[id] = &graphEdge{From: from, To: to, Direct: direct}
		edgeOrder = append(edgeOrder, id)
	}

	// One parse per request: the same facts drive the host, the row label, the
	// type and the warning flag for every later pass.
	facts := make([]urlFacts, len(entries))
	classes := make([]resClass, len(entries))
	for i, e := range entries {
		facts[i] = parseRequestURL(e.URL)
		classes[i] = classifyResource(e.Type, facts[i])
	}

	// The origin is decided over the whole capture, never the filtered slice:
	// narrowing to one type must not make a CDN host look like the opened page.
	origin := strings.TrimSpace(pinnedOrigin)
	if origin == "" {
		origin = originOf(entries)
	}

	typeSet := map[resClass]bool{}
	for _, t := range types {
		typeSet[t] = true
	}
	var included []int
	for i := range entries {
		// A response first seen mid-navigation can lack a URL. It cannot be
		// placed — no host, no resource — and must not become a nameless card.
		if facts[i].Host == "" {
			continue
		}
		if len(typeSet) > 0 && !typeSet[classes[i]] {
			continue
		}
		included = append(included, i)
	}

	hostID := func(h string) string { return "host:" + h }
	resID := func(u string) string { return "res:" + u }

	for _, idx := range included {
		e := entries[idx]
		host := facts[idx].Host
		class := classes[idx]
		flagged := facts[idx].Flagged

		hid := hostID(host)
		h, ok := nodes[hid]
		if !ok {
			h = &graphNode{ID: hid, Kind: "host", Label: host, Host: host}
			nodes[hid] = h
			order = append(order, hid)
		}
		h.Count++
		if flagged {
			h.Flagged = true
		}
		if e.Failed {
			h.Failed = true
		}

		rid := resID(e.URL)
		if r, exists := nodes[rid]; exists {
			r.Count++
			if e.Failed && !r.Failed {
				r.Failed = true
				h.Failed = true
				h.FailedCnt++
			}
			if flagged && !r.Flagged {
				r.Flagged = true
				h.Flagged = true
				h.FlaggedCnt++
			}
			if e.TS > r.TS {
				r.TS = e.TS
			}
		} else {
			nodes[rid] = &graphNode{
				ID: rid, Kind: "resource", Label: facts[idx].Label, Host: host,
				Class: class, Count: 1, Failed: e.Failed, Flagged: flagged, TS: e.TS,
			}
			order = append(order, rid)
			h.ResourceCnt++
			if e.Failed {
				h.FailedCnt++
			}
			if flagged {
				h.FlaggedCnt++
			}
		}
		addEdge(hid, rid, true)
	}

	// Pages, in the order the capture first saw them. An entry's OriginHost is
	// the tab's main-frame document host at capture time; navigation replaces
	// the page, so this is visit order. A streamer that predates OriginHost
	// sends nothing, and the capture then has one page: the origin.
	pages := []string{}
	pageSeen := map[string]bool{}
	pageSeq := map[string]int{}
	for _, idx := range included {
		page := strings.TrimSpace(entries[idx].OriginHost)
		if page == "" || pageSeen[page] {
			continue
		}
		pageSeen[page] = true
		pageSeq[page] = len(pages)
		pages = append(pages, page)
	}
	if origin != "" && !pageSeen[origin] {
		pageSeen[origin] = true
		pageSeq[origin] = len(pages)
		pages = append(pages, origin)
	}

	// The opened page always gets a host card, even when its own requests have
	// already left the capture window — and so does every other page.
	for _, page := range pages {
		hid := hostID(page)
		n, ok := nodes[hid]
		if !ok {
			n = &graphNode{ID: hid, Kind: "host", Label: page, Host: page}
			nodes[hid] = n
			order = append(order, hid)
		}
		n.Page = true
		n.Seq = pageSeq[page]
		if page == origin {
			n.Origin = true
		}
	}

	if origin != "" {
		resources := 0
		failed, flagged := 0, 0
		for _, id := range order {
			if n := nodes[id]; n.Kind == "resource" {
				resources++
				if n.Failed {
					failed++
				}
				if n.Flagged {
					flagged++
				}
			}
		}
		nodes[rootNodeID] = &graphNode{
			ID: rootNodeID, Kind: "root", Label: "[request]",
			Count: len(included), ResourceCnt: resources,
			FailedCnt: failed, FlaggedCnt: flagged,
			Failed: failed > 0, Flagged: flagged > 0,
		}
		order = append(order, rootNodeID)
		for _, page := range pages {
			addEdge(rootNodeID, hostID(page), false)
		}
	}

	// Which page asked for each host, in capture order. Guessing a trigger
	// chain from capture order instead stitched separate navigations into one
	// fake chain: a page opened before the current one — or the search engine
	// that led to it — appeared to be loaded by the current page, and the
	// hosts formed cycles no honest layering could untangle. The streamer
	// already knows the page, so use it.
	pagesByHost := map[string][]string{}
	for _, idx := range included {
		page := strings.TrimSpace(entries[idx].OriginHost)
		if page == "" {
			continue
		}
		host := facts[idx].Host
		seen := false
		for _, p := range pagesByHost[host] {
			if p == page {
				seen = true
				break
			}
		}
		if !seen {
			pagesByHost[host] = append(pagesByHost[host], page)
		}
	}
	for _, id := range order {
		n := nodes[id]
		// A page is placed by the spine above; its own requests belong to it.
		if n.Kind != "host" || pageSeen[n.Host] {
			continue
		}
		parent := ""
		for _, p := range pagesByHost[n.Host] {
			if origin != "" && p == origin {
				parent = origin
				break
			}
		}
		if parent == "" {
			for _, p := range pagesByHost[n.Host] {
				if pageSeen[p] {
					parent = p
					break
				}
			}
		}
		if parent == "" {
			parent = origin
		}
		if parent == "" || hostID(parent) == n.ID {
			continue
		}
		// Solid: the page requesting this host is a captured fact, not an
		// inference from capture order.
		addEdge(hostID(parent), n.ID, true)
	}

	m := graphModel{Origin: origin}
	for _, id := range order {
		m.Nodes = append(m.Nodes, *nodes[id])
	}
	for _, id := range edgeOrder {
		m.Edges = append(m.Edges, *edges[id])
	}
	return m
}

type expandMode string

const (
	expandOrigin    expandMode = "origin"
	expandAll       expandMode = "all"
	expandCollapsed expandMode = "collapsed"
)

func parseExpand(s string) (expandMode, bool) {
	switch expandMode(strings.ToLower(strings.TrimSpace(s))) {
	case "", expandOrigin:
		return expandOrigin, true
	case expandAll:
		return expandAll, true
	case expandCollapsed:
		return expandCollapsed, true
	}
	return "", false
}

func groupID(host string, class resClass) string {
	return "grp:" + host + ":" + string(class)
}

// expandModel applies the summary-first disclosure: a collapsed host is one
// card, an expanded host shows one row per resource type, and an expanded type
// group lists every resource of that type.
func expandModel(m graphModel, mode expandMode) graphModel {
	hostOpen := map[string]bool{}
	out := graphModel{Origin: m.Origin}
	originHostID := ""
	if m.Origin != "" {
		originHostID = "host:" + m.Origin
	}
	for _, n := range m.Nodes {
		if n.Kind != "host" && n.Kind != "root" {
			continue
		}
		out.Nodes = append(out.Nodes, n)
		if mode == expandAll || (mode == expandOrigin && n.ID == originHostID) {
			hostOpen[n.ID] = true
		}
	}
	return expandInto(m, out, hostOpen, mode == expandAll)
}

// goGroupPreview is the number of resource rows an expanded type group lists
// before it collapses the rest into one "more" row. It mirrors GROUP_PREVIEW in
// frontend/src/components/ResourceGraph.tsx: the PNG is a summary, and an
// uncapped group made the canvas (and the render) grow with the capture.
const goGroupPreview = 12

// expandInto adds type groups for the open hosts and, when `resources` is set,
// up to goGroupPreview of their resources plus a "more" row. Edges follow the
// same shape the viewer builds: host→group and group→resource, never
// host→resource.
func expandInto(m graphModel, out graphModel, hostOpen map[string]bool, resources bool) graphModel {
	type hostResources struct {
		byClass map[resClass][]graphNode
	}
	agg := map[string]*hostResources{}
	for _, n := range m.Nodes {
		if n.Kind != "resource" {
			continue
		}
		a := agg[n.Host]
		if a == nil {
			a = &hostResources{byClass: map[resClass][]graphNode{}}
			agg[n.Host] = a
		}
		a.byClass[n.Class] = append(a.byClass[n.Class], n)
	}

	// Trigger edges between hosts survive; leaf edges are rebuilt through the
	// type groups so the columns read host → type → file.
	spine := make(map[string]bool, len(out.Nodes))
	for _, n := range out.Nodes {
		spine[n.ID] = n.Kind == "root" || n.Kind == "host"
	}
	for _, e := range m.Edges {
		if spine[e.From] && spine[e.To] {
			out.Edges = append(out.Edges, e)
		}
	}

	for host, a := range agg {
		hid := "host:" + host
		if !hostOpen[hid] {
			continue
		}
		for _, class := range classOrder {
			list := a.byClass[class]
			if len(list) == 0 {
				continue
			}
			gid := groupID(host, class)
			var failed, flagged int
			for _, n := range list {
				if n.Failed {
					failed++
				}
				if n.Flagged {
					flagged++
				}
			}
			out.Nodes = append(out.Nodes, graphNode{
				ID: gid, Kind: "group", Label: className[class], Host: host,
				Class: class, Count: len(list), ResourceCnt: len(list),
				FailedCnt: failed, FlaggedCnt: flagged,
				Failed: failed > 0, Flagged: flagged > 0,
			})
			out.Edges = append(out.Edges, graphEdge{From: hid, To: gid, Direct: true})
			if !resources {
				continue
			}
			sort.SliceStable(list, func(i, j int) bool {
				if list[i].Failed != list[j].Failed {
					return list[i].Failed
				}
				if list[i].Flagged != list[j].Flagged {
					return list[i].Flagged
				}
				if list[i].Count != list[j].Count {
					return list[i].Count > list[j].Count
				}
				if list[i].TS != list[j].TS {
					return list[i].TS > list[j].TS
				}
				return list[i].Label < list[j].Label
			})
			shown := len(list)
			if shown > goGroupPreview {
				shown = goGroupPreview
			}
			shownFailed, shownFlagged := 0, 0
			for _, r := range list[:shown] {
				if r.Failed {
					shownFailed++
				}
				if r.Flagged {
					shownFlagged++
				}
				out.Nodes = append(out.Nodes, r)
				out.Edges = append(out.Edges, graphEdge{From: gid, To: r.ID, Direct: true})
			}
			if remaining := len(list) - shown; remaining > 0 {
				mid := "more:" + gid
				out.Nodes = append(out.Nodes, graphNode{
					ID: mid, Kind: "more", Label: fmt.Sprintf("show all %d more", remaining), Host: host,
					Class: class, Count: remaining, ResourceCnt: remaining,
					FailedCnt: failed - shownFailed, FlaggedCnt: flagged - shownFlagged,
					Failed: failed > shownFailed, Flagged: flagged > shownFlagged,
				})
				out.Edges = append(out.Edges, graphEdge{From: gid, To: mid, Direct: true})
			}
		}
	}
	return out
}

// --- layout ---------------------------------------------------------------

const (
	hostH      = 40.0
	resH       = 30.0
	groupH     = 30.0
	hostBaseW  = 180.0
	resBaseW   = 200.0
	groupBaseW = 160.0
	rootW      = 190.0
	colGap     = 92.0
	subColGap  = 30.0
	rowGap     = 10.0
	chPx       = 6.8
)

func nodeSize(n graphNode) (w float64, h float64) {
	switch n.Kind {
	case "root":
		return rootW, hostH
	case "host":
		controls := 38.0 + 8
		if n.ResourceCnt > 0 {
			controls += 16
		}
		if n.FailedCnt > 0 {
			controls += 22
		}
		if n.FlaggedCnt > 0 {
			controls += 22
		}
		textW := 45 + math.Min(30, float64(len(n.Label)))*chPx + controls
		return math.Max(hostBaseW, math.Round(textW)), hostH
	case "group":
		controls := 38.0 + 16 + 8
		if n.FailedCnt > 0 {
			controls += 25
		}
		if n.FlaggedCnt > 0 {
			controls += 25
		}
		textW := 29 + math.Min(18, float64(len(n.Label)))*chPx + controls
		return math.Max(groupBaseW, math.Round(textW)), groupH
	default:
		cnt := 0.0
		if n.Count > 1 {
			cnt += 28
		}
		if n.Flagged {
			cnt += 16
		}
		if n.Failed {
			cnt += 16
		}
		textW := math.Min(26, float64(len(n.Label)))*chPx + 28 + 40 + cnt
		return math.Max(resBaseW, math.Round(textW)), resH
	}
}

var kindRank = map[string]int{"root": 0, "host": 1, "group": 2, "resource": 3, "more": 3}

type layoutNode struct {
	Node  graphNode
	X, Y  float64
	W, H  float64
	Depth int
}

type layoutEdge struct {
	From, To *layoutNode
	Direct   bool
}

type layoutColumn struct {
	Depth       int
	X, W        float64
	Top, Bottom float64
	Label       bool
}

type graphLayout struct {
	Nodes   []layoutNode
	Edges   []layoutEdge
	Columns []layoutColumn
	TotalW  float64
	TotalH  float64
}

// computeDepths is the graph's vocabulary, one band per kind: entry, pages,
// hosts, types, files. It is deliberately not edge distance — an expanded
// page's type groups and the other hosts it loaded are both children of the
// page, and letting them share a column made the groups read as children of
// whichever host sat next to them. Fixed bands also keep the graph steady while
// the capture grows: a new third-party host never pushes the type rows sideways.
func computeDepths(nodes []graphNode) map[string]int {
	hasGroups := false
	for _, n := range nodes {
		if n.Kind == "group" {
			hasGroups = true
			break
		}
	}
	depths := make(map[string]int, len(nodes))
	for _, n := range nodes {
		switch n.Kind {
		case "root":
			depths[n.ID] = 0
		case "host":
			// Pages are what the user asked for; every other host is a provider.
			if n.Page {
				depths[n.ID] = 1
			} else {
				depths[n.ID] = 2
			}
		case "group":
			depths[n.ID] = 3
		default:
			// A resource row sits inside an open type group. With grouping off
			// (a single-type facet) it is drawn directly under its host, so it
			// takes the next band instead of leaving the type band empty.
			if hasGroups {
				depths[n.ID] = 4
			} else {
				depths[n.ID] = 3
			}
		}
	}
	return depths
}

type layoutDirection string

const (
	dirHorizontal layoutDirection = "horizontal"
	dirVertical   layoutDirection = "vertical"
)

func parseDirection(s string) (layoutDirection, bool) {
	switch layoutDirection(strings.ToLower(strings.TrimSpace(s))) {
	case "", dirHorizontal:
		return dirHorizontal, true
	case dirVertical:
		return dirVertical, true
	}
	return "", false
}

// layoutModel is the port of layoutGraph(): depths become bands, long sibling
// runs wrap into sub-columns, and the whole thing is top-anchored so the entry
// point stays in view.
func layoutModel(m graphModel, width, height float64, dir layoutDirection) graphLayout {
	var out graphLayout
	if len(m.Nodes) == 0 {
		return out
	}
	vertical := dir == dirVertical
	// The flow axis and the cross axis swap with the direction; these keep the
	// band maths below free of branching.
	alongOf := func(w, h float64) float64 {
		if vertical {
			return h
		}
		return w
	}
	crossOf := func(w, h float64) float64 {
		if vertical {
			return w
		}
		return h
	}
	crossLimit := math.Max(300, crossOf(width*1.35, height*1.7))

	depths := computeDepths(m.Nodes)
	byDepth := map[int][]graphNode{}
	var depthKeys []int
	for _, n := range m.Nodes {
		d := depths[n.ID]
		if _, ok := byDepth[d]; !ok {
			depthKeys = append(depthKeys, d)
		}
		byDepth[d] = append(byDepth[d], n)
	}
	sort.Ints(depthKeys)

	type measured struct {
		n    graphNode
		w, h float64
	}
	type band struct {
		depth      int
		chunks     [][]measured
		chunkCross []float64
		thickness  float64
	}

	bands := make([]band, 0, len(depthKeys))
	for _, d := range depthKeys {
		list := byDepth[d]
		sort.SliceStable(list, func(i, j int) bool {
			if kindRank[list[i].Kind] != kindRank[list[j].Kind] {
				return kindRank[list[i].Kind] < kindRank[list[j].Kind]
			}
			// Pages keep the order the capture visited them; every other row
			// stays alphabetical so a growing capture does not reshuffle.
			if list[i].Page != list[j].Page {
				return list[i].Page
			}
			if list[i].Page && list[i].Seq != list[j].Seq {
				return list[i].Seq < list[j].Seq
			}
			if list[i].Host != list[j].Host {
				return list[i].Host < list[j].Host
			}
			return list[i].Count > list[j].Count
		})
		ms := make([]measured, len(list))
		thickness := 0.0
		for i, n := range list {
			w, h := nodeSize(n)
			ms[i] = measured{n: n, w: w, h: h}
			if size := alongOf(w, h); size > thickness {
				thickness = size
			}
		}
		var chunks [][]measured
		var current []measured
		currentCross := -rowGap
		for _, m := range ms {
			crossSize := crossOf(m.w, m.h)
			if len(current) > 0 && currentCross+rowGap+crossSize > crossLimit {
				chunks = append(chunks, current)
				current = nil
				currentCross = -rowGap
			}
			current = append(current, m)
			currentCross += rowGap + crossSize
		}
		if len(current) > 0 {
			chunks = append(chunks, current)
		}
		chunkCross := make([]float64, len(chunks))
		for ci, chunk := range chunks {
			sum := 0.0
			for i, m := range chunk {
				sum += crossOf(m.w, m.h)
				if i > 0 {
					sum += rowGap
				}
			}
			chunkCross[ci] = sum
		}
		bands = append(bands, band{depth: d, chunks: chunks, chunkCross: chunkCross, thickness: thickness})
	}

	maxNodeCross := 60.0
	for _, b := range bands {
		for _, c := range b.chunkCross {
			if c > maxNodeCross {
				maxNodeCross = c
			}
		}
	}
	crossTotal := maxNodeCross + 60

	cursor := 24.0
	offsets := make([]float64, len(bands))
	for i, b := range bands {
		offsets[i] = cursor
		sub := float64(len(b.chunks))
		if sub < 1 {
			sub = 1
		}
		cursor += b.thickness*sub + subColGap*(sub-1) + colGap
	}
	spanTotal := cursor - colGap + 24

	index := map[string]*layoutNode{}
	for bi, b := range bands {
		for ci, chunk := range b.chunks {
			chunkOffset := offsets[bi] + float64(ci)*(b.thickness+subColGap)
			crossPos := 30.0
			left, top := math.Inf(1), math.Inf(1)
			right, bottom := math.Inf(-1), math.Inf(-1)
			for _, m := range chunk {
				along := chunkOffset + (b.thickness-alongOf(m.w, m.h))/2
				x, y := along, crossPos
				if vertical {
					x, y = crossPos, along
				}
				ln := &layoutNode{Node: m.n, X: x, Y: y, W: m.w, H: m.h, Depth: b.depth}
				out.Nodes = append(out.Nodes, *ln)
				index[m.n.ID] = ln
				left = math.Min(left, x)
				top = math.Min(top, y)
				right = math.Max(right, x+m.w)
				bottom = math.Max(bottom, y+m.h)
				crossPos += crossOf(m.w, m.h) + rowGap
			}
			col := layoutColumn{Depth: b.depth, Label: ci == 0}
			if vertical {
				col.X, col.W = left, right-left
				col.Top, col.Bottom = chunkOffset, chunkOffset+b.thickness
			} else {
				col.X, col.W = chunkOffset, b.thickness
				col.Top, col.Bottom = top, bottom
			}
			out.Columns = append(out.Columns, col)
		}
	}

	for _, e := range m.Edges {
		from, to := index[e.From], index[e.To]
		if from == nil || to == nil {
			continue
		}
		out.Edges = append(out.Edges, layoutEdge{From: from, To: to, Direct: e.Direct})
	}

	if vertical {
		out.TotalW, out.TotalH = crossTotal, spanTotal
	} else {
		out.TotalW, out.TotalH = spanTotal, crossTotal
	}
	return out
}
