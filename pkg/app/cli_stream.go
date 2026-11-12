package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const pyWSClass = `[\t\n\v\f\r \x{1c}-\x{1f}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

const visionAnalysisPrompt = "Describe everything visible in this image in thorough detail. " +
	"Include any text, code, data, objects, people, layout, colors, " +
	"and any other notable visual information."

var (
	streamOpenTags, streamCloseTags [][]rune
	streamMaxCloseTagLen            int
	commandSpinnerFrames            = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	reasoningParagraphRe            = regexp.MustCompile(`\n` + pyWSClass + `*\n+`)
	pasteRefRe                      = regexp.MustCompile(`\[Pasted text #\p{Nd}+: \p{Nd}+ lines \x{2192} (.+?)\]`)
	visionAnalyzeTool               = tools.VisionAnalyzeTool
	slowCommandStatus               = [][2]string{
		{"/skills search", "cli.stream.busy_skills_search"}, {"/skills browse", "cli.stream.busy_skills_browse"},
		{"/skills inspect", "cli.stream.busy_skills_inspect"}, {"/skills install", "cli.stream.busy_skills_install"},
		{"/skills", "cli.stream.busy_skills"}, {"/browser", "cli.stream.busy_browser"}}
	slowCommandStatusExact = map[string]string{
		"/reload-mcp":    "cli.stream.busy_reload_mcp",
		"/reload-skills": "cli.stream.busy_reload_skills",
		"/reload_skills": "cli.stream.busy_reload_skills"}
)

func init() {
	for _, name := range provider.ThinkTagNames {
		lower := strings.ToLower(name)
		streamOpenTags = append(streamOpenTags, []rune("<"+lower+">"))
		streamCloseTags = append(streamCloseTags, []rune("</"+lower+">"))
	}
	for _, tag := range streamCloseTags {
		streamMaxCloseTagLen = max(streamMaxCloseTagLen, len(tag))
	}
}

// Catalog string for the key in the active language with its format arguments
func (c *CLI) t(key string, kwargs map[string]any) string { return i18n.T(c.ctx, key, "", kwargs) }

// Reasoning preview label with its indent, also measured for the wrap width
func (c *CLI) thinkingPrefix() string { return "  " + c.t("cli.stream.thinking_label", nil) + " " }

// Whether the running agent mutes this notification reply
func (c *CLI) agentMutesNotificationReply() bool {
	return c.agent != nil && c.agent.MuteNotificationReply()
}

// Notification config of the running agent, nil without an agent
func (c *CLI) agentNotificationConfig() map[string]any {
	if c.agent == nil {
		return nil
	}
	return c.agent.NotificationConfig()
}

// Repaints through the throttle and logs a failure at debug level
func (c *CLI) invalidateQuiet() {
	if err := c.invalidate(); err != nil {
		cliLogger.Debug("invalidate failed", "error", err)
	}
}

// Sets the spinner text the status line shows
func (c *CLI) setSpinnerText(text string) {
	c.uiMu.Lock()
	c.spinnerText = text
	c.uiMu.Unlock()
}

// Updates the TUI spinner when the agent starts or stops thinking
func (c *CLI) onThinking(text any) {
	if c.agentMutesNotificationReply() {
		return
	}
	_, diagnostic := text.(config.DiagnosticText)
	shown := ""
	if text != nil {
		shown = pyrepr.Str(text)
	}
	show := func() error {
		if shown == "" {
			c.streamMu.Lock()
			c.flushReasoningPreview(true)
			c.streamMu.Unlock()
		}
		c.uiMu.Lock()
		c.spinnerText = shown
		c.toolStartTime = 0
		c.uiMu.Unlock()
		return c.invalidate()
	}
	if _, err := config.RenderNotification(c.ctx, show, "cli", diagnostic, c.agentNotificationConfig()); err != nil {
		cliLogger.Debug("thinking callback failed", "error", err)
	}
}

// Queues an out of band agent notice for printing at the next clean boundary
func (c *CLI) onNotice(notice provider.AgentNotice) {
	defer func() { _ = recover() }()
	text := notice.Text
	if c.agentMutesNotificationReply() || text == "" {
		return
	}
	level := notice.Level
	if level == "" {
		level = "info"
	}
	queue := func() error {
		c.streamMu.Lock()
		c.pendingCreditNotices = append(c.pendingCreditNotices, [2]string{level, text})
		c.streamMu.Unlock()
		return nil
	}
	_, _ = config.RenderNotification(c.ctx, queue, "cli", config.IsDiagnosticNotice(notice.Level, notice.Key), c.agentNotificationConfig())
}

// Prints the queued credit notices as level colored lines at turn end
func (c *CLI) flushCreditNotices() {
	defer func() { _ = recover() }()
	c.streamMu.Lock()
	pending := c.pendingCreditNotices
	c.pendingCreditNotices = nil
	c.streamMu.Unlock()
	colors := map[string]string{"error": "\033[31m", "warn": "\033[33m", "success": "\033[32m", "info": dimANSI}
	for _, n := range pending {
		color, ok := colors[n[0]]
		if !ok {
			color = dimANSI
		}
		cprint("  " + color + n[1] + resetANSI)
	}
}

// Leaves printed notice lines alone, the REPL holding no persistent slot to wipe
func (c *CLI) onNoticeClear(key string) {}

