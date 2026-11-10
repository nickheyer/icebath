package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	modernSpin                  = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
	modernRST                   = "\x1b[0m"
	modernBold                  = "\x1b[1m"
	modernDim                   = "\x1b[2m"
	modernRed                   = "\x1b[38;2;239;83;80m"
	modernSeparatorWidth        = 40
	modernReasoningPreviewLines = 10
	modernDefaultAccent         = "#FFBF00"
	modernDefaultResponseLabel  = " ☤ Icebath "
	modernDefaultPromptSymbol   = "❯"
	modernDefaultBrandGlyph     = "☤"
	modernDefaultBrandName      = "Icebath"
)

var (
	modernLayoutActive atomic.Bool
	modernGapPending   atomic.Bool
	modernANSIRe       = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	modernDiffFileRe   = regexp.MustCompile(`^a/(.+?) → b/(.+)$`)
)

// Reports whether this CLI draws the glyph layout
func isModern(c *CLI) bool { return c != nil && c.modernLayout }

// Reports the glyph layout, the default, with classic keeping the boxed transcript
func layoutFromConfig(display map[string]any) bool {
	return strings.ToLower(strings.TrimSpace(cliStr(display["cli_layout"], "modern"))) != "classic"
}

// Reads display.busy_input_mode, unset interrupting in both layouts
func busyInputModeFromConfig(display map[string]any) string {
	return strings.ToLower(strings.TrimSpace(cliStr(display["busy_input_mode"], "interrupt")))
}

// Color of the active skin for the key, the fallback when the skin holds none
func skinHex(key, fallback string) string {
	if skin := config.GetActiveSkin(context.Background()); skin != nil {
		if v := skin.GetColor(key, fallback); v != "" {
			return v
		}
	}
	return fallback
}

// True color foreground escape of a hex color, bold alone or nothing when the color does not parse
func modernFg(hexColor string, bold bool) string {
	digits := pystr.LstripChars(hexColor, "#")
	var rgb [3]string
	for i, start := range []int{0, 2, 4} {
		n, ok := pyint.ParseBase(pystr.Slice(digits, start, start+2), 16)
		if !ok {
			if bold {
				return modernBold
			}
			return ""
		}
		rgb[i] = n.String()
	}
	prefix := ""
	if bold {
		prefix = "1;"
	}
	return "\x1b[" + prefix + "38;2;" + rgb[0] + ";" + rgb[1] + ";" + rgb[2] + "m"
}

// Glyph and name from the skin response label, the caduceus and brand name when the label has one word
func brandGlyph() (string, string) {
	label := modernDefaultResponseLabel
	if skin := config.GetActiveSkin(context.Background()); skin != nil {
		label = skin.GetBranding("response_label", modernDefaultResponseLabel)
	}
	parts := pystr.SplitWhitespace(label, 1)
	if len(parts) == 2 {
		return parts[0], pystr.Strip(parts[1])
	}
	name := pystr.Strip(label)
	if name == "" {
		name = modernDefaultBrandName
	}
	return modernDefaultBrandGlyph, name
}

// Prompt symbol of the active skin with its padding removed
func promptGlyph() string {
	symbol := modernDefaultPromptSymbol
	if skin := config.GetActiveSkin(context.Background()); skin != nil {
		symbol = skin.GetBranding("prompt_symbol", modernDefaultPromptSymbol)
	}
	if symbol = pystr.Strip(symbol); symbol != "" {
		return symbol
	}
	return modernDefaultPromptSymbol
}

// Opening bar of a block, accent for user messages and answers, dim for reasoning, settling any owed gap
func separator(accent bool) string {
	modernGapPending.Store(false)
	color := modernDim
	if accent {
		color = modernFg(skinHex("ui_accent", modernDefaultAccent), false)
	}
	return color + strings.Repeat("─", modernSeparatorWidth) + modernRST
}

// Closing bar of a block, leaving one blank line owed to whatever prints next
func closingSeparator(accent bool) string {
	bar := separator(accent)
	modernGapPending.Store(true)
	return bar
}

// Newline paying the gap a closing bar left owed, empty when none is owed
func takeGap() string {
	if modernGapPending.Swap(false) {
		return "\n"
	}
	return ""
}

