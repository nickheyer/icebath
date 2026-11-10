package app

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/commands"
)

// Replaces collapsed paste references in the buffer with their real text, the text change that follows left uncollapsed
func (c *CLI) inlinePastes(buffer *ptBuffer) {
	existing := buffer.text()
	expanded, err := c.expandPasteReferences(existing)
	if err != nil {
		cliLogger.Debug("Failed to inline paste placeholders", "error", err)
		return
	}
	if expanded != existing {
		c.tui.skipPasteCollapse = true
		_ = buffer.setText(expanded)
		buffer.setCursorPosition(len([]rune(expanded)))
	}
}

// Clears an input buffer after a programmatic submit, its text kept in history
func (c *CLI) resetInputBuffer(buffer *ptBuffer) { buffer.reset(true) }

// Places text in the prompt buffer for editing
func (c *CLI) prefillInputBuffer(text string) {
	c.uiMu.Lock()
	app := c.app
	c.uiMu.Unlock()
	if app == nil {
		return
	}
	app.SetBuffer(text, len([]rune(text)))
	_ = app.Invalidate()
}

// Typed answers the slash confirm modal accepts, mapped onto the canonical choice values
var confirmAliases = map[string]string{
	"1": "once", "once": "once", "approve": "once", "yes": "once", "y": "once", "ok": "once",
	"2": "always", "always": "always", "remember": "always",
	"3": "cancel", "cancel": "cancel", "nevermind": "cancel", "no": "cancel", "n": "cancel"}

// Arguments that skip the confirmation of a destructive slash command
var destructiveSkipTokens = map[string]bool{"now": true, "--yes": true, "-y": true}

// Reads approvals.key, on unless the config turns it off
func approvalGateOn(ctx context.Context, key string) bool {
	approvals, ok := LoadCLIConfig(ctx)["approvals"].(map[string]any)
	if !ok {
		return true
	}
	value, present := approvals[key]
	return !present || pyTruthy(value)
}

// gatedConfirmPrompt is the copy of one once, always or cancel confirmation
type gatedConfirmPrompt struct {
	Title, Detail, Unchanged, AlwaysMsg, OnceVerb, PersistFailedMsg string
	Choices                                                         []slashConfirmChoice
}

// Asks the once, always or cancel question behind approvals.key, once without asking when the gate is off, empty on cancel
func gatedConfirm(c *CLI, command, key string, p gatedConfirmPrompt) string {
	if !approvalGateOn(c.ctx, key) {
		return "once"
	}
	out := console().Out
	raw, answered := c.promptTextInputModal(p.Title, p.Detail, p.Choices, 120)
	if !answered {
		fmt.Fprintln(out, c.t("cli.modal.cancelled_no_input", map[string]any{"command": command}))
		return ""
	}
	choice := c.normalizeSlashConfirmChoice(raw, p.Choices)
	switch choice {
	case "":
		fmt.Fprintln(out, c.t("cli.modal.unrecognized_choice", map[string]any{"raw": raw, "command": command}))
		return ""
	case "cancel":
		fmt.Fprintln(out, c.t("cli.modal.cancelled", map[string]any{"command": command, "unchanged": p.Unchanged}))
		return ""
	case "always":
		if SaveConfigValue(c.ctx, "approvals."+key, false) {
			fmt.Fprintln(out, p.AlwaysMsg)
			fmt.Fprintln(out, c.t("cli.modal.re_enable_hint", map[string]any{"setting": key}))
		} else if p.PersistFailedMsg != "" {
			fmt.Fprintln(out, p.PersistFailedMsg)
		} else {
			fmt.Fprintln(out, c.t("cli.modal.persist_opt_out_failed", map[string]any{"once_verb": p.OnceVerb}))
		}
	}
	return choice
}

// Platform the modal prompt fallback reads, swapped by tests
var modalGOOS = runtime.GOOS

// Prompts for a line of text, cancelled when an application runs and the caller is not on its loop
func (c *CLI) promptTextInput(promptText string) (string, bool) {
	result, ok := "", false
	ask := func() {
		line, err := console().Input(promptText)
		if err != nil {
			return
		}
		if line = strings.TrimSpace(line); line != "" {
			result, ok = line, true
		}
	}
	c.uiMu.Lock()
	app := c.app
	c.uiMu.Unlock()
	switch {
	case app != nil && !app.OnLoop():
		_ = c.invalidate()
		return "", false
	case app != nil:
		wasVisible := c.statusBarVisible
		c.statusBarVisible = false
		_ = app.Invalidate()
		app.RunInTerminal(ask)
		c.statusBarVisible = wasVisible
		_ = app.Invalidate()
	default:
		ask()
	}
	return result, ok
}

