// Colors and chrome shared by every icebath surface
package theme

import (
	"fmt"
	"image/color"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
)

// How much room chrome and cards leave around their content
type Density int

const (
	Comfortable Density = iota
	Compact
)

// Palette and border set that a deck paints with
type Theme struct {
	Name        string
	Bg          color.Color
	Surface     color.Color
	Text        color.Color
	TextDim     color.Color
	Steel       color.Color
	SteelBright color.Color
	Ghost       color.Color
	Accent      color.Color
	Ice         color.Color
	Pulse       color.Color
	Alive       color.Color
	Alarm       color.Color
	// Gradient stops the focus ring runs through around the focused card
	Ring    []color.Color
	Border  lipgloss.Border
	Glyphs  Glyphs
	Density Density
	// Palette limited to the sixteen ANSI colors with no blending, for terminals without true color
	Plain bool
}

// Deep night palette with an ice and magenta focus ring
func Blackwall() Theme {
	return Theme{
		Name:        "blackwall",
		Bg:          Hex(0x070a12),
		Surface:     Hex(0x0c1120),
		Text:        Hex(0xd6def7),
		TextDim:     Hex(0x667199),
		Steel:       Hex(0x2a3654),
		SteelBright: Hex(0x4b5d8c),
		Ghost:       Hex(0x1c2544),
		Accent:      Hex(0xffc857),
		Ice:         Hex(0x5ff1ff),
		Pulse:       Hex(0xff4fd8),
		Alive:       Hex(0x7cff9e),
		Alarm:       Hex(0xff5f6d),
		Ring:        []color.Color{Hex(0x5ff1ff), Hex(0x8a7dff), Hex(0xff4fd8), Hex(0x5ff1ff)},
		Border:      IcebathBorder(),
		Glyphs:      IcebathGlyphs(),
	}
}

// Paper palette for bright rooms with a cobalt and violet focus ring
func Light() Theme {
	return Theme{
		Name:        "light",
		Bg:          Hex(0xeef1f7),
		Surface:     Hex(0xfafbfe),
		Text:        Hex(0x1b2238),
		TextDim:     Hex(0x6b7390),
		Steel:       Hex(0xc3cadb),
		SteelBright: Hex(0x9aa5c2),
		Ghost:       Hex(0xd7dcea),
		Accent:      Hex(0xb8600a),
		Ice:         Hex(0x0a6fb3),
		Pulse:       Hex(0xb5179e),
		Alive:       Hex(0x1f8a4c),
		Alarm:       Hex(0xc9303e),
		Ring:        []color.Color{Hex(0x0a6fb3), Hex(0x5b3fd6), Hex(0xb5179e), Hex(0x0a6fb3)},
		Border:      IcebathBorder(),
		Glyphs:      IcebathGlyphs(),
	}
}

// Black and white palette with saturated signal colors and a thick border for low vision
func Contrast() Theme {
	return Theme{
		Name:        "contrast",
		Bg:          Hex(0x000000),
		Surface:     Hex(0x000000),
		Text:        Hex(0xffffff),
		TextDim:     Hex(0xbfbfbf),
		Steel:       Hex(0xffffff),
		SteelBright: Hex(0xffffff),
		Ghost:       Hex(0x808080),
		Accent:      Hex(0xffff00),
		Ice:         Hex(0x00ffff),
		Pulse:       Hex(0xff00ff),
		Alive:       Hex(0x00ff00),
		Alarm:       Hex(0xff0000),
		Ring:        []color.Color{Hex(0x00ffff), Hex(0xffff00)},
		Border:      lipgloss.ThickBorder(),
		Glyphs:      IcebathGlyphs(),
	}
}

// Sixteen color palette with an ASCII border for terminals without true color or box drawing
func Plain() Theme {
	return Theme{
		Name:        "plain",
		Bg:          lipgloss.Black,
		Surface:     lipgloss.Black,
		Text:        lipgloss.White,
		TextDim:     lipgloss.BrightBlack,
		Steel:       lipgloss.BrightBlack,
		SteelBright: lipgloss.White,
		Ghost:       lipgloss.BrightBlack,
		Accent:      lipgloss.Yellow,
		Ice:         lipgloss.Cyan,
		Pulse:       lipgloss.Magenta,
		Alive:       lipgloss.Green,
		Alarm:       lipgloss.Red,
		Ring:        []color.Color{lipgloss.Cyan},
		Border:      lipgloss.ASCIIBorder(),
		Glyphs:      ASCIIGlyphs(),
		Plain:       true,
	}
}

