package tui

import (
	"math"
	"strconv"
	"strings"
)

// Channels of the starmap, red, green and blue as floats
type starRGB struct{ r, g, b float64 }

// Leading hex integer of a string the way parseInt with radix 16 reads it, false when it has none
func jsParseHex(s string) (float64, bool) {
	s = strings.TrimLeftFunc(s, isJSSpace)
	sign := 1.0
	if s != "" && (s[0] == '-' || s[0] == '+') {
		if s[0] == '-' {
			sign = -1
		}
		s = s[1:]
	}
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		s = s[2:]
	}
	end := 0
	for end < len(s) && strings.IndexByte("0123456789abcdefABCDEF", s[end]) >= 0 {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(s[:end], 16, 64)
	if err != nil {
		f, _ := strconv.ParseFloat("0x"+s[:end]+"p0", 64)
		return sign * f, true
	}
	return sign * float64(n), true
}

// Number wrapped to a signed 32 bit integer the way JavaScript bit operators convert it
func jsToInt32(f float64) int32 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	m := math.Mod(math.Trunc(f), 4294967296)
	if m < 0 {
		m += 4294967296
	}
	return int32(uint32(m))
}

// Channels of a hex, amber when it does not parse
func hexToStarRGB(hex string) starRGB {
	s := strings.TrimPrefix(strings.TrimFunc(hex, isJSSpace), "#")
	if len(units(s)) == 3 {
		var b strings.Builder
		for _, c := range units(s) {
			b.WriteString(strings.Repeat(fromUnits([]uint16{c}), 2))
		}
		s = b.String()
	}
	n, ok := jsParseHex(s)
	if !ok || len(units(s)) < 6 {
		return starRGB{r: 255, g: 215, b: 0}
	}
	v := jsToInt32(n)
	return starRGB{r: float64(v >> 16 & 255), g: float64(v >> 8 & 255), b: float64(v & 255)}
}

// Hex of starmap channels rounded and clamped
func starRGBToHex(c starRGB) string { return toHex([3]float64{c.r, c.g, c.b}) }

func starMix(a, b starRGB, t float64) starRGB {
	p := math.Max(0, math.Min(1, t))
	return starRGB{r: a.r + (b.r-a.r)*p, g: a.g + (b.g-a.g)*p, b: a.b + (b.b-a.b)*p}
}

func starLuminance(c starRGB) float64 { return (0.2126*c.r + 0.7152*c.g + 0.114*c.b) / 255 }

// Hue in degrees, saturation and lightness of starmap channels
func starRGBToHSL(c starRGB) (float64, float64, float64) {
	h, s, l := rgbToHSL(c.r, c.g, c.b)
	return h * 360, s, l
}

// Starmap channels of a hue in degrees, saturation and lightness
func starHSLToRGB(h, s, l float64) starRGB {
	hue := math.Mod(math.Mod(h, 360)+360, 360)
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(hue/60, 2)-1))
	m := l - c/2
	var r, g, b float64
	switch {
	case hue < 60:
		r, g, b = c, x, 0
	case hue < 120:
		r, g, b = x, c, 0
	case hue < 180:
		r, g, b = 0, c, x
	case hue < 240:
		r, g, b = 0, x, c
	case hue < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return starRGB{r: (r + m) * 255, g: (g + m) * 255, b: (b + m) * 255}
}

// Ink across the hue wheel from a color, saturated and mid lightness
func complementaryInk(c starRGB) starRGB {
	h, s, l := starRGBToHSL(c)
	return starHSLToRGB(h+165, math.Max(s, 0.5), math.Max(0.5, math.Min(0.7, l)))
}

// Inks of the learning starmap derived from the theme primary and text colors
type starmapPalette struct {
	bg, dim, label, memory, skill starRGB
}

// Starmap inks for a primary and foreground, dark when the foreground is light
func deriveStarmapPalette(primaryHex, fgHex string) starmapPalette {
	primary := hexToStarRGB(primaryHex)
	dark := starLuminance(hexToStarRGB(fgHex)) > 0.55
	base, bg := starRGB{0, 0, 0}, starRGB{250, 250, 250}
	memoryMix := 0.18
	if dark {
		base, bg, memoryMix = starRGB{255, 255, 255}, starRGB{8, 8, 12}, 0.12
	}
	return starmapPalette{bg: bg, dim: starMix(base, bg, 0.7), label: starMix(base, bg, 0.35), memory: starMix(primary, base, memoryMix), skill: starMix(complementaryInk(primary), bg, 0.45)}
}

// Hex faded toward the starmap background by alpha
func (p starmapPalette) fadeHex(hex string, alpha float64) string {
	base := hexToStarRGB(hex)
	if alpha >= 0.999 {
		return starRGBToHex(base)
	}
	return starRGBToHex(starMix(p.bg, base, alpha))
}

// Ink of a starmap style faded toward the background, false for an unknown style
func (p starmapPalette) fadeInk(style string, alpha float64) (string, bool) {
	var base starRGB
	switch style {
	case "skill":
		base = p.skill
	case "memory":
		base = p.memory
	case "label":
		base = p.label
	case "dim":
		base = p.dim
	default:
		return "", false
	}
	if alpha >= 0.999 {
		return starRGBToHex(base), true
	}
	return starRGBToHex(starMix(p.bg, base, alpha)), true
}
