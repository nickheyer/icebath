package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
)

// Thread archive durations Discord accepts, in minutes
var validThreadAutoArchiveMinutes = []int{60, 1440, 4320, 10080}

// Pause before the second auto thread attempt, most failures being transient connect errors
var autoThreadRetryDelay = 750 * time.Millisecond

// Posts a thread creation payload to a channel or a message, the reason sent as the audit log header
func (a *DiscordAdapter) startThread(ctx context.Context, channelID, messageID string, body map[string]any, reason string) (*discordgo.Channel, error) {
	client := a.liveClient()
	if client == nil {
		return nil, fmt.Errorf("Not connected")
	}
	endpoint := discordgo.EndpointChannelThreads(channelID)
	if messageID != "" {
		endpoint = discordgo.EndpointChannelMessageThread(channelID, messageID)
	}
	opts := []discordgo.RequestOption{discordgo.WithContext(ctx)}
	if reason != "" {
		opts = append(opts, discordgo.WithAuditLogReason(reason))
	}
	raw, err := client.RequestWithBucketID(http.MethodPost, endpoint, body, endpoint, opts...)
	if err != nil {
		return nil, dpyError(err)
	}
	var th discordgo.Channel
	if err := json.Unmarshal(raw, &th); err != nil {
		return nil, err
	}
	if client.State != nil {
		client.State.ChannelAdd(&th)
	}
	return &th, nil
}

// Result of a created thread with its id, name and channel
func threadCreated(th *discordgo.Channel, name string) map[string]any {
	shown := th.Name
	if shown == "" {
		shown = name
	}
	return map[string]any{"success": true, "thread_id": th.ID, "thread_name": shown, "thread": th}
}

// Creates a private thread in the interaction channel, a seed message thread when Discord rejects it
func (a *DiscordAdapter) createThread(ctx context.Context, i *discordgo.Interaction, name, message string, autoArchiveDuration int) map[string]any {
	name = strings.TrimSpace(name)
	if name == "" {
		return map[string]any{"error": i18n.T(ctx, "platform.discord.command.thread.error_name_required", "", nil)}
	}
	if !slices.Contains(validThreadAutoArchiveMinutes, autoArchiveDuration) {
		allowed := make([]string, len(validThreadAutoArchiveMinutes))
		for n, v := range validThreadAutoArchiveMinutes {
			allowed[n] = strconv.Itoa(v)
		}
		return map[string]any{"error": i18n.T(ctx, "platform.discord.command.thread.error_invalid_archive", "", map[string]any{"allowed": strings.Join(allowed, ", ")})}
	}
	channel := a.resolveInteractionChannel(ctx, i)
	if channel == nil {
		return map[string]any{"error": i18n.T(ctx, "platform.discord.command.thread.error_no_channel", "", nil)}
	}
	if isDMChannel(channel) {
		return map[string]any{"error": i18n.T(ctx, "platform.discord.command.thread.error_dm_not_supported", "", nil)}
	}
	parent := a.threadParentChannel(channel)
	if parent == nil {
		return map[string]any{"error": i18n.T(ctx, "platform.discord.command.thread.error_no_parent", "", nil)}
	}
	displayName := "unknown user"
	if u := interactionUser(i); u != nil {
		if i.Member != nil && i.Member.Nick != "" {
			displayName = i.Member.Nick
		} else if shown := u.DisplayName(); shown != "" {
			displayName = shown
		}
	}
	reason := "Requested by " + displayName + " via /thread"
	starter := strings.TrimSpace(message)
	th, directErr := a.startThread(ctx, parent.ID, "", map[string]any{"name": name, "auto_archive_duration": autoArchiveDuration,
		"type": int(discordgo.ChannelTypeGuildPrivateThread), "invitable": true, "rate_limit_per_user": nil}, reason)
	if directErr == nil && starter != "" {
		_, directErr = a.channelSend(ctx, th.ID, starter, nil)
	}
	if directErr == nil {
		return threadCreated(th, name)
	}
	seedContent := starter
	if seedContent == "" {
		seedContent = i18n.T(ctx, "platform.discord.thread.seed_message", "", map[string]any{"name": name})
	}
	seed, fallbackErr := a.channelSend(ctx, parent.ID, seedContent, nil)
	if fallbackErr == nil {
		th, fallbackErr = a.startThread(ctx, parent.ID, seed.ID, map[string]any{"name": name, "auto_archive_duration": autoArchiveDuration, "rate_limit_per_user": nil}, reason)
	}
	if fallbackErr == nil {
		return threadCreated(th, name)
	}
	return map[string]any{"error": i18n.T(ctx, "platform.discord.command.thread.error_both_failed", "",
		map[string]any{"direct_error": directErr.Error(), "fallback_error": fallbackErr.Error()})}
}

