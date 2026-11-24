package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ReleaseDate is the release date shown beside the version in banners
const ReleaseDate = "2026.9.24"

const accentANSIDefault = "\033[1;38;2;156;199;232m"

const resetANSI = "\033[0m"

const streamPad = ""

const streamPartialPreviewLen = 60

// Dim plus italic attributes so dim text inherits the terminal foreground in both modes
const dimANSI = "\x1b[2;3m"

// True color ANSI escape of a #RRGGBB color, dark tuned colors remapped in light mode
func hexToANSI(hexColor string, bold bool) string {
	hexColor = maybeRemapForLightMode(hexColor)
	var rgb [3]int64
	for i, start := range []int{1, 3, 5} {
		n, err := strconv.ParseInt(strings.TrimSpace(pystr.Slice(hexColor, start, start+2)), 16, 0)
		if err != nil {
			if bold {
				return accentANSIDefault
			}
			return "\033[38;2;108;127;146m"
		}
		rgb[i] = n
	}
	prefix := ""
	if bold {
		prefix = "1;"
	}
	return fmt.Sprintf("\033[%s38;2;%d;%d;%dm", prefix, rgb[0], rgb[1], rgb[2])
}

// Terminal programs that default to light mode, none since Apple_Terminal is not reliable
var lightDefaultTermPrograms = map[string]bool{}

// Rec. 709 luma in the unit range of a #RGB or #RRGGBB color, false when malformed
func luminanceFromHex(hexStr string) (float64, bool) {
	s := strings.TrimLeft(strings.TrimSpace(hexStr), "#")
	if pystr.Len(s) == 3 {
		var b strings.Builder
		for _, c := range s {
			b.WriteRune(c)
			b.WriteRune(c)
		}
		s = b.String()
	}
	if pystr.Len(s) != 6 {
		return 0, false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return 0, false
		}
	}
	r, _ := strconv.ParseInt(s[0:2], 16, 0)
	g, _ := strconv.ParseInt(s[2:4], 16, 0)
	b, _ := strconv.ParseInt(s[4:6], 16, 0)
	return (0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)) / 255.0, true
}

var da1ReplyRe = regexp.MustCompile(`\x1b\[\?[0-9;]*c`)

// The light mode detection ladder for the profile of ctx: the theme setting, the terminal's background hint, COLORFGBG, OSC 11, then
// dark, a probe failure returned
func detectLightModeUncached(ctx context.Context) (bool, error) {
	switch settings.Theme.Get(ctx) {
	case "light":
		return true, nil
	case "dark":
		return false, nil
	}
	if lum, ok := luminanceFromHex(os.Getenv("ICEBATH_TUI_BACKGROUND")); ok {
		return lum >= 0.5, nil
	}
	parts := strings.Split(strings.TrimSpace(os.Getenv("COLORFGBG")), ";")
	last := parts[len(parts)-1]
	if pystr.StrIsDigit(last) {
		n, ok := pystr.IntText(last)
		if !ok {
			return false, fmt.Errorf("COLORFGBG background %s is not a whole number", last)
		}
		if n.IsInt64() && n.Int64() >= 0 && n.Int64() < 16 {
			return n.Int64() == 7 || n.Int64() == 15, nil
		}
	}
	bg, ok, err := queryOSC11Background()
	if err != nil {
		return false, err
	}
	if ok {
		if lum, ok := luminanceFromHex(bg); ok {
			return lum >= 0.5, nil
		}
	}
	return lightDefaultTermPrograms[strings.TrimSpace(os.Getenv("TERM_PROGRAM"))], nil
}

var (
	lightModeMu    sync.Mutex
	lightModeCache *bool
)

// Light terminal detection for the profile of ctx, queried once per process and dark on any failure
func detectLightMode(ctx context.Context) bool {
	lightModeMu.Lock()
	defer lightModeMu.Unlock()
	if lightModeCache != nil {
		return *lightModeCache
	}
	result, err := detectLightModeUncached(ctx)
	if err != nil {
		result = false
	}
	lightModeCache = &result
	return result
}

// Light mode equivalents of the dark skins' colors unreadable on paper backgrounds, standalone foregrounds only
var lightModeRemap = map[string]string{
	"#dfe9f2": "#141c26", "#6c7f92": "#647081", "#9cc7e8": "#2f6f9f", "#e3122b": "#c8102e",
	"#7fc7a5": "#2e8b63", "#ff4757": "#d12f3f", "#e0b35a": "#9a6b00", "#3b5169": "#7d93a9",
	"#d6def7": "#141c26", "#ffc857": "#9a6b00", "#ffd98a": "#9a6b00", "#5ff1ff": "#1d7f8f", "#7cff9e": "#2e8b63",
	"#ffffff": "#000000", "#bfbfbf": "#404040", "#ffff00": "#7a6a00", "#00ffff": "#006b6b", "#00ff00": "#006b00",
	"#00e038": "#006b1b", "#00ff41": "#006b1b", "#00cc34": "#006b1b", "#ffb000": "#8a5a00",
	"#f8f8f2": "#282a36", "#6272a4": "#6c664b", "#8be9fd": "#036a96", "#bd93f9": "#644ac9",
	"#50fa7b": "#14710a", "#ffb86c": "#a34d14", "#ff79c6": "#a3144d", "#ff5555": "#cb3a2a",
}

