package discord

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	discordMaxMessageLength = 2000
	discordSplitThreshold   = 1900
	discordMaxSplitMessages = 8
)

// Discord bot adapter for guild and DM messages, threads, slash commands, button approvals and reactions
type DiscordAdapter struct {
	*gateway.BasePlatformAdapter

	mu sync.Mutex

	client     *discordgo.Session
	readyEvent *gateway.InterruptEvent
	botTask    *botTask

	allowedUserIDs      strSet
	usernameResolvedIDs strSet
	allowedRoleIDs      strSet
	// Text of the allowed people and allowed roles settings the two sets above were read from
	allowlistStamp [2]string
	// Home of the profile this adapter serves, bound at connect time
	settingsHome string

	textBatchDelaySeconds      float64
	textBatchSplitDelaySeconds float64
	botTagDebounceUntil        map[string]float64

	threads                 *gateway.ThreadParticipationTracker
	recoveryStore           *DiscordRecoveryStore
	dedup                   *gateway.MessageDeduplicator
	disconnecting           bool
	warnedFailClosedDefault bool

	replyToMode               string
	allowedMentions           *discordgo.MessageAllowedMentions
	nonconversationalMessages *nonConversationalTracker
	lastSelfMessageID         map[string]string
	lastOverflowPreview       map[[2]string]string
	typingTasks               map[string]*typingTask
	desiredCommands           []map[string]any
	semanticThreadRenames     *semanticThreadRenames

	handleMessage func(ctx context.Context, event *gateway.MessageEvent) error

	link                         *gatewayLink
	runCtx                       context.Context
	postConnectTask              *bgTask
	livenessTask                 *bgTask
	livenessNotificationTask     *bgTask
	missedMessageBackfillTask    *bgTask
	livenessIntervalSeconds      float64
	livenessFailureThreshold     int
	heartbeatAckMaxAgeSeconds    float64
	maxLatencySeconds            float64
	eventMaxSilenceSeconds       float64
	lastDispatchedEventMonotonic *float64
	slashCommands                bool

	tree                    []*appCommand
	skillEntries            []skillEntry
	skillLookup             map[string][2]string
	skillGroupReservedNames map[string]bool
	skillGroupHiddenCount   int
	views                   viewStore

	voiceState
	voiceTimeoutSeconds    int
	playbackTimeoutSeconds int
	voiceFX                voiceFXConfig
}

// Truncated URL previews stay clickable as Discord markdown links
func (a *DiscordAdapter) FormatToolPreview(preview tools.ToolPreview) string {
	if preview.URL == "" {
		return preview.Text
	}
	return formatDiscordMarkdownLink(preview.Text, preview.URL)
}

// Seconds of a liveness window, a negative length switching the probe off like zero does
func livenessSeconds(d time.Duration) float64 { return max(d.Seconds(), 0) }

// Surfaces a post startup gateway session exit as a retryable fatal so the runner requeues the adapter
func (a *DiscordAdapter) handleBotTaskDone(ctx context.Context, task *botTask) {
	a.mu.Lock()
	disconnecting, current := a.disconnecting, a.botTask
	a.mu.Unlock()
	if disconnecting {
		return
	}
	if current != nil && task != current {
		return
	}
	if !a.IsConnected() {
		return
	}
	if errors.Is(task.err, context.Canceled) {
		return
	}
	message := "Discord gateway task exited without an exception"
	if task.err != nil {
		message = "Discord gateway task exited: " + cmp.Or(discordProxyError(ctx, task.err), task.err.Error())
	}
	pkgLog().Error(fmt.Sprintf("[%s] %s", a.Name(), message))
	a.SetFatalError(ctx, "discord_gateway_task_exited", message, true)
	go func() {
		if err := a.NotifyFatalError(ctx); err != nil {
			pkgLog().Warn(fmt.Sprintf("[%s] Failed to notify gateway supervisor about Discord task exit: %s", a.Name(), err))
		}
	}()
}

// Raised when Discord rejects the bot token at login
type loginFailure struct{ msg string }

func (e *loginFailure) Error() string { return e.msg }

// Raised when the gateway closes because privileged intents are not enabled
type privilegedIntentsRequired struct{ shardID *int }

func (e *privilegedIntentsRequired) Error() string {
	shard := "None"
	if e.shardID != nil {
		shard = fmt.Sprint(*e.shardID)
	}
	return "Shard ID " + shard + " is requesting privileged intents that have not been explicitly enabled in the developer portal. " +
		"It is recommended to go to https://discord.com/developers/applications/ and explicitly enable the privileged intents within your application's page. " +
		"If this is not possible, then consider disabling the privileged intents instead."
}

