package gatewayrun

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydifflib"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Discord adapter surface that hands captured voice channel speech back to the runner
type voiceInputBinder interface {
	SetVoiceInputCallback(fn func(ctx context.Context, guildID, userID int64, transcript string) error)
}

// Discord adapter surface naming the text channel a guild's voice call is bound to
type voiceTextChannelBinder interface {
	VoiceTextChannel(guildID int64) (int64, bool)
}

// Discord client cache reads of an adapter, each lookup nil when the client or the entry is absent
type discordVoiceCache interface {
	GuildMemberDisplayName(guildID, userID int64) any
	CachedUserDisplayName(userID int64) any
	CachedGuild(guildID int64) any
	CachedChannel(channelID int64) any
	SendToChannel(ctx context.Context, channel any, content string) error
}

// Discord adapter surface holding the source a voice join bound per guild
type voiceSourceStore interface {
	VoiceSource(guildID int64) (map[string]any, bool)
}

// Discord adapter surface splitting a channel into its thread id and parent chat
type threadChannelSplitter interface {
	ThreadIDAndChatForChannel(channel any) (*string, any)
	GetParentChannelID(channel any) *string
}

// Discord adapter surface reading a guild channel's current name and topic
type guildChannelLabeler interface {
	GuildChannelLabels(channel any) (name, topic *string, ok bool)
}

// Discord adapter surface gating senders by role
type roleAllowlistAdapter interface {
	AllowedRoleIDs() map[string]struct{}
	IsAllowedUser(userID string, guild any, isDM bool) any
}

// Discord adapter surface resolving a channel's bound prompt
type channelPromptResolver interface {
	ResolveChannelPrompt(channelID string, parentID *string) (any, error)
}

// Discord adapter surface resolving a channel's bound skills
type channelSkillsResolver interface {
	ResolveChannelSkills(channelID string, parentID *string) (any, error)
}

// Raw message of a synthetic voice event, carrying its guild the way a slash interaction does
type voiceInputRawMessage struct{ guildID int64 }

// Guild of the voice call the transcript came from
func (m voiceInputRawMessage) InteractionGuildID() any { return m.guildID }

// Routes voice transcripts back through the adapter that captured them
func (r *GatewayRunner) bindVoiceInputCallback(ctx context.Context, adapter gateway.PlatformAdapter) {
	binder, ok := adapter.(voiceInputBinder)
	if !ok {
		return
	}
	binder.SetVoiceInputCallback(func(c context.Context, guildID, userID int64, transcript string) error {
		return r.handleVoiceChannelInput(c, guildID, userID, transcript, adapter)
	})
}

// Python value of a voice owner profile, None for the primary bot
func ownerProfileValue(adapter gateway.PlatformAdapter) any {
	if adapter == nil {
		return nil
	}
	if owner := adapter.Base().OwnerProfile(); owner != "" {
		return owner
	}
	return nil
}

