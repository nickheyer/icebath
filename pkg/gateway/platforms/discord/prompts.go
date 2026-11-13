package discord

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Plain content mirroring an embed payload, the body cut to fit the message cap with a marker
func selfContainedPromptContent(ctx context.Context, header, body string, codeBlock bool, tail string) string {
	prefix, suffix := header+"\n\n", tail
	if codeBlock {
		prefix, suffix = header+"\n```bash\n", "\n```"+tail
	}
	truncatedSuffix := "\n" + i18n.T(ctx, "platform.discord.prompt.truncated_marker", "", nil)
	budget := max(0, discordMaxMessageLength-utf8.RuneCountInString(prefix)-utf8.RuneCountInString(suffix))
	if utf8.RuneCountInString(body) > budget {
		body = cut(body, max(0, budget-utf8.RuneCountInString(truncatedSuffix))) + truncatedSuffix
	}
	return prefix + body + suffix
}

// User mentions of numeric allowlist entries for approval prompts when approval_mentions is on
func (a *DiscordAdapter) approvalMentionContent(ctx context.Context) string {
	if !a.extraOrEnvFlag(ctx, "approval_mentions", "DISCORD_APPROVAL_MENTIONS", "false", true) {
		return ""
	}
	a.mu.Lock()
	var ids []string
	for uid := range a.allowedUserIDs {
		if isDigitString(uid) {
			ids = append(ids, uid)
		}
	}
	a.mu.Unlock()
	if len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)
	mentions := make([]string, len(ids))
	for i, uid := range ids {
		mentions[i] = "<@" + uid + ">"
	}
	return strings.Join(mentions, " ")
}

// Exec approval header worded for the language active at send time
func (a *DiscordAdapter) EAHeader(ctx context.Context) string {
	return "⚠️ **" + i18n.T(ctx, "gateway.exec_approval.header", "", nil) + "**\n\n" +
		i18n.T(ctx, "platform.discord.approval.question", "", nil) + "\n\n**" +
		i18n.T(ctx, "platform.discord.approval.requested_command_label", "", nil) + "**\n"
}

// Exec approval markup of the Discord card, a bash fence and a 300 character reason cap
func applyDiscordTraits(a *DiscordAdapter) {
	a.Traits.EACodeOpen, a.Traits.EACodeClose, a.Traits.EAReasonBudget = "```bash\n", "\n```\n", 300
}

// Bold reason label followed by a space
func (a *DiscordAdapter) EAReasonLabel(ctx context.Context) string {
	return "**" + gateway.EAReasonLabelLocalized(ctx) + ":** "
}

// Smart deny line after a blank line, the part before its first colon in bold
func (a *DiscordAdapter) EASmartDenyLine(ctx context.Context) string {
	line := gateway.EASmartDenyLineLocalized(ctx)
	if label, rest, ok := strings.Cut(line, ":"); ok {
		return "\n\n**" + label + ":**" + rest
	}
	return "\n\n**" + line + "**"
}

// Command preview room left in the message cap after the fixed card parts and the approval mentions
func (a *DiscordAdapter) ExecApprovalCmdBudget(ctx context.Context, description string, smartDenied bool) int {
	n := utf8.RuneCountInString
	fixed := n(a.EAHeader(ctx)) + n(a.Traits.EACodeOpen) + n(a.Traits.EACodeClose) + n(a.EAReasonLabel(ctx)) + n(description) + n("...") +
		n(a.EADeadlineLine(ctx)) + n(a.approvalMentionContent(ctx)) + 1
	if smartDenied {
		fixed += n(a.EASmartDenyLine(ctx))
	}
	return max(0, discordMaxMessageLength-fixed)
}

// User and role allowlists the views authorise clicks against
func (a *DiscordAdapter) allowlists() (strSet, strSet) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.allowedUserIDs, a.allowedRoleIDs
}

// Message an interactive prompt sends, its content, embed, view and mention policy
type promptMessage struct {
	content         string
	embed           *discordgo.MessageEmbed
	view            *hermesView
	allowedMentions *discordgo.MessageAllowedMentions
}

// Shared tail of interactive prompts that resolves the target, builds, sends and attaches the view to the message
func (a *DiscordAdapter) sendPrompt(ctx context.Context, chatID string, metadata map[string]any, build func(ch *discordgo.Channel) (promptMessage, error), failLog string) gateway.SendResult {
	client := a.liveClient()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}
	}
	msgID, err := a.sendPromptMessage(ctx, client, chatID, metadata, build)
	if err != nil {
		if failLog != "" {
			slog.Warn(fmt.Sprintf("[%s] %s failed: %s", a.Name(), failLog, err))
		}
		return gateway.SendResult{Success: false, Error: err.Error()}
	}
	return gateway.SendResult{Success: true, MessageID: msgID}
}

