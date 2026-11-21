package tui

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/tuigateway"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
	uitheme "github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Rendered blocks the cache holds before it starts over
const blockCacheLimit = 4000

// Columns every transcript block is indented by
const transcriptIndent = 2

// Identity of a rendered block, its content, width, the look it was painted with and what the user opened or closed in it
type blockKey struct {
	key   string
	width int
	look  string
	state string
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
	// Pictures drawn under the rows that carry them, nil or off drawing none
	images       *inlineImages
	inlineImages bool
}

// Scrollable rows of the transcript and the live turn, rendered once per content and width
type transcriptView struct {
	md       *render.Markdown
	cache    map[blockKey]blockRows
	offset   int
	follow   bool
	total    int
	height   int
	width    int
	manualAt float64
	now      func() float64
	// Blocks of the last layout in row order, the rows of the turn in flight last
	placed []placedBlock
	// What the user opened or closed, by the identity of the block and the part of it
	open map[uint64]map[string]bool
	// Whether the pasted text of sent prompts shows unless a section says otherwise
	pastesOpen bool
	// Mode of every details section at the last layout, a change of one dropping what the user chose among the trail sections
	details string
	// Identity the newest block was given
	lastID uint64
	// Transcript row of a header just clicked, minus one when none waits to be kept in the window
	keep int
	// Block the window is held at across the next layout
	anchor *rowAnchor
	// Whether the first row of the window shows the prompt kept in view over its own row
	sticky bool
	// Row each user prompt starts at and its text, for the prompt kept in view while its reply scrolls
	prompts []promptRow
	// Pictures wholly inside the window as last painted, their rows counted from the first row of the window
	inView []imageSpot
	stable struct {
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
	return &transcriptView{md: render.NewMarkdown(), cache: map[blockKey]blockRows{}, open: map[uint64]map[string]bool{}, follow: true, keep: -1, now: now}
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
	fmt.Fprint(h, m.Label, m.IsMoaReference, m.IsLiveReasoning, m.ThinkingTokens, m.ToolTokens, m.Todos, m.TodoIncomplete, m.TodoCollapsedByDefault, m.Images, m.ImageRevision, m.ImageRows)
	for _, paste := range m.Pastes {
		fmt.Fprint(h, paste.Label, paste.Digest)
	}
	return strconv.FormatUint(h.Sum64(), 36)
}

// Rows of one block with its parts that take a click, rendered on the first ask and kept until anything it paints or what the user opened in it changes
func (v *transcriptView) block(m msg, c renderCtx) blockRows {
	st := v.state(m)
	key := blockKey{key: messageHeightKey(m) + ":" + paintedFields(m), width: c.width, look: c.look, state: st.key()}
	if m.CreatedAt != nil && c.timestamps {
		key.key += "@" + strconv.FormatFloat(*m.CreatedAt, 'f', 0, 64)
	}
	if len(m.Images) > 0 && c.images != nil {
		key.key += "#" + strconv.Itoa(c.images.gen)
	}
	if rows, ok := v.cache[key]; ok {
		return rows
	}
	if len(v.cache) > blockCacheLimit {
		clear(v.cache)
	}
	rows := v.renderBlock(m, c, st)
	v.cache[key] = rows
	return rows
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

// Units of a system message past which it folds to its first line
const systemFoldUnits = 400

// Units of the first line a folded system message keeps
const systemFoldHead = 120

// Paints one transcript block by its kind and role under what the user opened or closed in it
func (v *transcriptView) renderBlock(m msg, c renderCtx, st blockState) blockRows {
	var out blockRows
	switch {
	case m.Kind == "intro":
		out = introRows(m.Info, c)
	case m.Kind == "panel" && m.PanelData != nil:
		out.lines = panelLines(*m.PanelData, c)
	case m.Kind == "trail":
		out = trailRows(m, c, st)
	case m.Kind == "diff":
		out.lines = diffLines(m, c)
	case m.Kind == "image":
		lines, spots := imageLines(m, c)
		return blockRows{lines: lines, images: spots}
	case m.Kind == "slash":
		out.lines = wrapUnder(m.Text, c.width, "", toneStyle(c.t.Color.Accent))
	case m.Kind == "delegation":
		out.lines = delegationLines(m, c)
	case m.Role == "user":
		lead := toneStyle(c.t.Color.Prompt).Bold(true).Render(c.ut.Glyphs.Prompt + " ")
		out.lines = userLines(m.Text, c, lead)
	case m.Role == "assistant":
		if len(m.Tools) > 0 {
			out = trailRows(msg{Kind: "trail", Role: "system", Tools: m.Tools}, c, st)
		}
		out.lines = append(out.lines, v.markdownLines(m.Text, c)...)
	case isUsageGaugeRow(m):
		out.lines = []string{strings.Repeat(" ", transcriptIndent) + usageGaugeRowView(c.t, m.Text)}
	default:
		tone := c.t.Color.Muted
		if isErrorLine(m.Text) || strings.HasPrefix(m.Text, "✗") {
			tone = c.t.Color.Error
		}
		if m.Role == "system" && m.Kind == "" && unitLen(m.Text) > systemFoldUnits {
			out = systemRows(m.Text, tone, c, st)
		} else {
			out.lines = wrapUnder(stripAnsi(m.Text), c.width, "", toneStyle(tone))
		}
	}
	if pictures, spots := imageLines(m, c); len(pictures) > 0 {
		at := len(out.lines) + 1
		out.lines = append(append(out.lines, ""), pictures...)
		for _, spot := range spots {
			spot.row += at
			out.images = append(out.images, spot)
		}
	}
	if len(m.Pastes) > 0 {
		out.add(pasteRows(m, c, st))
	}
	if stamp, ok := fmtMsgTimestamp(m.CreatedAt); ok && c.timestamps && len(out.lines) > 0 && m.Kind != "intro" {
		out.lines[0] = render.Fit(out.lines[0], max(c.width-ansi.StringWidth(stamp)-1, 1), c.ut) + " " + toneStyle(c.t.Color.Muted).Render(stamp)
	}
	return out
}

// Long system message folded to its first line and its size, the whole text under the header once opened
func systemRows(text, tone string, c renderCtx, st blockState) blockRows {
	plain := stripAnsi(text)
	first, _, _ := strings.Cut(plain, "\n")
	if first = jsSlice(trimJS(first), 0, systemFoldHead); first == "" {
		first = tr(txt.ChatBitsMessageLineSystemMessage)
	}
	size := " — " + tr(txt.ChatBitsMessageLineChars, groupDigits(float64(unitLen(text))))
	open := st.open(partSystem, false)
	room := max(c.width-transcriptIndent-2-ansi.StringWidth(size), 8)
	var out blockRows
	out.head(partSystem, open, strings.Repeat(" ", transcriptIndent)+chevron(open, c)+toneStyle(tone).Render(truncateEnd(first, room))+toneStyle(c.t.Color.Muted).Faint(true).Render(size))
	if open {
		out.lines = append(out.lines, nested(wrapUnder(plain, c.width-2, "", toneStyle(tone)))...)
	}
	return out
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
		more := tr(txt.TranscriptDelegationMoreCommand, rest)
		if c.expandKey != "" {
			more = tr(txt.TranscriptDelegationMore, rest, c.expandKey)
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
	mark := tr(txt.ChatBitsMessageLineLongMessage)
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

// Session banner of a fresh or resumed session, the art, the brand, model, folder, what the agent has loaded and the size of its system prompt on a row that takes a click
func introRows(raw *tuigateway.SessionInfo, c renderCtx) blockRows {
	info := infoOf(raw)
	pad := strings.Repeat(" ", transcriptIndent)
	name := c.t.Brand.Name
	if name == "" {
		name = "icebath"
	}
	head := toneStyle(c.t.Color.Primary).Bold(true).Render(strings.TrimSpace(c.ut.Glyphs.Brand + " " + name))
	if version := infoText(info.Version); version != "" && version != "unknown" {
		if released := infoText(info.ReleaseDate); released != "" {
			version += " (" + released + ")"
		}
		head += toneStyle(c.t.Color.Muted).Render("  " + version)
	}
	lines := bannerArt(c)
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	lines = append(lines, pad+head)
	if welcome := trimJS(c.t.Brand.Welcome); welcome != "" {
		lines = append(lines, wrapUnder(welcome, c.width, "", toneStyle(c.t.Color.Muted))...)
	}
	labels := []string{tr(txt.ChatBitsBrandingRowModel), tr(txt.ChatBitsBrandingRowProvider), tr(txt.ChatBitsBrandingRowCwd), tr(txt.ChatBitsBrandingRowSession), tr(txt.ChatBitsBrandingRowLoaded)}
	labelWidth := 0
	for _, label := range labels {
		labelWidth = max(labelWidth, ansi.StringWidth(label)+1)
	}
	row := func(label, value string) {
		if value != "" {
			lines = append(lines, pad+toneStyle(c.t.Color.Label).Render(label+strings.Repeat(" ", labelWidth-ansi.StringWidth(label)))+toneStyle(c.t.Color.Text).Render(truncateEnd(value, max(c.width-transcriptIndent-labelWidth, 8))))
		}
	}
	row(labels[0], infoText(info.Model))
	row(labels[1], infoText(info.Provider))
	row(labels[2], shortCwd(infoText(info.Cwd), max(c.width-transcriptIndent-labelWidth-3, 16)))
	row(labels[3], infoText(info.StoredSessionID))
	connected := 0
	for _, server := range info.MCPServers {
		if server.Connected {
			connected++
		}
	}
	counts := []string{}
	for _, part := range []struct {
		n   int
		key txt.Key
	}{{groupedCount(info.Tools), txt.ChatBitsBrandingToolsSummary}, {groupedCount(info.Skills), txt.ChatBitsBrandingSkillsSummary}, {connected, txt.ChatBitsBrandingMcpServersSummary}} {
		if part.n > 0 {
			counts = append(counts, tr(part.key, part.n))
		}
	}
	if len(counts) == 0 && val(info.Lazy) {
		counts = append(counts, tr(txt.ChatBitsBrandingScanningSkills)+"…")
	}
	row(labels[4], strings.Join(counts, " · "))
	var parts []blockPart
	if size := unitLen(val(info.SystemPrompt)); size > 0 {
		parts = append(parts, blockPart{name: partPrompt, from: len(lines), to: len(lines) + 1})
		title := toneStyle(c.t.Color.Label).Render(tr(txt.ChatBitsBrandingSystemPrompt)) + toneStyle(c.t.Color.Muted).Render(" "+tr(txt.ChatBitsBrandingCharsSuffix, groupDigits(float64(size))))
		lines = append(lines, pad+chevron(false, c)+ansi.Truncate(title, max(c.width-transcriptIndent*2-2, 8), "…"))
	}
	if notice := updateNotice(info); notice != "" {
		lines = append(lines, pad+toneStyle(c.t.Color.Warn).Bold(true).Render(truncateEnd(notice, max(c.width-transcriptIndent*2, 8))))
	}
	span := 0
	for _, line := range lines {
		span = max(span, ansi.StringWidth(line)-transcriptIndent)
	}
	return blockRows{lines: append(lines, pad+toneStyle(c.t.Color.Border).Render(strings.Repeat("─", max(min(span, c.width-transcriptIndent*2), 4)))), parts: parts}
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

// Task list under a header that takes a click, folded to the header alone or each item marked by its status and nested under its parent up to four levels, the header saying so when the turn ended with items still open
func todoRows(todos []todoItem, c renderCtx, incomplete, folded bool) blockRows {
	pad := strings.Repeat(" ", transcriptIndent)
	open := countPendingTodos(todos)
	head := toneStyle(c.t.Color.Label).Bold(true).Render(tr(txt.ChatBitsTodoTitle)) + toneStyle(c.t.Color.Muted).Render("  "+tr(txt.ChatBitsTodoOpenOf, open, len(todos)))
	if folded && isTodoDone(todos) {
		head = toneStyle(c.t.Color.Ok).Render("☑ " + tr(txt.ChatBitsTodoAllDone, len(todos)))
	}
	if incomplete && open > 0 {
		key := txt.ChatBitsTodoIncompleteOther
		if open == 1 {
			key = txt.ChatBitsTodoIncompleteOne
		}
		head += toneStyle(c.t.Color.Muted).Faint(true).Render(" " + tr(key, open))
	}
	var out blockRows
	out.head(partTodos, !folded, pad+chevron(!folded, c)+head)
	if folded {
		return out
	}
	pad += "  "
	lines := out.lines
	for _, row := range todoTree(todos) {
		todo := row.item
		mark, tone := "☐", c.t.Color.Text
		switch todo.Status {
		case "completed":
			mark, tone = "☑", c.t.Color.Ok
		case "in_progress":
			mark, tone = "◐", c.t.Color.Warn
		case "cancelled":
			mark, tone = "☒", c.t.Color.Muted
		}
		indent := pad + strings.Repeat("  ", min(row.depth, 4))
		lines = append(lines, indent+toneStyle(tone).Render(mark+" "+truncateEnd(todo.Content, max(c.width-ansi.StringWidth(indent)-2, 8))))
	}
	out.lines = lines
	return out
}

// Header of a tool calls section, how many calls it holds and the tokens they took
func toolsHead(count, tokens int, open bool, c renderCtx) string {
	head := chevron(open, c) + toneStyle(c.t.Color.Tool).Render(tr(txt.ChatBitsThinkingToolCalls)) + toneStyle(c.t.Color.Muted).Render(" ("+strconv.Itoa(count)+")")
	if tokens > 0 {
		head += toneStyle(c.t.Color.Muted).Render(" · " + tr(txt.ChatBitsThinkingTokens, render.Tokens(tokens)))
	}
	return head
}

// Trail of a turn, its reasoning and tool rows each under a header that opens and closes it and its task list, as the details sections allow and the user chose
func trailRows(m msg, c renderCtx, st blockState) blockRows {
	pad := strings.Repeat(" ", transcriptIndent)
	mode := func(section string) string {
		return sectionMode(section, c.details.DetailsMode, c.details.Sections, c.details.CommandOverride)
	}
	muted := toneStyle(c.t.Color.Muted)
	var out blockRows
	thinkingTokens := 0
	if thinking := cleanThinkingText(m.Thinking); thinking != "" && (m.IsMoaReference || mode(partThinking) != "hidden") {
		if thinkingTokens = m.ThinkingTokens; thinkingTokens == 0 {
			thinkingTokens = estimateTokensRough(thinking)
		}
		label := tr(txt.ChatBitsThinkingThinking)
		if m.IsMoaReference {
			label = tr(txt.ChatBitsThinkingReference)
		}
		open := st.open(partThinking, mode(partThinking) == "expanded" || m.IsLiveReasoning || m.IsMoaReference)
		head := chevron(open, c) + toneStyle(c.t.Color.Thinking).Render(label)
		if m.ThinkingTokens > 0 {
			head += muted.Render(" · " + tr(txt.ChatBitsThinkingTokens, render.Tokens(m.ThinkingTokens)))
		}
		if open {
			out.head(partThinking, true, pad+head)
			out.lines = append(out.lines, nested(wrapUnder(boundedRenderText(thinking, "", liveRenderMaxChars, liveRenderMaxLines), c.width-4, "", muted.Italic(true)))...)
		} else {
			preview := compactPreview(strings.Join(strings.Fields(thinking), " "), max(c.width-ansi.StringWidth(ansi.Strip(head))-transcriptIndent-3, 8))
			out.head(partThinking, false, pad+head+muted.Italic(true).Render("  "+preview))
		}
	}
	if len(m.Tools) > 0 && mode(partTools) != "hidden" {
		open := st.open(partTools, mode(partTools) == "expanded")
		out.head(partTools, open, pad+toolsHead(len(m.Tools), m.ToolTokens, open, c))
		if open {
			inner := c
			inner.width -= 2
			for _, line := range m.Tools {
				out.lines = append(out.lines, nested(toolRow(line, inner))...)
			}
		}
	}
	if thinkingTokens > 0 && m.ToolTokens > 0 && len(out.lines) > 0 {
		out.lines = append(out.lines, pad+muted.Faint(true).Render("Σ "+tr(txt.ChatBitsThinkingApproxTotal, render.Tokens(thinkingTokens+m.ToolTokens))))
	}
	if len(m.Todos) > 0 {
		out.add(todoRows(m.Todos, c, m.TodoIncomplete, !st.open(partTodos, !m.TodoCollapsedByDefault || mode(partTools) == "expanded")))
	}
	return out
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

// Rows of the turn in flight, its settled segments, running tools, streamed text, activity and tasks, the tools, the activity and the tasks each under a header that opens and closes them, with where each segment landed among the rows
func (v *transcriptView) liveRows(s *tuiStore, stream string, c renderCtx, tick int) (blockRows, []placedBlock) {
	pad := strings.Repeat(" ", transcriptIndent)
	turn := s.turn
	st := v.state(msg{ID: liveBlock})
	mode := func(section string) string {
		return sectionMode(section, c.details.DetailsMode, c.details.Sections, c.details.CommandOverride)
	}
	var out blockRows
	var placed []placedBlock
	gap := func() {
		if n := len(out.lines); n > 0 && out.lines[n-1] != "" {
			out.lines = append(out.lines, "")
		}
	}
	for i := range turn.StreamSegments {
		v.identify(&turn.StreamSegments[i])
		if seg := turn.StreamSegments[i]; blockRenders(seg, c.details) {
			gap()
			block := v.block(seg, c)
			placed = append(placed, placedBlock{id: seg.ID, row: len(out.lines), rows: len(block.lines), parts: block.parts, images: block.images})
			out.lines = append(out.lines, block.lines...)
		}
	}
	if mode(partTools) != "hidden" {
		calls := len(turn.Tools) + len(turn.StreamPendingTools)
		for _, line := range turn.TurnTrail {
			if line != analyzingToolOutput() {
				calls++
			}
		}
		open, inner := true, c
		if calls > 0 {
			open = st.open(partTools, mode(partTools) == "expanded")
			out.head(partTools, open, pad+toolsHead(calls, 0, open, c))
			inner.width -= 2
		}
		var rows []string
		for _, line := range turn.TurnTrail {
			rows = append(rows, toolRow(line, inner)...)
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
			rows = append(rows, pad+render.Spinner(tick, c.ut)+" "+toneStyle(c.t.Color.Tool).Render(truncateEnd(call, max(inner.width-transcriptIndent-40, 8)))+delegateMonitorHint(tool.Name, c)+elapsed)
			if tool.VerboseArgs != "" {
				rows = append(rows, wrapUnder(boundedRenderText(tool.VerboseArgs, "", 600, 8), inner.width-2, "", toneStyle(c.t.Color.Muted))...)
			}
		}
		for _, line := range turn.StreamPendingTools {
			rows = append(rows, toolRow(line, inner)...)
		}
		switch {
		case !open:
		case calls > 0:
			out.lines = append(out.lines, nested(rows)...)
		default:
			out.lines = append(out.lines, rows...)
		}
	}
	if strings.TrimSpace(stream) != "" {
		gap()
		out.lines = append(out.lines, v.streamLines(stream, c)...)
	}
	if len(turn.Activity) > 0 && mode(partActivity) != "hidden" {
		open := st.open(partActivity, mode(partActivity) == "expanded")
		head := c.t.Color.Muted
		for _, item := range turn.Activity {
			switch {
			case item.Tone == "error":
				head = c.t.Color.Error
			case item.Tone == "warn" && head != c.t.Color.Error:
				head = c.t.Color.Warn
			}
		}
		out.head(partActivity, open, pad+chevron(open, c)+toneStyle(head).Render(tr(txt.ChatBitsThinkingActivity))+toneStyle(c.t.Color.Muted).Render(" ("+strconv.Itoa(len(turn.Activity))+")"))
		for _, item := range turn.Activity {
			if !open {
				break
			}
			tone := c.t.Color.Muted
			switch item.Tone {
			case "warn":
				tone = c.t.Color.Warn
			case "error":
				tone = c.t.Color.Error
			}
			out.lines = append(out.lines, nested(wrapUnder(item.Text, c.width-2, "", toneStyle(tone)))...)
		}
	}
	if len(turn.Todos) > 0 {
		gap()
		out.add(todoRows(turn.Todos, c, false, turn.TodoCollapsed))
	}
	if mode("subagents") != "hidden" {
		if len(turn.Subagents) > 0 {
			out.lines = append(out.lines, pad+ansi.Truncate(spawnTreeHeadline(turn.Subagents, c), max(c.width-transcriptIndent, 8), "…"))
		}
		for _, sub := range turn.Subagents {
			mark, tone := render.Spinner(tick, c.ut), c.t.Color.Tool
			switch sub.Status {
			case "", "running", "finalizing":
			case "completed":
				mark, tone = toneStyle(c.t.Color.Ok).Render("✓"), c.t.Color.Muted
			default:
				glyph, glyphTone := agentStatusGlyph(sub.Status, c.t)
				mark, tone = toneStyle(glyphTone).Render(glyph), glyphTone
				if sub.Status == "queued" || sub.Status == "dispatched" {
					tone = c.t.Color.Muted
				}
			}
			detail := " · " + tr(txt.ChatBitsSubagentTools, sub.ToolCount)
			goal := truncateEnd(sub.Goal, max(c.width-transcriptIndent-ansi.StringWidth(detail)-4-sub.Depth*2, 8))
			out.lines = append(out.lines, pad+strings.Repeat("  ", sub.Depth)+mark+" "+toneStyle(tone).Render(goal)+toneStyle(c.t.Color.Muted).Render(detail))
		}
	}
	return out, placed
}

// Every row of the transcript followed by the live turn, blocks separated where their groups change, each block given its identity and its place among the rows kept for the clicks it takes
func (v *transcriptView) rows(history []msg, s *tuiStore, stream string, c renderCtx, tick int) []string {
	var lines []string
	var prev *msg
	v.prompts, v.placed = v.prompts[:0], v.placed[:0]
	details := ""
	for _, section := range sectionNames {
		details += sectionMode(section, c.details.DetailsMode, c.details.Sections, c.details.CommandOverride) + " "
	}
	if details != v.details {
		v.details = details
		v.dropSections()
	}
	for i := range history {
		v.identify(&history[i])
		m := history[i]
		if !blockRenders(m, c.details) || (c.focus && m.Role != "user" && m.Role != "assistant" && m.Kind != "intro") {
			continue
		}
		block := v.block(m, c)
		if len(block.lines) == 0 {
			continue
		}
		if !c.compact && (hasLeadGap(prev, m) || (prev != nil && messageGroup(*prev) == "user" && messageGroup(m) != "user") || (prev != nil && messageGroup(m) == "user")) {
			lines = append(lines, "")
		}
		if m.Role == "user" && m.Kind == "" {
			v.prompts = append(v.prompts, promptRow{row: len(lines), text: userDisplay(m.Text)})
		}
		v.placed = append(v.placed, placedBlock{id: m.ID, row: len(lines), rows: len(block.lines), parts: block.parts, images: block.images})
		lines = append(lines, block.lines...)
		prev = &history[i]
	}
	live, segments := v.liveRows(s, stream, c, tick)
	if len(live.lines) == 0 {
		delete(v.open, liveBlock)
		return lines
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	for _, segment := range segments {
		segment.row += len(lines)
		v.placed = append(v.placed, segment)
	}
	v.placed = append(v.placed, placedBlock{id: liveBlock, row: len(lines), rows: len(live.lines), parts: live.parts})
	return append(lines, live.lines...)
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
	v.total, v.height, v.width = len(lines), height, width
	v.settle()
	top := v.top()
	out := make([]string, 0, height)
	for i := top; i < top+height && i < len(lines); i++ {
		out = append(out, lines[i])
	}
	for len(out) < height {
		out = append(out, "")
	}
	sticky := v.stickyPrompt(top)
	if v.sticky = sticky != "" && height > 2; v.sticky {
		lead := toneStyle(c.t.Color.Prompt).Render(c.ut.Glyphs.Prompt + " ")
		out[0] = lead + toneStyle(c.t.Color.Muted).Render(truncateEnd(strings.ReplaceAll(sticky, "\n", " "), max(width-6, 8)))
	}
	v.inView = v.inView[:0]
	first := top
	if v.sticky {
		first++
	}
	for _, block := range v.placed {
		for _, spot := range block.images {
			if row := block.row + spot.row; row >= first && row+spot.rows <= top+height {
				spot.row = row - top
				v.inView = append(v.inView, spot)
			}
		}
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
