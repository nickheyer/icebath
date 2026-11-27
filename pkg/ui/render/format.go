package render

import (
	"fmt"
	"math"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Cuts a styled string to width, marking the cut with the theme ellipsis
func Truncate(s string, width int, t theme.Theme) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	tail := t.Glyphs.Ellipsis
	if width <= ansi.StringWidth(tail) {
		return ansi.Truncate(tail, width, "")
	}
	return ansi.Truncate(s, width, tail)
}

// Wraps a string at width, breaking long words
func Wrap(s string, width int) string {
	if width <= 0 {
		return s
	}
	return ansi.Hardwrap(ansi.Wordwrap(s, width, ""), width, true)
}

// Count with its noun, pluralized past one
func Count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	suffix := "s"
	switch {
	case strings.HasSuffix(noun, "ch"), strings.HasSuffix(noun, "sh"), strings.HasSuffix(noun, "s"), strings.HasSuffix(noun, "x"):
		suffix = "es"
	case len(noun) > 1 && noun[len(noun)-1] == 'y' && !strings.ContainsRune("aeiou", rune(noun[len(noun)-2])):
		noun, suffix = noun[:len(noun)-1], "ies"
	}
	return fmt.Sprintf("%d %s%s", n, noun, suffix)
}

// Renders text in the style, restarting the style after every reset inside the text so its background runs unbroken
func Paint(st lipgloss.Style, text string) string {
	probe := st.Render("x")
	at := strings.IndexByte(probe, 'x')
	if at <= 0 {
		return st.Render(text)
	}
	text = strings.ReplaceAll(text, "\x1b[0m", "\x1b[m")
	return st.Render(strings.ReplaceAll(text, "\x1b[m", "\x1b[m"+probe[:at]))
}

// Pads or cuts a string to exactly width columns
func Fit(s string, width int, t theme.Theme) string {
	s = Truncate(s, width, t)
	if w := ansi.StringWidth(s); w < width {
		s += strings.Repeat(" ", width-w)
	}
	return s
}

// Duration rounded to a readable unit
func Elapsed(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// Token count with a k or M suffix past a thousand
func Tokens(n int) string {
	switch {
	case n < 0:
		return "0"
	case n < 1000:
		return fmt.Sprint(n)
	case n < 1_000_000:
		return trimZero(fmt.Sprintf("%.1fk", float64(n)/1000))
	}
	return trimZero(fmt.Sprintf("%.2fM", float64(n)/1_000_000))
}

// Byte count in the largest unit that keeps it under a thousand
func Bytes(n int64) string {
	switch {
	case n < 0:
		return "0 B"
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return trimZero(fmt.Sprintf("%.1f KiB", float64(n)/1024))
	case n < 1<<30:
		return trimZero(fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)))
	}
	return trimZero(fmt.Sprintf("%.2f GiB", float64(n)/(1<<30)))
}

// Dollars with cents, or fractions of a cent for tiny amounts
func Money(f float64) string {
	switch {
	case f <= 0:
		return "$0.00"
	case f < 0.01:
		return fmt.Sprintf("$%.4f", f)
	}
	return fmt.Sprintf("$%.2f", f)
}

// Share as a whole percentage
func Percent(f float64) string {
	return fmt.Sprintf("%d%%", int(math.Round(min(max(f, 0), 1)*100)))
}

func trimZero(s string) string {
	if i := strings.Index(s, "."); i >= 0 {
		j := i + 1
		for j < len(s) && (s[j] >= '0' && s[j] <= '9') {
			j++
		}
		digits := strings.TrimRight(s[i:j], "0")
		if digits == "." {
			digits = ""
		}
		return s[:i] + digits + s[j:]
	}
	return s
}

// Spinner frame for a tick from the theme frames
func Spinner(tick int, t theme.Theme) string {
	frames := t.Glyphs.Spinner
	if len(frames) == 0 {
		return ""
	}
	return frames[tick%len(frames)]
}

// Dashed rule of width with the theme info mark at its middle, dashes alone when too narrow for one
func Divider(width int, t theme.Theme) string {
	if width <= 0 {
		return ""
	}
	st := lipgloss.NewStyle().Foreground(t.Steel)
	if width < 7 {
		return st.Render(strings.Repeat(t.Glyphs.Dashed, width))
	}
	left := (width - 3) / 2
	right := width - 3 - left
	return st.Render(strings.Repeat(t.Glyphs.Dashed, left)+" ") + lipgloss.NewStyle().Foreground(t.SteelBright).Render(t.Glyphs.Info) + st.Render(" "+strings.Repeat(t.Glyphs.Dashed, right))
}

// Glyph of a tool icon a tool registers, such as "read" or "shell", the plain tool glyph for an unknown icon
func ToolGlyph(icon string, t theme.Theme) string {
	g := t.Glyphs
	switch icon {
	case "read":
		return g.Read
	case "write":
		return g.Write
	case "edit":
		return g.Edit
	case "find":
		return g.Glob
	case "search":
		return g.Grep
	case "shell":
		return g.Shell
	case "output":
		return g.Output
	case "kill":
		return g.Kill
	case "fetch":
		return g.Fetch
	case "agent":
		return g.Spawn
	case "record":
		return g.Record
	case "plan":
		return g.Plan
	case "server":
		return g.Server
	case "ask":
		return g.Ask
	}
	return g.Tool
}

// Section heading in bold text
func Heading(text string, t theme.Theme) string {
	return lipgloss.NewStyle().Foreground(t.Text).Bold(true).Render(text)
}

// Dim text for secondary information
func Dim(text string, t theme.Theme) string {
	return lipgloss.NewStyle().Foreground(t.TextDim).Render(text)
}

// Text in the alarm color
func Bad(text string, t theme.Theme) string {
	return lipgloss.NewStyle().Foreground(t.Alarm).Render(text)
}

// Text in the alive color
func Good(text string, t theme.Theme) string {
	return lipgloss.NewStyle().Foreground(t.Alive).Render(text)
}

// Text in the accent color
func Warm(text string, t theme.Theme) string {
	return lipgloss.NewStyle().Foreground(t.Accent).Render(text)
}

// Text in the ice color
func Cool(text string, t theme.Theme) string {
	return lipgloss.NewStyle().Foreground(t.Ice).Render(text)
}
