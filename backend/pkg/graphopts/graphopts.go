// Package graphopts parses and validates the query options of the resource
// graph endpoint.
//
// The streamer owns rendering and repeats these checks. The control plane
// validates first so a bad request fails fast, and forwards only a canonical,
// whitelisted query — no client-controlled parameter reaches the renderer
// unchecked. Wording is kept in step with the streamer's own errors
// (browsers/isola-streamer/graph_draw.go) so both layers report the same
// reason.
package graphopts

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	// DefaultScale matches the streamer's default raster scale.
	DefaultScale = 2
	MinScale     = 1
	MaxScale     = 4

	DefaultWidth  = 1400
	DefaultHeight = 900
	MinDimension  = 320
	MaxDimension  = 8000

	// MaxOriginLen bounds the free-form origin hostname. It is only a display
	// label and a graph selector, so a longer value is a mistake, not a
	// legitimate request.
	MaxOriginLen = 255
)

// Options is a validated resource-graph request.
type Options struct {
	Theme     string
	Direction string
	Expand    string
	Types     []string
	Failed    bool
	Caption   bool
	Scale     int
	Width     int
	Height    int
	Origin    string
}

// graphTypes is the whitelist of resource classes the renderer understands.
var graphTypes = map[string]bool{
	"document":   true,
	"script":     true,
	"stylesheet": true,
	"image":      true,
	"font":       true,
	"xhr":        true,
	"media":      true,
	"other":      true,
}

// Parse validates the graph query and fills in the renderer's defaults.
// Unknown parameters are ignored; every accepted parameter is range-checked.
func Parse(q url.Values) (Options, error) {
	get := func(key string) string {
		return strings.TrimSpace(q.Get(key))
	}
	opts := Options{
		Theme:     "dark",
		Direction: "horizontal",
		Expand:    "origin",
		Caption:   true,
		Scale:     DefaultScale,
		Width:     DefaultWidth,
		Height:    DefaultHeight,
	}

	switch theme := strings.ToLower(get("theme")); theme {
	case "", "dark":
		opts.Theme = "dark"
	case "light":
		opts.Theme = "light"
	default:
		return opts, fmt.Errorf("theme must be dark or light")
	}

	switch dir := strings.ToLower(get("direction")); dir {
	case "", "horizontal":
		opts.Direction = "horizontal"
	case "vertical":
		opts.Direction = "vertical"
	default:
		return opts, fmt.Errorf("direction must be horizontal or vertical")
	}

	switch expand := strings.ToLower(get("expand")); expand {
	case "", "origin":
		opts.Expand = "origin"
	case "all":
		opts.Expand = "all"
	case "collapsed":
		opts.Expand = "collapsed"
	default:
		return opts, fmt.Errorf("expand must be origin, all or collapsed")
	}

	for _, raw := range strings.Split(get("type"), ",") {
		raw = strings.ToLower(strings.TrimSpace(raw))
		if raw == "" {
			continue
		}
		if !graphTypes[raw] {
			return opts, fmt.Errorf("unknown resource type %q", raw)
		}
		opts.Types = append(opts.Types, raw)
	}

	switch strings.ToLower(get("failed")) {
	case "", "0", "false", "no":
	case "1", "true", "yes":
		opts.Failed = true
	default:
		return opts, fmt.Errorf("failed must be 0 or 1")
	}

	if raw := get("scale"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < MinScale || n > MaxScale {
			return opts, fmt.Errorf("scale must be %d-%d", MinScale, MaxScale)
		}
		opts.Scale = n
	}
	if raw := get("width"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < MinDimension || n > MaxDimension {
			return opts, fmt.Errorf("width must be %d-%d", MinDimension, MaxDimension)
		}
		opts.Width = n
	}
	if raw := get("height"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < MinDimension || n > MaxDimension {
			return opts, fmt.Errorf("height must be %d-%d", MinDimension, MaxDimension)
		}
		opts.Height = n
	}

	if origin := get("origin"); origin != "" {
		if len(origin) > MaxOriginLen {
			return opts, fmt.Errorf("origin must be at most %d characters", MaxOriginLen)
		}
		opts.Origin = origin
	}

	switch strings.ToLower(get("caption")) {
	case "", "1", "true", "yes":
	case "0", "false", "no":
		opts.Caption = false
	default:
		return opts, fmt.Errorf("caption must be 0 or 1")
	}

	return opts, nil
}

// Query renders the resolved options as the canonical query the streamer
// receives. Defaults are included explicitly so the renderer never has to
// re-derive them, and every value is one Parse accepted.
func (o Options) Query() url.Values {
	q := url.Values{}
	q.Set("theme", o.Theme)
	q.Set("direction", o.Direction)
	q.Set("expand", o.Expand)
	if len(o.Types) > 0 {
		q.Set("type", strings.Join(o.Types, ","))
	}
	q.Set("failed", boolParam(o.Failed))
	q.Set("scale", strconv.Itoa(o.Scale))
	q.Set("width", strconv.Itoa(o.Width))
	q.Set("height", strconv.Itoa(o.Height))
	if o.Origin != "" {
		q.Set("origin", o.Origin)
	}
	q.Set("caption", boolParam(o.Caption))
	return q
}

func boolParam(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