// Remembers the provisional name so the semantic rename can tell a human edit apart
func stampAutoThreadName(th *discordgo.Channel, threadName string) *autoThread {
	return &autoThread{Channel: th, initialName: threadName}
}

// Creates a thread from a user message, the seed message path as fallback, each retried once
func (a *DiscordAdapter) autoCreateThread(ctx context.Context, m *inboundMessage) *autoThread {
	threadName := deriveAutoThreadName(ctx, m.Content)
	displayName := authorDisplayName(m.Message)
	if displayName == "" {
		displayName = "unknown user"
	}
	reason := "Auto-threaded from mention by " + displayName
	var lastDirect, lastFallback error
	for attempt := range 2 {
		th, err := a.startThread(ctx, m.ChannelID, m.ID, map[string]any{"name": threadName, "auto_archive_duration": 1440, "rate_limit_per_user": nil}, "")
		if err == nil {
			return stampAutoThreadName(th, threadName)
		}
		lastDirect = err
		seed, err := a.channelSend(ctx, m.ChannelID, i18n.T(ctx, "platform.discord.thread.seed_message", "", map[string]any{"name": threadName}), nil)
		if err == nil {
			th, err = a.startThread(ctx, seed.ChannelID, seed.ID, map[string]any{"name": threadName, "auto_archive_duration": 1440, "rate_limit_per_user": nil}, reason)
			if err == nil {
				return stampAutoThreadName(th, threadName)
			}
		}
		lastFallback = err
		if attempt == 0 {
			select {
			case <-time.After(autoThreadRetryDelay):
				continue
			case <-ctx.Done():
			}
			break
		}
	}
	pkgLog().Warn(fmt.Sprintf("[%s] Auto-thread creation failed after retry. Direct error: %v. Fallback error: %v", a.Name(), lastDirect, lastFallback))
	return nil
}

// Channel kinds discord.py gives create_thread, the text, news, forum and media channels
var handoffCreateThreadTypes = map[discordgo.ChannelType]bool{discordgo.ChannelTypeGuildText: true, discordgo.ChannelTypeGuildNews: true,
	discordgo.ChannelTypeGuildForum: true, discordgo.ChannelTypeGuildMedia: true}

// Channel kinds discord.py can send a seed message to
var handoffSendTypes = map[discordgo.ChannelType]bool{discordgo.ChannelTypeGuildText: true, discordgo.ChannelTypeGuildNews: true,
	discordgo.ChannelTypeGuildVoice: true, discordgo.ChannelTypeGuildStageVoice: true, discordgo.ChannelTypeGuildNewsThread: true,
	discordgo.ChannelTypeGuildPublicThread: true, discordgo.ChannelTypeGuildPrivateThread: true, discordgo.ChannelTypeDM: true, discordgo.ChannelTypeGroupDM: true}