var themes = map[string]func() Theme{"blackwall": Blackwall, "light": Light, "contrast": Contrast, "plain": Plain}

// Theme by name, false for an unknown name
func Named(name string) (Theme, bool) {
	fn, ok := themes[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return Theme{}, false
	}
	return fn(), true
}

// Theme names sorted
func Names() []string {
	out := make([]string, 0, len(themes))
	for n := range themes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

var borders = map[string]func() lipgloss.Border{"icebath": IcebathBorder, "rounded": lipgloss.RoundedBorder, "normal": lipgloss.NormalBorder, "thick": lipgloss.ThickBorder, "double": lipgloss.DoubleBorder, "block": lipgloss.BlockBorder, "hidden": lipgloss.HiddenBorder, "ascii": lipgloss.ASCIIBorder}

// Border set names sorted
func Borders() []string {
	out := make([]string, 0, len(borders))
	for n := range borders {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// What a person chooses to look at, resolved into a theme
type Style struct {
	Palette string  `json:"palette,omitempty"`
	Border  string  `json:"border,omitempty"`
	Motion  float64 `json:"motion"`
	Density string  `json:"density,omitempty"`
}

// Theme the style names, the Blackwall palette, its border and comfortable density where the style says nothing
func (s Style) Resolve() (Theme, error) {
	t := Blackwall()
	if s.Palette != "" {
		named, ok := Named(s.Palette)
		if !ok {
			return Theme{}, fmt.Errorf("theme: unknown palette %q, choose from %s", s.Palette, strings.Join(Names(), ", "))
		}
		t = named
	}
	if s.Border != "" {
		fn, ok := borders[strings.ToLower(s.Border)]
		if !ok {
			return Theme{}, fmt.Errorf("theme: unknown border %q, choose from %s", s.Border, strings.Join(Borders(), ", "))
		}
		t.Border = fn()
	}
	switch strings.ToLower(s.Density) {
	case "":
	case "compact":
		t.Density = Compact
	case "comfortable":
		t.Density = Comfortable
	default:
		return Theme{}, fmt.Errorf("theme: unknown density %q, choose compact or comfortable", s.Density)
	}
	if s.Motion < 0 || s.Motion > 1 {
		return Theme{}, fmt.Errorf("theme: motion %v is outside 0 to 1", s.Motion)
	}
	return t, nil
}

// Opaque color from a 24 bit RGB value
func Hex(v uint32) color.Color {
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}
}

// Color t of the way from a to b, treating a nil side as the other side
func Mix(a, b color.Color, t float64) color.Color {
	if a == nil {
		a = b
	}
	if b == nil {
		b = a
	}
	if a == nil || t <= 0 {
		return a
	}
	if t >= 1 {
		return b
	}
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	return color.RGBA{R: lerp8(ar, br, t), G: lerp8(ag, bg, t), B: lerp8(ab, bb, t), A: 0xff}
}

// Darkens c toward black by amount
func Shade(c color.Color, amount float64) color.Color {
	return Mix(c, color.Black, amount)
}

// Produces n colors that run through the stops and return to the first so they tile seamlessly
func Ring(n int, stops ...color.Color) []color.Color {
	if len(stops) == 0 {
		return nil
	}
	if len(stops) == 1 || n < 2 {
		out := make([]color.Color, max(n, 1))
		for i := range out {
			out[i] = stops[0]
		}
		return out
	}
	if stops[0] != stops[len(stops)-1] {
		stops = append(append([]color.Color{}, stops...), stops[0])
	}
	return lipgloss.Blend1D(n, stops...)
}

func lerp8(a, b uint32, t float64) uint8 {
	fa, fb := float64(a>>8), float64(b>>8)
	return uint8(fa + (fb-fa)*t + 0.5)
}
