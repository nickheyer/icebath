package consoleutil

import (
	"strconv"
	"strings"
)

// Styled text of a string carrying SGR colors and attributes and OSC 8 hyperlinks, every other escape sequence and every control character but newline and tab dropped
func decodeANSI(s string) segments {
	t, _ := scanANSI(s, Style{}, false)
	return t
}

// Text with every escape sequence removed, so text from outside the program can neither move the cursor nor restyle the terminal
func stripEscapes(s string) string {
	if strings.IndexByte(s, 0x1b) < 0 {
		return s
	}
	runs, _ := scanANSI(s, Style{}, true)
	return runs.plain()
}

// Runs of s styled from st on by the SGR and OSC 8 sequences in it, every other escape sequence dropped and, unless keepControls is set, every control character but newline and tab; the style in force at its end
func scanANSI(s string, st Style, keepControls bool) (segments, Style) {
	var out segments
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			out = append(out, segment{text.String(), st})
			text.Reset()
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b && i+1 < len(s) && s[i+1] == '[':
			params, final, end := scanCSI(s, i+2)
			if final == 'm' && (params == "" || params[0] < '<') {
				flush()
				st = applySGR(st, params)
			}
			i = end
		case c == 0x1b && i+1 < len(s) && s[i+1] == ']':
			body, end := scanOSC(s, i+2)
			if link, ok := hyperlinkTarget(body); ok {
				flush()
				st.link = link
			}
			i = end
		case c == 0x1b:
			i = scanEscape(s, i+1)
		case (c < 0x20 || c == 0x7f) && !keepControls:
			if c == '\n' || c == '\t' {
				text.WriteByte(c)
			}
			i++
		default:
			j := i + 1
			for j < len(s) && s[j] != 0x1b && (keepControls || s[j] >= 0x20 && s[j] != 0x7f) {
				j++
			}
			text.WriteString(s[i:j])
			i = j
		}
	}
	flush()
	return out, st
}

// Parameters and final byte of a CSI sequence whose parameters start at i, and the offset after it
func scanCSI(s string, i int) (string, byte, int) {
	start := i
	for i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f {
		i++
	}
	params := s[start:i]
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
		return params, s[i], i + 1
	}
	return params, 0, i
}

// Body of an OSC sequence starting at i, ended by BEL or ST, and the offset after it
func scanOSC(s string, i int) (string, int) {
	for j := i; j < len(s); j++ {
		switch {
		case s[j] == 0x07:
			return s[i:j], j + 1
		case s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\':
			return s[i:j], j + 2
		}
	}
	return s[i:], len(s)
}

// Offset after an escape sequence other than CSI and OSC whose bytes start at i
func scanEscape(s string, i int) int {
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) && s[i] >= 0x30 && s[i] <= 0x7e {
		i++
	}
	return i
}

// Address an OSC 8 hyperlink body opens, its control characters dropped so writing it back cannot end the sequence early; empty for the one that closes a link, false for any other OSC
func hyperlinkTarget(body string) (string, bool) {
	rest, ok := strings.CutPrefix(body, "8;")
	if !ok {
		return "", false
	}
	_, uri, ok := strings.Cut(rest, ";")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, uri), ok
}

// Number of an SGR parameter, zero when empty, 255 at most
func sgrNumber(s string) (int, bool) {
	if s == "" {
		return 0, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return min(n, 255), true
}

// Style in force after an SGR parameter list, a reset keeping the hyperlink
func applySGR(st Style, params string) Style {
	groups := strings.Split(params, ";")
	for i := 0; i < len(groups); i++ {
		sub := strings.Split(groups[i], ":")
		code, ok := sgrNumber(sub[0])
		if !ok {
			continue
		}
		switch {
		case code == 0:
			st = Style{link: st.link}
		case code == 38 || code == 48 || code == 58:
			var c colorSpec
			if len(sub) > 1 {
				c, ok = colonColor(sub[1:])
			} else {
				var used int
				c, used, ok = semicolonColor(groups[i+1:])
				i += used
			}
			if ok && code == 38 {
				st.fg = c
			} else if ok && code == 48 {
				st.bg = c
			}
		case code >= 30 && code <= 37:
			st.fg = basicColor(uint8(code - 30))
		case code == 39:
			st.fg = colorSpec{kind: colorDefault}
		case code >= 40 && code <= 47:
			st.bg = basicColor(uint8(code - 40))
		case code == 49:
			st.bg = colorSpec{kind: colorDefault}
		case code >= 90 && code <= 97:
			st.fg = basicColor(uint8(code - 90 + 8))
		case code >= 100 && code <= 107:
			st.bg = basicColor(uint8(code - 100 + 8))
		default:
			if on, off := sgrAttrs(code); on != 0 || off != 0 {
				st = st.add(Style{on: on, off: off})
			}
		}
	}
	return st
}

// Attributes an SGR code switches on and off
func sgrAttrs(code int) (on, off attr) {
	switch code {
	case 1:
		return attrBold, 0
	case 2:
		return attrDim, 0
	case 3:
		return attrItalic, 0
	case 4:
		return attrUnderline, 0
	case 5:
		return attrBlink, 0
	case 6:
		return attrBlink2, 0
	case 7:
		return attrReverse, 0
	case 8:
		return attrConceal, 0
	case 9:
		return attrStrike, 0
	case 21:
		return attrUnderline2, 0
	case 22:
		return 0, attrBold | attrDim
	case 23:
		return 0, attrItalic
	case 24:
		return 0, attrUnderline | attrUnderline2
	case 25:
		return 0, attrBlink | attrBlink2
	case 27:
		return 0, attrReverse
	case 28:
		return 0, attrConceal
	case 29:
		return 0, attrStrike
	case 51:
		return attrFrame, 0
	case 52:
		return attrEncircle, 0
	case 53:
		return attrOverline, 0
	case 54:
		return 0, attrFrame | attrEncircle
	case 55:
		return 0, attrOverline
	}
	return 0, 0
}

// Color of the parameters after a semicolon separated 38 or 48 and how many of them it took
func semicolonColor(rest []string) (colorSpec, int, bool) {
	if len(rest) == 0 {
		return colorSpec{}, 0, false
	}
	switch rest[0] {
	case "5":
		if len(rest) < 2 {
			return colorSpec{}, len(rest), false
		}
		n, ok := sgrNumber(rest[1])
		return colorSpec{kind: colorIndexed, n: uint8(n)}, 2, ok
	case "2":
		if len(rest) < 4 {
			return colorSpec{}, len(rest), false
		}
		c, ok := rgbOf(rest[1:4])
		return c, 4, ok
	}
	return colorSpec{}, 1, false
}

// Color of the colon separated sub parameters of a 38 or 48, an RGB color space id skipped
func colonColor(sub []string) (colorSpec, bool) {
	switch {
	case sub[0] == "5" && len(sub) >= 2:
		n, ok := sgrNumber(sub[1])
		return colorSpec{kind: colorIndexed, n: uint8(n)}, ok
	case sub[0] == "2" && len(sub) >= 5:
		return rgbOf(sub[2:5])
	case sub[0] == "2" && len(sub) == 4:
		return rgbOf(sub[1:4])
	}
	return colorSpec{}, false
}

func rgbOf(values []string) (colorSpec, bool) {
	var v [3]uint8
	for i, s := range values {
		n, ok := sgrNumber(s)
		if !ok {
			return colorSpec{}, false
		}
		v[i] = uint8(n)
	}
	return rgbColor(v[0], v[1], v[2]), true
}
