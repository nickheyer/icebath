package gateway

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tools"
)

// English source strings of the approval prompt behind the gateway.exec_approval catalog keys
const (
	EAHeaderText           = "Icebath wants to run a command that needs your OK"
	EAReasonLabelText      = "Why it was flagged"
	ApprovalTimedOutNotice = "⌛ Approval timed out after {window} — the command was NOT run. " +
		"Ask me to try again if you still want it, or raise approvals.timeout in config.yaml."
)

// Localized approval header in the active display language
func EAHeaderLocalized(ctx context.Context) string {
	return i18n.T(ctx, "gateway.exec_approval.header", "", nil)
}

// Localized label in front of the reason the command was flagged
func EAReasonLabelLocalized(ctx context.Context) string {
	return i18n.T(ctx, "gateway.exec_approval.reason_label", "", nil)
}

// Localized sentence saying a smart deny override covers one operation only
func EASmartDenyLineLocalized(ctx context.Context) string {
	return i18n.T(ctx, "gateway.exec_approval.smart_deny_line", "", nil)
}

// Localized reason shown when the approval carries none
func EADefaultReasonLocalized(ctx context.Context) string {
	return i18n.T(ctx, "gateway.exec_approval.default_reason", "", nil)
}

// Localized button label for each of the four approval tiers
func EAActionLabels(ctx context.Context) map[string]string {
	return map[string]string{
		"once":    i18n.T(ctx, "gateway.exec_approval.action_once", "", nil),
		"session": i18n.T(ctx, "gateway.exec_approval.action_session", "", nil),
		"always":  i18n.T(ctx, "gateway.exec_approval.action_always", "", nil),
		"deny":    i18n.T(ctx, "gateway.exec_approval.action_deny", "", nil),
	}
}

// Localized timeout notice for an already formatted window
func ApprovalTimedOutNoticeLocalized(ctx context.Context, window string) string {
	return i18n.T(ctx, "gateway.exec_approval.timed_out_notice", "", map[string]any{"window": window})
}

// Configured approvals timeout in seconds, 300 by default, a variable so tests can pin it
var ApprovalTimeoutSeconds = func(ctx context.Context) int {
	return tools.GetApprovalTimeout(urlSafetyEnv(ctx))
}

// Human wording for a timeout such as 5 minutes, shared with the CLI notice
func FormatApprovalWindow(seconds int) string {
	return tools.FormatApprovalWindow(seconds)
}

// Last line of every approval prompt saying that doing nothing is a safe no
func FormatApprovalDeadlineLine(ctx context.Context, timeoutS int) string {
	return i18n.T(ctx, "gateway.exec_approval.deadline", "", map[string]any{"window": FormatApprovalWindow(timeoutS)})
}

// Localized timed out notice for a timeout in seconds
func FormatApprovalTimedOutNotice(ctx context.Context, timeoutS int) string {
	return ApprovalTimedOutNoticeLocalized(ctx, FormatApprovalWindow(timeoutS))
}

// Adapter that renders native approval buttons by overriding the prompt hook
type ExecApprovalPromptSender interface {
	SendExecApprovalPrompt(ctx context.Context, prompt *ExecApprovalPrompt) (SendResult, error)
}

// Warning sign, the localized header and a blank line
func (b *BasePlatformAdapter) EAHeader(ctx context.Context) string {
	return "⚠️ " + EAHeaderLocalized(ctx) + "\n\n"
}

// Localized reason label followed by a colon and a space
func (b *BasePlatformAdapter) EAReasonLabel(ctx context.Context) string {
	return EAReasonLabelLocalized(ctx) + ": "
}

// Blank line then the localized smart deny sentence
func (b *BasePlatformAdapter) EASmartDenyLine(ctx context.Context) string {
	return "\n\n" + EASmartDenyLineLocalized(ctx)
}

// Button labels of the approval tiers, adapters with their own wording override it
func (b *BasePlatformAdapter) EAActionLabelsFor(ctx context.Context) map[string]string {
	return EAActionLabels(ctx)
}

// Text cut to budget code points with the suffix appended when it was cut
func TruncatePreview(text string, budget int, suffix string) string {
	r := []rune(text)
	if len(r) > budget {
		return string(r[:max(budget, 0)]) + suffix
	}
	return text
}

// Escape hook for the command preview and reason, unchanged by default
func (b *BasePlatformAdapter) EAEscape(text string) string { return text }

// Raw prefix that fits budget once escaped and measured in platform length units
func (b *BasePlatformAdapter) EAFit(text string, budget int, suffix string, escape func(string) string) string {
	self := b.Self()
	if escape == nil {
		escape = self.EAEscape
	}
	lenFn := self.MessageLenFn()
	return FitPrefix(text, budget, func(s string) int { return lenFn(escape(s)) }, suffix)
}

// Characters of command preview that fit, platforms with a hard message cap compute it
func (b *BasePlatformAdapter) ExecApprovalCmdBudget(description string, smartDenied bool) int {
	return b.Traits.EACmdBudget
}

// Deadline line with the configured approvals timeout after the deadline prefix
func (b *BasePlatformAdapter) EADeadlineLine(ctx context.Context) string {
	return b.Traits.EADeadlinePrefix + b.Self().EAEscape(FormatApprovalDeadlineLine(ctx, ApprovalTimeoutSeconds(ctx)))
}

