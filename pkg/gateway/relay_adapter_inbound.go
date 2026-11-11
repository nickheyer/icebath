package gateway

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const relaySeenInboundMax = 512

// Bridges a connector delivered event into the adapter path, an admission when it commits off the reader
func (a *RelayAdapter) onInbound(ctx context.Context, event *MessageEvent) (RelayAdmission, error) {
	runner := handlerOwner(a)
	holder, isHolder := runner.(launchAuthorityRunner)
	command, _ := event.GetCommand()
	native := isHolder && holder != nil && holder.SessionAuthority() != nil && command == ""
	dedupeKey, hasKey := "", false
	if !native {
		dedupeKey, hasKey = relayInboundDedupeKey(event)
	}
	if hasKey {
		a.mu.Lock()
		if a.seenInbound.has(dedupeKey) {
			a.mu.Unlock()
			relayAdapterLog().Info(fmt.Sprintf("relay inbound dropped as replay (dedupe key=%s)", dedupeKey))
			return nil, nil
		}
		a.seenInbound.add(dedupeKey)
		a.seenInbound.evict(relaySeenInboundMax)
		a.mu.Unlock()
	}
	consumed, err := a.inboundPrelude(ctx, event)
	if err != nil {
		if hasKey && (errors.Is(err, context.Canceled) || ctx.Err() != nil) {
			a.mu.Lock()
			a.seenInbound.remove(dedupeKey)
			a.mu.Unlock()
		}
		return nil, err
	}
	if consumed {
		return nil, nil
	}
	if native {
		event.Timestamp = time.Unix(0, 0).UTC()
		return admitRelayDelivery(ctx, a, runner, event)
	}
	return nil, a.HandleMessage(ctx, event)
}

// Scope capture, label memory, Slack thread stamp, prompt answer consumption and media localization of an inbound
func (a *RelayAdapter) inboundPrelude(ctx context.Context, event *MessageEvent) (bool, error) {
	a.captureIngressScope(event)
	if err := a.rememberDiscordLabels(ctx, event.Source); err != nil {
		return false, err
	}
	a.stampSlackSessionThread(event)
	consumed, err := a.consumePromptResponse(ctx, event)
	if err != nil || consumed {
		return consumed, err
	}
	return false, a.localizeInboundMedia(ctx, event)
}

// Replay identity of platform, chat and platform message id, false without a message id or chat
func relayInboundDedupeKey(event *MessageEvent) (string, bool) {
	source := event.Source
	if source == nil || event.MessageID == nil || *event.MessageID == "" || source.ChatID == "" {
		return "", false
	}
	return fmt.Sprintf("%s:%s:%s", string(source.Platform), source.ChatID, *event.MessageID), true
}

// Keys each top level fronted Slack message to a fresh session in thread per message mode
func (a *RelayAdapter) stampSlackSessionThread(event *MessageEvent) {
	src := event.Source
	if src == nil || string(src.Platform) != relaySlack {
		return
	}
	if src.ThreadID != nil && *src.ThreadID != "" {
		return
	}
	messageID, _ := relayEventIDs(event)
	if messageID == nil || *messageID == "" || !a.effectiveReplyInThread() {
		return
	}
	if !a.dmTopLevelThreadsAsSessions() {
		return
	}
	id := *messageID
	src.ThreadID = &id
}