// Asks a slash command confirmation through the prompt composer, false when it timed out or could not be shown
func (c *CLI) promptTextInputModal(title, detail string, choices []slashConfirmChoice, timeout float64) (string, bool) {
	if len(choices) == 0 {
		return "", false
	}
	c.uiMu.Lock()
	app := c.app
	c.uiMu.Unlock()
	if app == nil {
		return c.promptTextInput(c.t("cli.modal.choice_prompt", nil))
	}
	onLoop := app.OnLoop()
	stdinFallback := func() (string, bool) {
		if modalGOOS == "windows" && !onLoop {
			_ = c.invalidate()
			return "", false
		}
		return c.promptTextInput(c.t("cli.modal.choice_prompt", nil))
	}
	hasLoop := app.IsRunning()
	if !onLoop && !hasLoop {
		return stdinFallback()
	}
	response := make(chan string, 1)
	setup := func() {
		c.captureModalInputSnapshot()
		c.uiMu.Lock()
		c.slashConfirmState = &slashConfirmModalState{Title: title, Detail: detail, Choices: choices, response: response}
		c.slashConfirmDeadline = monotonic() + timeout
		c.uiMu.Unlock()
		_ = c.invalidate()
	}
	teardown := func() {
		c.uiMu.Lock()
		c.slashConfirmState, c.slashConfirmDeadline = nil, 0
		c.uiMu.Unlock()
		c.restoreModalInputSnapshot()
		_ = c.invalidate()
	}
	runOnLoop := func(fn func()) bool {
		if onLoop || !hasLoop {
			fn()
			return true
		}
		ready := make(chan struct{})
		if !app.CallSoonThreadsafe(func() { defer close(ready); fn() }) {
			return false
		}
		select {
		case <-ready:
			return true
		case <-time.After(5 * time.Second):
			return false
		}
	}
	if !runOnLoop(setup) {
		return stdinFallback()
	}
	defer func() {
		c.uiMu.Lock()
		open := c.slashConfirmState != nil
		c.uiMu.Unlock()
		if open {
			runOnLoop(teardown)
		}
	}()
	deadline := func() (float64, bool) {
		c.uiMu.Lock()
		defer c.uiMu.Unlock()
		return c.slashConfirmDeadline, true
	}
	result, timedOut := pollModalQueue(c, c.ctx, response, deadline, 5.0, func() { _ = c.invalidate() }, "")
	if timedOut {
		return "", false
	}
	runOnLoop(teardown)
	return result, result != ""
}

// Answers the open slash confirmation and closes it
func (c *CLI) submitSlashConfirmResponse(value string) {
	c.uiMu.Lock()
	state := c.slashConfirmState
	if state == nil {
		c.uiMu.Unlock()
		return
	}
	offer(state.response, value)
	c.slashConfirmState, c.slashConfirmDeadline = nil, 0
	c.uiMu.Unlock()
	_ = c.invalidate()
}

// Canonical choice a typed answer names among the offered ones, empty when it names none
func (c *CLI) normalizeSlashConfirmChoice(raw string, choices []slashConfirmChoice) string {
	typed := strings.ToLower(strings.TrimSpace(raw))
	if typed == "" {
		return ""
	}
	allowed := map[string]bool{}
	for _, choice := range choices {
		allowed[choice.Value] = true
	}
	if normalized, ok := confirmAliases[typed]; ok && allowed[normalized] {
		return normalized
	}
	if allowed[typed] {
		return typed
	}
	return ""
}

// Flat palette rows, the commands behind the help listing allowed on this surface plus installed skill commands
func (c *CLI) buildCommandPaletteEntries() []commandPaletteEntry {
	var entries []commandPaletteEntry
	for _, group := range commands.CommandsByCategory(commands.Default) {
		for _, row := range group.Rows {
			if c.commandAvailable(row.Command) {
				entries = append(entries, commandPaletteEntry{row.Command, group.Category, row.Description})
			}
		}
	}
	found := sessionSkillCommands(c)
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entries = append(entries, commandPaletteEntry{name, c.t("cli.palette.category_skill", nil), cliStr(found[name]["description"], "")})
	}
	return entries
}

// Opens the command palette, never over another modal
func (c *CLI) openCommandPalette() {
	c.uiMu.Lock()
	blocked := c.commandPaletteState != nil || c.modelPickerState != nil || c.clarifyState != nil || c.approvalState != nil ||
		c.slashConfirmState != nil || c.sudoState != nil || c.secretState != nil || c.connectionState != nil
	c.uiMu.Unlock()
	if blocked {
		return
	}
	c.captureModalInputSnapshot()
	entries := c.buildCommandPaletteEntries()
	c.uiMu.Lock()
	c.commandPaletteState = &commandPaletteModalState{Entries: entries}
	c.uiMu.Unlock()
	_ = c.invalidateWithin(0)
}

