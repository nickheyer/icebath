package pyrich

import (
	"regexp"
	"strconv"
	"strings"
)

const (
	colorDefault = iota
	colorStandard
	colorEightBit
	colorTruecolor
)

type richColor struct {
	typ     int
	number  int
	r, g, b int
}

var colorRE = regexp.MustCompile(`^#([0-9a-f]{6})$|^color\(([0-9]{1,3})\)$|^rgb\(([0-9\s,]+)\)$`)

// Color a rich color name parses to, false where Color.parse raises ColorParseError
func parseColor(name string) (richColor, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "default" {
		return richColor{typ: colorDefault}, true
	}
	if n, ok := ansiColorNames[name]; ok {
		return numberedColor(n), true
	}
	m := colorRE.FindStringSubmatch(name)
	switch {
	case m == nil:
		return richColor{}, false
	case m[1] != "":
		v, _ := strconv.ParseUint(m[1], 16, 32)
		return richColor{typ: colorTruecolor, r: int(v >> 16), g: int(v >> 8 & 0xff), b: int(v & 0xff)}, true
	case m[2] != "":
		n, _ := strconv.Atoi(m[2])
		if n > 255 {
			return richColor{}, false
		}
		return numberedColor(n), true
	}
	parts := strings.Split(m[3], ",")
	if len(parts) != 3 {
		return richColor{}, false
	}
	var rgb [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || v > 255 {
			return richColor{}, false
		}
		rgb[i] = v
	}
	return richColor{typ: colorTruecolor, r: rgb[0], g: rgb[1], b: rgb[2]}, true
}

func numberedColor(n int) richColor {
	if n < 16 {
		return richColor{typ: colorStandard, number: n}
	}
	return richColor{typ: colorEightBit, number: n}
}

func colorSystemLevel(system string) int {
	switch system {
	case "truecolor":
		return colorTruecolor
	case "256":
		return colorEightBit
	}
	return colorStandard
}

// Color converted to what the color system can show, as Color.downgrade
func (c richColor) downgrade(system string) richColor {
	level := colorSystemLevel(system)
	if c.typ == colorDefault || c.typ == level {
		return c
	}
	if level == colorEightBit && c.typ == colorTruecolor {
		return richColor{typ: colorEightBit, number: eightBit(c.r, c.g, c.b)}
	}
	if level == colorStandard {
		rgb := [3]int{c.r, c.g, c.b}
		if c.typ != colorTruecolor {
			rgb = eightBitPalette[c.number]
		}
		return richColor{typ: colorStandard, number: matchStandard(rgb[0], rgb[1], rgb[2])}
	}
	return c
}

// SGR parameters of the color in the foreground or background, as Color.get_ansi_codes
func (c richColor) codes(fg bool) string {
	lead := "38"
	if !fg {
		lead = "48"
	}
	switch c.typ {
	case colorDefault:
		if fg {
			return "39"
		}
		return "49"
	case colorStandard:
		return standardCode(c.number, fg)
	case colorEightBit:
		return lead + ";5;" + strconv.Itoa(c.number)
	}
	return lead + ";2;" + strconv.Itoa(c.r) + ";" + strconv.Itoa(c.g) + ";" + strconv.Itoa(c.b)
}