var lightModeRemapUpper = func() map[string]string {
	out := make(map[string]string, len(lightModeRemap))
	for k, v := range lightModeRemap {
		out[strings.ToUpper(k)] = v
	}
	return out
}()

// In light mode, the higher contrast equivalent of a dark tuned color; the colour filter knows no context, so the theme is the one of
// this process's profile, which the profile flag sets for the whole process
func maybeRemapForLightMode(hexColor string) string {
	if !detectLightMode(context.Background()) {
		return hexColor
	}
	if hexColor == "" || !strings.HasPrefix(hexColor, "#") {
		return hexColor
	}
	if mapped, ok := lightModeRemapUpper[strings.ToUpper(hexColor)]; ok {
		return mapped
	}
	return hexColor
}

// skinAwareANSI is an ANSI escape resolved from the skin on first use and reset after a skin switch
type skinAwareANSI struct {
	skinKey     string
	fallbackHex string
	bold        bool
	mu          sync.Mutex
	cached      *string
}

// The escape, read from the active skin once and cached
func (s *skinAwareANSI) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached == nil {
		hex := s.fallbackHex
		if skin := config.GetActiveSkin(context.Background()); skin != nil {
			hex = skin.GetColor(s.skinKey, s.fallbackHex)
		}
		v := hexToANSI(hex, s.bold)
		s.cached = &v
	}
	return *s.cached
}

var accentANSI = &skinAwareANSI{skinKey: "response_border", fallbackHex: "#9cc7e8", bold: true}

// The active skin accent color for legacy CLI output lines
func accentHex() string {
	if skin := config.GetActiveSkin(context.Background()); skin != nil {
		return skin.GetColor("ui_accent", "#9cc7e8")
	}
	return "#9cc7e8"
}

var richANSITokenRe = regexp.MustCompile(`(?:\x1b\](.*?)\x1b\\)|(?:\x1b([(@-Z\\-_]|\[[0-?]*[ -/]*[@-~]))`)

// Plain text of ANSI output as rich Text.from_ansi reads it, lines rejoined by newlines
func richPlainFromANSI(text string) string {
	lines := pystr.Splitlines(text, false)
	for i, line := range lines {
		lines[i] = richANSITokenRe.ReplaceAllString(line, "")
	}
	return strings.Join(lines, "\n")
}

var markdownStripSteps = []struct {
	re  *regexp2.Regexp
	rep string
}{
	{regexp2.MustCompile(`^\s{0,3}(?:[-_]\s*){3,}$`, regexp2.Multiline), ""},
	{regexp2.MustCompile(`^\s{0,3}(?:\*\s*){3}\s*$`, regexp2.Multiline), ""},
	{regexp2.MustCompile(`^\s{0,3}#{1,6}\s+`, regexp2.Multiline), ""},
	{regexp2.MustCompile("(```+|~~~+)", regexp2.None), ""},
	{regexp2.MustCompile("`([^`]*)`", regexp2.None), "$1"},
	{regexp2.MustCompile(`!\[([^\]]*)\]\([^\)]*\)`, regexp2.None), "$1"},
	{regexp2.MustCompile(`\[([^\]]+)\]\([^\)]*\)`, regexp2.None), "$1"},
	{regexp2.MustCompile(`\*\*\*([^*]+)\*\*\*`, regexp2.None), "$1"},
	{regexp2.MustCompile(`(?<!\w)___([^_]+)___(?!\w)`, regexp2.None), "$1"},
	{regexp2.MustCompile(`\*\*([^*]+)\*\*`, regexp2.None), "$1"},
	{regexp2.MustCompile(`(?<!\w)__([^_]+)__(?!\w)`, regexp2.None), "$1"},
	{regexp2.MustCompile(`\*([^\s*][^*]*?[^\s*])\*`, regexp2.None), "$1"},
	{regexp2.MustCompile(`(?<!\w)_([^_]+)_(?!\w)`, regexp2.None), "$1"},
	{regexp2.MustCompile(`~~([^~]+)~~`, regexp2.None), "$1"},
	{regexp2.MustCompile(`\n{3,}`, regexp2.None), "\n\n"},
}

