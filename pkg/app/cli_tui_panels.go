package app

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Visible height of the top or bottom input separator rule
func (c *CLI) tuiInputRuleHeight(position string, width *int) int {
	if position != "top" && position != "bottom" {
		panic("Unknown input rule position: " + position)
	}
	if c.statusBarSuppressedAfterResize.Load() {
		return 0
	}
	if position == "top" || !c.useMinimalTUIChrome(width) {
		return 1
	}
	return 0
}

// Wrapped rows of each line of a text
func wrapEachLine(text string, width int) []string {
	var out []string
	for _, line := range pystr.Splitlines(text, false) {
		out = append(out, wrapPanelTextKeepWS(line, width, "")...)
	}
	return out
}

// Renders the confirmation panel of a destructive slash command
func (c *CLI) getSlashConfirmDisplayFragments() []styledFragment {
	c.uiMu.Lock()
	state := c.slashConfirmState
	c.uiMu.Unlock()
	if state == nil {
		return nil
	}
	title := state.Title
	if title == "" {
		title = c.t("cli.tui.confirm_title", nil)
	}
	footer := c.t("cli.tui.confirm_footer", nil)
	labels := make([]string, len(state.Choices))
	for i, choice := range state.Choices {
		cursor := " "
		if i == state.Selected {
			cursor = "❯"
		}
		labels[i] = fmt.Sprintf("%s [%d] %s — %s", cursor, i+1, choice.Label, choice.Desc)
	}
	preview := wrapEachLine(state.Detail, 72)
	for _, row := range wrapRows(wrapPanelTextKeepWS, labels, 72, "    ") {
		preview = append(preview, row.Text)
	}
	preview = append(preview, footer)
	boxWidth := panelBoxWidth(title, preview, 56, 86)
	inner := max(8, boxWidth-2)
	detail := wrapEachLine(state.Detail, inner)
	choices := wrapRows(wrapPanelTextKeepWS, labels, inner, "    ")
	available := max(0, termRows()-panelReservedBelow)
	maxDetail := min(8, max(1, available-6-len(choices)))
	if len(detail) > maxDetail {
		detail = append(detail[:max(1, maxDetail-1)], c.t("cli.tui.detail_truncated", nil))
	}
	p := newPanel("class:approval-border", boxWidth, "", "")
	p.row("class:approval-title", title)
	p.blank()
	for _, line := range detail {
		p.row("class:approval-desc", line)
	}
	p.blank()
	for _, row := range choices {
		style := "class:approval-choice"
		if row.Index == state.Selected {
			style = "class:approval-selected"
		}
		p.row(style, row.Text)
	}
	p.blank()
	p.row("class:approval-cmd", footer)
	return p.close()
}

