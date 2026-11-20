package guikit

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

// The fonts are embedded, so a window looks the same on every variant
// whatever fontconfig has: Urbanist SemiBold for titles (the wordmark's face,
// as on the splash), Inter for the rest, JetBrains Mono for device paths,
// codes and keys. Inter is under its own licence, the others SIL OFL 1.1; the
// licences are in fonts/. Static cuts, because Gio's shaper uses a variable
// font's default instance only.
//
//go:embed fonts/*.ttf
var fontFiles embed.FS

// The typefaces of the embedded fonts.
const (
	FaceTitle font.Typeface = "Urbanist"
	FaceBody  font.Typeface = "Inter"
	FaceMono  font.Typeface = "JetBrains Mono"
)

// Fonts parses the embedded fonts.
func Fonts() []font.FontFace {
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

// NewShaper is a text shaper with the embedded fonts only, never the
// system's.
func NewShaper() *text.Shaper {
	return text.NewShaper(text.NoSystemFonts(), text.WithCollection(Fonts()))
}

// Style is one text style of the mocks.
type Style struct {
	Face   font.Typeface
	Weight font.Weight
	Size   unit.Sp
}

// Font is the font of s, for widget.Label and widget.Editor.
func (s Style) Font() font.Font { return font.Font{Typeface: s.Face, Weight: s.Weight} }

// The text styles the tools share. An app defines its own Style for a text
// only it has (a large key, a counter).
var (
	StTitle     = Style{FaceTitle, font.SemiBold, 28} // a page heading
	StHero      = Style{FaceTitle, font.SemiBold, 44} // a welcome page's title
	StBrand     = Style{FaceTitle, font.SemiBold, 17} // the sidebar's title
	StCaps      = Style{FaceBody, font.SemiBold, 11}  // small spaced capitals
	StBody      = Style{FaceBody, font.Normal, 14}
	StBody600   = Style{FaceBody, font.SemiBold, 14}
	StLead      = Style{FaceBody, font.Normal, 16} // an introduction paragraph
	StLabel     = Style{FaceBody, font.SemiBold, 13}
	StSmall     = Style{FaceBody, font.Normal, 13}
	StSmall500  = Style{FaceBody, font.Medium, 13}
	StSmall600  = Style{FaceBody, font.SemiBold, 13}
	StTiny      = Style{FaceBody, font.Normal, 12}
	StCard      = Style{FaceBody, font.SemiBold, 15} // a card's title
	StButton    = Style{FaceBody, font.SemiBold, 14}
	StButton500 = Style{FaceBody, font.Medium, 14}
	StMono      = Style{FaceMono, font.Medium, 14}
	StMono13    = Style{FaceMono, font.Medium, 13}
	StMono12    = Style{FaceMono, font.Medium, 12}
)

// Theme draws the shared widgets with one shaper. An app makes one with
// NewTheme and keeps it for the life of the window.
type Theme struct {
	Shaper *text.Shaper
}

// NewTheme is a theme on the embedded fonts.
func NewTheme() *Theme { return &Theme{Shaper: NewShaper()} }

// Material is a paint op for text of colour c.
func Material(gtx layout.Context, c color.NRGBA) op.CallOp {
	m := op.Record(gtx.Ops)
	paint.ColorOp{Color: c}.Add(gtx.Ops)
	return m.Stop()
}

// Label draws one run of text, wrapped to the constraints.
func (th *Theme) Label(gtx layout.Context, s Style, c color.NRGBA, txt string) layout.Dimensions {
	return widget.Label{}.Layout(gtx, th.Shaper, s.Font(), s.Size, txt, Material(gtx, c))
}

// LabelLines is Label with a line height as a multiple of the size, for the
// paragraphs the mocks set at 1.45 to 1.55.
func (th *Theme) LabelLines(gtx layout.Context, s Style, c color.NRGBA, scale float32, txt string) layout.Dimensions {
	return widget.Label{LineHeightScale: scale}.Layout(gtx, th.Shaper, s.Font(), s.Size, txt, Material(gtx, c))
}

// Centered is LabelLines with each line centred in the available width.
func (th *Theme) Centered(gtx layout.Context, s Style, c color.NRGBA, scale float32, txt string) layout.Dimensions {
	return widget.Label{Alignment: text.Middle, LineHeightScale: scale}.Layout(gtx, th.Shaper, s.Font(), s.Size, txt, Material(gtx, c))
}

// LabelMax is Label cut to maxLines lines.
func (th *Theme) LabelMax(gtx layout.Context, s Style, c color.NRGBA, maxLines int, txt string) layout.Dimensions {
	return widget.Label{MaxLines: maxLines}.Layout(gtx, th.Shaper, s.Font(), s.Size, txt, Material(gtx, c))
}
