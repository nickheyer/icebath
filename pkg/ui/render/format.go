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

// Cuts a styled string to width, marking the cut with an ellipsis
func Truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return ansi.Truncate(s, width, "…")
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
	if strings.HasSuffix(noun, "ch") || strings.HasSuffix(noun, "s") || strings.HasSuffix(noun, "x") {
		suffix = "es"
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
func Fit(s string, width int) string {
	s = Truncate(s, width)
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

// Horizontal bar of width filled to the fraction, lit in a gradient and dim past the fill
func Bar(fraction float64, width int, t theme.Theme) string {
	if width <= 0 {
		return ""
	}
	fraction = min(max(fraction, 0), 1)
	filled := int(math.Round(fraction * float64(width)))
	if t.Plain {
		return lipgloss.NewStyle().Foreground(t.Ice).Render(strings.Repeat("#", filled)) + lipgloss.NewStyle().Foreground(t.TextDim).Render(strings.Repeat("-", width-filled))
	}
	shades := lipgloss.Blend1D(max(width, 2), t.Pulse, t.Ice)
	var sb strings.Builder
	for i := 0; i < width; i++ {
		if i < filled {
			sb.WriteString(lipgloss.NewStyle().Foreground(shades[i]).Render("█"))
		} else {
			sb.WriteString(lipgloss.NewStyle().Foreground(t.Steel).Render("░"))
		}
	}
	return sb.String()
}

// Bar colored by how close the fraction sits to a danger threshold of one
func Gauge(fraction float64, width int, t theme.Theme) string {
	if width <= 0 {
		return ""
	}
	fraction = min(max(fraction, 0), 1)
	filled := int(math.Round(fraction * float64(width)))
	c := t.Alive
	switch {
	case fraction >= 0.9:
		c = t.Alarm
	case fraction >= 0.7:
		c = t.Accent
	}
	full, empty := "█", "░"
	if t.Plain {
		full, empty = "#", "-"
	}
	return lipgloss.NewStyle().Foreground(c).Render(strings.Repeat(full, filled)) + lipgloss.NewStyle().Foreground(t.Steel).Render(strings.Repeat(empty, width-filled))
}

// Frames of the working spinner
var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner frame for a tick, an ASCII one on plain themes
func Spinner(tick int, t theme.Theme) string {
	if t.Plain {
		return string("|/-\\"[tick%4])
	}
	return spinner[tick%len(spinner)]
}

// Glyph that stands for a tool by name
func ToolGlyph(name string) string {
	switch {
	case name == "Read":
		return "◇"
	case name == "Write":
		return "◆"
	case name == "Edit":
		return "✎"
	case name == "Glob":
		return "✱"
	case name == "Grep":
		return "⌕"
	case name == "Bash":
		return "⌘"
	case name == "BashOutput":
		return "⇣"
	case name == "KillShell":
		return "⊘"
	case name == "WebFetch":
		return "⇄"
	case name == "Agent":
		return "☍"
	case name == "Record":
		return "✎"
	case name == "Plan":
		return "☑"
	case strings.HasPrefix(name, "mcp__"):
		return "⚙"
	}
	return "▸"
}

// Styled key and value pair on one line
func KV(key, value string, t theme.Theme) string {
	return lipgloss.NewStyle().Foreground(t.TextDim).Render(key+" ") + lipgloss.NewStyle().Foreground(t.Text).Render(value)
}

// Section heading in the accent color
func Heading(text string, t theme.Theme) string {
	return lipgloss.NewStyle().Foreground(t.Accent).Bold(true).Render(text)
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

// Text in the pulse color
func Hot(text string, t theme.Theme) string {
	return lipgloss.NewStyle().Foreground(t.Pulse).Render(text)
}

// Lines of a string, an empty string giving none
func Lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// Last n lines of a string
func Tail(s string, n int) string {
	lines := Lines(s)
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// First line of a string cut to width
func Summary(s string, width int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	return Truncate(s, width)
}