// Renders the dangerous command approval panel, command and choices always kept and the description cut to fit
func (c *CLI) getApprovalDisplayFragments() []styledFragment {
	c.uiMu.Lock()
	var state *approvalModalState
	if c.approvalState != nil {
		copied := *c.approvalState
		state = &copied
	}
	c.uiMu.Unlock()
	if state == nil {
		return nil
	}
	wrap := wrapPanelTextKeepWS
	title := c.t("cli.tui.approval_title", nil)
	hasView := false
	preview := append(wrap(state.Description, 60, ""), wrap(state.Command, 60, "")...)
	for i, choice := range state.Choices {
		prefix := "  "
		if i == state.Selected {
			prefix = "❯ "
		}
		preview = append(preview, wrap(prefix+approvalChoiceLabel(c.ctx, choice), 60, "  ")...)
		hasView = hasView || choice == "view"
	}
	boxWidth := panelBoxWidth(title, preview, 46, 76)
	inner := max(8, boxWidth-2)
	cmd := wrap(state.Command, inner, "")
	if !state.ShowFull && hasView && len(cmd) > 4 {
		cmd = append(cmd[:3], wrap(c.t("cli.tui.approval_choose_full", map[string]any{"label": c.t("cli.tui.approval_view", nil)}), inner, "")...)
	}
	labels := make([]string, len(state.Choices))
	for i, choice := range state.Choices {
		cursor := " "
		if i == state.Selected {
			cursor = "❯"
		}
		labels[i] = fmt.Sprintf("%s %s. %s", cursor, numPrefix(i), approvalChoiceLabel(c.ctx, choice))
	}
	choices := wrapRows(wrap, labels, inner, "    ")
	available := max(0, termRows()-panelReservedBelow)
	compact := 5+len(cmd)+len(choices) > available
	chrome := 5
	if compact {
		chrome = 3
	}
	if maxCmd := max(1, available-chrome-len(choices)); len(cmd) > maxCmd {
		keep := 1
		if maxCmd > 1 {
			keep = max(1, maxCmd-1)
		}
		cmd = append(cmd[:keep], wrap(c.t("cli.tui.approval_command_truncated", nil), inner, "")...)
	}
	forDesc := available - (chrome + len(cmd) + len(choices))
	if !compact {
		forDesc--
	}
	forDesc = max(0, min(forDesc, 10))
	var desc []string
	if state.Description != "" {
		desc = wrap(state.Description, inner, "")
	}
	switch {
	case forDesc < 1 || len(desc) == 0:
		desc = nil
	case len(desc) > forDesc:
		desc = append(desc[:max(1, forDesc-1)], c.t("cli.tui.approval_description_truncated", nil))
	}
	p := newPanel("class:approval-border", boxWidth, "", "")
	p.row("class:approval-title", title)
	if !compact {
		p.blank()
	}
	for _, line := range cmd {
		p.row("class:approval-cmd", line)
	}
	if !compact {
		p.blank()
	}
	for _, row := range choices {
		style := "class:approval-choice"
		if row.Index == state.Selected {
			style = "class:approval-selected"
		}
		p.row(style, row.Text)
	}
	if len(desc) > 0 {
		if !compact {
			p.blank()
		}
		for _, line := range desc {
			p.row("class:approval-desc", line)
		}
	}
	return p.close()
}

// The normal prompt and the suffix special states render after their icon, for the active skin and profile
func (c *CLI) getTUIPromptSymbols() (string, string) {
	symbol := config.GetActivePromptSymbol(c.ctx, "❯ ")
	if symbol == "" {
		symbol = "❯ "
	}
	symbol = strings.TrimRight(symbol, " \t\n\r\v\f") + " "
	if profile := profiles.GetActiveProfileName(c.ctx); profile != "default" && profile != "custom" {
		symbol = profile + " " + symbol
	}
	stripped := strings.TrimRight(symbol, " \t\n\r\v\f")
	if stripped == "" {
		return "❯ ", "❯ "
	}
	parts := strings.Fields(stripped)
	candidate := ""
	if len(parts) > 0 {
		candidate = parts[len(parts)-1]
	}
	if strings.ContainsAny(candidate, "❯>$#›»→") {
		return symbol, strings.TrimRight(candidate, " \t\n\r\v\f") + " "
	}
	return symbol, symbol
}

// One character audio level bar from the recorder's current level
func (c *CLI) audioLevelBar() string {
	rec := c.voiceRecorderHandle()
	if rec == nil {
		return ""
	}
	return string([]rune(" ▁▂▃▄▅▆▇")[min(rec.CurrentRMS(), 8000)*7/8000])
}

// Prompt fragments for the current interactive state
func (c *CLI) getTUIPromptFragments() []styledFragment {
	symbol, suffix := c.getTUIPromptSymbols()
	width := c.getTUITerminalWidth()
	compact := c.useMinimalTUIChrome(&width)
	state := func(style, icon, extra string) []styledFragment {
		if compact {
			text := icon
			if extra != "" {
				text = strings.TrimRight(text+" "+strings.TrimSpace(extra), " \t\n\r\v\f")
			}
			return []styledFragment{{style, text + " "}}
		}
		if extra != "" {
			return []styledFragment{{style, icon + " " + extra + " " + suffix}}
		}
		return []styledFragment{{style, icon + " " + suffix}}
	}
	c.voiceLock.Lock()
	recording, processing, voiceMode := c.voiceRecording, c.voiceProcessing, c.voiceMode
	c.voiceLock.Unlock()
	c.uiMu.Lock()
	sudo, secret, connection := c.sudoState != nil, c.secretState != nil, c.connectionState != nil
	approval, slash := c.approvalState != nil, c.slashConfirmState != nil
	freetext, clarify := c.clarifyFreetext, c.clarifyState != nil
	command, agent := c.commandRunning, c.agentRunning
	c.uiMu.Unlock()
	switch {
	case recording:
		return state("class:voice-recording", "●", c.audioLevelBar())
	case processing:
		return state("class:voice-processing", "◉", "")
	case sudo:
		return state("class:sudo-prompt", "🔐", "")
	case secret:
		return state("class:sudo-prompt", "🔑", "")
	case connection:
		return state("class:prompt-working", "⚙", "")
	case approval || slash:
		return state("class:prompt-working", "⚠", "")
	case freetext:
		return state("class:clarify-selected", "✎", "")
	case clarify:
		return state("class:prompt-working", "?", "")
	case command:
		return state("class:prompt-working", c.commandSpinnerFrame(), "")
	case agent:
		return state("class:prompt-working", "☤", "")
	case voiceMode:
		return state("class:voice-prompt", "🎤", "")
	}
	return []styledFragment{{"class:prompt", symbol}}
}

