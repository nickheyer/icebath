package tui

import (
	"fmt"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
	uitheme "github.com/nickheyer/icebath/pkg/ui/theme"
)

// Rendered blocks the cache holds before it starts over
const blockCacheLimit = 4000

// Columns every transcript block is indented by
const transcriptIndent = 2

// Identity of a rendered block, its content, width and the look it was painted with
type blockKey struct {
	key   string
	width int
	look  string
}

// Everything a block is painted with, the session tones, the toolkit palette and the details in view
type renderCtx struct {
	t          theme
	ut         uitheme.Theme
	width      int
	details    detailsCtx
	timestamps bool
	look       string
	// Only the prompts and the replies show
	focus bool
	// No blank row between blocks
	compact bool
	// Chord that expands every folded detail, empty when unbound
	expandKey string
}

// Scrollable rows of the transcript and the live turn, rendered once per content and width
type transcriptView struct {
	md       *render.Markdown
	cache    map[blockKey][]string
	offset   int
	follow   bool
	total    int
	height   int
	manualAt float64
	now      func() float64
	// Row each user prompt starts at and its text, for the prompt kept in view while its reply scrolls
	prompts []promptRow
	stable  struct {
		key   blockKey
		lines []string
	}
}

// First row of a user prompt in the transcript
type promptRow struct {
	row  int
	text string
}

// View pinned to the newest row
func newTranscriptView(now func() float64) *transcriptView {
	return &transcriptView{md: render.NewMarkdown(), cache: map[blockKey][]string{}, follow: true, now: now}
}

func (v *transcriptView) lastManualScrollAt() float64 { return v.manualAt }
func (v *transcriptView) isSticky() bool              { return v.follow }
func (v *transcriptView) scrollToBottom()             { v.follow = true }

var _ scrollHandle = (*transcriptView)(nil)

// First row in view, the newest page while following
func (v *transcriptView) top() int {
	last := max(v.total-v.height, 0)
	if v.follow {
		return last
	}
	return min(max(v.offset, 0), last)
}

// Scrolls by rows, up for a negative count, following again once the newest row is reached
func (v *transcriptView) scrollBy(rows int) {
	next := v.top() + rows
	last := max(v.total-v.height, 0)
	v.offset = min(max(next, 0), last)
	v.follow = v.offset >= last
	v.manualAt = v.now()
}

// Jumps to the oldest row
func (v *transcriptView) scrollTop() {
	v.offset, v.follow, v.manualAt = 0, v.total <= v.height, v.now()
}

// Drops every rendered block, after a theme or details change
func (v *transcriptView) invalidate() { clear(v.cache) }

// Digest of what a block paints beyond its height key, the banner info, panel content and trail counts
func paintedFields(m msg) string {
	h := fnv.New64a()
	switch {
	case m.Kind == "intro":
		fmt.Fprint(h, m.Info)
	case m.Kind == "panel" && m.PanelData != nil:
		fmt.Fprintf(h, "%q", m.PanelData.Title)
		for _, s := range m.PanelData.Sections {
			title, text := "", ""
			if s.Title != nil {
				title = *s.Title
			}
			if s.Text != nil {
				text = *s.Text
			}
			fmt.Fprintf(h, "%q%q%q%q", title, text, s.Items, s.Rows)
		}
	}
	fmt.Fprint(h, m.Label, m.IsMoaReference, m.IsLiveReasoning, m.ThinkingTokens, m.ToolTokens, m.Todos)
	return strconv.FormatUint(h.Sum64(), 36)
}

// Rows of one block, rendered on the first ask and kept until anything it paints changes
func (v *transcriptView) block(m msg, c renderCtx) []string {
	key := blockKey{key: messageHeightKey(m) + ":" + paintedFields(m), width: c.width, look: c.look}
	if m.CreatedAt != nil && c.timestamps {
		key.key += "@" + strconv.FormatFloat(*m.CreatedAt, 'f', 0, 64)
	}
	if lines, ok := v.cache[key]; ok {
		return lines
	}
	if len(v.cache) > blockCacheLimit {
		clear(v.cache)
	}
	lines := v.renderBlock(m, c)
	v.cache[key] = lines
	return lines
}