// Header, fenced truncated command, flagged reason and deadline line, plus the smart deny line
func (b *BasePlatformAdapter) FormatExecApproval(ctx context.Context, command string, description *string, smartDenied bool) string {
	self := b.Self()
	desc := EADefaultReasonLocalized(ctx)
	if description != nil {
		desc = *description
	}
	if b.Traits.EAReasonBudget != 0 {
		desc = b.EAFit(desc, b.Traits.EAReasonBudget, "...", nil)
	}
	cmdPreview := b.EAFit(command, self.ExecApprovalCmdBudget(desc, smartDenied), "...", nil)
	text := self.EAHeader(ctx) + b.Traits.EACodeOpen + self.EAEscape(cmdPreview) + b.Traits.EACodeClose +
		self.EAReasonLabel(ctx) + self.EAEscape(desc) + b.EADeadlineLine(ctx)
	if smartDenied {
		return text + self.EASmartDenyLine(ctx)
	}
	return text
}

// Approval button rows, a smart deny offering neither the session nor the permanent tier
func (b *BasePlatformAdapter) ExecApprovalActions(ctx context.Context, allowPermanent, allowSession, smartDenied bool) []ExecApprovalAction {
	choices := []string{"once"}
	if !smartDenied && allowSession {
		choices = append(choices, "session")
		if allowPermanent {
			choices = append(choices, "always")
		}
	}
	choices = append(choices, "deny")
	labels := b.Self().EAActionLabelsFor(ctx)
	out := make([]ExecApprovalAction, 0, len(choices))
	for _, c := range choices {
		out = append(out, ExecApprovalAction{Label: labels[c], Choice: c, Style: b.Traits.EAActionStyles[c]})
	}
	return out
}

// Whether the adapter overrides the prompt hook with native approval buttons
func (b *BasePlatformAdapter) SupportsExecApprovalButtons() bool {
	return AdapterOverridesBaseMethod(b.Self(), "SendExecApprovalPrompt")
}

// Native button rendering of an approval prompt, not supported on the base without buttons
func (b *BasePlatformAdapter) SendExecApprovalPrompt(ctx context.Context, prompt *ExecApprovalPrompt) (SendResult, error) {
	return SendResult{Success: false, Error: "Not supported"}, nil
}

// Interactive approval prompt rendered natively by the adapter, not supported without buttons
func (b *BasePlatformAdapter) SendExecApproval(ctx context.Context, chatID, command, sessionKey string, description *string, metadata map[string]any, allowPermanent, allowSession, smartDenied bool, requestID *string) (SendResult, error) {
	desc := EADefaultReasonLocalized(ctx)
	if description != nil {
		desc = *description
	}
	prompt := &ExecApprovalPrompt{ChatID: chatID, SessionKey: sessionKey, Metadata: metadata, Command: command,
		Description: desc, SmartDenied: smartDenied, RequestID: requestID,
		Text:    b.FormatExecApproval(ctx, command, &desc, smartDenied),
		Actions: b.ExecApprovalActions(ctx, allowPermanent, allowSession, smartDenied)}
	return b.Self().SendExecApprovalPrompt(ctx, prompt)
}

// Clamped picker page of options with page, total_pages, start, end, total and page_info
func FormatChoicePage[T any](ctx context.Context, options []T, page, perPage int) ([]T, map[string]any) {
	total := len(options)
	totalPages := max(1, floorDiv(total+perPage-1, perPage))
	page = max(0, min(page, totalPages-1))
	start, end := page*perPage, min(page*perPage+perPage, total)
	pageInfo := ""
	if totalPages > 1 {
		pageInfo = i18n.T(ctx, "gateway.picker.page_info", "", map[string]any{"start": start + 1, "end": end, "total": total})
	}
	meta := map[string]any{"page": page, "total_pages": totalPages, "start": start, "end": end, "total": total,
		"page_info": pageInfo}
	return options[start:end], meta
}

// Three option slash command confirmation, not supported so the gateway text fallback runs
func (b *BasePlatformAdapter) SendSlashConfirm(ctx context.Context, chatID, title, message, sessionKey, confirmID string, metadata map[string]any) (SendResult, error) {
	return SendResult{Success: false, Error: "Not supported"}, nil
}

// Clarify prompt as a numbered list awaiting a typed reply, or the bare question when open ended
func (b *BasePlatformAdapter) SendClarify(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (SendResult, error) {
	var text string
	if len(choices) > 0 {
		entry := tools.GetPendingClarify(clarifyID)
		hint := i18n.T(ctx, "gateway.clarify.hint_single", "", nil)
		if entry != nil && entry.MultiSelect {
			hint = i18n.T(ctx, "gateway.clarify.hint_multi", "", nil)
		}
		lines := []string{i18n.T(ctx, "gateway.clarify.question", "", map[string]any{"question": question}), ""}
		for i, choice := range choices {
			lines = append(lines, i18n.T(ctx, "gateway.clarify.option_line", "", map[string]any{"index": i + 1, "choice": choice}))
		}
		lines = append(lines, "", hint)
		text = strings.Join(lines, "\n")
		tools.MarkClarifyAwaitingText(clarifyID)
	} else {
		text = i18n.T(ctx, "gateway.clarify.question", "", map[string]any{"question": question})
	}
	return b.Self().Send(ctx, chatID, text, "", metadata)
}