// Resolves the prompt channel, sends the built message and binds its view to the sent message
func (a *DiscordAdapter) sendPromptMessage(ctx context.Context, client *discordgo.Session, chatID string, metadata map[string]any, build func(ch *discordgo.Channel) (promptMessage, error)) (string, error) {
	ch, err := a.resolveChannel(ctx, promptTargetID(chatID, metadata))
	if err != nil {
		return "", err
	}
	pm, err := build(ch)
	if err != nil {
		return "", err
	}
	send := &discordgo.MessageSend{Content: pm.content, AllowedMentions: a.allowedMentions}
	if pm.allowedMentions != nil {
		send.AllowedMentions = pm.allowedMentions
	}
	if pm.embed != nil {
		send.Embeds = []*discordgo.MessageEmbed{pm.embed}
	}
	if pm.view != nil {
		pm.view.mu.Lock()
		pm.view.liveAuth = a.componentLiveAuth
		send.Components = pm.view.componentsLocked()
		pm.view.mu.Unlock()
	}
	msg, err := client.ChannelMessageSendComplex(ch.ID, send, discordgo.WithContext(ctx))
	if err != nil {
		return "", dpyError(err)
	}
	if pm.view != nil {
		a.views.storeView(ctx, &pm.view.uiView, msg.ID)
		pm.view.mu.Lock()
		pm.view.message = msg
		pm.view.mu.Unlock()
	}
	return msg.ID, nil
}

// Approval card whose content carries the payload and whose embed carries the state, retired when its request settles
func (a *DiscordAdapter) SendExecApprovalPrompt(ctx context.Context, prompt *gateway.ExecApprovalPrompt) (gateway.SendResult, error) {
	requireAdmin, adminUserIDs := ResolveExecApprovalAdminGate(a.Config.Extra)
	choices := strSet{}
	for _, c := range prompt.Choices() {
		choices[c] = struct{}{}
	}
	requestID := ""
	if prompt.RequestID != nil {
		requestID = *prompt.RequestID
	}
	users, roles := a.allowlists()
	view, err := a.newExecApprovalView(ctx, prompt.SessionKey, users, roles, requireAdmin, adminUserIDs, choices.has("always"), choices.has("session"),
		prompt.SmartDenied, requestID)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if view.requestID != "" && !a.observeExecApprovalSettle(ctx, prompt.SessionKey, view) {
		view.mu.Lock()
		view.pendingRetire = "gone"
		view.mu.Unlock()
	}
	result := a.sendPrompt(ctx, prompt.ChatID, prompt.Metadata, func(*discordgo.Channel) (promptMessage, error) {
		content := prompt.Text
		pm := promptMessage{view: &view.hermesView,
			embed: newEmbed(truncateDiscordComponentText("⚠️ "+gateway.EAHeaderLocalized(ctx), discordEmbedTitleLimit), "", colorOrange)}
		if mentions := a.approvalMentionContent(ctx); mentions != "" {
			content = mentions + "\n" + content
			pm.allowedMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeUsers}}
		}
		pm.content = content
		return pm, nil
	}, "")
	view.mu.Lock()
	pending := view.pendingRetire
	view.mu.Unlock()
	if result.Success && pending != "" {
		view.retire(ctx, pending, true)
	}
	return result, nil
}

// Retires the card when its request settles, parking the reason on the view while the card is still in flight
func (a *DiscordAdapter) observeExecApprovalSettle(ctx context.Context, sessionKey string, view *execApprovalView) bool {
	bg := context.WithoutCancel(ctx)
	return tools.ObserveGatewaySettle(sessionKey, view.requestID, func(reason string) {
		view.mu.Lock()
		if view.message == nil {
			view.pendingRetire = reason
			view.mu.Unlock()
			return
		}
		view.mu.Unlock()
		a.TrackBackgroundTask(gateway.SpawnAdapterTask(bg, func(tctx context.Context) error {
			view.retire(tctx, reason, true)
			return nil
		}))
	})
}

// Three button slash command confirmation whose message lives in the content under a header only card
func (a *DiscordAdapter) SendSlashConfirm(ctx context.Context, chatID, title, message, sessionKey, confirmID string, metadata map[string]any) (gateway.SendResult, error) {
	return a.sendPrompt(ctx, chatID, metadata, func(*discordgo.Channel) (promptMessage, error) {
		header := title
		if header == "" {
			header = i18n.T(ctx, "platform.discord.approval.confirm_title", "", nil)
		}
		users, roles := a.allowlists()
		view, err := a.newSlashConfirmView(ctx, sessionKey, confirmID, users, roles)
		if err != nil {
			return promptMessage{}, err
		}
		return promptMessage{content: selfContainedPromptContent(ctx, "**"+header+"**", message, false, ""), view: &view.hermesView,
			embed: newEmbed(truncateDiscordComponentText(header, discordEmbedTitleLimit), "", colorOrange)}, nil
	}, ""), nil
}