// Wraps text to the block width under an indent, the first row led by a mark
func wrapUnder(text string, width int, lead string, st lipgloss.Style) []string {
	room := max(width-transcriptIndent, 8)
	var out []string
	for i, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		for j, row := range strings.Split(ansi.Wrap(line, room, ""), "\n") {
			prefix := strings.Repeat(" ", transcriptIndent)
			if i == 0 && j == 0 && lead != "" {
				prefix = lead
			}
			out = append(out, prefix+st.Render(row))
		}
	}
	return out
}

// Paints one transcript block by its kind and role
func (v *transcriptView) renderBlock(m msg, c renderCtx) []string {
	var lines []string
	switch {
	case m.Kind == "intro":
		lines = introLines(m.Info, c)
	case m.Kind == "panel" && m.PanelData != nil:
		lines = panelLines(*m.PanelData, c)
	case m.Kind == "trail":
		lines = trailLines(m, c)
	case m.Kind == "diff":
		lines = diffLines(m, c)
	case m.Kind == "slash":
		lines = wrapUnder(m.Text, c.width, "", toneStyle(c.t.Color.Accent))
	case m.Kind == "delegation":
		lines = delegationLines(m, c)
	case m.Role == "user":
		lead := toneStyle(c.t.Color.Prompt).Bold(true).Render(c.ut.Glyphs.Prompt + " ")
		lines = userLines(m.Text, c, lead)
	case m.Role == "assistant":
		if len(m.Tools) > 0 {
			lines = trailLines(msg{Kind: "trail", Role: "system", Tools: m.Tools}, c)
		}
		lines = append(lines, v.markdownLines(m.Text, c)...)
	case isUsageGaugeRow(m):
		lines = []string{strings.Repeat(" ", transcriptIndent) + usageGaugeRowView(c.t, m.Text)}
	default:
		tone := c.t.Color.Muted
		if strings.HasPrefix(m.Text, "error:") || strings.HasPrefix(m.Text, "✗") {
			tone = c.t.Color.Error
		}
		lines = wrapUnder(stripAnsi(m.Text), c.width, "", toneStyle(tone))
	}
	if stamp, ok := fmtMsgTimestamp(m.CreatedAt); ok && c.timestamps && len(lines) > 0 && m.Kind != "intro" {
		lines[0] = render.Fit(lines[0], max(c.width-ansi.StringWidth(stamp)-1, 1), c.ut) + " " + toneStyle(c.t.Color.Muted).Render(stamp)
	}
	return lines
}

// Lines a folded delegation result shows before its more line
const delegationPreviewLines = 4

// Delegation result under its header, its first lines and the keys that open it, every line once expanded
func delegationLines(m msg, c renderCtx) []string {
	pad := strings.Repeat(" ", transcriptIndent)
	lines := []string{pad + toneStyle(c.t.Color.Tool).Render(truncateEnd("◇ "+m.Label, max(c.width-transcriptIndent, 8)))}
	body := strings.Split(strings.TrimRight(stripAnsi(m.Text), "\n"), "\n")
	shown := len(body)
	switch sectionMode("subagents", c.details.DetailsMode, c.details.Sections, c.details.CommandOverride) {
	case "hidden":
		shown = 0
	case "collapsed":
		shown = min(shown, delegationPreviewLines)
	}
	muted := toneStyle(c.t.Color.Muted)
	room := max(c.width-transcriptIndent-2, 8)
	for _, row := range body[:shown] {
		for _, part := range strings.Split(ansi.Wrap(row, room, ""), "\n") {
			lines = append(lines, pad+"  "+muted.Render(part))
		}
	}
	if rest := len(body) - shown; rest > 0 {
		more := tr("transcript.delegation.moreCommand", rest)
		if c.expandKey != "" {
			more = tr("transcript.delegation.more", rest, c.expandKey)
		}
		lines = append(lines, pad+"  "+muted.Faint(true).Render(truncateEnd(more, room)))
	}
	return lines
}