// Active reasoning display callback for the current mode, nil when reasoning is hidden
func (c *CLI) currentReasoningCallback() func(string) {
	if c.showReasoning && c.streamingEnabled {
		return c.streamReasoningDelta
	}
	if c.verbose && !c.showReasoning {
		return c.onReasoning
	}
	return nil
}

// Renders a buffered reasoning preview as one thinking block, five lines unless verbose
func (c *CLI) emitReasoningPreview(reasoningText string) {
	previewText := pystr.Strip(reasoningText)
	if previewText == "" {
		return
	}
	prefix := c.thinkingPrefix()
	wrapWidth := max(30, terminalColumns()-pystr.Len(prefix)-2)
	paragraphs := []string{}
	for _, paragraph := range reasoningParagraphRe.Split(strings.ReplaceAll(previewText, "\r\n", "\n"), -1) {
		kept := []string{}
		for _, line := range pystr.Splitlines(paragraph, false) {
			if s := pystr.Strip(line); s != "" {
				kept = append(kept, s)
			}
		}
		if compact := strings.Join(kept, " "); compact != "" {
			paragraphs = append(paragraphs, strings.Join(pystr.TextwrapWrap(compact, pystr.DefaultTextwrap(wrapWidth)), "\n"))
		}
	}
	previewText = strings.Join(paragraphs, "\n")
	if previewText == "" {
		return
	}
	if c.verbose {
		cprint(dimANSI + prefix + previewText + resetANSI)
		return
	}
	lines := pystr.Splitlines(previewText, false)
	preview := previewText
	if len(lines) > 5 {
		preview = strings.Join(lines[:5], "\n") + "\n  " + c.t("cli.stream.thinking_more_lines", map[string]any{"count": len(lines) - 5})
	}
	cprint(dimANSI + prefix + preview + resetANSI)
}

// Flushes buffered reasoning text at a line or word boundary, all of it when forced
func (c *CLI) flushReasoningPreview(force bool) {
	if c.reasoningPreviewBuf == "" {
		return
	}
	buf := pystr.Runes(c.reasoningPreviewBuf)
	targetWidth := max(40, terminalColumns()-pystr.Len(c.thinkingPrefix())-4)
	var flush []rune
	flushed := false
	if force {
		flush, buf, flushed = buf, nil, true
	} else {
		lineBreak := -1
		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				lineBreak = i
				break
			}
		}
		text := c.reasoningPreviewBuf
		boundary := false
		for _, suffix := range []string{"\n\n", ".\n", "!\n", "?\n", ":\n"} {
			boundary = boundary || strings.HasSuffix(text, suffix)
		}
		if lineBreak != -1 && (lineBreak >= max(16, targetWidth/3) || boundary) {
			flush, buf, flushed = buf[:lineBreak+1], buf[lineBreak+1:], true
		} else if len(buf) >= targetWidth {
			searchStart := max(20, targetWidth/2)
			searchEnd := min(len(buf), max(targetWidth+targetWidth/3, targetWidth+8))
			cut := -1
			for i := searchEnd - 1; i >= searchStart; i-- {
				if strings.ContainsRune(" \t.!?,;:", buf[i]) {
					cut = i
					break
				}
			}
			if cut != -1 {
				flush, buf, flushed = buf[:cut+1], buf[cut+1:], true
			}
		}
	}
	flushText := pystr.FromRunes(flush)
	if flushed && flushText != "" {
		c.reasoningPreviewBuf = pystr.Lstrip(pystr.FromRunes(buf))
		c.emitReasoningPreview(flushText)
		return
	}
	c.reasoningPreviewBuf = pystr.FromRunes(buf)
}

// Buffers intermediate reasoning during tool call loops and flushes it at natural boundaries
func (c *CLI) onReasoning(reasoningText string) {
	if reasoningText == "" {
		return
	}
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	c.reasoningPreviewBuf += reasoningText
	c.flushReasoningPreview(false)
}

// Clock suffix of a submitted message preview, empty unless timestamps are shown
func (c *CLI) timestampNow() string { return pydatetime.Strftime(time.Now(), c.timestampFormat) }

// Rich markup of the submitted user message, long input cut to its first and last lines
func (c *CLI) formatSubmittedUserMessagePreview(userInput string) string {
	tsSuffix := ""
	if c.showTimestamps {
		tsSuffix = " [dim]" + c.timestampNow() + "[/]"
	}
	lines := strings.Split(userInput, "\n")
	mark := "●"
	if isModern(c) {
		mark = pyrich.Escape(promptGlyph())
	}
	accent := accentHex()
	if len(lines) <= 1 {
		return "[bold " + accent + "]" + mark + "[/] [bold]" + pyrich.Escape(userInput) + "[/]" + tsSuffix
	}
	firstLines := max(1, c.userMessagePreviewFirstLines)
	lastLines := max(0, c.userMessagePreviewLastLines)
	head := lines[:min(firstLines, len(lines))]
	tailCount := min(lastLines, max(0, len(lines)-len(head)))
	tail := lines[len(lines)-tailCount:]
	hiddenMiddleCount := len(lines) - len(head) - len(tail)
	if hiddenMiddleCount < 0 {
		hiddenMiddleCount, tail = 0, nil
	}
	previewLines := []string{"[bold " + accent + "]" + mark + "[/] [bold]" + pyrich.Escape(head[0]) + "[/]" + tsSuffix}
	for _, line := range head[1:] {
		previewLines = append(previewLines, "[bold]"+pyrich.Escape(line)+"[/]")
	}
	if hiddenMiddleCount > 0 {
		key := "cli.stream.preview_more_lines_other"
		if hiddenMiddleCount == 1 {
			key = "cli.stream.preview_more_lines_one"
		}
		previewLines = append(previewLines, "[dim]"+pyrich.Escape(c.t(key, map[string]any{"count": hiddenMiddleCount}))+"[/]")
	}
	for _, line := range tail {
		previewLines = append(previewLines, "[bold]"+pyrich.Escape(line)+"[/]")
	}
	return strings.Join(previewLines, "\n")
}

