// Colors and chrome shared by every icebath surface
package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Palette and border set that a deck paints with
type Theme struct {
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
	Glow        []color.Color
	Shadow      float64
	Border      lipgloss.Border
}

// Deep night palette with an ice and magenta focus ring
func Blackwall() Theme {
	return Theme{
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
		Glow:        []color.Color{Hex(0x5ff1ff), Hex(0x8a7dff), Hex(0xff4fd8), Hex(0x5ff1ff)},
		Shadow:      0.45,
		Border:      lipgloss.RoundedBorder(),
	}
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