// Visible prompt text for width calculations
func (c *CLI) getTUIPromptText() string {
	var b strings.Builder
	for _, f := range c.getTUIPromptFragments() {
		b.WriteString(f.Text)
	}
	return b.String()
}

// Layers skin colors over the base style, hex tokens remapped on a light terminal unless the style paints a background
func (c *CLI) buildTUIStyleDict() *styleDict {
	d := c.tui.styleBase.clone()
	d.update(config.GetPromptToolkitStyleOverrides(c.ctx))
	if !detectLightMode() {
		return d
	}
	for _, key := range d.keys {
		tokens := strings.Fields(d.values[key])
		paintsBackground := false
		for _, tok := range tokens {
			paintsBackground = paintsBackground || strings.HasPrefix(tok, "bg:")
		}
		if paintsBackground || d.values[key] == "" {
			continue
		}
		for i, tok := range tokens {
			if strings.HasPrefix(tok, "#") {
				tokens[i] = maybeRemapForLightMode(tok)
			}
		}
		d.values[key] = strings.Join(tokens, " ")
	}
	return d
}

// Refreshes the style of a running prompt after a skin change
func (c *CLI) applyTUISkinStyle() bool {
	if c.tui.app == nil || c.tui.styleBase == nil {
		return false
	}
	if err := c.tui.app.setStyle(c.buildTUIStyleDict()); err != nil {
		panic(err)
	}
	_ = c.invalidateWithin(0)
	return true
}

// Widgets a wrapping CLI inserts between the spacer and the status bar
func (c *CLI) getExtraTUIWidgets() []*ptWindow {
	if c.tui.extraWidgets != nil {
		return c.tui.extraWidgets()
	}
	return nil
}

// Lets a wrapping CLI add key bindings next to the main input
func (c *CLI) registerExtraTUIKeybindings(kb *ptKeyBindings, app *ptApplication) {
	if c.tui.extraKeybindings != nil {
		c.tui.extraKeybindings(kb, app)
	}
}

// tuiLayoutChildren names every widget the root column orders
type tuiLayoutChildren struct {
	sudo, secret, connection, approval, slashConfirm, clarify, modelPicker, commandPalette *ptWindow
	spinner, spacer, statusBar, inputRuleTop, imageBar, inputArea, inputRuleBot            *ptWindow
	voiceStatusBar, completionsMenu                                                        *ptWindow
}

// Ordered children of the root column, the status bar last under the modern layout
func (c *CLI) buildTUILayoutChildren(w tuiLayoutChildren) []*ptWindow {
	ordered := []*ptWindow{{name: "top", height: func() int { return 0 }}, w.sudo, w.secret, w.connection, w.approval, w.slashConfirm, w.clarify,
		w.modelPicker, w.commandPalette, w.spinner, w.spacer}
	ordered = append(ordered, c.getExtraTUIWidgets()...)
	ordered = append(ordered, c.tui.petWidget, c.tui.stashPanelWidget, c.tui.subagentDockWidget, w.statusBar, w.inputRuleTop, w.imageBar,
		w.inputArea, w.inputRuleBot, w.voiceStatusBar, w.completionsMenu)
	if c.modernLayout && w.statusBar != nil {
		for i, item := range ordered {
			if item == w.statusBar {
				ordered = append(append(ordered[:i:i], ordered[i+1:]...), w.statusBar)
				break
			}
		}
	}
	out := ordered[:0:0]
	for _, item := range ordered {
		if item != nil {
			out = append(out, item)
		}
	}
	return out
}