// Replaces pasted text markers with their file contents, a marker kept when its file is unreadable
func (c *CLI) expandPasteReferences(text string) (string, error) {
	if !strings.Contains(text, "[Pasted text #") {
		return text, nil
	}
	var decodeErr error
	out := pasteRefRe.ReplaceAllStringFunc(text, func(match string) string {
		path := pasteRefRe.FindStringSubmatch(match)[1]
		raw, err := os.ReadFile(path)
		if err != nil {
			cliLogger.Warn(fmt.Sprintf("Paste file gone or unreadable, returning placeholder: %s", path))
			return match
		}
		content, err := pystr.DecodeUTF8Sig(raw)
		if err != nil && decodeErr == nil {
			decodeErr = err
		}
		return content
	})
	if decodeErr != nil {
		return "", decodeErr
	}
	return out, nil
}

// Renders a submitted user message in the chat scrollback style, a timeline notification as one dim line
func (c *CLI) printUserMessagePreview(userInput any) {
	if n, ok := userInput.(tools.TimelineNotification); ok {
		_, _ = config.RenderNotification(c.ctx, func() error {
			chatConsolePrint("[dim]◈ " + pyrich.Escape(n.DisplayText) + "[/dim]")
			return nil
		}, "cli", n.NotificationCategory == "diagnostic", nil)
		return
	}
	modern := c.modernLayout
	if modern {
		cprint("\n" + separator(true))
	} else {
		chatConsolePrint("[" + accentHex() + "]" + strings.Repeat("─", 40) + "[/]")
	}
	text := ""
	if pyrepr.Truthy(userInput) {
		text = pyrepr.Str(userInput)
	}
	switch {
	case strings.Contains(text, "\n"):
		if modern {
			defaultChatConsole.PrintSoft(c.formatSubmittedUserMessagePreview(text))
		} else {
			chatConsolePrint(c.formatSubmittedUserMessagePreview(text))
		}
	case modern:
		defaultChatConsole.PrintSoft(userEchoMarkup(text, accentHex(), ""))
	default:
		chatConsolePrint("[bold " + accentHex() + "]●[/] [bold]" + pyrich.Escape(text) + "[/]")
	}
	if modern {
		cprint(closingSeparator(true))
	}
}

// Streams reasoning tokens into the dim block above the response
func (c *CLI) streamReasoningDelta(text string) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	c.streamReasoning(text)
}

// Reasoning delta renderer run with the stream lock held, the live reasoning block in production
var streamReasoningSink = (*CLI).renderReasoningDelta

// Routes a reasoning delta to the installed renderer with the stream lock held
func (c *CLI) streamReasoning(text string) { streamReasoningSink(c, text) }

// Opens the reasoning block on the first token and prints complete or overlong lines as they arrive
func (c *CLI) renderReasoningDelta(text string) {
	if text == "" {
		return
	}
	c.reasoningShownThisTurn = true
	if c.streamBoxOpened {
		return
	}
	line := func(s string) string { return dimANSI + s + resetANSI }
	if c.modernLayout {
		line = reasoningTextLine
		if !c.reasoningBoxOpened {
			c.reasoningBoxOpened, c.reasoningStartedAt = true, monotonic()
			c.reasoningBuf = ""
			cprint("\n" + separator(false))
		}
	} else if !c.reasoningBoxOpened {
		c.reasoningBoxOpened = true
		w := scrollbackBoxWidth()
		label := " " + c.t("cli.chat.reasoning_label", nil) + " "
		fill := w - 2 - pystr.Len(label)
		cprint("\n" + dimANSI + "┌─" + label + strings.Repeat("─", max(fill-1, 0)) + "┐" + resetANSI)
	}
	c.reasoningBuf += text
	for strings.Contains(c.reasoningBuf, "\n") {
		var head string
		head, c.reasoningBuf, _ = strings.Cut(c.reasoningBuf, "\n")
		cprint(line(head))
	}
	if pystr.Len(c.reasoningBuf) > 80 {
		cprint(line(c.reasoningBuf))
		c.reasoningBuf = ""
	}
}

// Prints an agent status line, holding it while a response or reasoning block is open
func (c *CLI) agentStatusPrint(text string) {
	c.streamMu.Lock()
	if c.streamBoxLive || c.reasoningBoxOpened {
		c.heldStatusLines = append(c.heldStatusLines, text)
		c.streamMu.Unlock()
		return
	}
	c.streamMu.Unlock()
	cprint(text)
}

// Prints the status lines held while a block was open
func (c *CLI) releaseHeldStatusLines() {
	held := c.heldStatusLines
	c.heldStatusLines = []string{}
	for _, line := range held {
		cprint(line)
	}
}