// Downloads connector rehosted attachments to local paths, dropping dead rehost URLs and keeping public ones
func (a *RelayAdapter) localizeInboundMedia(ctx context.Context, event *MessageEvent) error {
	urls := event.MediaURLs
	if len(urls) == 0 {
		return nil
	}
	types := append(slices.Clone(event.MediaTypes), make([]string, len(urls))...)
	names := append(slices.Clone(event.MediaFilenames), make([]string, len(urls))...)
	client := a.getMediaClient(ctx)
	localURLs, localTypes, localNames := []string{}, []string{}, []string{}
	keep := func(url, mime, name string) {
		localURLs, localTypes, localNames = append(localURLs, url), append(localTypes, mime), append(localNames, name)
	}
	for i, url := range urls {
		mime, name := types[i], names[i]
		if url == "" {
			continue
		}
		if client == nil {
			if !strings.Contains(url, "/relay/media/") {
				keep(url, mime, name)
			}
			continue
		}
		var suggested *string
		if name != "" {
			suggested = &name
		}
		path, gotName, ok, err := client.DownloadWithName(ctx, url, suggested)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			relayAdapterLog().Debug("relay inbound media localization failed", "exc_info", err)
			return nil
		}
		if ok {
			keep(path, mime, gotName)
		} else if !strings.Contains(url, "/relay/media/") {
			keep(url, mime, name)
		}
	}
	event.MediaURLs, event.MediaTypes, event.MediaFilenames = localURLs, localTypes, localNames
	return nil
}

// Warms the per chat egress routing caches from a synthetic event
func (a *RelayAdapter) PrimeRoutingCache(event *MessageEvent) {
	if event == nil || event.Source == nil {
		return
	}
	a.captureScope(event)
}

// Remembers a chat's platform, author, scope, chat type, profile and latest message ts from an inbound event
func (a *RelayAdapter) captureScope(event *MessageEvent) {
	src := event.Source
	if src == nil || src.ChatID == "" {
		return
	}
	chat := src.ChatID
	messageID, _ := relayEventIDs(event)
	a.mu.Lock()
	defer a.mu.Unlock()
	if p := string(src.Platform); p != "" && p != string(PlatformRelay) {
		a.platformByChat[chat] = p
	}
	for _, field := range []struct {
		value *string
		cache map[string]string
	}{{src.UserID, a.dmUserByChat}, {src.ScopeID, a.scopeByChat}, {&src.ChatType, a.chatTypeByChat}, {src.Profile, a.profileByChat}} {
		if field.value != nil && *field.value != "" {
			field.cache[chat] = *field.value
		}
	}
	if messageID != nil && *messageID != "" {
		a.lastInboundTSByChat[chat] = *messageID
	}
}

// Outbound metadata carrying the chat's scope, author and profile discriminators when known and unset
func (a *RelayAdapter) withScope(chatID string, metadata map[string]any) map[string]any {
	meta := maps.Clone(metadata)
	if meta == nil {
		meta = map[string]any{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, field := range []struct {
		key   string
		cache map[string]string
	}{{"scope_id", a.scopeByChat}, {"user_id", a.dmUserByChat}, {"profile", a.profileByChat}} {
		if !pyrepr.Truthy(meta[field.key]) {
			if v := field.cache[chatID]; v != "" {
				meta[field.key] = v
			}
		}
	}
	return meta
}

// Whether the transport advertised the platform among its handshake identities
func (a *RelayAdapter) FrontsPlatform(platform Platform) bool {
	value := string(platform)
	if value == "" {
		return false
	}
	lister, ok := a.transport.(interface{ RelayIdentities() any })
	if !ok {
		return false
	}
	ids, _ := lister.RelayIdentities().([]any)
	for _, id := range ids {
		if pair, ok := id.([]any); ok && len(pair) == 2 && pyrepr.Str(pair[0]) == value {
			return true
		}
	}
	return false
}

// Whether one fronted platform hosts the flat continuable cron surface, the primary bit when unnegotiated
func (a *RelayAdapter) SupportsInchannelContinuableForPlatform(platform string) (bool, error) {
	if d := a.negotiatedDescriptor(platform); d != nil {
		return d.SupportsInchannelContinuable, nil
	}
	return a.Traits.SupportsInchannelContinuable, nil
}

// Bridges a connector delivered stop into the per session interrupt path
func (a *RelayAdapter) OnInterrupt(ctx context.Context, sessionKey, chatID string) error {
	a.InterruptSessionActivity(ctx, sessionKey, chatID, nil)
	return nil
}