// Repaints the command spinner on a timer and ticks the subagent monitor, the idle prompt never repainted
func (c *CLI) tuiSpinnerLoop() {
	for !c.exitRequested() {
		c.uiMu.Lock()
		app, monitor, running := c.app, c.subagentMonitor, c.commandRunning
		c.uiMu.Unlock()
		if app == nil {
			sleepSeconds(0.1)
			continue
		}
		if monitor != nil {
			monitor.tick()
		}
		if running {
			_ = c.invalidateWithin(0.1)
			sleepSeconds(0.1)
		} else {
			sleepSeconds(0.2)
		}
	}
}

// Renders the batch clarify panel, one status line per question and the active question's choices under it
func (c *CLI) getClarifyDisplayFragments() []styledFragment {
	c.uiMu.Lock()
	state, freetext := c.clarifyState, c.clarifyFreetext
	c.uiMu.Unlock()
	if state == nil {
		return nil
	}
	title := c.t("cli.tui.clarify_title", map[string]any{"agent_name": agentName(c.ctx)})
	header := tn(c.ctx, "cli.tui.clarify_question_count", len(state.Questions), nil)
	selectedIndices := map[int]bool{}
	if state.MultiSelect {
		selectedIndices = state.SelectedIndices
	}
	type span struct {
		hasSelected, hasEnd bool
		selStart, end       int
	}
	statusRows := func(width int) ([]styledFragment, span) {
		var rows []styledFragment
		var focus span
		add := func(style, label, indent string) {
			for _, wrapped := range wrapPanelTextTrim(label, width, indent) {
				rows = append(rows, styledFragment{style, wrapped})
			}
		}
		for idx, entry := range state.Questions {
			locked, answered := state.Answers[entry.QID]
			marker := "·"
			if answered {
				marker = "✓"
			} else if idx == state.Active {
				marker = "▸"
			}
			rowStyle := "class:clarify-choice"
			if idx == state.Active {
				rowStyle = "class:clarify-selected"
			}
			add(rowStyle, marker+" "+entry.Question, "  ")
			if answered {
				text := "—"
				if locked != nil {
					text = *locked
				}
				add("class:clarify-answer", "    "+text, "    ")
			}
			if idx != state.Active {
				continue
			}
			for i, choice := range state.Choices {
				cursor, style := " ", "class:clarify-choice"
				if i == state.Selected && !freetext {
					cursor, style = "❯", "class:clarify-selected"
				}
				box := ""
				if state.MultiSelect {
					box = "[ ] "
					if selectedIndices[i] {
						box = "[x] "
					}
				}
				first := len(rows)
				add(style, fmt.Sprintf("  %s %s%s. %s", cursor, box, numPrefix(i), choice), "      ")
				if i == state.Selected && !freetext {
					focus.hasSelected, focus.selStart = true, first
				}
			}
			if len(state.Choices) > 0 {
				other := len(state.Choices)
				mid := numPrefix(other)
				if state.MultiSelect {
					box := "[ ]"
					if selectedIndices[other] {
						box = "[x]"
					}
					mid = box + " " + mid
				}
				suffix := ""
				if text := state.AnswerMeta[entry.QID].OtherText; text != "" {
					suffix = c.t("cli.tui.clarify_other_answer", map[string]any{"text": text})
				}
				orKey := func(key string) string {
					if suffix != "" {
						return suffix
					}
					return c.t(key, nil)
				}
				var label, style string
				switch {
				case freetext:
					label, style = "  ❯ "+mid+". "+orKey("cli.tui.clarify_other_type_below"), "class:clarify-active-other"
				case state.Selected == other:
					label, style = "  ❯ "+mid+". "+orKey("cli.tui.clarify_other_type_answer"), "class:clarify-selected"
				default:
					label, style = "    "+mid+". "+orKey("cli.tui.clarify_other_type_answer"), "class:clarify-choice"
				}
				first := len(rows)
				add(style, label, "      ")
				if freetext || state.Selected == other {
					focus.hasSelected, focus.selStart = true, first
				}
			} else if freetext {
				add("class:clarify-active-other", "  "+c.t("cli.tui.clarify_guidance", nil), "")
			}
			focus.hasEnd, focus.end = true, len(rows)
		}
		return rows, focus
	}
	previewRows, _ := statusRows(60)
	preview := []string{header}
	for _, row := range previewRows {
		preview = append(preview, row.Text)
	}
	boxWidth := panelBoxWidth(title, preview, 46, 76)
	rows, focus := statusRows(max(8, boxWidth-2))
	if budget := max(1, termRows()-panelReservedBelow-3); len(rows) > budget && focus.hasEnd {
		selStart := focus.end - 1
		if focus.hasSelected {
			selStart = focus.selStart
		}
		first := max(0, min(min(selStart, focus.end-budget), len(rows)-budget))
		rows = rows[first : first+budget]
	}
	p := newPanel("class:clarify-border", boxWidth, title, "class:clarify-title")
	p.row("class:clarify-question", header)
	for _, row := range rows {
		p.row(row.Style, row.Text)
	}
	return p.close()
}