// Closes the live reasoning block after printing its buffered tail
func (c *CLI) closeReasoningBox() {
	if !c.reasoningBoxOpened {
		return
	}
	if c.modernLayout {
		if c.reasoningBuf != "" {
			cprint(reasoningTextLine(c.reasoningBuf))
		}
		var elapsed *float64
		if c.reasoningStartedAt != 0 {
			e := monotonic() - c.reasoningStartedAt
			elapsed = &e
		}
		cprint(thoughtLine(elapsed))
		cprint(closingSeparator(false))
		c.reasoningBuf, c.reasoningBoxOpened = "", false
		if !c.streamBoxLive {
			c.releaseHeldStatusLines()
		}
		return
	}
	if c.reasoningBuf != "" {
		cprint(dimANSI + c.reasoningBuf + resetANSI)
		c.reasoningBuf = ""
	}
	w := scrollbackBoxWidth()
	cprint(dimANSI + "└" + strings.Repeat("─", max(w-2, 0)) + "┘" + resetANSI)
	c.reasoningBoxOpened = false
	if !c.streamBoxLive {
		c.releaseHeldStatusLines()
	}
}

// Index of needle in hay at or after start, minus one when absent
func runeFind(hay, needle []rune, start int) int {
	for i := max(start, 0); i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// Reports whether the rune slice ends with the suffix
func runeHasSuffix(s, suffix []rune) bool {
	return len(s) >= len(suffix) && runeFind(s[len(s)-len(suffix):], suffix, 0) == 0
}

// Line buffered streaming callback, nil marking a turn boundary that flushes and resets the display
func (c *CLI) streamDelta(text any) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if text == nil {
		c.flushStreamLocked()
		c.resetStreamStateLocked()
		return
	}
	if !pyrepr.Truthy(text) {
		return
	}
	c.streamDeltaText(pyrepr.Str(text))
}

// Filters reasoning tags out of streamed text, routing their content to the reasoning block when shown
func (c *CLI) streamDeltaText(text string) {
	if text == "" {
		return
	}
	c.streamStarted = true
	c.streamPrefilt += text
	if !c.inReasoningBlock {
		prefilt := pystr.Runes(c.streamPrefilt)
		lower := pystr.Runes(pystr.Lower(c.streamPrefilt))
		clamp := func(i int) int { return max(0, min(i, len(prefilt))) }
		for _, tag := range streamOpenTags {
			searchStart := 0
			for {
				idx := runeFind(lower, tag, searchStart)
				if idx == -1 {
					break
				}
				preceding := prefilt[:clamp(idx)]
				lastNewline := -1
				for i := len(preceding) - 1; i >= 0; i-- {
					if preceding[i] == '\n' {
						lastNewline = i
						break
					}
				}
				boundary := pystr.Strip(pystr.FromRunes(preceding[lastNewline+1:])) == "" && (lastNewline != -1 || !c.streamMidLine)
				if boundary {
					if len(preceding) > 0 {
						c.emitStreamText(pystr.FromRunes(preceding))
						c.streamMidLine = preceding[len(preceding)-1] != '\n'
					}
					c.inReasoningBlock = true
					prefilt = prefilt[clamp(idx+len(tag)):]
					c.streamPrefilt = pystr.FromRunes(prefilt)
					break
				}
				searchStart = idx + 1
			}
			if c.inReasoningBlock {
				break
			}
		}
		if !c.inReasoningBlock {
			safe := prefilt
			for _, tag := range streamOpenTags {
				for i := 1; i < len(tag); i++ {
					if runeHasSuffix(lower, tag[:i]) {
						safe = prefilt[:max(0, len(prefilt)-i)]
						break
					}
				}
			}
			if len(safe) > 0 {
				c.emitStreamText(pystr.FromRunes(safe))
				c.streamMidLine = safe[len(safe)-1] != '\n'
				c.streamPrefilt = pystr.FromRunes(prefilt[len(safe):])
			}
			return
		}
	}
	prefilt := pystr.Runes(c.streamPrefilt)
	lower := pystr.Runes(pystr.Lower(c.streamPrefilt))
	clamp := func(i int) int { return max(0, min(i, len(prefilt))) }
	for _, tag := range streamCloseTags {
		idx := runeFind(lower, tag, 0)
		if idx == -1 {
			continue
		}
		c.inReasoningBlock = false
		if c.showReasoning {
			if inner := prefilt[:clamp(idx)]; len(inner) > 0 {
				c.streamReasoning(pystr.FromRunes(inner))
			}
		}
		after := pystr.FromRunes(prefilt[clamp(idx+len(tag)):])
		c.streamPrefilt = ""
		if after != "" {
			c.streamDeltaText(after)
		}
		return
	}
	if len(prefilt) > streamMaxCloseTagLen {
		if c.showReasoning {
			c.streamReasoning(pystr.FromRunes(prefilt[:len(prefilt)-streamMaxCloseTagLen]))
		}
		c.streamPrefilt = pystr.FromRunes(prefilt[len(prefilt)-streamMaxCloseTagLen:])
	}
}

// Prints one response line in the skin text color when the skin sets one
func (c *CLI) emitStreamLine(printedLine string) {
	if c.streamTextANSI != "" {
		cprint(streamPad + c.streamTextANSI + printedLine + resetANSI)
		return
	}
	cprint(streamPad + printedLine)
}

// Prints the held table block realigned as a whole after stripping cell markdown
func (c *CLI) flushStreamTableBuf() {
	buf := c.streamTableBuf
	c.streamTableBuf = []string{}
	c.inStreamTable = false
	if len(buf) == 0 {
		return
	}
	joined := strings.Join(buf, "\n")
	if c.finalResponseMarkdown == "strip" {
		joined = stripMarkdownSyntax(joined)
	}
	width := terminalWidthForStreaming()
	for _, ln := range strings.Split(tools.RealignMarkdownTables(joined, &width), "\n") {
		c.emitStreamLine(ln)
	}
}

