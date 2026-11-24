package consoleutil

import (
	"image/color"
	"strconv"
	"strings"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

type colorKind uint8

const (
	colorUnset colorKind = iota
	colorDefault
	colorBasic
	colorIndexed
	colorRGB
)

// Color of a style: the terminal default, one of the sixteen standard colors, a 256 palette entry or a 24-bit value
type colorSpec struct {
	kind    colorKind
	n       uint8
	r, g, b uint8
}

func basicColor(n uint8) colorSpec { return colorSpec{kind: colorBasic, n: n} }

func rgbColor(r, g, b uint8) colorSpec { return colorSpec{kind: colorRGB, r: r, g: g, b: b} }

// Palette entry n, one of the sixteen standard colors below 16
func paletteColor(n uint8) colorSpec {
	if n < 16 {
		return basicColor(n)
	}
	return colorSpec{kind: colorIndexed, n: n}
}

// Color a name, #rrggbb, color(n), rgb(r,g,b) or default describes, false when it describes none
func parseColor(name string) (colorSpec, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "default" {
		return colorSpec{kind: colorDefault}, true
	}
	if n, ok := colorNames()[name]; ok {
		return paletteColor(n), true
	}
	switch {
	case len(name) == 7 && name[0] == '#':
		v, err := strconv.ParseUint(name[1:], 16, 32)
		if err != nil {
			return colorSpec{}, false
		}
		return rgbColor(uint8(v>>16), uint8(v>>8), uint8(v)), true
	case strings.HasPrefix(name, "color(") && strings.HasSuffix(name, ")"):
		digits := name[len("color(") : len(name)-1]
		n, ok := byteNumber(digits)
		if !ok {
			return colorSpec{}, false
		}
		return paletteColor(n), true
	case strings.HasPrefix(name, "rgb(") && strings.HasSuffix(name, ")"):
		parts := strings.Split(name[len("rgb("):len(name)-1], ",")
		if len(parts) != 3 {
			return colorSpec{}, false
		}
		var v [3]uint8
		for i, p := range parts {
			n, ok := byteNumber(strings.TrimSpace(p))
			if !ok {
				return colorSpec{}, false
			}
			v[i] = n
		}
		return rgbColor(v[0], v[1], v[2]), true
	}
	return colorSpec{}, false
}

// Value of one to three decimal digits no greater than 255
func byteNumber(digits string) (uint8, bool) {
	if digits == "" || len(digits) > 3 || strings.TrimLeft(digits, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n > 255 {
		return 0, false
	}
	return uint8(n), true
}

// SGR parameters of the color as a foreground or background on the profile, converted to what the profile can show, empty when it shows no color
func (c colorSpec) codes(fg bool, p colorprofile.Profile) string {
	switch c.kind {
	case colorUnset:
		return ""
	case colorDefault:
		if p <= colorprofile.ASCII {
			return ""
		}
		if fg {
			return "39"
		}
		return "49"
	}
	var in color.Color
	switch c.kind {
	case colorBasic:
		in = ansi.BasicColor(c.n)
	case colorIndexed:
		in = ansi.IndexedColor(c.n)
	default:
		in = color.RGBA{R: c.r, G: c.g, B: c.b, A: 0xff}
	}
	lead := "38"
	if !fg {
		lead = "48"
	}
	switch v := p.Convert(in).(type) {
	case nil:
		return ""
	case ansi.BasicColor:
		base := 30
		if !fg {
			base = 40
		}
		if n := int(v); n >= 8 {
			return strconv.Itoa(base + 52 + n)
		}
		return strconv.Itoa(base + int(v))
	case ansi.IndexedColor:
		return lead + ";5;" + strconv.Itoa(int(v))
	default:
		r, g, b, _ := v.RGBA()
		return lead + ";2;" + strconv.Itoa(int(r>>8)) + ";" + strconv.Itoa(int(g>>8)) + ";" + strconv.Itoa(int(b>>8))
	}
}