// Renders a titled panel with hint rows and a scrolling selectable list, the scroll offset updated in place
func (c *CLI) renderScrollListPanel(selected int, scrollOffset *int, title string, hints, labels []string, minWidth, maxWidth int, indent string) []styledFragment {
	boxWidth := panelBoxWidth(title, append(append([]string{}, hints...), labels...), minWidth, maxWidth)
	inner := max(8, boxWidth-2)
	labelWidth := max(8, inner-max(2, utf8.RuneCountInString(indent)))
	rows := termRows()
	if c.app != nil {
		if _, r, ok := c.app.Size(); ok {
			rows = r
		}
	}
	offset, visible := computeModelPickerViewport(selected, *scrollOffset, len(labels), rows, 5+len(hints))
	*scrollOffset = offset
	p := newPanel("class:clarify-border", boxWidth, title, "class:clarify-title")
	p.blank()
	for _, line := range hints {
		p.row("class:clarify-hint", line)
	}
	p.blank()
	for idx := offset; idx < min(offset+visible, len(labels)); idx++ {
		style, lead := "class:clarify-choice", "  "
		if idx == selected {
			style, lead = "class:clarify-selected", "❯ "
		}
		for _, wrapped := range prefixWrappedRows(wrapPanelTextTrim, labels[idx], labelWidth, lead, indent) {
			p.row(style, wrapped)
		}
	}
	p.blank()
	return p.close()
}

// Integer stored under a key of a picker state, the fallback when absent
func stateInt(state map[string]any, key string, def int) int {
	if n, ok := pyIntOf(state[key]); ok {
		return n
	}
	return def
}

// Renders the model picker panel for its provider, effort or model stage
func (c *CLI) getModelPickerDisplayFragments() []styledFragment {
	state, _ := c.modelPickerState.(map[string]any)
	if state == nil {
		return nil
	}
	var title string
	var hints, choices []string
	stage := cliStr(state["stage"], "provider")
	switch stage {
	case "provider":
		title = c.t("cli.tui.picker_title_provider", nil)
		providers, _ := state["providers"].([]map[string]any)
		for _, p := range providers {
			models, _ := p["models"].([]string)
			count := stateInt(p, "total_models", len(models))
			label := tn(c.ctx, "cli.tui.picker_provider_row", count, map[string]any{"name": p["name"]})
			if p["is_current"] == true {
				label += c.t("cli.tui.picker_current_marker", nil)
			}
			choices = append(choices, label)
		}
		choices = append(choices, c.t("cli.shared.cancel", nil))
		unknown := c.t("cli.shared.unknown", nil)
		hints = []string{c.t("cli.tui.picker_current_hint", map[string]any{"model": cliStr(state["current_model"], unknown), "provider": cliStr(state["current_provider"], unknown)})}
	case "reasoning":
		picked := c.t("cli.shared.noun_model", nil)
		if result, ok := state["switch_result"].(*provider.ModelSwitchResult); ok && result != nil && result.NewModel != "" {
			picked = result.NewModel
		}
		title = c.t("cli.tui.picker_title_effort", map[string]any{"model": picked})
		current := "medium"
		if c.reasoningConfig != nil {
			if c.reasoningConfig["enabled"] == false {
				current = "none"
			} else {
				current = cliStr(c.reasoningConfig["effort"], "medium")
			}
		}
		for _, row := range pickerReasoningRows() {
			label := row[1]
			if row[0] == current {
				label = c.t("cli.tui.picker_current_labeled", map[string]any{"label": label})
			}
			choices = append(choices, label)
		}
		choices = append(choices, c.t("cli.shared.back", nil), c.t("cli.shared.cancel", nil))
		hints = []string{c.t("cli.tui.picker_effort_hint", nil)}
	default:
		providerData, _ := state["provider_data"].(map[string]any)
		modelList, _ := state["model_list"].([]string)
		name := cliStr(providerData["name"], cliStr(providerData["slug"], c.t("cli.tui.picker_provider_fallback", nil)))
		title = c.t("cli.tui.picker_title_models", map[string]any{"provider": name})
		query := cliStr(state["filter"], "")
		pairs := c.filterModelPickerEntries(modelList, query)
		state["_filtered_pairs"] = pairs
		labels := make([]string, len(pairs))
		for i, pair := range pairs {
			labels[i] = pair.Model
		}
		rowLabels, limitLine := provider.AnnotateWindowPlanRows(c.ctx, providerData, labels)
		choices = append(rowLabels, c.t("cli.shared.back", nil), c.t("cli.shared.cancel", nil))
		var hint string
		switch {
		case query != "":
			hint = c.t("cli.tui.picker_filter_hint", map[string]any{"query": query, "shown": len(labels), "total": len(modelList)})
		case len(modelList) > 0:
			hint = c.t("cli.tui.picker_select_hint", map[string]any{"count": len(modelList)})
		default:
			hint = c.t("cli.tui.picker_empty", nil)
		}
		hints = []string{hint}
		if limitLine != "" {
			hints = []string{limitLine, hint}
		}
	}
	offset := stateInt(state, "_scroll_offset", 0)
	fragments := c.renderScrollListPanel(stateInt(state, "selected", 0), &offset, title, hints, choices, 46, 84, "  ")
	state["_scroll_offset"] = offset
	return fragments
}

