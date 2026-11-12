package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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
	name = pystr.Strip(name)
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
	starter := pystr.Strip(message)
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
	slog.Warn(fmt.Sprintf("[%s] Auto-thread creation failed after retry. Direct error: %v. Fallback error: %v", a.Name(), lastDirect, lastFallback))
	return nil
}