// Response text renderer run with the stream lock held, the live response block in production
var emitStreamTextSink = (*CLI).renderStreamText

// Routes visible response text to the installed renderer with the stream lock held
func (c *CLI) emitStreamText(text string) { emitStreamTextSink(c, text) }

// Opens the response block on the first visible text and prints every completed line
func (c *CLI) renderStreamText(text string) {
	if text == "" {
		return
	}
	c.closeReasoningBox()
	if !c.streamBoxOpened {
		text = strings.TrimLeft(text, "\n")
		if text == "" {
			return
		}
		c.streamBoxOpened = true
		c.streamBoxLive = true
		label, textHex := "☤ Icebath", "#FFF8DC"
		if skin := config.GetActiveSkin(c.ctx); skin != nil {
			label = skin.GetBranding("response_label", "☤ Icebath")
			textHex = skin.GetColor("banner_text", "#FFF8DC")
		}
		c.streamTextANSI = ""
		var rgb [3]string
		parsed := true
		for i, start := range []int{1, 3, 5} {
			n, ok := pyint.ParseBase(pystr.Slice(textHex, start, start+2), 16)
			if !ok {
				parsed = false
				break
			}
			rgb[i] = n.String()
		}
		if parsed {
			c.streamTextANSI = "\033[38;2;" + rgb[0] + ";" + rgb[1] + ";" + rgb[2] + "m"
		}
		if c.showTimestamps {
			label = label + " " + c.timestampNow()
		}
		if c.modernLayout {
			stamp := ""
			if c.showTimestamps {
				stamp = c.timestampNow()
			}
			cprint(answerLabel(stamp))
		} else {
			w := scrollbackBoxWidth()
			fill := w - 2 - statusBarDisplayWidth(label)
			cprint("\n" + accentANSI.String() + "╭─" + label + strings.Repeat("─", max(fill-1, 0)) + "╮" + resetANSI)
		}
	}
	c.streamedTextThisTurn += text
	c.streamBuf += text
	for strings.Contains(c.streamBuf, "\n") {
		var line string
		line, c.streamBuf, _ = strings.Cut(c.streamBuf, "\n")
		if c.inStreamTable {
			if tools.LooksLikeTableRow(line) || tools.IsTableDivider(line) {
				c.streamTableBuf = append(c.streamTableBuf, line)
				continue
			}
			c.flushStreamTableBuf()
		} else if tools.LooksLikeTableRow(line) {
			c.streamTableBuf = append(c.streamTableBuf, line)
			c.inStreamTable = true
			continue
		}
		if c.finalResponseMarkdown == "strip" {
			line = stripMarkdownSyntax(line)
		}
		c.emitStreamLine(line)
	}
	if c.streamBuf != "" && !c.inStreamTable && !strings.HasPrefix(pystr.Lstrip(c.streamBuf), "|") && pystr.Len(c.streamBuf) >= 80 {
		buf := pystr.Runes(c.streamBuf)
		preview := buf[len(buf)-streamPartialPreviewLen:]
		cut := -1
		for i, r := range preview {
			if r == ' ' {
				cut = i
				break
			}
		}
		if cut > 0 && cut < len(preview)-1 {
			preview = preview[cut+1:]
		}
		c.setSpinnerText("… " + pystr.FromRunes(preview))
		c.invalidateQuiet()
	}
}

// Prints any remaining partial line and closes the response block
func (c *CLI) flushStream() {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	c.flushStreamLocked()
}

func (c *CLI) flushStreamLocked() {
	if c.inReasoningBlock && c.streamPrefilt != "" {
		c.inReasoningBlock = false
		c.emitStreamText(c.streamPrefilt)
		c.streamPrefilt = ""
	}
	c.closeReasoningBox()
	if c.streamBuf != "" && c.inStreamTable && (tools.LooksLikeTableRow(c.streamBuf) || tools.IsTableDivider(c.streamBuf)) {
		c.streamTableBuf = append(c.streamTableBuf, c.streamBuf)
		c.streamBuf = ""
	}
	if len(c.streamTableBuf) > 0 {
		c.flushStreamTableBuf()
	}
	if c.streamBuf != "" {
		line := c.streamBuf
		if c.finalResponseMarkdown == "strip" {
			line = stripMarkdownSyntax(line)
		}
		c.emitStreamLine(line)
		c.streamBuf = ""
	}
	if c.streamBoxOpened && c.streamBoxLive {
		if c.modernLayout {
			cprint(closingSeparator(true))
		} else {
			w := scrollbackBoxWidth()
			cprint(accentANSI.String() + "╰" + strings.Repeat("─", max(w-2, 0)) + "╯" + resetANSI)
		}
	}
	c.streamBoxLive = false
	c.releaseHeldStatusLines()
}

// Resets the streaming state before each agent invocation
func (c *CLI) resetStreamState() {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	c.resetStreamStateLocked()
}

func (c *CLI) resetStreamStateLocked() {
	c.streamBuf = ""
	c.streamStarted = false
	c.streamBoxOpened = false
	c.streamTextANSI = ""
	c.streamPrefilt = ""
	c.inReasoningBlock = false
	c.streamMidLine = false
	c.reasoningBoxOpened = false
	c.reasoningBuf = ""
	c.reasoningPreviewBuf = ""
	c.toolGenAnnounced = nil
	c.streamTableBuf = []string{}
	c.inStreamTable = false
	c.streamBoxLive = false
}