// Kind of a failure and the proxy's host when the proxy URL the Discord connections use holds a secret the raw error text could repeat, empty otherwise
func discordProxyError(ctx context.Context, err error) string {
	proxyURL, proxyErr := gateway.ResolveProxyURL(ctx, "DISCORD_PROXY", nil, "")
	if proxyErr != nil {
		return ""
	}
	return gateway.DescribeProxyError(err, proxyURL)
}

// Maps a startup error to its fatal code, message and retryability by type, unknown types stay retryable and shown stands for the error text when set
func (a *DiscordAdapter) classifyConnectException(err error, shown string) (string, string, bool) {
	text := cmp.Or(shown, err.Error())
	var login *loginFailure
	if errors.As(err, &login) {
		return "discord_auth_error", fmt.Sprintf("Discord bot token rejected: %s. The token is invalid or was revoked — regenerate it in the Discord Developer Portal and update DISCORD_BOT_TOKEN.", text), false
	}
	var intents *privilegedIntentsRequired
	if errors.As(err, &intents) {
		a.mu.Lock()
		users, roles := keysOf(a.allowedUserIDs), keysOf(a.allowedRoleIDs)
		a.mu.Unlock()
		return "discord_intents_required", formatPrivilegedIntentsGuidance(needsServerMembersIntent(users, roles)), false
	}
	return "discord_connect_error", "Discord startup failed: " + text, true
}

func keysOf(s strSet) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	return out
}

// Thread id and chat id of a channel, the chat being the thread itself for threads
func threadIDAndChatForChannel(ch *discordgo.Channel) (threadID, chatID *string) {
	if ch == nil || ch.ID == "" {
		return nil, nil
	}
	id := ch.ID
	if ch.IsThread() {
		return &id, &id
	}
	return nil, &id
}

// Source fields of one normalized platform event
type platformEventSource struct {
	chatID, userID, userName, threadID, guildID, messageID *string
}

// Session source the gateway authorizes against, failing closed on missing identity
func (a *DiscordAdapter) sourceForPlatformEvent(ctx context.Context, f platformEventSource) (*gateway.SessionSource, error) {
	if f.userID == nil || *f.userID == "" || f.chatID == nil || *f.chatID == "" {
		return nil, errors.New("gateway_platform_event requires actor and chat identities")
	}
	chatType := "group"
	if f.threadID != nil && *f.threadID != "" {
		chatType = "thread"
	}
	return a.BuildSource(ctx, *f.chatID, gateway.SourceFields{ChatType: chatType, UserID: f.userID, UserName: f.userName,
		ThreadID: f.threadID, GuildID: f.guildID, MessageID: f.messageID}), nil
}

// Forwards one envelope to the gateway boundary, errors logged and never escaping
func (a *DiscordAdapter) firePlatformEvent(ctx context.Context, event map[string]any, source *gateway.SessionSource) {
	handler := a.PlatformEventHandler()
	if handler == nil {
		return
	}
	if err := handler(ctx, event, source); err != nil {
		pkgLog().Debug(fmt.Sprintf("[%s] gateway_platform_event dispatch error", a.Name()), "err", err)
	}
}

// Plugin hook registry probe the platform event fast path consults
var hasHook = plugins.HasHook

// Whether any plugin hooks gateway platform events
func platformEventsSubscribed(ctx context.Context) bool {
	return hasHook(ctx, "gateway_platform_event")
}

// Builds one event payload and source, dropping nil builds and normalize errors, then dispatches it
func (a *DiscordAdapter) emitPlatformEvent(ctx context.Context, eventType string, build func() (map[string]any, *platformEventSource)) {
	if !platformEventsSubscribed(ctx) {
		return
	}
	payload, fields := build()
	if payload == nil {
		return
	}
	source, err := a.sourceForPlatformEvent(ctx, *fields)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("[%s] %s normalize error", a.Name(), eventType), "err", err)
		return
	}
	a.firePlatformEvent(ctx, map[string]any{"platform": "discord", "event_type": eventType, "payload": payload}, source)
}

// First n code points of a string
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func strPtr(s string) *string { return &s }

