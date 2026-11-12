package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dlclark/regexp2/v2"
	"github.com/mattn/go-runewidth"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ReleaseDate is the release date shown beside the version in banners
const ReleaseDate = "2026.9.24"

var reasoningTags = provider.ThinkTagNames

// Banner version label, swapped by tests that pin the version text
var bannerVersionLabel = formatBannerVersionLabel

var toolCallTags = []string{"tool_call", "tool_calls", "tool_result", "function_call", "function_calls"}

// Reasoning tag patterns, closed block, unterminated block and orphan closer per tag
var reasoningTagRes = func() [][3]*regexp.Regexp {
	out := make([][3]*regexp.Regexp, 0, len(reasoningTags))
	for _, tag := range reasoningTags {
		q := regexp.QuoteMeta(tag)
		out = append(out, [3]*regexp.Regexp{
			regexp.MustCompile(`(?is)<` + q + `>.*?</` + q + `>\s*`),
			regexp.MustCompile(`(?is)<` + q + `>.*$`),
			regexp.MustCompile(`(?i)</` + q + `>\s*`)})
	}
	return out
}()

// Leaked tool call XML block patterns, namespaced tags included
var toolCallTagRes = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(toolCallTags))
	for _, tag := range toolCallTags {
		out = append(out, regexp.MustCompile(`(?is)<(?:[\w.-]+:)?`+tag+`\b[^>]*>.*?</(?:[\w.-]+:)?`+tag+`>\s*`))
	}
	return out
}()

var namedFunctionCallRe = regexp2.MustCompile(`(?:(?<=^)|(?<=[\n\r.!?:]))[ \t]*<function\b[^>]*\bname\s*=[^>]*>(?:(?:(?!</function>).)*)</function>\s*`, regexp2.Singleline|regexp2.IgnoreCase)

// Strips reasoning blocks, closed, unterminated or orphan closed, and leaked tool call XML from display text
func stripReasoningTags(text string) string {
	cleaned := text
	for _, res := range reasoningTagRes {
		for _, re := range res {
			cleaned = re.ReplaceAllString(cleaned, "")
		}
	}
	for _, re := range toolCallTagRes {
		cleaned = re.ReplaceAllString(cleaned, "")
	}
	cleaned, _ = namedFunctionCallRe.Replace(cleaned, "", -1, -1)
	cleaned = provider.StripStrayToolCallClosers(cleaned)
	cleaned = provider.StripUnterminatedToolCalls(cleaned)
	return pystr.Strip(cleaned)
}

// Assistant content as text, text parts of a content list joined by newlines
func assistantContentAsText(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var parts []string
		for _, part := range v {
			m, ok := part.(map[string]any)
			if !ok || m["type"] != "text" {
				continue
			}
			if text := cliStr(m["text"], ""); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return cliStr(content, "")
}

// Assistant content as the text a copy command takes, reasoning tags stripped
func assistantCopyText(content any) string {
	return stripReasoningTags(assistantContentAsText(content))
}

const accentANSIDefault = "\033[1;38;2;255;215;0m"

const boldANSI = "\033[1m"

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
			return "\033[38;2;184;134;11m"
		}
		rgb[i] = n
	}
	prefix := ""
	if bold {
		prefix = "1;"
	}
	return fmt.Sprintf("\033[%s38;2;%d;%d;%dm", prefix, rgb[0], rgb[1], rgb[2])
}

var (
	trueRe  = regexp.MustCompile(`^(1|true|on|yes|y)$`)
	falseRe = regexp.MustCompile(`^(0|false|off|no|n)$`)
)

// Terminal programs that default to light mode, none since Apple_Terminal is not reliable
var lightDefaultTermPrograms = map[string]bool{}