// User text under the prompt glyph, a long paste backed message folded to its first words and a muted long message mark
func userLines(text string, c renderCtx, lead string) []string {
	body := toneStyle(c.t.Color.Text).Bold(true)
	if unitLen(text) <= longMsg || !isPasteBackedText(text) {
		return wrapUnder(text, c.width, lead, body)
	}
	mark := tr("chatBits.messageLine.longMessage")
	head, rest, _ := strings.Cut(userDisplay(text), mark)
	lines := wrapUnder(head, c.width, lead, body)
	last := len(lines) - 1
	lines[last] += toneStyle(c.t.Color.Muted).Faint(true).Render(mark) + body.Render(rest)
	return lines
}

// Assistant text as markdown rows under the block indent
func (v *transcriptView) markdownLines(text string, c renderCtx) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	rendered := v.md.Render(text, max(c.width-transcriptIndent, 10), c.ut)
	rows := strings.Split(rendered, "\n")
	for len(rows) > 0 && strings.TrimSpace(ansi.Strip(rows[0])) == "" {
		rows = rows[1:]
	}
	for len(rows) > 0 && strings.TrimSpace(ansi.Strip(rows[len(rows)-1])) == "" {
		rows = rows[:len(rows)-1]
	}
	pad := strings.Repeat(" ", transcriptIndent)
	for i := range rows {
		rows[i] = pad + rows[i]
	}
	return rows
}

// Offset where the streamed text can be split so the part before it never changes again
func stableStreamBoundary(text string) int {
	cut := strings.LastIndex(text, "\n\n")
	for cut > 0 {
		if strings.Count(text[:cut], "```")%2 == 0 {
			return cut + 2
		}
		cut = strings.LastIndex(text[:cut], "\n\n")
	}
	return 0
}

// Streamed reply rows, the settled paragraphs rendered once and only the growing tail redone
func (v *transcriptView) streamLines(text string, c renderCtx) []string {
	cut := stableStreamBoundary(text)
	var lines []string
	if cut > 0 {
		key := blockKey{key: hashText(text[:cut]) + ":" + strconv.Itoa(cut), width: c.width, look: c.look}
		if v.stable.key != key {
			v.stable.key, v.stable.lines = key, v.markdownLines(text[:cut], c)
		}
		lines = append(lines, v.stable.lines...)
		if len(lines) > 0 {
			lines = append(lines, "")
		}
	}
	return append(lines, v.markdownLines(text[cut:], c)...)
}

// Session banner of a fresh or resumed session, the brand, model, folder and what the agent has loaded
func introLines(raw *tuigateway.SessionInfo, c renderCtx) []string {
	info := infoOf(raw)
	pad := strings.Repeat(" ", transcriptIndent)
	name := c.t.Brand.Name
	if name == "" {
		name = "icebath"
	}
	head := toneStyle(c.t.Color.Primary).Bold(true).Render(strings.TrimSpace(c.t.Brand.Icon + " " + name))
	if version := infoText(info.Version); version != "" && version != "unknown" {
		head += toneStyle(c.t.Color.Muted).Render("  " + version)
	}
	lines := []string{pad + head}
	if welcome := trimJS(c.t.Brand.Welcome); welcome != "" {
		lines = append(lines, wrapUnder(welcome, c.width, "", toneStyle(c.t.Color.Muted))...)
	}
	row := func(label, value string) {
		if value != "" {
			lines = append(lines, pad+toneStyle(c.t.Color.Label).Render(fmt.Sprintf("%-9s", label))+toneStyle(c.t.Color.Text).Render(truncateEnd(value, max(c.width-transcriptIndent-9, 8))))
		}
	}
	row("model", infoText(info.Model))
	row("provider", infoText(info.Provider))
	row("cwd", shortCwd(infoText(info.Cwd), max(c.width-14, 16)))
	row("session", infoText(info.StoredSessionID))
	counts := []string{}
	for _, part := range []struct {
		n     int
		label string
	}{{groupedCount(info.Tools), "tools"}, {groupedCount(info.Skills), "skills"}, {len(info.MCPServers), "MCP servers"}} {
		if part.n > 0 {
			counts = append(counts, strconv.Itoa(part.n)+" "+part.label)
		}
	}
	row("loaded", strings.Join(counts, " · "))
	return append(lines, pad+toneStyle(c.t.Color.Border).Render(strings.Repeat("─", max(min(c.width-transcriptIndent*2, 60), 4))))
}