// Message edit and delete payload and source, nil for bot authors and unidentified messages
func (a *DiscordAdapter) messageEventParts(m *discordgo.Message, extra func(*discordgo.Message, *discordgo.User) map[string]any) (map[string]any, *platformEventSource) {
	author := m.Author
	if author != nil && author.Bot {
		return nil, nil
	}
	threadID, chatID := threadIDAndChatForChannel(a.channelOf(m))
	if chatID == nil || m.ID == "" {
		return nil, nil
	}
	payload := map[string]any{"chat_id": cut(*chatID, 128), "message_id": cut(m.ID, 128), "thread_id": nil}
	if threadID != nil && *threadID != "" {
		payload["thread_id"] = cut(*threadID, 128)
	}
	for k, v := range extra(m, author) {
		payload[k] = v
	}
	src := &platformEventSource{chatID: chatID, threadID: threadID, messageID: strPtr(m.ID)}
	if author != nil {
		if author.ID != "" {
			src.userID = strPtr(author.ID)
		}
		src.userName = strPtr(author.DisplayName())
		if m.Member != nil && m.Member.Nick != "" {
			src.userName = strPtr(m.Member.Nick)
		}
	}
	if m.GuildID != "" {
		src.guildID = strPtr(m.GuildID)
	}
	return payload, src
}

// Thread create and rename payload and source, the owner standing in as the actor
func threadEventParts(th *discordgo.Channel, extra func(*discordgo.Channel, *string) map[string]any) (map[string]any, *platformEventSource) {
	if th == nil || th.ID == "" {
		return nil, nil
	}
	var ownerID *string
	if th.OwnerID != "" {
		ownerID = strPtr(th.OwnerID)
	}
	payload := map[string]any{"thread_id": cut(th.ID, 128), "parent_chat_id": nil}
	if th.ParentID != "" {
		payload["parent_chat_id"] = cut(th.ParentID, 128)
	}
	for k, v := range extra(th, ownerID) {
		payload[k] = v
	}
	src := &platformEventSource{chatID: strPtr(th.ID), userID: ownerID, threadID: strPtr(th.ID)}
	if th.GuildID != "" {
		src.guildID = strPtr(th.GuildID)
	}
	return payload, src
}

// Normalizes a message edit into a message_edited event
func (a *DiscordAdapter) onPlatformMessageEdit(ctx context.Context, before, after *discordgo.Message) {
	m := after
	if m == nil {
		m = before
	}
	if m == nil {
		return
	}
	a.emitPlatformEvent(ctx, "message_edited", func() (map[string]any, *platformEventSource) {
		return a.messageEventParts(m, func(msg *discordgo.Message, _ *discordgo.User) map[string]any {
			out := map[string]any{"text": cut(msg.Content, 8192), "edited_at": nil}
			if msg.EditedTimestamp != nil {
				out["edited_at"] = cut(isoformat(*msg.EditedTimestamp), 64)
			}
			return out
		})
	})
}

// Normalizes a cached message deletion into a message_deleted event sourced from its author
func (a *DiscordAdapter) onPlatformMessageDelete(ctx context.Context, m *discordgo.Message) {
	if m == nil {
		return
	}
	a.emitPlatformEvent(ctx, "message_deleted", func() (map[string]any, *platformEventSource) {
		return a.messageEventParts(m, func(_ *discordgo.Message, author *discordgo.User) map[string]any {
			var authorID any
			if author != nil && author.ID != "" {
				authorID = cut(author.ID, 128)
			}
			return map[string]any{"author_id": authorID}
		})
	})
}

// Normalizes a thread creation into a thread_created event
func (a *DiscordAdapter) onPlatformThreadCreate(ctx context.Context, th *discordgo.Channel) {
	a.emitPlatformEvent(ctx, "thread_created", func() (map[string]any, *platformEventSource) {
		return threadEventParts(th, func(t *discordgo.Channel, ownerID *string) map[string]any {
			out := map[string]any{"name": cut(t.Name, 256), "owner_id": nil}
			if ownerID != nil {
				out["owner_id"] = cut(*ownerID, 128)
			}
			return out
		})
	})
}

// Normalizes a thread rename into a thread_renamed event, other updates dropped
func (a *DiscordAdapter) onPlatformThreadUpdate(ctx context.Context, before, after *discordgo.Channel) {
	a.emitPlatformEvent(ctx, "thread_renamed", func() (map[string]any, *platformEventSource) {
		if after == nil {
			return nil, nil
		}
		var oldName any
		if before != nil {
			if before.Name == after.Name {
				return nil, nil
			}
			oldName = cut(before.Name, 256)
		}
		return threadEventParts(after, func(*discordgo.Channel, *string) map[string]any {
			return map[string]any{"old_name": oldName, "new_name": cut(after.Name, 256)}
		})
	})
}

// Cancels the gateway session goroutine and waits for it to exit
func (a *DiscordAdapter) cancelBotTask() {
	a.mu.Lock()
	task := a.botTask
	a.mu.Unlock()
	if task != nil && !task.isDone() {
		task.cancel()
		<-task.done
	}
	a.mu.Lock()
	a.botTask = nil
	a.mu.Unlock()
}