// User facing status message for a slower slash command
func (c *CLI) slowCommandStatus(command string) string {
	cmdLower := pystr.Strip(pystr.Lower(command))
	if key, ok := slowCommandStatusExact[cmdLower]; ok {
		return c.t(key, nil)
	}
	for _, row := range slowCommandStatus {
		if strings.HasPrefix(cmdLower, row[0]) {
			return c.t(row[1], nil)
		}
	}
	return c.t("cli.stream.processing_command", nil)
}

// Current spinner frame for slow slash commands
func (c *CLI) commandSpinnerFrame() string {
	return commandSpinnerFrames[int(monotonic()*10)%len(commandSpinnerFrames)]
}

// Shows a busy state in the TUI while fn runs, reserving the composer when blocksInput is set
func (c *CLI) busyCommand(status string, blocksInput bool, fn func()) (err error) {
	c.uiMu.Lock()
	previousBlocksInput := c.commandBlocksInput
	c.commandRunning = true
	c.commandBlocksInput = blocksInput
	c.commandStatus = status
	c.uiMu.Unlock()
	if err := c.invalidateWithin(0); err != nil {
		return err
	}
	defer func() {
		c.uiMu.Lock()
		c.commandRunning = false
		c.commandBlocksInput = previousBlocksInput
		c.commandStatus = ""
		c.uiMu.Unlock()
		if ierr := c.invalidateWithin(0); ierr != nil && err == nil {
			err = ierr
		}
	}()
	cprint("⏳ " + status)
	fn()
	return nil
}

// Describes attached images through the vision model and prepends the descriptions to the user text
func (c *CLI) preprocessImagesWithVision(text string, images []string, announce bool) string {
	notify := func(key string) {
		_, _ = config.RenderNotification(c.ctx, func() error {
			cprint("  " + dimANSI + c.t(key, nil) + resetANSI)
			return nil
		}, "cli", false, c.agentNotificationConfig())
	}
	enrichedParts := []string{}
	for _, imgPath := range images {
		info, err := os.Stat(imgPath)
		if err != nil {
			continue
		}
		if announce {
			cprint("  " + dimANSI + c.t("cli.stream.vision_analyzing", map[string]any{"name": filepath.Base(imgPath), "size_kb": info.Size() / 1024}) + resetANSI)
		}
		result, failure := c.analyzeImage(imgPath)
		switch {
		case failure != "":
			enrichedParts = append(enrichedParts, "[The user attached an image but analysis failed ("+failure+"). "+
				"You can try examining it with vision_analyze using "+
				"image_url: "+imgPath+"]")
			if announce {
				notify("cli.stream.vision_error")
			}
		case pyrepr.Truthy(result["success"]):
			description := ""
			if v, ok := result["analysis"]; ok {
				description = pyrepr.Str(v)
			}
			enrichedParts = append(enrichedParts, "[The user attached an image. Here's what it contains:\n"+description+"]\n"+
				"[If you need a closer look, use vision_analyze with "+
				"image_url: "+imgPath+"]")
			if announce {
				cprint("  " + dimANSI + c.t("cli.stream.vision_analyzed", nil) + resetANSI)
			}
		default:
			enrichedParts = append(enrichedParts, "[The user attached an image but it couldn't be analyzed. "+
				"You can try examining it with vision_analyze using "+
				"image_url: "+imgPath+"]")
			if announce {
				notify("cli.stream.vision_failed")
			}
		}
	}
	if len(enrichedParts) > 0 {
		prefix := strings.Join(enrichedParts, "\n\n")
		if text != "" {
			return prefix + "\n\n" + text
		}
		return prefix
	}
	if text != "" {
		return text
	}
	return "What do you see in this image?"
}

// Runs the vision tool on one image and decodes its JSON result, the failure text when either step fails
func (c *CLI) analyzeImage(imgPath string) (result map[string]any, failure string) {
	defer func() {
		if r := recover(); r != nil {
			result, failure = nil, panicText(r)
		}
	}()
	decoded, err := pyjson.Loads(visionAnalyzeTool(c.ctx, c.env, imgPath, visionAnalysisPrompt))
	if err != nil {
		return nil, err.Error()
	}
	obj, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, "'" + pyrepr.TypeName(decoded) + "' object has no attribute 'get'"
	}
	result = map[string]any{}
	for _, key := range []string{"success", "analysis"} {
		if v, ok := obj.Get(key); ok {
			result[key] = v
		}
	}
	return result, ""
}

// Text of a recovered panic value, the error message when it is an error
func panicText(r any) string {
	if err, ok := r.(error); ok {
		return err.Error()
	}
	return fmt.Sprint(r)
}

// Prints Rich markup through the command safe console, prompt safe once the TUI is live
func (c *CLI) consolePrint(markup string) {
	if c.app != nil {
		chatConsolePrint(markup)
		return
	}
	(&pyrich.Console{Out: c.console.Out, Color: c.console.ShouldUseColor(), Terminal: c.console.StdoutIsTTY(), Width: terminalColumns()}).Print(markup)
}