// Entries of every group of a grouped listing
func groupedCount(groups map[string][]string) int {
	n := 0
	for _, entries := range groups {
		n += len(entries)
	}
	return n
}

// Panel a slash command printed, its title then every section as text, items or aligned rows
func panelLines(p panelData, c renderCtx) []string {
	pad := strings.Repeat(" ", transcriptIndent)
	lines := []string{pad + toneStyle(c.t.Color.Primary).Bold(true).Render(p.Title)}
	for _, s := range p.Sections {
		if s.Title != nil && *s.Title != "" {
			lines = append(lines, "", pad+toneStyle(c.t.Color.Label).Bold(true).Render(*s.Title))
		}
		if s.Text != nil {
			lines = append(lines, wrapUnder(stripAnsi(*s.Text), c.width, "", toneStyle(c.t.Color.Text))...)
		}
		for _, item := range s.Items {
			lines = append(lines, wrapUnder(item, c.width-2, pad+toneStyle(c.t.Color.Muted).Render("· "), toneStyle(c.t.Color.Text))...)
		}
		keyWidth := 0
		for _, r := range s.Rows {
			keyWidth = max(keyWidth, ansi.StringWidth(r[0]))
		}
		keyWidth = min(keyWidth, max(c.width/2, 8))
		for _, r := range s.Rows {
			key := render.Fit(r[0], keyWidth, c.ut)
			value := truncateEnd(strings.ReplaceAll(r[1], "\n", " "), max(c.width-transcriptIndent-keyWidth-2, 8))
			lines = append(lines, pad+toneStyle(c.t.Color.Accent).Render(key)+"  "+toneStyle(c.t.Color.Text).Render(value))
		}
	}
	return lines
}

// Tool row painted by its outcome, a failed or refused call with its reason wrapped below
func toolRow(line string, c renderCtx) []string {
	pad := strings.Repeat(" ", transcriptIndent)
	rows := strings.Split(line, "\n")
	head := strings.TrimSpace(rows[0])
	tone, verdict := c.t.Color.Tool, ""
	switch {
	case strings.HasSuffix(head, "⊘"):
		tone, verdict = c.t.Color.Warn, c.t.Color.Warn
	case strings.HasSuffix(head, "✗"):
		tone, verdict = c.t.Color.Error, c.t.Color.Error
	case strings.HasSuffix(strings.TrimSpace(line), "✗"):
		tone = c.t.Color.Error
	}
	hint := ""
	if strings.HasPrefix(rows[0], "Delegate Task") && verdict == "" {
		hint = delegateMonitorHint("delegate_task", c)
	}
	out := []string{pad + toneStyle(tone).Render(truncateEnd(rows[0], max(c.width-transcriptIndent-ansi.StringWidth(hint), 8))) + hint}
	if verdict != "" {
		for _, row := range strings.Split(ansi.Wrap(strings.Join(rows[1:], " "), max(c.width-transcriptIndent-2, 8), ""), "\n") {
			out = append(out, pad+"  "+toneStyle(verdict).Render(row))
		}
		return out
	}
	for _, extra := range rows[1:] {
		out = append(out, pad+"  "+toneStyle(c.t.Color.Muted).Render(truncateEnd(extra, max(c.width-transcriptIndent-2, 8))))
	}
	return out
}

