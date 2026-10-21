package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Server-side rendering of the resource graph. The layout and the model come
// from graph.go; this file only paints it, so a headless caller (API or MCP)
// gets the same picture without a browser.

type graphTheme struct {
	Canvas      color.RGBA
	GridMinor   color.RGBA
	GridMajor   color.RGBA
	ColumnLine  color.RGBA
	ColumnLabel color.RGBA
	Card        color.RGBA
	CardRoot    color.RGBA
	CardFailed  color.RGBA
	CardFlagged color.RGBA
	Border      color.RGBA
	Label       color.RGBA
	Sublabel    color.RGBA
	Neutral     color.RGBA
	Badge       color.RGBA
	BadgeText   color.RGBA
	Accent      map[resClass]color.RGBA
}

func rgb(hex string) color.RGBA {
	var r, g, b uint8
	if _, err := fmt.Sscanf(strings.TrimPrefix(hex, "#"), "%02x%02x%02x", &r, &g, &b); err != nil {
		return color.RGBA{A: 255}
	}
	return color.RGBA{R: r, G: g, B: b, A: 255}
}

// blend composites src over dst at alpha a (0..1) — the class-tinted cards are
// translucent in the UI and must be flattened for a PNG.
func blend(dst, src color.RGBA, a float64) color.RGBA {
	mix := func(d, s uint8) uint8 {
		return uint8(math.Round(float64(d)*(1-a) + float64(s)*a))
	}
	return color.RGBA{R: mix(dst.R, src.R), G: mix(dst.G, src.G), B: mix(dst.B, src.B), A: 255}
}

var graphThemes = map[string]graphTheme{
	"dark": {
		Canvas:      rgb("#05070d"),
		GridMinor:   rgb("#0b111c"),
		GridMajor:   rgb("#111a29"),
		ColumnLine:  rgb("#1b2635"),
		ColumnLabel: rgb("#6b7c93"),
		Card:        rgb("#141d2b"),
		CardRoot:    rgb("#0e1a2a"),
		CardFailed:  rgb("#3c1014"),
		CardFlagged: rgb("#2c2413"),
		Border:      rgb("#2a3547"),
		Label:       rgb("#dbe4f0"),
		Sublabel:    rgb("#94a3b8"),
		Neutral:     rgb("#64748b"),
		Badge:       rgb("#0a111d"),
		BadgeText:   rgb("#c3cedd"),
		Accent: map[resClass]color.RGBA{
			classDocument:   rgb("#22d3ee"),
			classScript:     rgb("#a78bfa"),
			classStylesheet: rgb("#f472b6"),
			classImage:      rgb("#34d399"),
			classFont:       rgb("#fbbf24"),
			classXHR:        rgb("#60a5fa"),
			classMedia:      rgb("#fb923c"),
			classOther:      rgb("#94a3b8"),
		},
	},
	"light": {
		Canvas:      rgb("#f4f4f5"),
		GridMinor:   rgb("#eef0f3"),
		GridMajor:   rgb("#e6e9ee"),
		ColumnLine:  rgb("#d7dce3"),
		ColumnLabel: rgb("#94a3b8"),
		Card:        rgb("#ffffff"),
		CardRoot:    rgb("#ffffff"),
		CardFailed:  rgb("#fef2f2"),
		CardFlagged: rgb("#fffbeb"),
		Border:      rgb("#d8dee7"),
		Label:       rgb("#1e293b"),
		Sublabel:    rgb("#475569"),
		Neutral:     rgb("#64748b"),
		Badge:       rgb("#eef2f6"),
		BadgeText:   rgb("#334155"),
		Accent: map[resClass]color.RGBA{
			classDocument:   rgb("#0891b2"),
			classScript:     rgb("#7c3aed"),
			classStylesheet: rgb("#db2777"),
			classImage:      rgb("#059669"),
			classFont:       rgb("#b45309"),
			classXHR:        rgb("#2563eb"),
			classMedia:      rgb("#c2410c"),
			classOther:      rgb("#64748b"),
		},
	},
}

