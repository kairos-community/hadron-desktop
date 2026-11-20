package main

import (
	"embed"
	"image/color"

	"gioui.org/font"
	"gioui.org/font/opentype"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
)

// The colours of the design note's "Visual system" table, which are
// theme/palette.toml's night palette, the same as the splash and GRUB.
var (
	colDesk1     = rgb(0x2e2c17) // the brand glow at the top of the desktop
	colDesk2     = rgb(0x141411)
	colDesk3     = rgb(0x0c0d0b)
	colWindow    = rgb(0x1d1d1a)
	colSidebar   = rgb(0x181915)
	colFooter    = rgb(0x1b1b18)
	colField     = rgb(0x2a2b25)
	colAccent    = rgb(0xe4de48) // brand: the installer is seen before any theme
	colOnAccent  = rgb(0x141411)
	colGlow      = rgb(0xf9f1af)
	colCheck     = rgb(0x9f9615)
	colText      = rgb(0xf4f5f2)
	colText2     = rgb(0xced0c8)
	colDim       = rgb(0x909287)
	colRing      = rgb(0x595b52)
	colError     = rgb(0xff7172)
	colErrorText = rgb(0xffaaa9)
	colDisabled  = rgb(0x393a34)
	colKeyBox    = rgb(0x141411)
	colWarn      = rgb(0xea8a18) // the palette's yellow: a warning, not an error
)

func rgb(v uint32) color.NRGBA {
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}
}

// alpha is c at opacity a (0..1), as the mocks' rgba() values.
func alpha(c color.NRGBA, a float32) color.NRGBA {
	c.A = uint8(a*255 + 0.5)
	return c
}

// The fonts are embedded, so the window looks the same on every variant
// whatever fontconfig has: Space Grotesk for titles, Inter for the rest,
// JetBrains Mono for device paths, layout codes and the recovery key. All
// three are SIL OFL 1.1; the licences are in fonts/. Static cuts, because
// Gio's shaper uses a variable font's default instance only. Space Grotesk
// publishes no 600 cut, so the 600 titles use its Bold.
//
//go:embed fonts/*.ttf
var fontFiles embed.FS

const (
	faceTitle = "Space Grotesk"
	faceBody  = "Inter"
	faceMono  = "JetBrains Mono"
)

func loadFonts() []font.FontFace {
	entries, err := fontFiles.ReadDir("fonts")
	if err != nil {
		panic(err)
	}
	var faces []font.FontFace
	for _, e := range entries {
		b, err := fontFiles.ReadFile("fonts/" + e.Name())
		if err != nil {
			panic(err)
		}
		ff, err := opentype.ParseCollection(b)
		if err != nil {
			panic(err)
		}
		faces = append(faces, ff...)
	}
	return faces
}

func newShaper() *text.Shaper {
	return text.NewShaper(text.NoSystemFonts(), text.WithCollection(loadFonts()))
}

// style is one text style of the mocks.
type style struct {
	face   font.Typeface
	weight font.Weight
	size   unit.Sp
}

var (
	stTitle     = style{faceTitle, font.Bold, 28}
	stHero      = style{faceTitle, font.Bold, 40}
	stDoneTitle = style{faceTitle, font.Bold, 38}
	stInstTitle = style{faceTitle, font.Bold, 26}
	stBrand     = style{faceTitle, font.Bold, 17}
	stAvatar    = style{faceTitle, font.Bold, 40}
	stBody      = style{faceBody, font.Normal, 14}
	stBody500   = style{faceBody, font.Medium, 14}
	stBody600   = style{faceBody, font.SemiBold, 14}
	stLead      = style{faceBody, font.Normal, 16}
	stLabel     = style{faceBody, font.SemiBold, 13}
	stSmall     = style{faceBody, font.Normal, 13}
	stSmall500  = style{faceBody, font.Medium, 13}
	stSmall600  = style{faceBody, font.SemiBold, 13}
	stTiny      = style{faceBody, font.Normal, 12}
	stCard      = style{faceBody, font.SemiBold, 15}
	stButton    = style{faceBody, font.SemiBold, 14}
	stButton500 = style{faceBody, font.Medium, 14}
	stBig       = style{faceBody, font.SemiBold, 15}
	stMono      = style{faceMono, font.Medium, 14}
	stMono13    = style{faceMono, font.Medium, 13}
	stMono12    = style{faceMono, font.Medium, 12}
	stMonoKey   = style{faceMono, font.Medium, 20}
	stMonoIndex = style{faceMono, font.Medium, 11}
	stMonoField = style{faceMono, font.Medium, 18}
)

func (s style) font() font.Font { return font.Font{Typeface: s.face, Weight: s.weight} }

// material is a paint op for text of colour c.
func material(gtx layout.Context, c color.NRGBA) op.CallOp {
	m := op.Record(gtx.Ops)
	paint.ColorOp{Color: c}.Add(gtx.Ops)
	return m.Stop()
}

// label draws one run of text, wrapped to the constraints.
func (u *UI) label(gtx layout.Context, s style, c color.NRGBA, txt string) layout.Dimensions {
	return widget.Label{}.Layout(gtx, u.shaper, s.font(), s.size, txt, material(gtx, c))
}

// labelLines is label with a line height as a multiple of the size, for the
// paragraphs the mocks set at 1.45 to 1.55.
func (u *UI) labelLines(gtx layout.Context, s style, c color.NRGBA, scale float32, txt string) layout.Dimensions {
	return widget.Label{LineHeightScale: scale}.Layout(gtx, u.shaper, s.font(), s.size, txt, material(gtx, c))
}

// centered is label with each line centred in the available width.
func (u *UI) centered(gtx layout.Context, s style, c color.NRGBA, scale float32, txt string) layout.Dimensions {
	return widget.Label{Alignment: text.Middle, LineHeightScale: scale}.Layout(gtx, u.shaper, s.font(), s.size, txt, material(gtx, c))
}

// over is c at opacity a laid on bg, mixed in sRGB as a browser does, so the
// mocks' rgba() values come out as drawn there. Gio blends in linear light,
// which makes a faint overlay much brighter, so translucent fills are mixed
// here into opaque colours instead.
func over(bg, c color.NRGBA, a float32) color.NRGBA {
	m := func(x, y uint8) uint8 { return uint8(float32(x)*(1-a) + float32(y)*a + 0.5) }
	return color.NRGBA{R: m(bg.R, c.R), G: m(bg.G, c.G), B: m(bg.B, c.B), A: 0xff}
}