// Task list rows, each item marked by its status and nested under its parent
func todoLines(todos []todoItem, c renderCtx) []string {
	pad := strings.Repeat(" ", transcriptIndent)
	open := countPendingTodos(todos)
	lines := []string{pad + toneStyle(c.t.Color.Label).Bold(true).Render("Tasks") + toneStyle(c.t.Color.Muted).Render(fmt.Sprintf("  %d open of %d", open, len(todos)))}
	for _, todo := range todos {
		mark, tone := "☐", c.t.Color.Text
		switch todo.Status {
		case "completed":
			mark, tone = "☑", c.t.Color.Ok
		case "in_progress":
			mark, tone = "◐", c.t.Color.Warn
		case "cancelled":
			mark, tone = "☒", c.t.Color.Muted
		}
		indent := pad
		if todo.Parent != "" {
			indent += "  "
		}
		lines = append(lines, indent+toneStyle(tone).Render(mark+" "+truncateEnd(todo.Content, max(c.width-ansi.StringWidth(indent)-2, 8))))
	}
	return lines
}

// Trail of a turn, its reasoning, tool rows and task list as the details sections allow
func trailLines(m msg, c renderCtx) []string {
	pad := strings.Repeat(" ", transcriptIndent)
	mode := func(section string) string {
		return sectionMode(section, c.details.DetailsMode, c.details.Sections, c.details.CommandOverride)
	}
	var lines []string
	if thinking := cleanThinkingText(m.Thinking); thinking != "" && (m.IsMoaReference || mode("thinking") != "hidden") {
		label := "thinking"
		if m.IsMoaReference {
			label = "reference"
		}
		head := toneStyle(c.t.Color.Thinking).Render("◇ " + label)
		if m.ThinkingTokens > 0 {
			head += toneStyle(c.t.Color.Muted).Render(" · " + render.Tokens(m.ThinkingTokens) + " tokens")
		}
		if mode("thinking") == "expanded" || m.IsLiveReasoning {
			lines = append(lines, pad+head)
			lines = append(lines, wrapUnder(boundedRenderText(thinking, "", liveRenderMaxChars, liveRenderMaxLines), c.width-2, "", toneStyle(c.t.Color.Muted).Italic(true))...)
		} else {
			preview := compactPreview(strings.Join(strings.Fields(thinking), " "), max(c.width-ansi.StringWidth(ansi.Strip(head))-transcriptIndent-3, 8))
			lines = append(lines, pad+head+toneStyle(c.t.Color.Muted).Italic(true).Render("  "+preview))
		}
	}
	if len(m.Tools) > 0 && mode("tools") != "hidden" {
		for _, line := range m.Tools {
			lines = append(lines, toolRow(line, c)...)
		}
		if m.ToolTokens > 0 {
			lines = append(lines, pad+toneStyle(c.t.Color.Muted).Render("  "+render.Tokens(m.ToolTokens)+" tool tokens"))
		}
	}
	if len(m.Todos) > 0 {
		if m.TodoCollapsedByDefault && mode("tools") != "expanded" {
			lines = append(lines, pad+toneStyle(c.t.Color.Ok).Render(fmt.Sprintf("☑ %d tasks done", len(m.Todos))))
		} else {
			lines = append(lines, todoLines(m.Todos, c)...)
		}
	}
	return lines
}

// Inline diff of a file edit, added rows in the added tone and removed rows in the removed tone
func diffLines(m msg, c renderCtx) []string {
	body, ok := diffSegmentBody(m)
	if !ok {
		body = m.Text
	}
	pad := strings.Repeat(" ", transcriptIndent)
	var lines []string
	for _, row := range strings.Split(strings.TrimRight(stripAnsi(body), "\n"), "\n") {
		tone := c.t.Color.Muted
		switch {
		case strings.HasPrefix(row, "+++"), strings.HasPrefix(row, "---"):
			tone = c.t.Color.Label
		case strings.HasPrefix(row, "@@"):
			tone = c.t.Color.Accent
		case strings.HasPrefix(row, "+"):
			tone = c.t.Color.DiffAdded
		case strings.HasPrefix(row, "-"):
			tone = c.t.Color.DiffRemoved
		}
		lines = append(lines, pad+toneStyle(tone).Render(truncateEnd(strings.ReplaceAll(row, "\t", "    "), max(c.width-transcriptIndent, 8))))
	}
	return lines
}