// Closes open blocks once and announces the tool whose arguments the model began generating
func (c *CLI) onToolGenStart(toolName string) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if c.streamBoxOpened {
		c.flushStreamLocked()
		c.streamBoxOpened = false
	}
	c.closeReasoningBox()
	if c.toolGenAnnounced == nil {
		c.toolGenAnnounced = map[string]bool{}
	}
	if c.toolGenAnnounced[toolName] {
		return
	}
	c.toolGenAnnounced[toolName] = true
	what := tools.BridgeGeneratingPhrase(toolName)
	if what == "" {
		what = toolName
	}
	emoji := tools.GetToolEmoji(c.env, toolName, "⚡")
	if c.modernLayout {
		c.setSpinnerText(emoji + " " + c.t("cli.modern.preparing", map[string]any{"what": what}))
		c.invalidateQuiet()
		return
	}
	cprint("  ┊ " + c.t("cli.stream.tool_preparing", map[string]any{"emoji": emoji, "what": what}))
}

// Number a progress extra holds, false when it holds none
func progressNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Tool lifecycle events driving the spinner, the turn summary and the stacked scrollback tool lines
func (c *CLI) onToolProgress(eventType, functionName, preview string, functionArgs map[string]any, extra map[string]any) {
	switch eventType {
	case "moa.reference":
		label := functionName
		if label == "" {
			label = c.t("cli.stream.reference_noun", nil)
		}
		header := c.t("cli.stream.reference_header", map[string]any{"label": label})
		if idx, count := extra["moa_index"], extra["moa_count"]; pyrepr.Truthy(idx) && pyrepr.Truthy(count) {
			header = c.t("cli.stream.reference_header_indexed", map[string]any{"index": idx, "count": count, "label": label})
		}
		func() {
			c.streamMu.Lock()
			defer c.streamMu.Unlock()
			defer func() { _ = recover() }()
			c.flushReasoningPreview(true)
		}()
		cprint("  " + dimANSI + "┊ ◇ " + header + resetANSI)
		func() {
			defer func() {
				if recover() != nil && pystr.Strip(preview) != "" {
					cprint("  " + dimANSI + pystr.Strip(preview) + resetANSI)
				}
			}()
			c.emitReasoningPreview(preview)
		}()
		c.invalidateQuiet()
		return
	case "moa.aggregating":
		if functionName != "" {
			c.setSpinnerText(c.t("cli.stream.aggregating_detail", map[string]any{"detail": functionName}))
		} else {
			c.setSpinnerText(c.t("cli.stream.aggregating", nil))
		}
		c.invalidateQuiet()
		return
	}
	isError := pyrepr.Truthy(extra["is_error"])
	switch {
	case eventType == "tool.started":
		c.uiMu.Lock()
		c.petReasoning = false
		c.uiMu.Unlock()
		c.streamMu.Lock()
		c.toolGenAnnounced = nil
		c.streamMu.Unlock()
	case eventType == "tool.completed" && isError:
		c.uiMu.Lock()
		c.petTurnError = true
		c.uiMu.Unlock()
	case strings.HasPrefix(eventType, "reasoning"):
		c.uiMu.Lock()
		c.petReasoning = true
		c.uiMu.Unlock()
	}
	if eventType == "tool.completed" {
		c.onToolCompletedProgress(functionName, extra, isError)
		return
	}
	if eventType != "tool.started" {
		c.showToolProgress(eventType, functionName, preview, extra["percent"])
		return
	}
	if functionName == "" || strings.HasPrefix(functionName, "_") {
		return
	}
	label := preview
	if label == "" {
		label = functionName
	}
	if pl := tools.GetToolPreviewMaxLen(); pl > 0 && pystr.Len(label) > pl {
		label = pystr.Slice(label, 0, pl-3) + "..."
	}
	spinner := tools.ToolRowEmoji(c.env, functionName, functionArgs, "") + " " + label
	if functionArgs == nil {
		functionArgs = map[string]any{}
	}
	c.streamMu.Lock()
	if c.runningToolLabels == nil {
		c.runningToolLabels = map[string][]string{}
	}
	c.runningToolLabels[functionName] = append(c.runningToolLabels[functionName], spinner)
	if c.pendingToolInfo == nil {
		c.pendingToolInfo = map[string][]map[string]any{}
	}
	c.pendingToolInfo[functionName] = append(c.pendingToolInfo[functionName], functionArgs)
	c.streamMu.Unlock()
	c.uiMu.Lock()
	c.spinnerText = spinner
	c.toolStartTime = monotonic()
	c.uiMu.Unlock()
	c.invalidateQuiet()
}

// Commits the scrollback line of one completed tool call and the one time long tool hint
func (c *CLI) onToolCompletedProgress(functionName string, extra map[string]any, isError bool) {
	c.uiMu.Lock()
	c.toolStartTime = 0
	c.uiMu.Unlock()
	c.streamMu.Lock()
	c.forgetToolLabel(functionName)
	c.streamMu.Unlock()
	c.turnSummaryRecord(functionName, extra["result"], isError)
	if c.focusViewEnabled {
		func() {
			defer func() { _ = recover() }()
			c.noteFocusHiddenLine(functionName)
		}()
	}
	if functionName != "" && (c.toolProgressMode == "new" || c.toolProgressMode == "all" || c.toolProgressMode == "verbose") {
		durationValue, hasDuration := extra["duration"]
		duration, numeric := progressNumber(durationValue)
		if !hasDuration {
			duration, numeric = 0.0, true
		}
		c.streamMu.Lock()
		stored, known := c.pendingToolInfo[functionName]
		storedArgs := map[string]any{}
		if len(stored) > 0 {
			storedArgs, stored = stored[0], stored[1:]
			c.pendingToolInfo[functionName] = stored
		}
		if known && len(stored) == 0 {
			delete(c.pendingToolInfo, functionName)
		}
		repeat := c.toolProgressMode == "new" && functionName == c.lastScrollbackTool
		if !repeat {
			c.lastScrollbackTool = functionName
		}
		c.streamMu.Unlock()
		if repeat {
			c.invalidateQuiet()
			return
		}
		if numeric {
			c.printCompletedToolLine(functionName, storedArgs, duration, extra["result"], isError)
			c.maybeShowLongToolHint(duration)
		}
	}
	c.invalidateQuiet()
}