// --- fonts ----------------------------------------------------------------

var (
	fontOnce   sync.Once
	fontParsed map[string]*opentype.Font
	fontFaces  = map[string]font.Face{}
)

func loadFonts() {
	fontOnce.Do(func() {
		fontParsed = map[string]*opentype.Font{}
		for name, data := range map[string][]byte{
			"regular": goregular.TTF,
			"bold":    gobold.TTF,
			"mono":    gomono.TTF,
			"monob":   gomonobold.TTF,
		} {
			f, err := opentype.Parse(data)
			if err != nil {
				continue
			}
			fontParsed[name] = f
		}
	})
}

func face(kind string, size float64) font.Face {
	loadFonts()
	px := math.Max(6, math.Round(size))
	key := fmt.Sprintf("%s:%.0f", kind, px)
	if f, ok := fontFaces[key]; ok {
		return f
	}
	parsed := fontParsed[kind]
	if parsed == nil {
		parsed = fontParsed["regular"]
	}
	f, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: px, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		f, _ = opentype.NewFace(fontParsed["regular"], &opentype.FaceOptions{Size: px, DPI: 72})
	}
	fontFaces[key] = f
	return f
}

func textWidth(f font.Face, s string) float64 {
	return float64(font.MeasureString(f, s)) / 64
}