// Renders the command palette panel
func (c *CLI) getCommandPaletteDisplayFragments() []styledFragment {
	state := c.commandPaletteState
	if state == nil {
		return nil
	}
	rows := c.commandPaletteVisibleEntries()
	count := len(rows)
	state.VisibleCount = &count
	total := len(state.Entries)
	hint := c.t("cli.tui.palette_hint", map[string]any{"total": total})
	if state.Filter != "" {
		hint = c.t("cli.tui.palette_filter_hint", map[string]any{"query": state.Filter, "shown": len(rows), "total": total})
	}
	labels := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Description != "" {
			labels = append(labels, row.Command+"  —  "+row.Description)
		} else {
			labels = append(labels, row.Command)
		}
	}
	if len(labels) == 0 {
		labels = []string{c.t("cli.tui.palette_empty", nil)}
	}
	return c.renderScrollListPanel(state.Selected, &state.ScrollOffset, c.t("cli.tui.palette_title", nil), []string{hint}, labels, 50, 90, "    ")
}

// Renders a bordered sudo style panel, the last body line set apart by a blank row
func (c *CLI) renderSudoStylePanel(title string, bodyLines, rowStyles []string) []styledFragment {
	p := newPanel("class:sudo-border", panelBoxWidth(title, bodyLines, 46, 76), title, "class:sudo-title")
	p.blank()
	for i, text := range bodyLines {
		if i == len(bodyLines)-1 && i > 0 {
			p.blank()
		}
		style := "class:sudo-text"
		if i < len(rowStyles) {
			style = rowStyles[i]
		}
		p.row(style, text)
	}
	p.blank()
	return p.close()
}

// Renders the sudo panel, or the vault code, save login or unlock panel sharing it
func (c *CLI) getSudoDisplayFragments() []styledFragment {
	state := c.sudoModal()
	if state == nil {
		return nil
	}
	lines := func(key string, kwargs map[string]any) []string { return strings.Split(c.t(key, kwargs), "\n") }
	switch {
	case state.VaultCode != nil:
		return c.renderSudoStylePanel(c.t("cli.tui.vault_code_title", map[string]any{"site": state.VaultCode.Site}),
			lines("cli.tui.vault_code_body", map[string]any{"site": state.VaultCode.Site, "agent_name": agentName(c.ctx)}), nil)
	case state.VaultSave != nil && state.VaultSave.Step == "identifier":
		return c.renderSudoStylePanel(c.t("cli.tui.vault_save_title", map[string]any{"site": state.VaultSave.Site}), lines("cli.tui.vault_save_body", nil), nil)
	case state.VaultSave != nil:
		return c.renderSudoStylePanel(c.t("cli.tui.vault_save_title", map[string]any{"site": state.VaultSave.Site}),
			lines("cli.tui.vault_save_pw_body", map[string]any{"origin": state.VaultSave.Origin}), nil)
	case state.VaultBackend != "":
		return c.renderSudoStylePanel(c.t("cli.tui.vault_unlock_title", map[string]any{"backend": state.VaultBackend}),
			lines("cli.tui.vault_unlock_body", map[string]any{"backend": state.VaultBackend}), nil)
	}
	return c.renderSudoStylePanel(c.t("cli.tui.sudo_title", nil), []string{c.t("cli.tui.sudo_body", nil)}, nil)
}