// Rows of the turn in flight, its settled segments, reasoning, running tools, streamed text, activity and tasks
func (v *transcriptView) liveLines(s *tuiStore, stream string, c renderCtx, tick int) []string {
	pad := strings.Repeat(" ", transcriptIndent)
	turn := s.turn
	var lines []string
	gap := func() {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
	}
	for _, seg := range turn.StreamSegments {
		if blockRenders(seg, c.details) {
			gap()
			lines = append(lines, v.block(seg, c)...)
		}
	}
	thinkingMode := sectionMode("thinking", c.details.DetailsMode, c.details.Sections, c.details.CommandOverride)
	if turn.ReasoningActive && thinkingMode != "hidden" && trimJS(turn.Reasoning) != "" {
		gap()
		lines = append(lines, trailLines(msg{Kind: "trail", Role: "system", Thinking: turn.Reasoning, ThinkingTokens: turn.ReasoningTokens, IsLiveReasoning: thinkingMode == "expanded"}, c)...)
	}
	if sectionMode("tools", c.details.DetailsMode, c.details.Sections, c.details.CommandOverride) != "hidden" {
		for _, line := range turn.TurnTrail {
			lines = append(lines, toolRow(line, c)...)
		}
		for _, tool := range turn.Tools {
			call := formatToolCall(tool.Name, tool.Context)
			if len(tool.Labels) > 0 {
				call = formatToolLabels(tool.Labels)
			}
			elapsed := ""
			if !tool.StartedAt.IsZero() {
				if d := s.now().Sub(tool.StartedAt); d >= time.Second {
					elapsed = toneStyle(c.t.Color.Muted).Render(" " + render.Elapsed(d))
				}
			}
			if tool.Progress != "" {
				elapsed += toneStyle(c.t.Color.Muted).Render(" · " + tool.Progress)
			} else if d := s.now().Sub(tool.StartedAt); !tool.StartedAt.IsZero() && d >= 12*time.Second {
				elapsed += toneStyle(c.t.Color.Muted).Italic(true).Render(" · " + longRunCharms[int(d/(12*time.Second))%len(longRunCharms)])
			}
			lines = append(lines, pad+render.Spinner(tick, c.ut)+" "+toneStyle(c.t.Color.Tool).Render(truncateEnd(call, max(c.width-transcriptIndent-40, 8)))+delegateMonitorHint(tool.Name, c)+elapsed)
			if tool.VerboseArgs != "" {
				lines = append(lines, wrapUnder(boundedRenderText(tool.VerboseArgs, "", 600, 8), c.width-2, "", toneStyle(c.t.Color.Muted))...)
			}
		}
		for _, name := range turn.StreamPendingTools {
			lines = append(lines, pad+toneStyle(c.t.Color.Muted).Render("… "+name))
		}
	}
	if strings.TrimSpace(stream) != "" {
		gap()
		lines = append(lines, v.streamLines(stream, c)...)
	}
	if sectionMode("activity", c.details.DetailsMode, c.details.Sections, c.details.CommandOverride) != "hidden" {
		for _, item := range turn.Activity {
			tone := c.t.Color.Muted
			switch item.Tone {
			case "warn":
				tone = c.t.Color.Warn
			case "error":
				tone = c.t.Color.Error
			}
			lines = append(lines, wrapUnder(item.Text, c.width, "", toneStyle(tone))...)
		}
	}
	if len(turn.Todos) > 0 {
		gap()
		if turn.TodoCollapsed {
			lines = append(lines, pad+toneStyle(c.t.Color.Label).Render(fmt.Sprintf("Tasks  %d open of %d", countPendingTodos(turn.Todos), len(turn.Todos))))
		} else {
			lines = append(lines, todoLines(turn.Todos, c)...)
		}
	}
	if sectionMode("subagents", c.details.DetailsMode, c.details.Sections, c.details.CommandOverride) != "hidden" {
		if len(turn.Subagents) > 0 {
			lines = append(lines, pad+ansi.Truncate(spawnTreeHeadline(turn.Subagents, c), max(c.width-transcriptIndent, 8), "…"))
		}
		for _, sub := range turn.Subagents {
			mark, tone := render.Spinner(tick, c.ut), c.t.Color.Tool
			switch sub.Status {
			case "completed":
				mark, tone = toneStyle(c.t.Color.Ok).Render("✓"), c.t.Color.Muted
			case "failed", "error", "interrupted":
				mark, tone = toneStyle(c.t.Color.Error).Render("✗"), c.t.Color.Error
			}
			detail := fmt.Sprintf(" · %d tools", sub.ToolCount)
			goal := truncateEnd(sub.Goal, max(c.width-transcriptIndent-ansi.StringWidth(detail)-4-sub.Depth*2, 8))
			lines = append(lines, pad+strings.Repeat("  ", sub.Depth)+mark+" "+toneStyle(tone).Render(goal)+toneStyle(c.t.Color.Muted).Render(detail))
		}
	}
	return lines
}

