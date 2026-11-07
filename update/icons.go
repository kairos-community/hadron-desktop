package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// The tray icons are drawn here at startup instead of shipped as PNG files:
// a coloured disc (one Hadron palette colour per state) with a dark glyph,
// so the repository carries no binary assets. The disc stands out on the
// dark panels; the glyph is the palette's on_accent, which has at least
// 4.5:1 contrast on every disc colour (white on the yellow disc was 1.9:1).
// Colours: generated from theme/palette.toml by tools/gen-theme.py.

var iconColors = map[string]color.NRGBA{
	"idle":   rgb(0x7aa2f7),
	"update": rgb(0x9ece6a),
	"busy":   rgb(0xe0af68),
	"reboot": rgb(0xbb9af7),
	"error":  rgb(0xf7768e),
}

var glyphColor = rgb(0x16161e)

// rgb is the opaque colour 0xRRGGBB.
func rgb(v uint32) color.NRGBA {
	return color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 0xff}
}

// iconFor is the icon of a daemon state.
func iconFor(s State) string {
	switch s {
	case StateUpdateAvailable:
		return "update"
	case StateInspecting, StateApplying, StateExtension:
		return "busy"
	case StateRebootPending:
		return "reboot"
	case StateError:
		return "error"
	}
	return "idle"
}

// segDist is the distance from (x, y) to the segment a-b.
func segDist(x, y, ax, ay, bx, by float64) float64 {
	dx, dy := bx-ax, by-ay
	t := math.Max(0, math.Min(1, ((x-ax)*dx+(y-ay)*dy)/(dx*dx+dy*dy)))
	return math.Hypot(x-ax-t*dx, y-ay-t*dy)
}

// glyph reports whether the point (x, y), in [-1, 1] with y growing down,
// belongs to the glyph of kind.
func glyph(kind string, x, y float64) bool {
	switch kind {
	case "idle": // a check mark: up to date
		return segDist(x, y, -0.45, 0.0, -0.12, 0.33) < 0.13 || segDist(x, y, -0.12, 0.33, 0.48, -0.33) < 0.13
	case "update": // an arrow down onto a bar: download
		shaft := math.Abs(x) < 0.12 && y > -0.55 && y < 0.15
		head := y >= 0.05 && y < 0.45 && math.Abs(x) < (0.45-y)*1.1
		bar := math.Abs(x) < 0.5 && y > 0.55 && y < 0.68
		return shaft || head || bar
	case "busy": // three dots: working
		for _, cx := range []float64{-0.42, 0, 0.42} {
			if math.Hypot(x-cx, y) < 0.15 {
				return true
			}
		}
	case "reboot": // a power symbol: restart
		r := math.Hypot(x, y)
		ring := r > 0.38 && r < 0.54 && !(y < 0 && math.Abs(x) < 0.2)
		stem := math.Abs(x) < 0.08 && y > -0.6 && y < -0.05
		return ring || stem
	case "error": // an exclamation mark
		return (math.Abs(x) < 0.11 && y > -0.58 && y < 0.15) || math.Hypot(x, y-0.42) < 0.13
	}
	return false
}

// iconPNG draws the icon of kind at size × size pixels, 4×4 supersampled.
func iconPNG(kind string, size int) []byte {
	const ss = 4
	fill := iconColors[kind]
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	half := float64(size) / 2
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var disc, ink int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss - half) / half
					y := (float64(py) + (float64(sy)+0.5)/ss - half) / half
					if math.Hypot(x, y) > 0.95 {
						continue
					}
					disc++
					if glyph(kind, x, y) {
						ink++
					}
				}
			}
			if disc == 0 {
				continue
			}
			// The disc colour blended with the glyph colour by glyph
			// coverage; alpha by disc coverage, so the edge is smooth on
			// any panel.
			mix := func(c, g uint8) uint8 {
				return uint8((int(c)*(disc-ink) + int(g)*ink) / disc)
			}
			img.SetNRGBA(px, py, color.NRGBA{mix(fill.R, glyphColor.R), mix(fill.G, glyphColor.G), mix(fill.B, glyphColor.B), uint8(0xff * disc / (ss * ss))})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img) // encoding an in-memory NRGBA cannot fail
	return buf.Bytes()
}