// Markdown markers removed for plain text display, structure carrying quotes, lists and checkboxes kept
func stripMarkdownSyntax(text string) string {
	plain := richPlainFromANSI(text)
	for _, step := range markdownStripSteps {
		plain, _ = step.re.Replace(plain, step.rep, -1, -1)
	}
	return strings.Trim(plain, "\n")
}

var windowsPathWithDotSegmentRe = regexp.MustCompile(`(?i)(?:\b[a-z]:\\|\\\\)[^\s` + "`" + `]*\\\.[^\s` + "`" + `]*`)

// Doubles the backslash before hidden dirs in Windows paths so CommonMark keeps the dot
func preserveWindowsDotSegmentsForMarkdown(text string) string {
	if !strings.Contains(text, `\.`) {
		return text
	}
	return windowsPathWithDotSegmentRe.ReplaceAllStringFunc(text, func(match string) string {
		var b strings.Builder
		runes := []rune(match)
		for i, r := range runes {
			b.WriteRune(r)
			if r == '\\' && i+1 < len(runes) && runes[i+1] == '.' && (i == 0 || runes[i-1] != '\\') {
				b.WriteRune('\\')
			}
		}
		return b.String()
	})
}

// Terminal columns, 80 when unknown
func terminalColumns() int {
	cols, _ := pyshutil.GetTerminalSize(80, 24)
	return cols
}

// Display cells inside the streamed response box with a small margin for resize races
func terminalWidthForStreaming() int { return max(20, terminalColumns()-pystr.Len(streamPad)-2) }

// Final assistant content as markdown, stripped text or raw text
func renderFinalAssistantContent(text, mode string) pyrich.Renderable {
	panelWidth := max(20, terminalColumns()-4)
	switch strings.ToLower(strings.TrimSpace(cliStr(mode, "render"))) {
	case "strip":
		return &pyrich.Text{Plain: tools.RealignMarkdownTables(stripMarkdownSyntax(text), &panelWidth)}
	case "raw":
		return &pyrich.Text{Plain: text}
	}
	plain := richPlainFromANSI(text)
	plain = preserveWindowsDotSegmentsForMarkdown(plain)
	plain = tools.RealignMarkdownTables(plain, &panelWidth)
	return &pyrich.Markdown{Source: plain}
}

// Text still to display after a streamed response transform, the suffix or the whole replaced response
func postStreamTransformOutput(response string, result map[string]any) string {
	if result == nil || !cliTruthy(result["response_transformed"]) {
		return ""
	}
	original := cliStr(result["pre_transform_response"], "")
	if original != "" && strings.HasPrefix(response, original) {
		return response[len(original):]
	}
	return "\n" + i18n.T(context.Background(), "cli.render.response_transformed", "", nil) + "\n" + response
}

// Prints ANSI text and a newline straight to the console
func ptPrintANSI(text string) { fmt.Fprintln(console().Out, text) }

// Prints ANSI text and a newline straight to the console
func cprint(text string) { ptPrintANSI(text) }

var oscEscapeRe = regexp.MustCompile(`\x1b\][\s\S]*?(?:\x07|\x1b\\)`)

// ChatConsole is the rich console drop in routing rendered ANSI through cprint so colors survive the prompt
type ChatConsole struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	inner *pyrich.Console
}

// A chat console rendering true color into a buffer that each print flushes line by line
func newChatConsole() *ChatConsole {
	c := &ChatConsole{}
	c.inner = &pyrich.Console{Out: &c.buf, Color: true, Terminal: true, NoHighlight: true, Width: 80, ColorSystem: "truecolor"}
	return c
}

// Renders through the inner console at the terminal width and flushes every line through cprint
func (c *ChatConsole) emit(render func(*pyrich.Console)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
	c.inner.Width = terminalColumns()
	render(c.inner)
	rendered := strings.TrimRight(oscEscapeRe.ReplaceAllString(c.buf.String(), ""), "\n")
	for _, line := range strings.Split(rendered, "\n") {
		cprint(line)
	}
}

// Prints markup lines through cprint
func (c *ChatConsole) Print(markup ...string) { c.emit(func(r *pyrich.Console) { r.Print(markup...) }) }

// Prints a renderable through cprint with a base style under it and soft wrapping when asked
func (c *ChatConsole) PrintRenderableWith(r pyrich.Renderable, o pyrich.PrintOptions) (err error) {
	c.emit(func(con *pyrich.Console) { err = con.PrintRenderableWith(r, o) })
	return err
}

// Prints a panel through cprint
func (c *ChatConsole) PrintPanel(p pyrich.Panel) { c.emit(func(r *pyrich.Console) { r.PrintPanel(p) }) }

var defaultChatConsole = newChatConsole()

// Prints rich markup through the chat console
func chatConsolePrint(markup string) { defaultChatConsole.Print(markup) }