// Clarify prompt with a button per choice plus Other, or a reply hint when there are no choices
func (a *DiscordAdapter) SendClarify(ctx context.Context, chatID, question string, choices []string, clarifyID, sessionKey string, metadata map[string]any) (gateway.SendResult, error) {
	return a.sendPrompt(ctx, chatID, metadata, func(*discordgo.Channel) (promptMessage, error) {
		clarifyTitle := i18n.T(ctx, "platform.discord.prompt.clarify_title", "", nil)
		pm := promptMessage{embed: newEmbed(truncateDiscordComponentText("❓ "+clarifyTitle, discordEmbedTitleLimit), "", colorOrange)}
		var clean []string
		for _, c := range choices {
			if s := pystr.Strip(c); s != "" {
				clean = append(clean, s)
			}
		}
		clean = clean[:min(len(clean), 24)]
		hint := i18n.T(ctx, "platform.discord.prompt.clarify_hint_text", "", nil)
		if len(clean) > 0 {
			hint = i18n.T(ctx, "platform.discord.prompt.clarify_hint_buttons", "", nil)
			users, roles := a.allowlists()
			view, err := a.newClarifyChoiceView(ctx, clean, clarifyID, users, roles)
			if err != nil {
				return promptMessage{}, err
			}
			pm.view = &view.hermesView
		}
		pm.content = selfContainedPromptContent(ctx, "❓ **"+clarifyTitle+"**", pystr.Strip(question), false, "\n\n"+hint)
		return pm, nil
	}, "send_clarify"), nil
}

// Yes and no prompt of the gateway update watcher, marked non conversational when the metadata says so
func (a *DiscordAdapter) SendUpdatePrompt(ctx context.Context, chatID, prompt, def, sessionKey string, metadata map[string]any) (gateway.SendResult, error) {
	result := a.sendPrompt(ctx, chatID, metadata, func(*discordgo.Channel) (promptMessage, error) {
		defaultHint := ""
		if def != "" {
			defaultHint = i18n.T(ctx, "platform.discord.prompt.default_hint", "", map[string]any{"default": def})
		}
		updateTitle := i18n.T(ctx, "platform.discord.prompt.update_title", "", nil)
		users, roles := a.allowlists()
		view, err := a.newUpdatePromptView(ctx, sessionKey, users, roles)
		if err != nil {
			return promptMessage{}, err
		}
		return promptMessage{content: selfContainedPromptContent(ctx, "☤ **"+updateTitle+"**", prompt+defaultHint, false, ""), view: &view.hermesView,
			embed: newEmbed(truncateDiscordComponentText("☤ "+updateTitle, discordEmbedTitleLimit), prompt+defaultHint, colorGold)}, nil
	}, "")
	if result.Success && metadataMarksNonconversational(metadata) {
		a.nonconversationalMessages.markMany(ctx, []string{fmt.Sprint(result.MessageID)})
	}
	return result, nil
}

// Two step provider then model select picker
func (a *DiscordAdapter) SendModelPicker(ctx context.Context, chatID string, providers []map[string]any, currentModel, currentProvider, sessionKey string,
	onModelSelected func(ctx context.Context, chatID, modelID, providerSlug string) (string, error), metadata map[string]any) (gateway.SendResult, error) {
	return a.sendPrompt(ctx, chatID, metadata, func(*discordgo.Channel) (promptMessage, error) {
		users, roles := a.allowlists()
		view, err := a.newModelPickerView(ctx, providers, currentModel, currentProvider, sessionKey, onModelSelected, users, roles)
		if err != nil {
			return promptMessage{}, err
		}
		return promptMessage{view: &view.hermesView, embed: newEmbed(tDiscord(ctx, "platform.discord.picker.title", discordEmbedTitleLimit, nil),
			providerSelectText(ctx, currentModel, currentProvider), colorBlue)}, nil
	}, "send_model_picker"), nil
}

// Flat select picker whose first title line heads the card and the rest describe it
func (a *DiscordAdapter) SendChoicePicker(ctx context.Context, chatID, title string, choices []map[string]any, sessionKey string,
	onChoiceSelected func(ctx context.Context, chatID, value string) (string, error), metadata map[string]any) (gateway.SendResult, error) {
	return a.sendPrompt(ctx, chatID, metadata, func(*discordgo.Channel) (promptMessage, error) {
		heading := i18n.T(ctx, "platform.discord.picker.choice_default_title", "", nil)
		description := ""
		if title != "" {
			lines := pystr.Splitlines(title, false)
			heading = lines[0]
			description = strings.Join(lines[1:], "\n")
		}
		users, roles := a.allowlists()
		view, err := a.newChoicePickerView(ctx, choices, onChoiceSelected, users, roles)
		if err != nil {
			return promptMessage{}, err
		}
		return promptMessage{view: &view.hermesView, embed: newEmbed(truncateDiscordComponentText("⚙ "+heading, discordEmbedTitleLimit), description, colorBlue)}, nil
	}, "send_choice_picker"), nil
}
