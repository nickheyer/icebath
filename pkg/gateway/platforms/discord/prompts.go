package discord

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/i18n"
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