// Rich markup of a submitted single line user message led by the prompt glyph
func userEchoMarkup(text, accent, tsSuffix string) string {
	return "[bold " + accent + "]" + pyrich.Escape(promptGlyph()) + "[/] [bold]" + pyrich.Escape(text) + "[/]" + tsSuffix
}

// Rows opening an answer, the accent bar then the skin brand glyph and name
func answerLabel(timestamp string) string {
	glyph, name := brandGlyph()
	stamp := ""
	if timestamp != "" {
		stamp = " " + modernDim + timestamp + modernRST
	}
	return "\n" + separator(true) + "\n" + modernFg(skinHex("ui_accent", modernDefaultAccent), true) + glyph + " " + name + modernRST + stamp
}

// Dim italic thought summary closing a reasoning block, with the seconds when known
func thoughtLine(seconds *float64) string {
	ctx := context.Background()
	took := i18n.T(ctx, "cli.modern.thought", "", nil)
	if seconds != nil && *seconds != 0 {
		took = i18n.T(ctx, "cli.modern.thought_for", "", map[string]any{"seconds": fmt.Sprintf("%.0f", max(*seconds, 0.0))})
	}
	return "\x1b[2;3m∴ " + took + modernRST
}

// One streamed reasoning line, dim with no frame
func reasoningTextLine(line string) string { return modernDim + line + modernRST }

// Reasoning block printed after the fact, a dim bar, ten lines unless full, then the thought summary
func thoughtRows(seconds *float64, text string, full bool) []string {
	lines := pystr.Splitlines(pystr.Strip(text), false)
	shown := lines
	if !full && len(lines) > modernReasoningPreviewLines {
		shown = lines[:modernReasoningPreviewLines]
	}
	rows := []string{"", separator(false)}
	for _, line := range shown {
		rows = append(rows, reasoningTextLine(line))
	}
	if len(shown) < len(lines) {
		more := i18n.T(context.Background(), "cli.chat.reasoning_more_lines", "", map[string]any{"count": len(lines) - len(shown)})
		rows = append(rows, modernDim+"  "+more+modernRST)
	}
	return append(rows, thoughtLine(seconds), closingSeparator(false))
}

// Scrollback line of a finished tool call in the active layout
func completedToolLine(c *CLI, functionName string, args map[string]any, duration float64, result any, isError bool) string {
	if isModern(c) {
		return takeGap() + toolRow(c.ctx, c.env, functionName, args, duration, result, isError)
	}
	return "  " + tools.GetCuteToolMessage(c.ctx, c.env, functionName, args, duration, result)
}