// fitText truncates to the widest prefix that fits, with an ellipsis.
func fitText(f font.Face, s string, maxW float64) string {
	if maxW <= 0 || textWidth(f, s) <= maxW {
		return s
	}
	const ellipsis = "…"
	runes := []rune(s)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if textWidth(f, string(runes[:mid])+ellipsis) <= maxW {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo == 0 {
		return ellipsis
	}
	return string(runes[:lo]) + ellipsis
}

// --- canvas ---------------------------------------------------------------

type gCanvas struct {
	img   *image.RGBA
	scale float64
}

func (c *gCanvas) px(v float64) float64 { return v * c.scale }

// textW is a face's rendered width in logical (layout) units.
func (c *gCanvas) textW(f font.Face, s string) float64 { return textWidth(f, s) / c.scale }

const kappa = 0.5522847498

func roundRectPath(z *vector.Rasterizer, x, y, w, h, r float64) {
	if r <= 0.01 {
		z.MoveTo(float32(x), float32(y))
		z.LineTo(float32(x+w), float32(y))
		z.LineTo(float32(x+w), float32(y+h))
		z.LineTo(float32(x), float32(y+h))
		z.ClosePath()
		return
	}
	k := r * kappa
	z.MoveTo(float32(x+r), float32(y))
	z.LineTo(float32(x+w-r), float32(y))
	z.CubeTo(float32(x+w-r+k), float32(y), float32(x+w), float32(y+r-k), float32(x+w), float32(y+r))
	z.LineTo(float32(x+w), float32(y+h-r))
	z.CubeTo(float32(x+w), float32(y+h-r+k), float32(x+w-r+k), float32(y+h), float32(x+w-r), float32(y+h))
	z.LineTo(float32(x+r), float32(y+h))
	z.CubeTo(float32(x+r-k), float32(y+h), float32(x), float32(y+h-r+k), float32(x), float32(y+h-r))
	z.LineTo(float32(x), float32(y+r))
	z.CubeTo(float32(x), float32(y+r-k), float32(x+r-k), float32(y), float32(x+r), float32(y))
	z.ClosePath()
}

func (c *gCanvas) fillRoundRect(x, y, w, h, r float64, col color.RGBA) {
	if w <= 0 || h <= 0 || col.A == 0 {
		return
	}
	var z vector.Rasterizer
	z.Reset(c.img.Bounds().Dx(), c.img.Bounds().Dy())
	roundRectPath(&z, c.px(x), c.px(y), c.px(w), c.px(h), c.px(r))
	z.Draw(c.img, c.img.Bounds(), image.NewUniform(col), image.Point{})
}

func (c *gCanvas) fillRect(x, y, w, h float64, col color.RGBA) {
	c.fillRoundRect(x, y, w, h, 0, col)
}

func (c *gCanvas) line(x0, y0, x1, y1, width float64, col color.RGBA) {
	c.strokePolyline([][2]float64{{x0, y0}, {x1, y1}}, width, col)
}

// strokePolyline fills a polygon offset around the polyline, which gives
// antialiased caps and joins without a stroking rasteriser.
func (c *gCanvas) strokePolyline(pts [][2]float64, width float64, col color.RGBA) {
	if len(pts) < 2 || width <= 0 {
		return
	}
	half := c.px(width) / 2
	scaled := make([][2]float64, len(pts))
	for i, p := range pts {
		scaled[i] = [2]float64{c.px(p[0]), c.px(p[1])}
	}
	left := make([][2]float64, 0, len(scaled))
	right := make([][2]float64, 0, len(scaled))
	for i := range scaled {
		var nx, ny float64
		if i == 0 {
			nx, ny = normal(scaled[0], scaled[1])
		} else if i == len(scaled)-1 {
			nx, ny = normal(scaled[i-1], scaled[i])
		} else {
			ax, ay := normal(scaled[i-1], scaled[i])
			bx, by := normal(scaled[i], scaled[i+1])
			nx, ny = ax+bx, ay+by
			if l := math.Hypot(nx, ny); l > 1e-6 {
				nx, ny = nx/l, ny/l
			}
		}
		left = append(left, [2]float64{scaled[i][0] + nx*half, scaled[i][1] + ny*half})
		right = append(right, [2]float64{scaled[i][0] - nx*half, scaled[i][1] - ny*half})
	}
	var z vector.Rasterizer
	z.Reset(c.img.Bounds().Dx(), c.img.Bounds().Dy())
	z.MoveTo(float32(left[0][0]), float32(left[0][1]))
	for _, p := range left[1:] {
		z.LineTo(float32(p[0]), float32(p[1]))
	}
	for i := len(right) - 1; i >= 0; i-- {
		z.LineTo(float32(right[i][0]), float32(right[i][1]))
	}
	z.ClosePath()
	z.Draw(c.img, c.img.Bounds(), image.NewUniform(col), image.Point{})
}

func normal(a, b [2]float64) (float64, float64) {
	dx, dy := b[0]-a[0], b[1]-a[1]
	l := math.Hypot(dx, dy)
	if l < 1e-6 {
		return 0, 0
	}
	return -dy / l, dx / l
}

// dashedPolyline splits a sampled curve into dash/space runs by arc length.
func (c *gCanvas) dashedPolyline(pts [][2]float64, width float64, col color.RGBA, dash, gap float64) {
	if len(pts) < 2 {
		return
	}
	var run [][2]float64
	drawing := true
	remaining := dash
	run = append(run, pts[0])
	for i := 1; i < len(pts); i++ {
		a, b := pts[i-1], pts[i]
		segLen := math.Hypot(b[0]-a[0], b[1]-a[1])
		pos := 0.0
		for segLen-pos > 1e-9 {
			step := math.Min(remaining, segLen-pos)
			t := (pos + step) / segLen
			point := [2]float64{a[0] + (b[0]-a[0])*t, a[1] + (b[1]-a[1])*t}
			if drawing {
				run = append(run, point)
			}
			pos += step
			remaining -= step
			if remaining <= 1e-9 {
				if drawing && len(run) > 1 {
					c.strokePolyline(run, width, col)
				}
				drawing = !drawing
				run = [][2]float64{point}
				if drawing {
					remaining = dash
				} else {
					remaining = gap
				}
			}
		}
	}
	if drawing && len(run) > 1 {
		c.strokePolyline(run, width, col)
	}
}

func (c *gCanvas) text(f font.Face, x, baseline float64, col color.RGBA, s string) {
	if s == "" {
		return
	}
	d := font.Drawer{
		Dst:  c.img,
		Src:  image.NewUniform(col),
		Face: f,
		Dot: fixed.Point26_6{
			X: fixed.Int26_6(math.Round(c.px(x) * 64)),
			Y: fixed.Int26_6(math.Round(c.px(baseline) * 64)),
		},
	}
	d.DrawString(s)
}

// --- options --------------------------------------------------------------

type graphOptions struct {
	Theme      string
	Direction  layoutDirection
	Expand     expandMode
	Types      []resClass
	FailedOnly bool
	Scale      float64
	Width      float64
	Height     float64
	Origin     string
	Caption    bool
}

func parseGraphOptions(q map[string][]string) (graphOptions, error) {
	get := func(key string) string {
		if v, ok := q[key]; ok && len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	opts := graphOptions{
		Theme: "dark", Direction: dirHorizontal, Expand: expandOrigin,
		Scale: 2, Width: 1400, Height: 900, Caption: true,
	}
	switch theme := strings.ToLower(get("theme")); theme {
	case "", "dark":
		opts.Theme = "dark"
	case "light":
		opts.Theme = "light"
	default:
		return opts, fmt.Errorf("theme must be dark or light")
	}
	dir, ok := parseDirection(get("direction"))
	if !ok {
		return opts, fmt.Errorf("direction must be horizontal or vertical")
	}
	opts.Direction = dir
	mode, ok := parseExpand(get("expand"))
	if !ok {
		return opts, fmt.Errorf("expand must be origin, all or collapsed")
	}
	opts.Expand = mode

	for _, raw := range strings.Split(get("type"), ",") {
		raw = strings.ToLower(strings.TrimSpace(raw))
		if raw == "" {
			continue
		}
		class, ok := parseClass(raw)
		if !ok {
			return opts, fmt.Errorf("unknown resource type %q", raw)
		}
		opts.Types = append(opts.Types, class)
	}

	switch strings.ToLower(get("failed")) {
	case "", "0", "false", "no":
	case "1", "true", "yes":
		opts.FailedOnly = true
	default:
		return opts, fmt.Errorf("failed must be 0 or 1")
	}

	if raw := get("scale"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 4 {
			return opts, fmt.Errorf("scale must be 1-4")
		}
		opts.Scale = float64(n)
	}
	if raw := get("width"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 320 || n > 8000 {
			return opts, fmt.Errorf("width must be 320-8000")
		}
		opts.Width = float64(n)
	}
	if raw := get("height"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 320 || n > 8000 {
			return opts, fmt.Errorf("height must be 320-8000")
		}
		opts.Height = float64(n)
	}
	opts.Origin = get("origin")
	switch strings.ToLower(get("caption")) {
	case "", "1", "true", "yes":
	case "0", "false", "no":
		opts.Caption = false
	default:
		return opts, fmt.Errorf("caption must be 0 or 1")
	}
	return opts, nil
}

// --- render ---------------------------------------------------------------

const (
	graphPad    = 48.0
	graphFooter = 46.0
)

type graphStats struct {
	Requests  int
	Hosts     int
	Resources int
	Failed    int
}

func graphStatsOf(m graphModel) graphStats {
	var s graphStats
	for _, n := range m.Nodes {
		switch n.Kind {
		case "host":
			s.Hosts++
		case "resource":
			s.Resources++
			if n.Failed {
				s.Failed++
			}
		case "root":
			s.Requests = n.Count
		}
	}
	return s
}

// renderGraphPNG builds and paints the graph. Entries are the streamer's
// network log; filters mirror the viewer's facets.
func renderGraphPNG(entries []netDumpEntry, opts graphOptions, now time.Time) image.Image {
	theme := graphThemes[opts.Theme]
	if opts.FailedOnly {
		filtered := make([]netDumpEntry, 0, len(entries))
		for _, e := range entries {
			if e.Failed {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	full := buildGraphModel(entries, opts.Origin, opts.Types)
	stats := graphStatsOf(full)

	if len(full.Nodes) == 0 {
		return renderEmptyGraph(opts, theme, now)
	}

	layout := layoutModel(expandModel(full, opts.Expand), opts.Width, opts.Height, opts.Direction)
	return paintGraph(layout, stats, full.Origin, opts, theme, now)
}

func renderEmptyGraph(opts graphOptions, theme graphTheme, now time.Time) image.Image {
	w, h := 720.0, 220.0
	img := image.NewRGBA(image.Rect(0, 0, int(w*opts.Scale), int(h*opts.Scale)))
	c := &gCanvas{img: img, scale: opts.Scale}
	c.fillRect(0, 0, w, h, theme.Canvas)
	c.text(face("bold", 14*opts.Scale), 32, 96, theme.Label, "No requests captured yet")
	c.text(face("regular", 11*opts.Scale), 32, 120, theme.Neutral,
		"Open a page in the session, then request this image again.")
	if opts.Caption {
		c.text(face("mono", 10*opts.Scale), 32, 168, theme.Neutral, captionLine("", graphStats{}, opts.Theme, now))
	}
	return img
}

func captionLine(origin string, stats graphStats, theme string, now time.Time) string {
	plural := func(n int, one, many string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, one)
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	parts := make([]string, 0, 6)
	if origin != "" {
		parts = append(parts, origin)
	}
	parts = append(parts,
		plural(stats.Requests, "request", "requests"),
		plural(stats.Hosts, "host", "hosts"),
		plural(stats.Resources, "resource", "resources"),
	)
	if stats.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", stats.Failed))
	}
	parts = append(parts, theme, now.UTC().Format("2006-01-02 15:04")+"Z")
	return strings.Join(parts, " · ")
}

func paintGraph(layout graphLayout, stats graphStats, origin string, opts graphOptions, theme graphTheme, now time.Time) image.Image {
	footer := 0.0
	if opts.Caption {
		footer = graphFooter
	}
	contentW := math.Max(layout.TotalW, 320)
	contentH := math.Max(layout.TotalH, 200)
	w := contentW + graphPad*2
	h := contentH + graphPad*2 + footer

	img := image.NewRGBA(image.Rect(0, 0, int(math.Round(w*opts.Scale)), int(math.Round(h*opts.Scale))))
	c := &gCanvas{img: img, scale: opts.Scale}

	c.fillRect(0, 0, w, h, theme.Canvas)
	// Grid, drawn in the content area only.
	for gx := graphPad; gx < w-graphPad; gx += 24 {
		c.line(gx, graphPad, gx, graphPad+contentH, 0.5, theme.GridMinor)
	}
	for gy := graphPad; gy < graphPad+contentH; gy += 24 {
		c.line(graphPad, gy, w-graphPad, gy, 0.5, theme.GridMinor)
	}
	for gx := graphPad; gx < w-graphPad; gx += 120 {
		c.line(gx, graphPad, gx, graphPad+contentH, 0.5, theme.GridMajor)
	}
	for gy := graphPad; gy < graphPad+contentH; gy += 120 {
		c.line(graphPad, gy, w-graphPad, gy, 0.5, theme.GridMajor)
	}

	// Translate the layout into the padded frame.
	shift := func(n *layoutNode) (float64, float64) { return graphPad + n.X, graphPad + n.Y }

	// Column guides + depth labels.
	for _, col := range layout.Columns {
		if opts.Direction == dirVertical {
			c.dashedPolyline([][2]float64{
				{graphPad + col.X - 14, graphPad + col.Top - 14},
				{graphPad + col.X - 14, graphPad + col.Bottom + 14},
			}, 1, theme.ColumnLine, 2, 6)
			if col.Label {
				label := fmt.Sprintf("L%d", col.Depth)
				f := face("monob", 9*opts.Scale)
				c.text(f, graphPad+col.X-14-c.textW(f, label)/2, graphPad+col.Top-22, theme.ColumnLabel, label)
			}
			continue
		}
		c.dashedPolyline([][2]float64{
			{graphPad + col.X - 14, graphPad + col.Top - 24},
			{graphPad + col.X - 14, graphPad + col.Bottom + 14},
		}, 1, theme.ColumnLine, 2, 6)
		if col.Label {
			label := fmt.Sprintf("L%d", col.Depth)
			f := face("monob", 9*opts.Scale)
			c.text(f, graphPad+col.X-14-c.textW(f, label)/2, graphPad+col.Top-32, theme.ColumnLabel, label)
		}
	}

	// Edges behind the cards.
	for _, e := range layout.Edges {
		x0, y0 := shift(e.From)
		x1, y1 := shift(e.To)
		var startX, startY, endX, endY float64
		if opts.Direction == dirVertical {
			startX, startY = x0+e.From.W/2, y0+e.From.H
			endX, endY = x1+e.To.W/2, y1
		} else {
			startX, startY = x0+e.From.W, y0+e.From.H/2
			endX, endY = x1, y1+e.To.H/2
		}
		pts := edgeCurve(startX, startY, endX, endY, opts.Direction == dirVertical)
		if e.Direct {
			c.strokePolyline(pts, 1, blend(theme.Canvas, theme.Neutral, 0.55))
		} else {
			c.dashedPolyline(pts, 1.2, blend(theme.Canvas, theme.Neutral, 0.5), 7, 5)
		}
	}

	labelBold := face("bold", 12.5*opts.Scale)
	smallFace := face("regular", 11.5*opts.Scale)
	monoFace := face("mono", 10*opts.Scale)
	monoBold := face("monob", 9*opts.Scale)

	for i := range layout.Nodes {
		n := &layout.Nodes[i]
		x, y := shift(n)
		accent := theme.Accent[n.Node.Class]
		if n.Node.Kind == "root" || n.Node.Kind == "host" {
			accent = theme.Accent[classDocument]
		}
		bg := theme.Card
		switch {
		case n.Node.Kind == "root":
			bg = theme.CardRoot
		case n.Node.Failed:
			bg = theme.CardFailed
		case n.Node.Flagged:
			bg = theme.CardFlagged
		case n.Node.Kind == "group":
			bg = blend(theme.Canvas, accent, 0.10)
		case n.Node.Kind == "resource":
			bg = blend(theme.Canvas, accent, 0.07)
		}
		// Border by drawing a slightly larger rounded rect underneath.
		c.fillRoundRect(x, y, n.W, n.H, 10, blend(bg, accent, 0.55))
		c.fillRoundRect(x+1, y+1, n.W-2, n.H-2, 9, bg)
		c.fillRoundRect(x+0.5, y+6, 3, n.H-12, 1.5, accent)

		switch n.Node.Kind {
		case "root", "host":
			c.fillRoundRect(x+13, y+n.H/2-9, 22, 18, 5, blend(bg, accent, 0.16))
			glyph := "H"
			if n.Node.Kind == "root" {
				glyph = "R"
			}
			c.text(monoBold, x+24-c.textW(monoBold, glyph)/2, y+n.H/2+4.5, accent, glyph)

			badge := strconv.Itoa(n.Node.Count)
			bw := math.Max(26, c.textW(monoFace, badge)+12)
			reserved := 8 + bw + 10
			if n.Node.Failed {
				reserved += 16
			}
			label := fitText(labelBold, n.Node.Label, c.px(n.W-45-reserved))
			c.text(labelBold, x+45, y+n.H/2+4.5, theme.Label, label)
			c.fillRoundRect(x+n.W-8-bw, y+n.H/2-9, bw, 18, 5, theme.Badge)
			c.text(monoFace, x+n.W-8-bw+(bw-c.textW(monoFace, badge))/2, y+n.H/2+4, theme.BadgeText, badge)
			if n.Node.Failed {
				cx := x + n.W - 16 - bw - 8
				c.line(cx-3, y+n.H/2-3, cx+3, y+n.H/2+3, 1.6, theme.Accent[classImage])
				c.line(cx-3, y+n.H/2+3, cx+3, y+n.H/2-3, 1.6, theme.Accent[classImage])
			}
		case "group":
			c.fillRoundRect(x+10, y+n.H/2-3.5, 7, 7, 3.5, accent)
			badge := strconv.Itoa(n.Node.Count)
			bw := math.Max(24, c.textW(monoFace, badge)+12)
			label := fitText(smallFace, n.Node.Label, c.px(n.W-29-8-bw-12))
			c.text(smallFace, x+29, y+n.H/2+4, theme.Label, label)
			c.fillRoundRect(x+n.W-8-bw, y+n.H/2-8, bw, 16, 4, theme.Badge)
			c.text(monoBold, x+n.W-8-bw+(bw-c.textW(monoBold, badge))/2, y+n.H/2+3.5, theme.BadgeText, badge)
		default:
			chip := classShort[n.Node.Class]
			cw := math.Max(30, c.textW(monoBold, chip)+12)
			c.fillRoundRect(x+9, y+n.H/2-9, cw, 18, 5, blend(bg, accent, 0.18))
			c.text(monoBold, x+9+(cw-c.textW(monoBold, chip))/2, y+n.H/2+3.5, accent, chip)
			badge := ""
			bw := 0.0
			if n.Node.Count > 1 {
				badge = "×" + strconv.Itoa(n.Node.Count)
				bw = math.Max(24, c.textW(monoFace, badge)+10)
			}
			reserve := 8.0
			if bw > 0 {
				reserve += bw + 8
			}
			label := fitText(smallFace, n.Node.Label, c.px(n.W-9-cw-9-reserve))
			c.text(smallFace, x+9+cw+9, y+n.H/2+3.5, theme.Label, label)
			if bw > 0 {
				c.fillRoundRect(x+n.W-8-bw, y+n.H/2-8, bw, 16, 4, theme.Badge)
				c.text(monoFace, x+n.W-8-bw+(bw-c.textW(monoFace, badge))/2, y+n.H/2+3.5, theme.BadgeText, badge)
			}
		}
	}

	if opts.Caption {
		c.text(face("mono", 10*opts.Scale), graphPad, h-graphFooter/2+3,
			theme.Neutral, captionLine(origin, stats, opts.Theme, now))
		mark := "isola"
		mf := face("bold", 12*opts.Scale)
		c.text(mf, w-graphPad-c.textW(mf, mark), h-graphFooter/2+4, theme.Neutral, mark)
	}
	return img
}

// edgeCurve samples a cubic from one card's edge to another's, bending along
// the flow axis so curves stay inside their band.
func edgeCurve(x0, y0, x1, y1 float64, vertical bool) [][2]float64 {
	const steps = 24
	pts := make([][2]float64, 0, steps+1)
	if vertical {
		dy := y1 - y0
		c1y, c2y := y0+dy*0.45, y1-dy*0.45
		for i := 0; i <= steps; i++ {
			t := float64(i) / steps
			pts = append(pts, [2]float64{cubic(x0, x0, x1, x1, t), cubic(y0, c1y, c2y, y1, t)})
		}
		return pts
	}
	dx := x1 - x0
	c1x, c2x := x0+dx*0.45, x1-dx*0.45
	for i := 0; i <= steps; i++ {
		t := float64(i) / steps
		pts = append(pts, [2]float64{cubic(x0, c1x, c2x, x1, t), cubic(y0, y0, y1, y1, t)})
	}
	return pts
}

func cubic(p0, p1, p2, p3, t float64) float64 {
	mt := 1 - t
	return mt*mt*mt*p0 + 3*mt*mt*t*p1 + 3*mt*t*t*p2 + t*t*t*p3
}

// graphHandler serves GET /graph.png on the streamer's HTTP port.
func graphHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	opts, err := parseGraphOptions(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	img := renderGraphPNG(snapshotNetworkLog(), opts, time.Now())
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	if err := png.Encode(w, img); err != nil {
		log.Printf("graph: encode png: %v", err)
	}
}