// Collapses whitespace, lowercases and drops every non word non space rune of a transcript
func normalizeVoiceTranscript(transcript string) string {
	lowered := pystr.Lower(strings.Join(pystr.SplitWhitespace(transcript, -1), " "))
	var b strings.Builder
	for _, r := range lowered {
		if pystr.IsAlnum(r) || r == '_' || pystr.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Whether the transcript repeats a recent utterance of the same speaker on the same binding
func (r *GatewayRunner) isDuplicateVoiceTranscript(binding [3]any, userID int64, transcript string) bool {
	normalized := normalizeVoiceTranscript(transcript)
	if normalized == "" {
		return false
	}
	now, key := pythread.Monotonic(), [4]any{binding[0], binding[1], binding[2], userID}
	if r.recentVoiceTranscripts == nil {
		r.recentVoiceTranscripts = map[[4]any][]voiceTranscriptSeen{}
	}
	var recent []voiceTranscriptSeen
	for _, seen := range r.recentVoiceTranscripts[key] {
		if now-seen.at <= 12.0 {
			recent = append(recent, seen)
		}
	}
	for _, seen := range recent {
		prior := seen.text
		if prior == normalized {
			r.recentVoiceTranscripts[key] = recent
			return true
		}
		a, b := []rune(prior), []rune(normalized)
		if min(len(a), len(b)) >= 16 && pydifflib.NewSequenceMatcher(a, b, true).Ratio() >= 0.95 {
			r.recentVoiceTranscripts[key] = recent
			return true
		}
	}
	recent = append(recent, voiceTranscriptSeen{at: now, text: normalized})
	if len(recent) > 5 {
		recent = recent[len(recent)-5:]
	}
	r.recentVoiceTranscripts[key] = recent
	return false
}

// Non empty display name string of a cached Discord object, nil otherwise
func displayNameText(name any) *string {
	if text, ok := name.(string); ok && text != "" {
		return &text
	}
	return nil
}

// Source of a voice utterance, the bound text channel's own source when stored else a synthetic one
func voiceInputSource(ctx context.Context, adapter gateway.PlatformAdapter, guildID, userID, textChannelID int64) (*gateway.SessionSource, error) {
	cache, _ := adapter.(discordVoiceCache)
	var userName *string
	var channel any
	if cache != nil {
		userName = displayNameText(cache.GuildMemberDisplayName(guildID, userID))
		channel = cache.CachedChannel(textChannelID)
	}
	cachedUser := func() *string {
		if cache == nil {
			return nil
		}
		return displayNameText(cache.CachedUserDisplayName(userID))
	}
	uid := fmt.Sprint(userID)
	var source *gateway.SessionSource
	stored, hasStored := map[string]any(nil), false
	if store, ok := adapter.(voiceSourceStore); ok {
		stored, hasStored = store.VoiceSource(guildID)
	}
	if hasStored && len(stored) > 0 {
		restored, err := gateway.SessionSourceFromDict(ctx, stored)
		if err != nil {
			return nil, err
		}
		source = restored
		if userName == nil && source.UserID != nil && *source.UserID == uid {
			userName = source.UserName
		}
		if userName == nil || *userName == "" {
			userName = cachedUser()
		}
		name := uid
		if userName != nil && *userName != "" {
			name = *userName
		}
		source.UserID, source.UserName = &uid, &name
		source.MessageID = nil
	} else {
		if userName == nil || *userName == "" {
			userName = cachedUser()
		}
		var threadID *string
		splitter, canSplit := adapter.(threadChannelSplitter)
		if channel != nil && canSplit {
			threadID, _ = splitter.ThreadIDAndChatForChannel(channel)
		}
		var parentID *string
		if threadID != nil && *threadID != "" && canSplit {
			parentID = splitter.GetParentChannelID(channel)
		}
		chatType := "group"
		if threadID != nil && *threadID != "" {
			chatType = "thread"
		}
		name := uid
		if userName != nil && *userName != "" {
			name = *userName
		}
		guild := fmt.Sprint(guildID)
		source = adapter.Base().BuildSource(ctx, fmt.Sprint(textChannelID), gateway.SourceFields{ChatType: chatType,
			UserID: &uid, UserName: &name, ThreadID: threadID, GuildID: &guild, ParentChatID: parentID})
	}
	if labeler, ok := adapter.(guildChannelLabeler); ok && channel != nil {
		if name, topic, ok := labeler.GuildChannelLabels(channel); ok {
			source.ChatName, source.ChatTopic = name, topic
		}
	}
	source.SetTransportAdapterRef(adapter)
	return source, nil
}

// First n runes of a string, the Python slice of a str
func runePrefix(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Dispatches one transcribed voice utterance through the adapter that captured it, after route, auth and duplicate gates
func (r *GatewayRunner) handleVoiceChannelInput(ctx context.Context, guildID, userID int64, transcript string, adapter gateway.PlatformAdapter) error {
	if adapter == nil {
		adapter, _ = r.adapters.Get(gateway.PlatformDiscord)
	}
	var textChannelID int64
	if binder, ok := adapter.(voiceTextChannelBinder); ok && adapter != nil {
		textChannelID, _ = binder.VoiceTextChannel(guildID)
	}
	if textChannelID == 0 {
		return nil
	}
	source, err := voiceInputSource(ctx, adapter, guildID, userID, textChannelID)
	if err != nil {
		return err
	}
	gateway.ClearIdentity(source)
	if r.canonicalize(ctx, source, adapter.Base().OwnerProfile(), "") == nil {
		gateway.DropRejectedRoute(ctx, adapter, source)
		return nil
	}
	if gate, ok := adapter.(roleAllowlistAdapter); ok && len(gate.AllowedRoleIDs()) > 0 {
		var guild any
		if cache, ok := adapter.(discordVoiceCache); ok {
			guild = cache.CachedGuild(guildID)
		}
		source.RoleAuthorized = guild != nil && gate.IsAllowedUser(fmt.Sprint(userID), guild, false) == true
	}
	authorized, err := r.isUserAuthorizedForSource(ctx, source, false)
	if err != nil {
		return err
	}
	if !authorized {
		runLog().Debug(fmt.Sprintf("Unauthorized voice input from user %d, ignoring", userID))
		return nil
	}
	binding := [3]any{ownerProfileValue(adapter), guildID, textChannelID}
	if r.isDuplicateVoiceTranscript(binding, userID, transcript) {
		runLog().Info(fmt.Sprintf("Suppressing duplicate voice transcript for guild=%d user=%d: %s", guildID, userID, runePrefix(transcript, 100)))
		return nil
	}
	if cache, ok := adapter.(discordVoiceCache); ok {
		func() {
			defer func() { _ = recover() }()
			if channel := cache.CachedChannel(textChannelID); channel != nil {
				safeText := strings.ReplaceAll(runePrefix(transcript, 2000), "@everyone", "@​everyone")
				safeText = strings.ReplaceAll(safeText, "@here", "@​here")
				_ = cache.SendToChannel(ctx, channel, i18n.T(ctx, "gateway.voice.transcript_echo", "", map[string]any{"user": userID, "text": safeText}))
			}
		}()
	}
	var channelPrompt *string
	var autoSkill any
	var parentID *string
	if source.ParentChatID != nil && *source.ParentChatID != "" {
		parentID = source.ParentChatID
	}
	channelID := fmt.Sprint(textChannelID)
	if resolver, ok := adapter.(channelPromptResolver); ok {
		func() {
			defer func() { _ = recover() }()
			if resolved, err := resolver.ResolveChannelPrompt(channelID, parentID); err == nil {
				if text, ok := resolved.(string); ok {
					channelPrompt = &text
				}
			}
		}()
	}
	if skills, ok := adapter.(channelSkillsResolver); ok {
		bound, err := skills.ResolveChannelSkills(channelID, parentID)
		if err != nil {
			return err
		}
		if list, ok := bound.([]any); ok {
			autoSkill = list
		}
	}
	event := gateway.NewMessageEvent(transcript)
	event.Source, event.MessageType, event.RawMessage = source, gateway.MessageTypeVoice, voiceInputRawMessage{guildID: guildID}
	event.ChannelPrompt, event.AutoSkill = channelPrompt, autoSkill
	return adapter.HandleMessage(ctx, event)
}