// One row per completed call led by the tool emoji, a red cross when that entry failed
func toolRow(ctx context.Context, env *tools.Env, functionName string, args map[string]any, duration float64, result any, isError bool) string {
	if args == nil {
		args = map[string]any{}
	}
	rows := tools.GetToolRowParts(ctx, env, functionName, args, result, isError)
	neutral := tools.IsNeutralFailure(functionName, result)
	out := make([]string, 0, len(rows))
	for index, row := range rows {
		verb := row.Verb
		if verb == "" {
			verb = functionName
		}
		verb = pystr.Strip(verb)
		mark := row.Emoji
		if row.Failure != nil && !neutral {
			mark = modernRed + "✗" + modernRST
		} else if mark == "" {
			mark = "●"
		}
		line := mark + " " + modernBold + pystr.Upper(pystr.Slice(verb, 0, 1)) + pystr.Slice(verb, 1, pystr.Len(verb)) + modernRST
		if row.Detail != "" {
			line += " " + row.Detail
		}
		if index == len(rows)-1 && duration >= 0.1 {
			line += "  " + modernDim + fmt.Sprintf("%.1fs", duration) + modernRST
		}
		if row.Failure != nil && *row.Failure != "" {
			color := modernRed
			if neutral {
				color = modernDim
			}
			line += color + *row.Failure + modernRST
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// Records whether the modern layout is active for the process
func setLayoutActive(enabled bool) { modernLayoutActive.Store(enabled) }

// Reports whether the modern layout is active for the process
func layoutActive() bool { return modernLayoutActive.Load() }

// Path relative to the terminal working directory, unchanged when it lies outside it
func displayPath(path string) string {
	if strings.HasPrefix(path, "//") {
		path = path[1:]
	}
	if !pypath.IsAbs(path) {
		return path
	}
	cwd := os.Getenv("TERMINAL_CWD")
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	base, err := pypath.Abspath(cwd)
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(base, pypath.Normpath(path))
	if err != nil {
		return path
	}
	if strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// Wraps a line printer so a diff hangs behind a dim gutter with one relative path as its header
func gutterPrint(printFn func(string)) func(string) {
	header := pystr.Strip(modernANSIRe.ReplaceAllString(i18n.T(context.Background(), "display.diff.review_header", "", nil), ""))
	return func(text string) {
		for _, line := range strings.Split(text, "\n") {
			plain := pystr.Strip(modernANSIRe.ReplaceAllString(line, ""))
			if plain == header {
				continue
			}
			if m := modernDiffFileRe.FindStringSubmatch(plain); m != nil {
				oldPath, newPath := displayPath(m[1]), displayPath(m[2])
				shown := newPath
				if oldPath != newPath {
					shown = oldPath + " → " + newPath
				}
				line = modernBold + shown + modernRST
			}
			if plain != "" {
				printFn(modernDim + "│" + modernRST + " " + line)
			} else {
				printFn(modernDim + "│" + modernRST)
			}
		}
	}
}

// The printer itself, wrapped in the gutter when the glyph layout is active in this process
func diffPrinter(printFn func(string)) func(string) {
	if layoutActive() {
		return gutterPrint(printFn)
	}
	return printFn
}

// Dim line closing each turn with the summary and the wall clock
func turnFooter(summary string) string {
	body := pystr.Strip(pystr.LstripChars(summary, "⋯ "))
	format := "%-I:%M %p"
	if IsWindows() {
		format = "%I:%M %p"
	}
	clock := pydatetime.Strftime(time.Now(), format)
	if body != "" {
		return takeGap() + modernDim + "─ " + body + " · " + clock + modernRST
	}
	return takeGap() + modernDim + "─ " + clock + modernRST
}

var (
	hintPairsFor = hintPairs
	cwdLabelFor  = cwdLabel
)

// Working directory with the home prefix shortened and its git branch in parentheses
func cwdLabel() string {
	cwd := os.Getenv("TERMINAL_CWD")
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	homeDir := pypath.Expanduser("~")
	short := cwd
	if cwd == homeDir || strings.HasPrefix(cwd, homeDir+string(os.PathSeparator)) {
		short = "~" + cwd[len(homeDir):]
	}
	if branch := currentGitBranch(cwd); branch != "" {
		return short + " (" + branch + ")"
	}
	return short
}

// What the agent is doing, the command status or the spinner text with its face or tool emoji
func activityText(c *CLI) string {
	c.uiMu.Lock()
	commandRunning, commandStatus, spinnerText, agentRunning := c.commandRunning, c.commandStatus, c.spinnerText, c.agentRunning
	c.uiMu.Unlock()
	if commandRunning {
		return commandStatus
	}
	if text := pystr.Strip(spinnerText); text != "" {
		return text
	}
	if agentRunning {
		return c.t("cli.modern.working", nil)
	}
	return ""
}

// Compact time the running turn has taken, empty when no turn runs
func turnElapsed(c *CLI) string {
	c.uiMu.Lock()
	started := c.promptStartTime
	c.uiMu.Unlock()
	if started == nil {
		return ""
	}
	return provider.FormatDurationCompact(max(0.0, wallClock()-*started))
}

// Quiet style of the composer rules, the skin session border
func ruleStyle() string { return "fg:" + skinHex("session_border", "#8B8682") }

// Fragments drawn on the terminal background instead of the status bar fill
func noBg(frags []styledFragment) []styledFragment {
	out := make([]styledFragment, len(frags))
	for i, f := range frags {
		out[i] = styledFragment{pystr.Strip(f.Style + " bg:default"), f.Text}
	}
	return out
}

// Fragments as they are when they fit the width, else their plain text trimmed to it
func fitFragments(frags []styledFragment, width int) []styledFragment {
	used := 0
	var plain strings.Builder
	for _, f := range frags {
		used += statusBarDisplayWidth(f.Text)
		plain.WriteString(f.Text)
	}
	if used <= width {
		return frags
	}
	return []styledFragment{{ruleStyle(), trimStatusBarText(plain.String(), width)}}
}

// Top composer rule carrying the live status on the left and the session title on the right
func topRuleFragments(c *CLI) []styledFragment {
	width := max(10, tuiTerminalWidthFor(c))
	rule, accent := ruleStyle(), "fg:"+skinHex("ui_accent", modernDefaultAccent)+" bold"
	agentRunning, commandRunning := c.busyState()
	left := []styledFragment{}
	if agentRunning || commandRunning {
		spin := []rune(modernSpin)
		frame := string(spin[int(monotonic()*10)%len(spin)])
		left = append(left, styledFragment{rule, "── "}, styledFragment{accent, frame + " "}, styledFragment{"", activityText(c)})
		for _, extra := range []string{turnElapsed(c), c.spinnerTokenFlow()} {
			if extra != "" {
				left = append(left, styledFragment{"class:status-bar-dim bg:default", " · " + extra})
			}
		}
		left = append(left, styledFragment{rule, " "})
	}
	right := []styledFragment{}
	if title := c.getStatusBarSessionTitle(); title != "" {
		right = []styledFragment{{rule, " "}, {"fg:" + skinHex("ui_label", "#DAA520"), title}, {rule, " ─"}}
	}
	widthOf := func(frags []styledFragment) int {
		n := 0
		for _, f := range frags {
			n += statusBarDisplayWidth(f.Text)
		}
		return n
	}
	used := widthOf(left) + widthOf(right)
	if used >= width {
		right, used = nil, widthOf(left)
	}
	frags := append(append([]styledFragment{}, left...), styledFragment{rule, strings.Repeat("─", max(0, width-used))})
	return fitFragments(append(frags, right...), width)
}

// Keys that do something right now, for the right half of the footer
func hintPairs(c *CLI) [][2]string {
	c.uiMu.Lock()
	modal := c.approvalState != nil || c.clarifyState != nil || c.sudoState != nil || c.secretState != nil || c.connectionState != nil ||
		c.slashConfirmState != nil || c.modelPickerState != nil || c.commandPaletteState != nil
	agentRunning := c.agentRunning
	c.uiMu.Unlock()
	if modal {
		return nil
	}
	if agentRunning {
		enter := "interrupt"
		if c.busyInputMode == "steer" || c.busyInputMode == "queue" {
			enter = c.busyInputMode
		}
		pairs := [][2]string{{"enter", c.t("cli.modern.hint_"+enter, nil)}}
		if enter != "queue" {
			pairs = append(pairs, [2]string{"tab", c.t("cli.modern.hint_queue", nil)})
		}
		return append(pairs, [2]string{"ctrl+c", c.t("cli.modern.hint_stop", nil)})
	}
	return [][2]string{{"/", c.t("cli.modern.hint_commands", nil)}, {"@", c.t("cli.modern.hint_files", nil)}, {"!", c.t("cli.modern.hint_shell", nil)}}
}

// Footer under the composer with the status segments on the left and the live key hints on the right
func statusLineFragments(c *CLI) []styledFragment {
	return renderSafely(statusLineFragmentsUnguarded, c, "_status_line_fragments")
}

// Runs a render callback, a failing frame reducing to a bare line with the model name
func renderSafely(render func(*CLI) []styledFragment, c *CLI, name string) (frags []styledFragment) {
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Debug(name+" failed", "error", r)
			frags = []styledFragment{{"", " " + c.statusBarFallbackModel()}}
		}
	}()
	return render(c)
}

// The classic bar segments, then the working directory and the key hints when there is room
func statusLineFragmentsUnguarded(c *CLI) []styledFragment {
	width := max(10, tuiTerminalWidthFor(c))
	dim, strong := statusBarDim, statusBarStrong
	snapshot := statusBarSnapshotFor(c)
	fieldSet := c.getStatusBarFieldSet()
	on := func(name string) bool { return fieldSet == nil || fieldSet[name] }
	left := []styledFragment{}
	for _, seg := range c.statusBarSegments(snapshot, width, fieldSet, c.isSessionYoloActive(), true) {
		if len(left) > 0 {
			left = append(left, styledFragment{dim, " · "})
		}
		left = append(left, seg...)
	}
	if on("stash") {
		if stash := c.stashIndicator(); stash != "" {
			left = append(left, styledFragment{dim, " · "}, styledFragment{strong, stash})
		}
	}
	if battery := cliStr(snapshot["battery_label"], ""); battery != "" && on("battery") {
		left = append([]styledFragment{{"", " "}, {batteryStatusStyle(cliStr(snapshot["battery_category"], "dim")), battery}, {dim, " ·"}}, left...)
	}
	if vim := c.vimModeLabel(); vim != "" {
		left = append(left, styledFragment{dim, " · "}, styledFragment{strong, vim})
	}
	right := []styledFragment{}
	if width >= 76 {
		for _, pair := range hintPairsFor(c) {
			if len(right) > 0 {
				right = append(right, styledFragment{dim, " · "})
			}
			right = append(right, styledFragment{strong, pair[0]}, styledFragment{dim, " " + pair[1]})
		}
	}
	widthOf := func(frags []styledFragment) int {
		n := 0
		for _, f := range frags {
			n += statusBarDisplayWidth(f.Text)
		}
		return n
	}
	usedLeft, usedRight := widthOf(left), widthOf(right)
	room := width - usedLeft - usedRight - 6
	if fieldSet == nil && room >= 12 {
		cwd := cwdLabelFor()
		if pystr.Len(cwd) > room {
			cwd = "…" + pystr.Slice(cwd, pystr.Len(cwd)-(room-1), pystr.Len(cwd))
		}
		left = append(left, styledFragment{dim, " · "}, styledFragment{dim, cwd})
		usedLeft += 3 + statusBarDisplayWidth(cwd)
	}
	if len(right) > 0 && usedLeft+usedRight+2 > width {
		right, usedRight = nil, 0
	}
	frags := append([]styledFragment{}, left...)
	if len(right) > 0 {
		frags = append(frags, styledFragment{"", strings.Repeat(" ", max(1, width-usedLeft-usedRight-1))})
		frags = append(append(frags, right...), styledFragment{"", " "})
	}
	return noBg(fitFragments(frags, width))
}

// Reports whether Tab queues the composer text while a turn runs
func canQueueWhileBusy(c *CLI) bool {
	app := c.tui.app
	if app == nil {
		return false
	}
	buf := app.buffer
	text := pystr.Strip(buf.text())
	agentRunning, _ := c.busyState()
	return agentRunning && text != "" && buf.completeState == nil && !looksLikeSlashCommand(text) && !strings.HasPrefix(text, "!") && !modalPromptActive(c)
}

// Builds the composer rules of the glyph layout and binds Tab to queue the draft while a turn runs
func modernInputRules(c *CLI, kb *ptKeyBindings) (*ptWindow, *ptWindow) {
	top := &ptWindow{name: "input-rule-top", kind: ptWindowText,
		fragments: func() []styledFragment { return renderSafely(topRuleFragments, c, "top_rule_fragments") },
		height:    func() int { return c.tuiInputRuleHeight("top", nil) }}
	bottom := &ptWindow{name: "input-rule-bottom", kind: ptWindowText,
		fragments: func() []styledFragment {
			return []styledFragment{{ruleStyle(), strings.Repeat("─", max(0, tuiTerminalWidthFor(c)))}}
		},
		height: func() int { return c.tuiInputRuleHeight("bottom", nil) }}
	kb.add(func(e *ptEvent) {
		buf := e.currentBuffer()
		text := pystr.Strip(buf.text())
		images := append([]string{}, c.attachedImages...)
		c.attachedImages = nil
		var payload any = text
		if len(images) > 0 {
			payload = submittedInput{Text: text, Images: images}
		}
		c.tuiEnterWhileBusy(text, images, payload, "queue")
		c.inlinePastes(buf)
		buf.reset(true)
		_ = e.app.Invalidate()
	}, ptBind{filter: func() bool { return canQueueWhileBusy(c) }, eager: true}, "tab")
	return top, bottom
}