// Every row of the transcript followed by the live turn, blocks separated where their groups change
func (v *transcriptView) rows(history []msg, s *tuiStore, stream string, c renderCtx, tick int) []string {
	var lines []string
	var prev *msg
	v.prompts = v.prompts[:0]
	for i := range history {
		m := history[i]
		if !blockRenders(m, c.details) || (c.focus && m.Role != "user" && m.Role != "assistant" && m.Kind != "intro") {
			continue
		}
		block := v.block(m, c)
		if len(block) == 0 {
			continue
		}
		if !c.compact && (hasLeadGap(prev, m) || (prev != nil && messageGroup(*prev) == "user" && messageGroup(m) != "user") || (prev != nil && messageGroup(m) == "user")) {
			lines = append(lines, "")
		}
		if m.Role == "user" && m.Kind == "" {
			v.prompts = append(v.prompts, promptRow{row: len(lines), text: userDisplay(m.Text)})
		}
		lines = append(lines, block...)
		prev = &history[i]
	}
	if live := v.liveLines(s, stream, c, tick); len(live) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, live...)
	}
	return lines
}

// Prompt of the exchange at the top of the view once the prompt itself scrolled above it
func (v *transcriptView) stickyPrompt(top int) string {
	text := ""
	for _, p := range v.prompts {
		if p.row >= top {
			break
		}
		text = p.text
	}
	return text
}

// Width the transcript wraps to, one column short of the pane so the scrollbar never covers text
func scrollbarRoom(width int) int {
	if width > 4 {
		return width - 1
	}
	return width
}

// Window of rows at the scroll position with a scrollbar column when the transcript overflows
func (v *transcriptView) view(lines []string, width, height int, c renderCtx) string {
	v.total, v.height = len(lines), height
	top := v.top()
	out := make([]string, 0, height)
	for i := top; i < top+height && i < len(lines); i++ {
		out = append(out, lines[i])
	}
	for len(out) < height {
		out = append(out, "")
	}
	if sticky := v.stickyPrompt(top); sticky != "" && height > 2 {
		lead := toneStyle(c.t.Color.Prompt).Render(c.ut.Glyphs.Prompt + " ")
		out[0] = lead + toneStyle(c.t.Color.Muted).Render(truncateEnd(strings.ReplaceAll(sticky, "\n", " "), max(width-6, 8)))
	}
	if v.total > height && width > 4 {
		scrollbarRows(out, top, v.total, width, c.t, c.ut)
	}
	return strings.Join(out, "\n")
}

// Window rows fitted one column short of the width with a scrollbar beside them, its thumb at the scroll position
func scrollbarRows(out []string, top, total, width int, t theme, ut uitheme.Theme) {
	height := len(out)
	thumb := max(height*height/total, 1)
	start := top * (height - thumb) / (total - height)
	for i := range out {
		mark := toneStyle(t.Color.Border).Render("│")
		if i >= start && i < start+thumb {
			mark = toneStyle(t.Color.Accent).Render("┃")
		}
		out[i] = render.Fit(out[i], width-1, ut) + mark
	}
}
