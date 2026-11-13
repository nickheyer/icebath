package whatsappcloud

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Localized text of a key in the active display language
func t(ctx context.Context, key string) string { return i18n.T(ctx, key, "", nil) }

// Posts an interactive message and on success remembers the state id to session key mapping for the tap
func (a *WhatsAppCloudAdapter) sendInteractive(ctx context.Context, chatID string, interactive any, metadata map[string]any,
	state *pydict.Dict[string, string], stateID, sessionKey string) (gateway.SendResult, error) {
	result, err := a.postMessageResult(ctx, outboundPayload(chatID, "interactive", interactive, replyToFrom(metadata)),
		postLogs{fail: "[whatsapp_cloud] interactive send failed", reject: "[whatsapp_cloud] interactive rejected (status=%d): %s"})
	if err == nil && result.Success {
		a.mu.Lock()
		gateway.BoundedPut(state, stateID, sessionKey, interactiveStateCacheSize)
		a.mu.Unlock()
	}
	return result, err
}

// Button or list row title stripped and cut to the limit with an ellipsis that counts
func truncateButtonLabel(text string, limit int) string {
	text = pystr.Strip(text)
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:max(1, limit-1)]) + "…"
}

// Interactive body text cut to the limit with a three dot tail
func truncateBody(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-3]) + "..."
}

// Quick reply button rows of id and title pairs
func replyButtons(buttons ...[2]string) []any {
	out := make([]any, 0, len(buttons))
	for _, b := range buttons {
		out = append(out, pyjson.ObjectOf("type", "reply", "reply", pyjson.ObjectOf("id", b[0], "title", b[1])))
	}
	return out
}

// Interactive button body with up to three quick reply buttons
func buttonInteractive(bodyText string, buttons ...[2]string) *pyjson.Object {
	return pyjson.ObjectOf("type", "button", "body", pyjson.ObjectOf("text", bodyText), "action", pyjson.ObjectOf("buttons", replyButtons(buttons...)))
}

// Clarify as native buttons for up to three choices, a list with an Other row beyond, plain text without choices
func (a *WhatsAppCloudAdapter) SendClarify(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (gateway.SendResult, error) {
	question = pystr.Strip(question)
	if len(choices) == 0 {
		replyTo, _ := replyToFrom(metadata).(string)
		return a.Send(ctx, chatID, "❓ "+question, replyTo, nil)
	}
	if len(choices) > 10 {
		choices = choices[:10]
	}
	var list []string
	for _, c := range choices {
		if c = pystr.Strip(c); c != "" {
			list = append(list, c)
		}
	}
	lines := make([]string, len(list))
	for i, c := range list {
		lines[i] = fmt.Sprintf("%d. %s", i+1, c)
	}
	bodyText := truncateBody("❓ "+question+"\n\n"+strings.Join(lines, "\n"), 1024)
	var interactive *pyjson.Object
	if len(list) <= 3 {
		buttons := make([][2]string, len(list))
		for idx := range list {
			buttons[idx] = [2]string{fmt.Sprintf("cl:%s:%d", clarifyID, idx), truncateButtonLabel(fmt.Sprint(idx+1), 20)}
		}
		interactive = buttonInteractive(bodyText, buttons...)
	} else {
		rows := make([]any, 0, len(list)+1)
		for idx, choice := range list {
			rows = append(rows, pyjson.ObjectOf("id", fmt.Sprintf("cl:%s:%d", clarifyID, idx), "title", truncateButtonLabel(fmt.Sprint(idx+1), 24),
				"description", truncateButtonLabel(choice, 72)))
		}
		rows = append(rows, pyjson.ObjectOf("id", "cl:"+clarifyID+":other",
			"title", truncateButtonLabel(t(ctx, "platform.whatsapp.clarify_other_title"), 24),
			"description", truncateButtonLabel(t(ctx, "platform.whatsapp.clarify_other_description"), 72)))
		interactive = pyjson.ObjectOf("type", "list", "body", pyjson.ObjectOf("text", bodyText),
			"action", pyjson.ObjectOf("button", truncateButtonLabel(t(ctx, "platform.whatsapp.clarify_list_button"), 20),
				"sections", []any{pyjson.ObjectOf("title", t(ctx, "platform.whatsapp.clarify_list_section"), "rows", rows)}))
	}
	return a.sendInteractive(ctx, chatID, interactive, metadata, a.clarifyState, clarifyID, sessionKey)
}

// Approval header in WhatsApp bold over a blank line
func (a *WhatsAppCloudAdapter) EAHeader(ctx context.Context) string {
	return "⚠️ *" + gateway.EAHeaderLocalized(ctx) + "*\n\n"
}

// Approve and Deny buttons resolving through the gateway approval queue on tap
func (a *WhatsAppCloudAdapter) SendExecApprovalPrompt(ctx context.Context, prompt *gateway.ExecApprovalPrompt) (gateway.SendResult, error) {
	approvalID := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	interactive := buttonInteractive(truncateBody(prompt.Text, 1024),
		[2]string{"appr:" + approvalID + ":approve", truncateButtonLabel(t(ctx, "platform.whatsapp.approve_button"), 20)},
		[2]string{"appr:" + approvalID + ":deny", truncateButtonLabel(t(ctx, "platform.whatsapp.deny_button"), 20)})
	return a.sendInteractive(ctx, prompt.ChatID, interactive, prompt.Metadata, a.execApprovalState, approvalID, prompt.SessionKey)
}

// Approve Once, Always and Cancel buttons for a caller supplied confirm id
func (a *WhatsAppCloudAdapter) SendSlashConfirm(ctx context.Context, chatID, title, message, sessionKey, confirmID string, metadata map[string]any) (gateway.SendResult, error) {
	interactive := buttonInteractive(truncateBody("*"+title+"*\n\n"+message, 1024),
		[2]string{"sc:once:" + confirmID, truncateButtonLabel(t(ctx, "platform.whatsapp.slash_confirm_once"), 20)},
		[2]string{"sc:always:" + confirmID, truncateButtonLabel(t(ctx, "platform.whatsapp.slash_confirm_always"), 20)},
		[2]string{"sc:cancel:" + confirmID, truncateButtonLabel(t(ctx, "platform.whatsapp.slash_confirm_cancel"), 20)})
	return a.sendInteractive(ctx, chatID, interactive, metadata, a.slashConfirmState, confirmID, sessionKey)
}