// Renders the secret capture panel
func (c *CLI) getSecretDisplayFragments() []styledFragment {
	state := c.secretModal()
	if state == nil {
		return nil
	}
	prompt := state.Prompt
	if prompt == "" {
		name := state.VarName
		if name == "" {
			name = c.t("cli.tui.secret_fallback_name", nil)
		}
		prompt = c.t("cli.tui.secret_prompt", map[string]any{"var_name": name})
	}
	content := []string{prompt}
	if help := state.Metadata["help"]; help != nil && help != "" && help != false {
		content = append(content, cliStr(help, ""))
	}
	content = append(content, c.t("cli.tui.secret_body", nil))
	return c.renderSudoStylePanel(c.t("cli.tui.secret_title", nil), content, nil)
}

// Renders the connection setup card, the active field or action highlighted
func (c *CLI) getConnectionDisplayFragments() []styledFragment {
	state := c.connectionModal()
	lines := c.connectionRenderLines()
	if state == nil || len(lines) == 0 {
		return nil
	}
	body := append([]string{}, lines[1:]...)
	styles := make([]string, len(body))
	for i := range styles {
		styles[i] = "class:sudo-text"
	}
	instructionsOffset := 0
	if cliStr(state.Target["instructions"], "") != "" {
		instructionsOffset = 1
	}
	switch {
	case state.Phase == "form" || state.Phase == "failed":
		fieldOffset := instructionsOffset
		if state.Phase == "failed" {
			fieldOffset++
		}
		if state.FieldIndex >= 0 && state.FieldIndex < len(state.Fields) {
			if i := fieldOffset + state.FieldIndex; i < len(styles) {
				styles[i] = "class:clarify-selected"
			}
		} else if len(body) > 0 {
			action := len(body) - 1
			styles[action] = "class:clarify-selected"
			selected := min(1, max(0, state.Selected))
			labels := []string{c.t("cli.tui.connection_connect", nil), c.t("cli.shared.cancel", nil)}
			for i := range labels {
				if i == selected {
					labels[i] = "▸ " + labels[i]
				}
			}
			body[action] = strings.Join(labels, "    ")
		}
	case state.Phase == "authorized" && len(body) > 0:
		styles[len(styles)-1] = "class:clarify-selected"
		body[len(body)-1] = "▸ " + c.t("cli.tui.connection_continue", nil)
	}
	return c.renderSudoStylePanel(lines[0], body, styles)
}

// Connection phases to the catalog keys of their hints
var tuiConnectionHintKeys = map[string]string{"form": "cli.tui.hint_connection_form", "failed": "cli.tui.hint_connection_form",
	"url": "cli.tui.hint_connection_url", "authorized": "cli.tui.hint_connection_authorized", "waiting": "cli.tui.hint_connection_waiting"}

