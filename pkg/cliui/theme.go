package cliui

import (
	"fmt"
	"image/color"
	"runtime"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Theme is the palette prompts draw with, every tone a hex color
type Theme struct {
	Accent string
	Frost  string
	Muted  string
	Faint  string
	OK     string
	Warn   string
	Err    string
}

// Brand palette every prompt draws with, signal red over ice tones
var promptTheme = Theme{Accent: "#f50325", Frost: "#9bdcf7", Muted: "#8b9aa5", Faint: "#5b6872", OK: "#62d394", Warn: "#f2c14e", Err: "#ff5d6c"}

// Color and attributes one run of prompt text is drawn with
type textStyle struct {
	color         string
	bold, reverse bool
}

// Reports an output terminal that renders text attributes
func (c *Console) styled() bool {
	if term, _ := c.getenv("TERM"); term == "dumb" {
		return false
	}
	return c.stdoutTTY()
}

// Number of colors the terminal shows, zero when color is off
func (c *Console) colorDepth() int {
	if !c.ShouldUseColor() {
		return 0
	}
	if v, _ := c.getenv("COLORTERM"); v == "truecolor" || v == "24bit" {
		return 1 << 24
	}
	if _, ok := c.getenv("WT_SESSION"); ok {
		return 1 << 24
	}
	term, _ := c.getenv("TERM")
	for _, name := range []string{"kitty", "ghostty", "alacritty", "wezterm", "foot", "direct", "contour"} {
		if strings.Contains(term, name) {
			return 1 << 24
		}
	}
	if strings.Contains(term, "256color") {
		return 256
	}
	return 16
}

// Red, green and blue of a six digit hex color, false when it is not one
func parseHex(hex string) (color.RGBA, bool) {
	s := strings.TrimPrefix(hex, "#")
	if len(s) != 6 {
		return color.RGBA{}, false
	}
	n, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return color.RGBA{}, false
	}
	return color.RGBA{R: uint8(n >> 16), G: uint8(n >> 8), B: uint8(n), A: 255}, true
}

// Foreground parameters of a hex color at the depth the terminal shows, empty when color is off
func (c *Console) fg(hex string) string {
	depth := c.colorDepth()
	col, ok := parseHex(hex)
	if depth == 0 || !ok {
		return ""
	}
	switch depth {
	case 1 << 24:
		return fmt.Sprintf("38;2;%d;%d;%d", col.R, col.G, col.B)
	case 256:
		return fmt.Sprintf("38;5;%d", uint8(ansi.Convert256(col)))
	}
	n := int(ansi.Convert16(col))
	if n < 8 {
		return strconv.Itoa(30 + n)
	}
	return strconv.Itoa(90 + n - 8)
}

// Wraps text in the color and attributes of a style, the plain text on a terminal that renders neither
func (c *Console) paint(text string, s textStyle) string {
	if text == "" || !c.styled() {
		return text
	}
	var codes []string
	if s.bold {
		codes = append(codes, "1")
	}
	if s.reverse {
		codes = append(codes, "7")
	}
	if code := c.fg(s.color); code != "" {
		codes = append(codes, code)
	}
	if len(codes) == 0 {
		return text
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + text + "\x1b[0m"
}

// Marks prompts draw with, in Unicode or in ASCII for a terminal that lacks the glyphs
type glyphSet struct {
	active, answered, cursor, checked, unchecked string
	up, down, arrows, back, ellipsis, sep, mask  string
}

var unicodeGlyphs = glyphSet{active: "●", answered: "›", cursor: "❯", checked: "[✓]", unchecked: "[ ]",
	up: "↑", down: "↓", arrows: "↑↓", back: "←", ellipsis: "…", sep: "·", mask: "•"}

var asciiGlyphs = glyphSet{active: "*", answered: ">", cursor: ">", checked: "[x]", unchecked: "[ ]",
	up: "^", down: "v", arrows: "up/down", back: "left", ellipsis: "...", sep: "|", mask: "*"}

// Reports a UTF-8 locale on a terminal whose font carries the prompt marks
func (c *Console) unicode() bool {
	if term, _ := c.getenv("TERM"); term == "linux" {
		return false
	}
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v, ok := c.getenv(name); ok && v != "" {
			upper := strings.ToUpper(v)
			return strings.Contains(upper, "UTF-8") || strings.Contains(upper, "UTF8")
		}
	}
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

func (c *Console) glyphs() glyphSet {
	if c.unicode() {
		return unicodeGlyphs
	}
	return asciiGlyphs
}

// Text cut to a display width with an ellipsis, escape sequences kept whole
func (c *Console) clip(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(text) <= width {
		return text
	}
	return ansi.Truncate(text, width, c.glyphs().ellipsis)
}
