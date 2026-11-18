package app

import (
	"context"
	"fmt"
	"runtime"
	"strings"
)

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

// Prompts for a line of text on the console, false when nothing was typed
func (c *CLI) promptTextInput(promptText string) (string, bool) {
	line, err := console().Input(promptText)
	if err != nil {
		return "", false
	}
	line = strings.TrimSpace(line)
	return line, line != ""
}

// Asks a slash command confirmation through the prompt composer, false when it timed out or could not be shown
func (c *CLI) promptTextInputModal(title, detail string, choices []slashConfirmChoice, timeout float64) (string, bool) {
	if len(choices) == 0 {
		return "", false
	}
	return c.promptTextInput(c.t("cli.modal.choice_prompt", nil))
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