// Closes the command palette and restores the draft
func (c *CLI) closeCommandPalette() {
	c.uiMu.Lock()
	c.commandPaletteState = nil
	c.uiMu.Unlock()
	c.restoreModalInputSnapshot()
	_ = c.invalidateWithin(0)
}

// Reports whether every character of needle appears in hay in order
func isSubsequence(needle, hay string) bool {
	rest := []rune(hay)
	for _, ch := range needle {
		i := 0
		for i < len(rest) && rest[i] != ch {
			i++
		}
		if i == len(rest) {
			return false
		}
		rest = rest[i+1:]
	}
	return true
}

// Palette rows matching the filter, ranked exact name, name prefix, name substring, name subsequence, then description
func (c *CLI) commandPaletteVisibleEntries() []commandPaletteEntry {
	c.uiMu.Lock()
	state := c.commandPaletteState
	c.uiMu.Unlock()
	if state == nil {
		return nil
	}
	q := strings.ToLower(strings.TrimSpace(state.Filter))
	if q == "" {
		return append([]commandPaletteEntry{}, state.Entries...)
	}
	qn := strings.TrimLeft(q, "/")
	buckets := make([][]commandPaletteEntry, 5)
	for _, row := range state.Entries {
		name := strings.TrimLeft(strings.ToLower(row.Command), "/")
		rank := -1
		switch {
		case name == qn:
			rank = 0
		case strings.HasPrefix(name, qn):
			rank = 1
		case strings.Contains(name, qn):
			rank = 2
		case isSubsequence(qn, name):
			rank = 3
		case strings.Contains(strings.ToLower(row.Description), q):
			rank = 4
		}
		if rank >= 0 {
			buckets[rank] = append(buckets[rank], row)
		}
	}
	var out []commandPaletteEntry
	for _, bucket := range buckets {
		out = append(out, bucket...)
	}
	return out
}

// Fills the selected command into the composer, never running it
func (c *CLI) handleCommandPaletteSelection() {
	c.uiMu.Lock()
	state := c.commandPaletteState
	c.uiMu.Unlock()
	if state == nil {
		return
	}
	rows := c.commandPaletteVisibleEntries()
	if state.Selected < 0 || state.Selected >= len(rows) {
		c.closeCommandPalette()
		return
	}
	cmd := rows[state.Selected].Command
	c.closeCommandPalette()
	c.uiMu.Lock()
	app := c.app
	c.uiMu.Unlock()
	if app != nil {
		app.SetBuffer(cmd+" ", len([]rune(cmd))+1)
		_ = c.invalidateWithin(0)
	}
}

// Splits the skip tokens out of a destructive slash command, returning the remaining argument text and whether one was found
func splitDestructiveSkip(cmdText string) (string, bool) {
	tokens := strings.Fields(cmdText)
	if len(tokens) == 0 {
		return "", false
	}
	if strings.HasPrefix(tokens[0], "/") {
		tokens = tokens[1:]
	}
	var kept []string
	for _, tok := range tokens {
		if !destructiveSkipTokens[strings.ToLower(tok)] {
			kept = append(kept, tok)
		}
	}
	return strings.Join(kept, " "), len(kept) != len(tokens)
}

// Confirms a destructive slash command, once, always or empty when cancelled, a skip token in the original bypassing the question
func (c *CLI) confirmDestructiveSlash(command, detail, cmdOriginal string) string {
	if cmdOriginal != "" {
		if _, skip := splitDestructiveSkip(cmdOriginal); skip {
			return "once"
		}
	}
	return gatedConfirm(c, command, "destructive_slash_confirm", gatedConfirmPrompt{
		Title:  c.t("cli.modal.destructive_title", map[string]any{"command": command}),
		Detail: detail,
		Choices: []slashConfirmChoice{
			{"once", c.t("cli.modal.approve_once", nil), c.t("cli.modal.approve_once_detail", nil)},
			{"always", c.t("cli.modal.always_approve", nil), c.t("cli.modal.always_approve_detail", nil)},
			{"cancel", c.t("cli.modal.cancel", nil), c.t("cli.modal.cancel_detail", nil)}},
		Unchanged:        c.t("cli.modal.conversation_unchanged", nil),
		AlwaysMsg:        c.t("cli.modal.destructive_always_msg", nil),
		PersistFailedMsg: c.t("cli.modal.persist_failed_proceeding", nil)})
}