// Prints the completed tool line, a rendering failure leaving scrollback untouched
func (c *CLI) printCompletedToolLine(functionName string, args map[string]any, duration float64, result any, isError bool) {
	defer func() { _ = recover() }()
	cprint(completedToolLine(c, functionName, args, duration, result, isError))
}

// Shows the verbose hint once after the first long tool in the noisiest mode and persists that it was seen
func (c *CLI) maybeShowLongToolHint(duration float64) {
	defer func() { _ = recover() }()
	if c.longToolHintFired || c.toolProgressMode != "all" || duration < 30.0 {
		return
	}
	cfg := CLIConfig(c.ctx)
	if tools.IsSeen(cfg, tools.OnboardingToolProgressFlag) {
		return
	}
	c.longToolHintFired = true
	cprint("  " + dimANSI + tools.ToolProgressHintCLI() + resetANSI)
	tools.MarkSeen(c.ctx, filepath.Join(home.Dir(c.ctx), "config.yaml"), tools.OnboardingToolProgressFlag)
	onboarding, ok := cfg["onboarding"].(map[string]any)
	if !ok {
		onboarding = map[string]any{}
		cfg["onboarding"] = onboarding
	}
	seen, ok := onboarding["seen"].(map[string]any)
	if !ok {
		seen = map[string]any{}
		onboarding["seen"] = seen
	}
	seen[tools.OnboardingToolProgressFlag] = true
}

// Drops the oldest running label of a tool once one of its calls completes
func (c *CLI) forgetToolLabel(functionName string) {
	labels := c.runningToolLabels[functionName]
	if len(labels) == 0 {
		return
	}
	if len(labels) == 1 {
		delete(c.runningToolLabels, functionName)
		return
	}
	c.runningToolLabels[functionName] = labels[1:]
}

// Rides live tool progress on the spinner line while exactly one call of that tool runs alone
func (c *CLI) showToolProgress(eventType, functionName, message string, percent any) {
	c.streamMu.Lock()
	running := c.runningToolLabels[functionName]
	total := 0
	for _, labels := range c.runningToolLabels {
		total += len(labels)
	}
	c.streamMu.Unlock()
	if eventType != "tool.progress" || len(running) != 1 || total != 1 {
		return
	}
	parts := []string{running[0]}
	if percent != nil {
		parts = append(parts, pyrepr.Str(percent)+"%")
	}
	if message != "" {
		parts = append(parts, message)
	}
	c.setSpinnerText(strings.Join(parts, " · "))
	c.invalidateQuiet()
}

// Captures the local before state of a write capable tool call
func (c *CLI) onToolStart(toolCallID, functionName string, functionArgs map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Debug(fmt.Sprintf("Edit snapshot capture failed for %s", functionName), "error", r)
		}
	}()
	snapshot := tools.CaptureLocalEditSnapshot(c.env, functionName, functionArgs, nil)
	if snapshot == nil {
		return
	}
	c.streamMu.Lock()
	if c.pendingEditSnapshots == nil {
		c.pendingEditSnapshots = map[string]*tools.LocalEditSnapshot{}
	}
	c.pendingEditSnapshots[toolCallID] = snapshot
	c.streamMu.Unlock()
}

// Announces a background delegation once and renders file edits as an inline diff
func (c *CLI) onToolComplete(toolCallID, functionName string, functionArgs map[string]any, functionResult string) {
	if functionName == "delegate_task" {
		c.announceBackgroundDelegation(functionResult)
	}
	c.streamMu.Lock()
	snapshot := c.pendingEditSnapshots[toolCallID]
	delete(c.pendingEditSnapshots, toolCallID)
	c.streamMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Debug(fmt.Sprintf("Edit diff preview failed for %s", functionName), "error", r)
		}
	}()
	tools.RenderEditDiffWithDelta(c.ctx, functionName, functionResult, functionArgs, snapshot, diffPrinter(cprint))
}

// Prints the dim running in background line when a delegate call was dispatched to the background
func (c *CLI) announceBackgroundDelegation(functionResult string) {
	defer func() { _ = recover() }()
	decoded, err := pyjson.Loads(functionResult)
	if err != nil {
		return
	}
	obj, ok := decoded.(*pyjson.Object)
	if !ok {
		return
	}
	status, _ := obj.Get("status")
	mode, _ := obj.Get("mode")
	if !pyrepr.Equal(status, "dispatched") || !pyrepr.Equal(mode, "background") {
		return
	}
	var n any = int64(1)
	if count, _ := obj.Get("count"); pyrepr.Truthy(count) {
		n = count
	}
	key := "cli.stream.background_running_other"
	if pyrepr.Equal(n, int64(1)) {
		key = "cli.stream.background_running_one"
	}
	cprint("\033[2m" + c.t(key, map[string]any{"count": n}) + "\033[0m")
}