// Renames a thread, a guarded rename leaving a thread a person renamed or one that already carried another name alone
func (a *DiscordAdapter) RenameThread(ctx context.Context, threadID, name string, opts gateway.ThreadRenameOptions) (bool, error) {
	client := a.liveClient()
	if client == nil {
		return false, nil
	}
	id, err := snowflakeInt(threadID)
	if err != nil {
		return false, nil
	}
	cleaned := strings.TrimSpace(whitespaceRunRE.ReplaceAllString(name, " "))
	if cleaned == "" {
		return false, nil
	}
	if gateway.UTF16Len(cleaned) > 80 {
		cleaned = strings.TrimRightFunc(gateway.PrefixWithinUTF16Limit(cleaned, 77), unicode.IsSpace) + "..."
	}
	thread := a.stateChannel(id)
	if thread == nil {
		if thread, err = client.Channel(id, discordgo.WithContext(ctx)); err != nil {
			pkgLog().Debug(fmt.Sprintf("[%s] Failed to resolve Discord thread %s for rename: %s", a.Name(), threadID, dpyError(err)))
			return false, nil
		}
	}
	current := thread.Name
	if opts.OnlyIfCurrentName != nil && current != *opts.OnlyIfCurrentName {
		pkgLog().Info(fmt.Sprintf("[%s] Discord semantic thread rename skipped for %s: current name %s != expected %s",
			a.Name(), threadID, anyutil.Quote(current), anyutil.Quote(*opts.OnlyIfCurrentName)))
		return false, nil
	}
	if current == cleaned {
		return true, nil
	}
	edit := func() error {
		_, err := client.ChannelEdit(id, &discordgo.ChannelEdit{Name: cleaned}, discordgo.WithContext(ctx),
			discordgo.WithAuditLogReason("Icebath semantic session title"))
		return err
	}
	if opts.OnlyIfCurrentName != nil {
		err = a.semanticThreadRenames.attempt(id, *opts.OnlyIfCurrentName, cleaned, edit)
	} else {
		err = edit()
	}
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("[%s] Failed to rename Discord thread %s: %s", a.Name(), threadID, dpyError(err)))
		return false, nil
	}
	pkgLog().Info(fmt.Sprintf("[%s] Renamed Discord thread %s from %s to %s", a.Name(), threadID, anyutil.Quote(current), anyutil.Quote(cleaned)))
	return true, nil
}

// Handoff thread under a text channel, a seed message thread when the direct create fails, none for DMs
func (a *DiscordAdapter) CreateHandoffThread(ctx context.Context, parentChatID, name string) (string, bool, error) {
	client := a.liveClient()
	if client == nil {
		return "", false, nil
	}
	parentID, err := snowflakeInt(parentChatID)
	if err != nil {
		return "", false, nil
	}
	parent := a.stateChannel(parentID)
	if parent == nil {
		if parent, err = client.Channel(parentID, discordgo.WithContext(ctx)); err != nil {
			pkgLog().Warn(fmt.Sprintf("[%s] Handoff thread: cannot resolve parent %s: %s", a.Name(), parentChatID, dpyError(err)))
			return "", false, nil
		}
	}
	if parent.Type == discordgo.ChannelTypeDM {
		pkgLog().Info(fmt.Sprintf("[%s] Handoff thread: parent %s is a DM; threads not supported here", a.Name(), parentChatID))
		return "", false, nil
	}
	threadName := strings.TrimSpace(name)
	if name == "" {
		threadName = "handoff"
	}
	if threadName = cut(threadName, 80); threadName == "" {
		threadName = "handoff"
	}
	const reason = "Icebath session handoff"
	if handoffCreateThreadTypes[parent.Type] {
		body := map[string]any{"name": threadName, "auto_archive_duration": 1440, "type": int(discordgo.ChannelTypeGuildPrivateThread), "invitable": true}
		if parent.Type == discordgo.ChannelTypeGuildForum || parent.Type == discordgo.ChannelTypeGuildMedia {
			body = map[string]any{"name": threadName, "auto_archive_duration": 1440, "message": map[string]any{}}
		}
		th, err := a.startThread(ctx, parent.ID, "", body, reason)
		if err == nil {
			return th.ID, true, nil
		}
		pkgLog().Debug(fmt.Sprintf("[%s] Handoff thread: direct create failed (%s); trying seed-message fallback", a.Name(), err))
	}
	if !handoffSendTypes[parent.Type] {
		return "", false, nil
	}
	seed, err := client.ChannelMessageSendComplex(parent.ID, &discordgo.MessageSend{AllowedMentions: a.allowedMentions,
		Content: i18n.T(ctx, "platform.discord.thread.handoff_seed", "", map[string]any{"name": threadName})}, discordgo.WithContext(ctx))
	if err == nil {
		var th *discordgo.Channel
		if th, err = a.startThread(ctx, parent.ID, seed.ID, map[string]any{"name": threadName, "auto_archive_duration": 1440}, reason); err == nil {
			return th.ID, true, nil
		}
	} else {
		err = dpyError(err)
	}
	pkgLog().Warn(fmt.Sprintf("[%s] Handoff thread: both create paths failed for parent %s: %s", a.Name(), parentChatID, err))
	return "", false, nil
}