// Rec. 709 luma in the unit range of a #RGB or #RRGGBB color, false when malformed
func luminanceFromHex(hexStr string) (float64, bool) {
	s := pystr.LstripChars(pystr.Strip(hexStr), "#")
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

// The light mode detection ladder, env overrides, theme, background, COLORFGBG, OSC 11, then dark
func detectLightModeUncached() bool {
	for _, v := range []string{"ICEBATH_LIGHT", "ICEBATH_TUI_LIGHT"} {
		val := pystr.Lower(pystr.Strip(os.Getenv(v)))
		if trueRe.MatchString(val) {
			return true
		}
		if falseRe.MatchString(val) {
			return false
		}
	}
	switch pystr.Lower(pystr.Strip(os.Getenv("ICEBATH_TUI_THEME"))) {
	case "light":
		return true
	case "dark":
		return false
	}
	if lum, ok := luminanceFromHex(os.Getenv("ICEBATH_TUI_BACKGROUND")); ok {
		return lum >= 0.5
	}
	parts := strings.Split(pystr.Strip(os.Getenv("COLORFGBG")), ";")
	last := parts[len(parts)-1]
	if pystr.StrIsDigit(last) {
		if n, ok := pystr.IntText(last); ok && n.IsInt64() && n.Int64() >= 0 && n.Int64() < 16 {
			return n.Int64() == 7 || n.Int64() == 15
		}
	}
	if bg, ok := queryOSC11Background(); ok {
		if lum, ok := luminanceFromHex(bg); ok {
			return lum >= 0.5
		}
	}
	return lightDefaultTermPrograms[pystr.Strip(os.Getenv("TERM_PROGRAM"))]
}

var (
	lightModeMu    sync.Mutex
	lightModeCache *bool
)

// Light terminal detection, queried once per process and dark on any failure
func detectLightMode() bool {
	lightModeMu.Lock()
	defer lightModeMu.Unlock()
	if lightModeCache != nil {
		return *lightModeCache
	}
	result := detectLightModeUncached()
	lightModeCache = &result
	return result
}

// Forgets the cached light mode so the next read detects again
func resetLightModeCache() {
	lightModeMu.Lock()
	lightModeCache = nil
	lightModeMu.Unlock()
}

// Installs the skin color remap and detects light mode before the prompt owns the tty
func primeLightModeForInteractive() {
	installSkinLightModeHook()
	if console().StdinIsTTY() && console().StdoutIsTTY() {
		detectLightMode()
	}
}

// Light mode equivalents of skin colors unreadable on cream backgrounds, standalone foregrounds only
var lightModeRemap = map[string]string{
	"#FFF8DC": "#1A1A1A", "#FFD700": "#9A6B00", "#FFBF00": "#8A5A00", "#B8860B": "#5C4500",
	"#DAA520": "#6B4F00", "#F1E6CF": "#1A1A1A", "#c9d1d9": "#24292F", "#EAF7FF": "#0F1B26",
	"#F5F5F5": "#1A1A1A", "#FFF0D4": "#1A1A1A", "#CD7F32": "#8A4F1A", "#FFEFB5": "#3A2A00",
}

var lightModeRemapUpper = func() map[string]string {
	out := make(map[string]string, len(lightModeRemap))
	for k, v := range lightModeRemap {
		out[strings.ToUpper(k)] = v
	}
	return out
}()

// In light mode, the higher contrast equivalent of a dark tuned color
func maybeRemapForLightMode(hexColor string) string {
	if !detectLightMode() {
		return hexColor
	}
	if hexColor == "" || !strings.HasPrefix(hexColor, "#") {
		return hexColor
	}
	if mapped, ok := lightModeRemapUpper[pystr.Upper(hexColor)]; ok {
		return mapped
	}
	return hexColor
}

// Routes every skin color read through the light mode remap, idempotently
func installSkinLightModeHook() { config.InstallSkinColorFilter(maybeRemapForLightMode) }

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

// Clears the cache so the next access re reads the skin
func (s *skinAwareANSI) Reset() {
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
}

var accentANSI = &skinAwareANSI{skinKey: "response_border", fallbackHex: "#FFD700", bold: true}

// Wraps the text in an SGR attribute when stdout is a real tty, plain otherwise
func ttyWrap(s, sgr string) string {
	if console().StdoutIsTTY() {
		return sgr + s + "\x1b[0m"
	}
	return s
}

// Bold when stdout is a real tty
func boldTTY(s string) string { return ttyWrap(s, "\x1b[1m") }

// Dim italic when stdout is a real tty
func dimTTY(s string) string { return ttyWrap(s, "\x1b[2;3m") }

// The active skin accent color for legacy CLI output lines
func accentHex() string {
	if skin := config.GetActiveSkin(context.Background()); skin != nil {
		return skin.GetColor("ui_accent", "#FFBF00")
	}
	return "#FFBF00"
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
	return pystr.StripChars(plain, "\n")
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
	switch pystr.Lower(pystr.Strip(cliStr(mode, "render"))) {
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

// Clamps the persistent output line limit to at least 10, 200 when the value does not parse
func coerceOutputHistoryLimit(value any) int {
	n, ok := pyIntOf(value)
	if !ok {
		return 200
	}
	return max(10, n)
}

// paintedLine is a recorded output line tagged with the width it was painted at
type paintedLine struct {
	Text  string
	Width *int
	plain bool
}

// Drops the recorded output and the chrome floor cleared with the screen
func clearOutputHistory() {
	outputHistoryMu.Lock()
	outputHistory = nil
	outputHistoryMu.Unlock()
	setChromeFloor(nil)
}

// Reports whether output is being recorded for replay
func outputHistoryRecording() bool {
	outputHistoryMu.Lock()
	defer outputHistoryMu.Unlock()
	return outputHistoryEnabled && !outputHistoryReplaying && !outputHistorySuppressed
}

// Appends entries to the bounded history, the oldest dropped past the line limit
func appendOutputHistory(entries ...any) {
	outputHistoryMu.Lock()
	defer outputHistoryMu.Unlock()
	outputHistory = append(outputHistory, entries...)
	if over := len(outputHistory) - outputHistoryMaxLines; over > 0 {
		outputHistory = append([]any(nil), outputHistory[over:]...)
	}
}

// Records a line, a painted line or a callable rendering lines when recording is on
func recordOutputHistoryEntry(entry any) {
	if !outputHistoryRecording() {
		return
	}
	if text, ok := entry.(string); ok {
		entry = &paintedLine{Text: text, plain: true}
	}
	appendOutputHistory(entry)
}

// Width the terminal soft wraps a print at right now, nil when unknown
func paintedColumns() *int {
	if app := tuiApp(); app != nil {
		if cols, _, ok := app.Size(); ok {
			return &cols
		}
		return nil
	}
	_, cols, err := console().Size()
	if err != nil {
		return nil
	}
	return &cols
}

// Records text as painted now, force skipping the recording check made at print request time
func recordOutputHistory(text string, force bool) {
	if !force && !outputHistoryRecording() {
		return
	}
	width := paintedColumns()
	var lines []any
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		lines = append(lines, &paintedLine{Text: line, Width: width})
	}
	appendOutputHistory(lines...)
}

var ansiSequenceRe = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

// The line without its first visible cells, escape sequences kept for their styling
func ansiDropCells(line string, cells int) string {
	var out strings.Builder
	pos := 0
	drop := func(segment string) {
		for _, ch := range segment {
			if cells > 0 {
				cells -= runewidth.RuneWidth(ch)
			} else {
				out.WriteRune(ch)
			}
		}
	}
	for _, loc := range ansiSequenceRe.FindAllStringIndex(line, -1) {
		drop(line[pos:loc[0]])
		out.WriteString(line[loc[0]:loc[1]])
		pos = loc[1]
	}
	drop(line[pos:])
	return out.String()
}

var (
	reflowTermPrefixes   = []string{"tmux", "screen", "xterm-kitty", "alacritty", "foot", "xterm-ghostty", "wezterm", "contour", "vte"}
	noReflowTermPrefixes = []string{"linux", "st", "mosh", "vt", "cons", "rxvt"}
	multiplexerEnv       = []string{"TMUX", "STY"}
	noReflowEnv          = []string{"XTERM_VERSION"}
	reflowEnv            = []string{"VTE_VERSION", "KITTY_WINDOW_ID", "WT_SESSION", "KONSOLE_VERSION", "ALACRITTY_WINDOW_ID", "WEZTERM_PANE", "GHOSTTY_RESOURCES_DIR"}
	reflowTermPrograms   = []string{"iTerm.app", "Apple_Terminal", "WezTerm", "vscode", "ghostty", "Tabby", "Hyper"}
)

// Reports one of the environment names set
func anyEnvSet(names []string) bool {
	for _, name := range names {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

// Reports one of the prefixes starting the text
func hasAnyPrefix(text string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// Whether the terminal re wraps shown rows when its width changes, known false when nothing says
func terminalReflows() (bool, bool) {
	term := strings.ToLower(os.Getenv("TERM"))
	if hasAnyPrefix(term, reflowTermPrefixes) {
		return true, true
	}
	if hasAnyPrefix(term, noReflowTermPrefixes) {
		return false, true
	}
	if anyEnvSet(multiplexerEnv) {
		return true, true
	}
	if anyEnvSet(noReflowEnv) {
		return false, true
	}
	if anyEnvSet(reflowEnv) {
		return true, true
	}
	for _, program := range reflowTermPrograms {
		if os.Getenv("TERM_PROGRAM") == program {
			return true, true
		}
	}
	if os.Getenv("LC_TERMINAL") == "iTerm2" {
		return true, true
	}
	return false, false
}

// Rows the line fills when the terminal soft wraps it at columns
func lineRows(line string, columns int) int {
	width := runewidth.StringWidth(ansiSequenceRe.ReplaceAllString(line, ""))
	if columns <= 0 {
		return 1
	}
	return max(1, (width+columns-1)/columns)
}

// Newest lines filling maxRows, wrapped at the painted width or columns, the oldest cut to its bottom rows
func outputTailFitting(lines []*paintedLine, maxRows, columns int, painted bool) []*paintedLine {
	var kept []*paintedLine
	used := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if used >= maxRows {
			break
		}
		line := lines[i]
		cols := columns
		if painted && line.Width != nil && *line.Width != 0 {
			cols = *line.Width
		}
		height := lineRows(line.Text, cols)
		if used+height > maxRows {
			kept = append(kept, &paintedLine{Text: ansiDropCells(line.Text, (height-(maxRows-used))*cols), plain: true})
			break
		}
		used += height
		kept = append(kept, line)
	}
	for l, r := 0, len(kept)-1; l < r; l, r = l+1, r-1 {
		kept[l], kept[r] = kept[r], kept[l]
	}
	return kept
}

// The recorded output as the lines a replay paints, callable entries rendered now
func outputHistoryLines() []*paintedLine {
	outputHistoryMu.Lock()
	entries := append([]any(nil), outputHistory...)
	outputHistoryMu.Unlock()
	var rendered []*paintedLine
	for _, entry := range entries {
		switch e := entry.(type) {
		case *paintedLine:
			rendered = append(rendered, e)
		case func() []string:
			for _, line := range e() {
				rendered = append(rendered, &paintedLine{Text: line, plain: true})
			}
		case func() string:
			for _, line := range pystr.Splitlines(e(), false) {
				rendered = append(rendered, &paintedLine{Text: line, plain: true})
			}
		case string:
			rendered = append(rendered, &paintedLine{Text: e, plain: true})
		}
	}
	return rendered
}

// Rows the whole recorded output fills, nil when that reaches the limit or recording is off
func outputHistoryRows(limit, columns int, painted bool) *int {
	outputHistoryMu.Lock()
	enabled := outputHistoryEnabled
	outputHistoryMu.Unlock()
	if !enabled {
		return nil
	}
	total := 0
	lines := outputHistoryLines()
	for i := len(lines) - 1; i >= 0; i-- {
		cols := columns
		if painted && lines[i].Width != nil && *lines[i].Width != 0 {
			cols = *lines[i].Width
		}
		total += lineRows(lines[i].Text, cols)
		if total >= limit {
			return nil
		}
	}
	return &total
}

// Prints ANSI text and a newline straight to the console
func ptPrintANSI(text string) { fmt.Fprintln(console().Out, text) }

// heldPaint is one paint deferred while a resize awaits its recovery
type heldPaint struct {
	seq   int64
	paint func()
}

// paintGateEntry is the running application and the gate telling when output must wait for a resize recovery
type paintGateEntry struct {
	app  tuiApplication
	gate func() bool
}

var (
	heldPaintsMu  sync.Mutex
	heldPaints    []heldPaint
	paintsHeld    bool
	paintSeq      atomic.Int64
	renderStateMu sync.Mutex
	paintGate     *paintGateEntry
	chromeFloor   *int
	suspectRows   int
)

// Installs the resize gate of the running application, nil clearing it
func setPaintGate(app tuiApplication, gate func() bool) {
	renderStateMu.Lock()
	defer renderStateMu.Unlock()
	if gate == nil {
		paintGate = nil
		return
	}
	paintGate = &paintGateEntry{app: app, gate: gate}
}

// Records the rows from the top of the prompt chrome to the bottom of the screen, nil when unknown
func setChromeFloor(rows *int) {
	renderStateMu.Lock()
	chromeFloor = rows
	renderStateMu.Unlock()
}

// Rows from the top of the prompt chrome to the bottom of the screen, nil when unknown
func currentChromeFloor() *int {
	renderStateMu.Lock()
	defer renderStateMu.Unlock()
	return chromeFloor
}

// Adds rows output scrolled into scrollback while the width changed under it
func addSuspectRows(rows int) {
	renderStateMu.Lock()
	suspectRows += max(0, rows)
	renderStateMu.Unlock()
}

// Takes and resets the suspect row count
func takeSuspectRows() int {
	renderStateMu.Lock()
	defer renderStateMu.Unlock()
	rows := suspectRows
	suspectRows = 0
	return rows
}

// Holds cprint paints until released while a resize awaits its recovery
func holdPaints() {
	heldPaintsMu.Lock()
	if !paintsHeld {
		paintsHeld, heldPaints = true, nil
	}
	heldPaintsMu.Unlock()
}

// Paints, in request order, what holdPaints held
func releasePaints() {
	heldPaintsMu.Lock()
	held := heldPaints
	heldPaints, paintsHeld = nil, false
	heldPaintsMu.Unlock()
	sort.SliceStable(held, func(i, j int) bool { return held[i].seq < held[j].seq })
	for _, h := range held {
		func() {
			defer func() { _ = recover() }()
			h.paint()
		}()
	}
}

// Queues the paint when paints are held, or must be because the width changed under the app
func paintHeld(seq int64, paint func(), app tuiApplication) bool {
	renderStateMu.Lock()
	gate := paintGate
	renderStateMu.Unlock()
	if gate != nil && gate.app == app && gate.gate() {
		holdPaints()
	}
	heldPaintsMu.Lock()
	defer heldPaintsMu.Unlock()
	if !paintsHeld {
		return false
	}
	heldPaints = append(heldPaints, heldPaint{seq: seq, paint: paint})
	return true
}

// Prints ANSI text through the live application renderer, else straight to the console
func cprint(text string) {
	app := tuiApp()
	if app == nil || !app.IsRunning() {
		recording := outputHistoryRecording()
		releasePaints()
		if recording {
			recordOutputHistory(text, true)
		}
		ptPrintANSI(text)
		return
	}
	paintThroughTUIApp(text)
}

// Paints text above the running prompt, on the loop directly, from elsewhere scheduled onto it
func paintThroughTUIApp(text string) {
	app := tuiApp()
	recording := outputHistoryRecording()
	seq := paintSeq.Add(1)
	painted := func(paint func()) func() {
		return func() {
			if recording {
				recordOutputHistory(text, true)
			}
			paint()
		}
	}
	paintFallback := painted(func() { ptPrintANSI(text) })
	if app == nil || !app.IsRunning() {
		releasePaints()
		paintFallback()
		return
	}
	paintPT := painted(func() { app.PrintAbove(text) })
	if app.OnLoop() {
		if !paintHeld(seq, paintPT, app) {
			paintPT()
		}
		return
	}
	var schedule func()
	schedule = func() {
		if !app.IsRunning() {
			paintFallback()
			return
		}
		if paintHeld(seq, schedule, app) {
			return
		}
		if app.TerminalBusy() {
			app.RunInTerminal(func() {
				if !paintHeld(seq, schedule, app) {
					paintPT()
				}
			})
			return
		}
		floor := currentChromeFloor()
		if floor != nil {
			f := max(*floor, app.ChromeRows())
			floor = &f
		}
		columns, _, _ := app.Size()
		paintPT()
		printed := 0
		for _, line := range strings.Split(text, "\n") {
			printed += lineRows(line, columns)
		}
		if cols, _, _ := app.Size(); cols != columns {
			addSuspectRows(printed)
		}
		if floor != nil {
			f := max(0, *floor-printed)
			setChromeFloor(&f)
		}
	}
	if !app.CallSoonThreadsafe(schedule) {
		paintFallback()
	}
}

// replayFit limits a history replay to the newest lines fitting rows from screen row top
type replayFit struct {
	Rows, Columns int
	Painted       bool
	Top           *int
}

// Repaints recent output above the prompt after a full screen clear
func replayOutputHistory(fit *replayFit, output tuiApplication) {
	outputHistoryMu.Lock()
	if !outputHistoryEnabled || len(outputHistory) == 0 {
		outputHistoryMu.Unlock()
		return
	}
	outputHistoryReplaying = true
	outputHistoryMu.Unlock()
	defer func() {
		outputHistoryMu.Lock()
		outputHistoryReplaying = false
		outputHistoryMu.Unlock()
	}()
	rendered := outputHistoryLines()
	var top *int
	columns := 0
	if fit != nil {
		rendered = outputTailFitting(rendered, fit.Rows, fit.Columns, fit.Painted)
		columns, top = fit.Columns, fit.Top
	}
	if len(rendered) == 0 {
		return
	}
	texts := make([]string, len(rendered))
	for i, line := range rendered {
		texts[i] = line.Text
	}
	joined := strings.Join(texts, "\n")
	if output == nil {
		if app := tuiApp(); app != nil && app.IsRunning() {
			app.PrintAbove(joined)
		} else {
			ptPrintANSI(joined)
		}
	} else {
		output.PrintRaw(joined + "\n")
		cols, rows, _ := output.Size()
		if top != nil {
			t := *top
			for _, line := range rendered {
				t += lineRows(line.Text, columns)
			}
			f := max(0, rows-t)
			setChromeFloor(&f)
			if cols != columns {
				addSuspectRows(t + 1 - rows)
			}
		}
	}
	width := paintedColumns()
	if fit != nil {
		w := columns
		width = &w
	}
	for _, line := range rendered {
		if !line.plain {
			line.Width = width
		}
	}
}

// Prepends a one shot note to a user message, folded into the first text part of a content list
func prependNoteToMessage(message any, note string) any {
	note = pystr.Strip(note)
	if note == "" {
		return message
	}
	switch m := message.(type) {
	case string:
		if m == "" {
			return note
		}
		return note + "\n\n" + m
	case []any:
		parts := append([]any(nil), m...)
		for i, part := range parts {
			dict, ok := part.(map[string]any)
			if !ok || dict["type"] != "text" {
				continue
			}
			text := cliStr(dict["text"], "")
			copied := make(map[string]any, len(dict)+1)
			for k, v := range dict {
				copied[k] = v
			}
			if text == "" {
				copied["text"] = note
			} else {
				copied["text"] = note + "\n\n" + text
			}
			parts[i] = copied
			return parts
		}
		return append([]any{map[string]any{"type": "text", "text": note}}, parts...)
	}
	return message
}

// Reports whether a prompt application currently owns the live terminal
func PromptAppIsRunning() bool { return ptAppIsRunning() }

func ptAppIsRunning() bool {
	app := tuiApp()
	return app != nil && app.IsRunning()
}

// Prints to the console unless a prompt application owns the terminal, then through it
func cliVisiblePrint(text string) {
	if ptAppIsRunning() {
		cprint(text)
		return
	}
	fmt.Fprintln(console().Out, text)
}

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

// Prints markup on unwrapped lines through cprint so the terminal reflows them
func (c *ChatConsole) PrintSoft(markup string) {
	c.emit(func(r *pyrich.Console) { r.PrintSoft(markup) })
}

// Prints a renderable through cprint
func (c *ChatConsole) PrintRenderable(r pyrich.Renderable) {
	c.emit(func(con *pyrich.Console) { con.PrintRenderable(r) })
}

// Prints a renderable through cprint with a base style under it and soft wrapping when asked
func (c *ChatConsole) PrintRenderableWith(r pyrich.Renderable, o pyrich.PrintOptions) (err error) {
	c.emit(func(con *pyrich.Console) { err = con.PrintRenderableWith(r, o) })
	return err
}

// Prints a table through cprint
func (c *ChatConsole) PrintTable(t *pyrich.Table) {
	c.emit(func(r *pyrich.Console) { r.PrintTable(t) })
}

// Prints a panel through cprint
func (c *ChatConsole) PrintPanel(p pyrich.Panel) { c.emit(func(r *pyrich.Console) { r.PrintPanel(p) }) }

// No op status so slash helpers do not duplicate the busy command indicator
func (c *ChatConsole) Status(string) func() { return func() {} }

var defaultChatConsole = newChatConsole()

// Prints rich markup through the chat console
func chatConsolePrint(markup string) { defaultChatConsole.Print(markup) }

// A compact banner that fits the current terminal width
func buildCompactBanner() string {
	ctx := context.Background()
	skin := config.GetActiveSkin(ctx)
	color := func(key, def string) string {
		if skin != nil {
			return skin.GetColor(key, def)
		}
		return def
	}
	borderColor := color("banner_border", "#FFD700")
	titleColor := color("banner_title", "#FFBF00")
	dimColor := color("banner_dim", "#B8860B")
	tinyLine := "☤ NOUS ICEBATH"
	if skin != nil && skin.Name != "default" {
		tinyLine = skin.GetBranding("agent_name", "Icebath Agent")
	}
	line1 := i18n.T(ctx, "cli.render.banner_tagline", "", map[string]any{"name": tinyLine})
	var versionLine string
	if os.Getenv("ICEBATH_FAST_STARTUP_BANNER") == "1" {
		info, err := updaterecord.GetVersionInfo()
		if err != nil {
			info = updaterecord.VersionInfo{BaseVersion: "unknown", DerivedVersion: "unknown", Source: "unknown"}
		}
		versionLine = i18n.T(ctx, "cli.render.banner_version", "", map[string]any{"version": info.DerivedVersion, "date": ReleaseDate})
	} else {
		versionLine = bannerVersionLabel()
	}
	w := min(terminalColumns()-2, 88)
	if w < 30 {
		return "\n[" + titleColor + "]" + tinyLine + "[/] [dim " + dimColor + "]- Nous Research[/]\n"
	}
	inner := w - 2
	bar := strings.Repeat("═", w)
	contentWidth := inner - 2
	line1 = pystr.Ljust(pystr.Slice(line1, 0, contentWidth), contentWidth)
	line2 := pystr.Ljust(pystr.Slice(versionLine, 0, contentWidth), contentWidth)
	border := "[bold " + borderColor + "]"
	return "\n" + border + "╔" + bar + "╗[/]\n" +
		border + "║[/] [" + titleColor + "]" + line1 + "[/] " + border + "║[/]\n" +
		border + "║[/] [dim " + dimColor + "]" + line2 + "[/] " + border + "║[/]\n" +
		border + "╚" + bar + "╝[/]\n"
}

// Stable panel width wide enough for the title and content including borders
func panelBoxWidth(title string, contentLines []string, minWidth, maxWidth int) int {
	termCols, _ := pyshutil.GetTerminalSize(100, 20)
	longest := max(pystr.Len(title), minWidth-4)
	for _, line := range contentLines {
		longest = max(longest, pystr.Len(line))
	}
	inner := min(max(longest+4, minWidth-2), maxWidth-2, max(24, termCols-6))
	return inner + 2
}

// Wraps panel text, each source line on its own, keepWS preserving whitespace for command previews
func wrapPanelText(text string, width int, subsequentIndent string, keepWS bool) []string {
	parts := []string{text}
	if keepWS {
		parts = pystr.Splitlines(pystr.ExpandTabs(text, 4), false)
	}
	o := pystr.DefaultTextwrap(max(8, width))
	o.SubsequentIndent = subsequentIndent
	if keepWS {
		o.ReplaceWhitespace, o.DropWhitespace = false, false
	} else {
		o.BreakLongWords, o.BreakOnHyphens = false, false
	}
	var wrapped []string
	for _, part := range parts {
		rows := pystr.TextwrapWrap(part, o)
		if len(rows) == 0 {
			rows = []string{""}
		}
		wrapped = append(wrapped, rows...)
	}
	if len(wrapped) == 0 {
		return []string{""}
	}
	return wrapped
}

// Wraps panel text preserving whitespace
func wrapPanelTextKeepWS(text string, width int, subsequentIndent string) []string {
	return wrapPanelText(text, width, subsequentIndent, true)
}

// Appends one bordered content row to the panel fragments
func appendPanelLine(lines *[]styledFragment, borderStyle, contentStyle, text string, boxWidth int) {
	*lines = append(*lines, styledFragment{borderStyle, "│ "}, styledFragment{contentStyle, pystr.Ljust(text, max(0, boxWidth-2))}, styledFragment{borderStyle, " │\n"})
}

// Appends one blank bordered row to the panel fragments
func appendBlankPanelLine(lines *[]styledFragment, borderStyle string, boxWidth int) {
	*lines = append(*lines, styledFragment{borderStyle, "│" + strings.Repeat(" ", boxWidth) + "│\n"})
}