// Hint row above the input for the open modal prompt or the running command, with its countdown
func (c *CLI) tuiHintText() []styledFragment {
	countdown := func(deadline float64) string { return fmt.Sprintf("  (%ds)", max(0, int(deadline-monotonic()))) }
	hint := func(key string, count string) []styledFragment {
		return []styledFragment{{"class:hint", "  " + c.t(key, nil)}, {"class:clarify-countdown", count}}
	}
	if conn := c.connectionModal(); conn != nil {
		key, ok := tuiConnectionHintKeys[conn.Phase]
		if !ok {
			key = "cli.tui.hint_connection_default"
		}
		count := ""
		if deadline, _ := conn.Payload["deadline_at"]; pyFloat(deadline) != 0 {
			count = fmt.Sprintf("  (%ds)", max(0, int(pyFloat(deadline)-wallClock())))
		}
		return hint(key, count)
	}
	c.uiMu.Lock()
	sudo, secret, approval, slash := c.sudoState, c.secretState != nil, c.approvalState != nil, c.slashConfirmState != nil
	sudoDeadline, secretDeadline, approvalDeadline, slashDeadline := c.sudoDeadline, c.secretDeadline, c.approvalDeadline, c.slashConfirmDeadline
	clarify, freetext, clarifyDeadline := c.clarifyState != nil, c.clarifyFreetext, c.clarifyDeadline
	running, blocks := c.commandRunning, c.commandBlocksInput
	c.uiMu.Unlock()
	switch {
	case sudo != nil:
		key := "cli.tui.hint_sudo"
		if (sudo.VaultSave != nil && sudo.VaultSave.Step == "identifier") || sudo.VaultCode != nil {
			key = "cli.tui.hint_vault_username"
		}
		return hint(key, countdown(sudoDeadline))
	case secret:
		return hint("cli.tui.hint_secret", countdown(secretDeadline))
	case approval:
		return hint("cli.tui.hint_approval", countdown(approvalDeadline))
	case slash:
		return hint("cli.tui.hint_slash_confirm", countdown(slashDeadline))
	case clarify:
		count := ""
		if clarifyDeadline != nil {
			count = countdown(*clarifyDeadline)
		}
		if freetext {
			return hint("cli.tui.hint_clarify_freetext", count)
		}
		return hint("cli.tui.hint_clarify_batch", count)
	case running:
		detail := c.t("cli.tui.hint_input_queues", nil)
		if blocks {
			detail = c.t("cli.tui.hint_input_disabled", nil)
		}
		return []styledFragment{{"class:hint", "  " + c.t("cli.tui.hint_command_running", map[string]any{"frame": c.commandSpinnerFrame(), "detail": detail})}}
	}
	return nil
}

// Gray text shown in the empty input for the current state
func (c *CLI) tuiPlaceholderText() string {
	c.voiceLock.Lock()
	recording, processing, voiceMode, live := c.voiceRecording, c.voiceProcessing, c.voiceMode, c.voiceLiveText
	c.voiceLock.Unlock()
	c.uiMu.Lock()
	sudo, secret, approval, slash := c.sudoState, c.secretState != nil, c.approvalState != nil, c.slashConfirmState != nil
	freetext, clarify := c.clarifyFreetext, c.clarifyState != nil
	running, status, agent := c.commandRunning, c.commandStatus, c.agentRunning
	c.uiMu.Unlock()
	switch {
	case recording:
		if live != "" {
			return live
		}
		return c.t("cli.tui.placeholder_recording", map[string]any{"shortcut": c.voiceRecordKeyLabel()})
	case processing:
		if live != "" {
			return live
		}
		return c.t("cli.tui.placeholder_transcribing", nil)
	case sudo != nil:
		if sudo.VaultSave != nil && sudo.VaultSave.Step == "identifier" {
			return c.t("cli.tui.placeholder_vault_username", nil)
		}
		if sudo.VaultCode != nil {
			return c.t("cli.tui.placeholder_vault_code", nil)
		}
		return c.t("cli.tui.placeholder_password", nil)
	case secret:
		return c.t("cli.tui.placeholder_secret", nil)
	case approval:
		return ""
	case slash:
		return c.t("cli.tui.placeholder_choice", nil)
	case freetext:
		return c.t("cli.tui.placeholder_answer", nil)
	case clarify:
		return ""
	case running:
		if status == "" {
			status = c.t("cli.tui.placeholder_processing_command", nil)
		}
		return c.commandSpinnerFrame() + " " + status
	case agent:
		if c.modernLayout {
			return c.t("cli.modern.placeholder_busy_"+c.busyInputMode, nil)
		}
		return c.t("cli.tui.placeholder_busy", nil)
	case voiceMode:
		return c.t("cli.tui.placeholder_voice_idle", map[string]any{"shortcut": c.voiceRecordKeyLabel()})
	}
	if c.promptStash != nil {
		if hint := c.promptStash.ComposerHint(); hint != "" {
			return hint
		}
	}
	return c.composerPlaceholder
}

// Renders the stash browse panel
func (c *CLI) getStashPanelDisplayFragments() []styledFragment {
	if c.promptStash == nil {
		return nil
	}
	return c.renderStashPanel(c.promptStash.PanelRows(), c.promptStash.PanelCursor, c.getTUITerminalWidth())
}
