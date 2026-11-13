package tui

import (
	"image/color"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	uitheme "github.com/nickheyer/icebath/pkg/ui/theme"
)

// Sixteen terminal colors by the names a skin may use for a tone
var ansiToneNames = map[string]int{
	"black": 0, "red": 1, "green": 2, "yellow": 3, "blue": 4, "magenta": 5, "cyan": 6, "white": 7,
	"gray": 8, "grey": 8, "blackBright": 8, "redBright": 9, "greenBright": 10, "yellowBright": 11,
	"blueBright": 12, "magentaBright": 13, "cyanBright": 14, "whiteBright": 15,
}

// Text with surrounding whitespace removed
func trimJS(s string) string { return strings.TrimFunc(s, isJSSpace) }

// Color of a theme tone, a hex or rgb value, an indexed color or a named terminal color
func toneColor(tone string) color.Color {
	tone = trimJS(tone)
	if m := ansi256Re.FindStringSubmatch(tone); m != nil {
		n, _ := strconv.Atoi(m[1])
		return lipgloss.Color(strconv.Itoa(min(n, 255)))
	}
	if n, named := ansiToneNames[strings.TrimPrefix(tone, "ansi:")]; named {
		return lipgloss.Color(strconv.Itoa(n))
	}
	if rgb, ok := parseColor(tone); ok {
		return lipgloss.Color(toHex(rgb))
	}
	return lipgloss.NoColor{}
}

// Text painted with a theme tone as its foreground or its background
func paint(text, tone string, foreground bool) string {
	if text == "" {
		return ""
	}
	c := toneColor(tone)
	if _, none := c.(lipgloss.NoColor); none {
		return text
	}
	if foreground {
		return lipgloss.NewStyle().Foreground(c).Render(text)
	}
	return lipgloss.NewStyle().Background(c).Render(text)
}

// Style with a theme tone as its foreground
func toneStyle(tone string) lipgloss.Style { return lipgloss.NewStyle().Foreground(toneColor(tone)) }

// Zone every clock of the terminal UI prints in
func clockLocation() *time.Location { return time.Local }

// Hour and minute of an epoch second in the local zone
func clockText(epochSeconds float64) string {
	return time.Unix(int64(epochSeconds), 0).In(clockLocation()).Format("15:04")
}

// Toolkit palette carrying the tones of a skin, so cards, bar and dialogs follow the session theme
func toolkitTheme(t theme, light bool) uitheme.Theme {
	base := uitheme.Blackwall()
	if light {
		base = uitheme.Light()
	}
	set := func(dst *color.Color, tone string) {
		if c := toneColor(tone); c != nil {
			if _, none := c.(lipgloss.NoColor); !none {
				*dst = c
			}
		}
	}
	set(&base.Text, t.Color.Text)
	set(&base.TextDim, t.Color.Muted)
	set(&base.Accent, t.Color.Primary)
	set(&base.Ice, t.Color.Accent)
	set(&base.Pulse, t.Color.Warn)
	set(&base.Alive, t.Color.Ok)
	set(&base.Alarm, t.Color.Error)
	set(&base.SteelBright, t.Color.Border)
	if prompt := trimJS(t.Brand.Prompt); prompt != "" {
		base.Glyphs.Prompt = prompt
	}
	base.Name = "session"
	return base
}

// Whether the terminal UI runs on macOS, where the command key stands in for some control chords
var isMac = runtime.GOOS == "darwin"

// Environment lookup of the process, a variable so tests can stand in for it
var getenv = os.Getenv
