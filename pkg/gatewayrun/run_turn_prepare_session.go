package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Text of an optional string as Python formats it, None when absent
func noneOrText(s *string) string {
	if s == nil {
		return "None"
	}
	return *s
}

// Python str of a metadata value stripped, empty for a falsy value
func metadataText(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	return strings.TrimSpace(noneText(value))
}

// Completion ids a routed internal event carries, empty when it names none
func metadataCompletionIDs(value any) []string {
	var ids []string
	switch items := value.(type) {
	case []string:
		ids = append(ids, items...)
	case []any:
		for _, item := range items {
			ids = append(ids, noneText(item))
		}
	}
	return ids
}

// Session entry of the source after topic recovery and internal route guards, nil when the event is dropped
func (r *GatewayRunner) hmwaResolveSession(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource) (*gateway.SessionSource, *gateway.SessionEntry, string, error) {
	metadata := event.Metadata
	expectedSessionKey := metadataText(metadata["gateway_session_key"])
	var recovered *string
	if expectedSessionKey == "" {
		recovered = r.recoverTelegramLobbyThreadID(ctx, source)
	}
	if recovered != nil {
		runLog().Info(fmt.Sprintf("telegram topic recovery: chat=%s user=%s %s -> %s",
			source.ChatID, noneOrText(source.UserID), pyrepr.Repr(optAny(source.ThreadID)), *recovered))
		copied := *source
		copied.ThreadID = recovered
		source = &copied
		event.Source = source
	}
	if expectedSessionKey != "" {
		derived, err := r.sessionKeyForSource(ctx, source)
		if err != nil {
			return nil, nil, "", err
		}
		if derived != expectedSessionKey {
			runLog().Warn(fmt.Sprintf("Dropping internally routed event after route recovery: expected session=%s derived=%s",
				expectedSessionKey, derived))
			return nil, nil, "", nil
		}
	}
	strictSession := pyrepr.Truthy(metadata["gateway_session_strict"])
	pinnedSessionID := metadataText(metadata["gateway_session_id"])
	var entry *gateway.SessionEntry
	if strictSession {
		entry = r.asyncStore().Store().LookupBySessionKey(ctx, expectedSessionKey)
		if entry == nil || pinnedSessionID == "" || entry.SessionID != pinnedSessionID {
			pinned, key := pinnedSessionID, expectedSessionKey
			if pinned == "" {
				pinned = "missing"
			}
			if key == "" {
				key = "missing"
			}
			runLog().Warn(fmt.Sprintf("Dropping internally routed event: expected session id=%s is no longer current for key=%s", pinned, key))
			return nil, nil, "", nil
		}
	} else {
		created, err := r.asyncStore().Store().GetOrCreateSession(ctx, source, false, !event.Internal)
		if err != nil {
			return nil, nil, "", err
		}
		entry = created
	}
	sessionKey := entry.SessionKey
	if owner, generation, claimed := event.ClaimedOwner(); claimed {
		authority, err := gateway.ActiveAuthority(ctx, r)
		if err != nil {
			return nil, nil, "", err
		}
		if authority == nil {
			return nil, nil, "", &gateway.PyError{Kind: "AttributeError", Msg: "'NoneType' object has no attribute 'pending_stops'"}
		}
		if authority.ClaimedTurnStopped(owner, generation) {
			chain, err := authority.DB.GetCompressionChain(ctx, owner)
			if err != nil {
				return nil, nil, "", err
			}
			moved := true
			for _, id := range chain {
				if id == entry.SessionID {
					moved = false
					break
				}
			}
			if moved {
				runLog().Warn(fmt.Sprintf("Dropping a stopped claimed turn: its route moved from %s to %s before it ran", owner, entry.SessionID))
				gateway.RecordInterruptedExecution(ctx)
				return nil, nil, "", nil
			}
		}
	}
	if !strictSession && pinnedSessionID != "" {
		resolved, err := r.resolveAsyncDelegationSession(ctx, entry, pinnedSessionID, metadataText(metadata["gateway_proven_session_id"]),
			metadataCompletionIDs(metadata["gateway_completion_ids"]))
		if err != nil {
			return nil, nil, "", err
		}
		if resolved == nil {
			return nil, nil, "", nil
		}
		entry = resolved
	}
	r.cacheSessionSource(sessionKey, source)
	if r.isTelegramTopicLane(ctx, source) {
		healed, err := r.hmwaHealTelegramTopicBinding(ctx, source, entry, sessionKey)
		if err != nil {
			return nil, nil, "", err
		}
		entry = healed
	}
	accepted, err := resolveHeartbeatOwner(ctx, r, event, entry)
	if err != nil {
		return nil, nil, "", err
	}
	if !accepted {
		return nil, nil, "", nil
	}
	return source, entry, sessionKey, nil
}

// Follows the topic binding healed to its compression tip, or records a fresh one, returning the session entry
func (r *GatewayRunner) hmwaHealTelegramTopicBinding(ctx context.Context, source *gateway.SessionSource, entry *gateway.SessionEntry, sessionKey string) (*gateway.SessionEntry, error) {
	db := r.sessionDB(ctx)
	var storedSessionID string
	bound := false
	if db != nil {
		binding, err := db.DB().GetTelegramTopicBinding(ctx, source.ChatID, optText(source.ThreadID), telegramTopicProfileName(source))
		if err != nil {
			runLog().Debug("Failed to read Telegram topic binding", "error", err)
		} else if binding != nil && binding.Len() > 0 {
			bound = true
			value, _ := binding.Get("session_id")
			if pyrepr.Truthy(value) {
				storedSessionID = noneText(value)
			}
		}
	}
	if !bound {
		if err := r.recordTelegramTopicBinding(ctx, source, entry); err != nil {
			runLog().Debug("Failed to record Telegram topic binding", "error", err)
		}
		return entry, nil
	}
	boundSessionID := storedSessionID
	if boundSessionID != "" && db != nil {
		canonical, err := db.DB().GetCompressionTip(ctx, boundSessionID)
		if err != nil {
			runLog().Debug(fmt.Sprintf("compression-tip lookup failed for %s", boundSessionID), "error", err)
			canonical = boundSessionID
		}
		if canonical != "" && canonical != boundSessionID {
			boundSessionID = canonical
		}
	}
	if boundSessionID != "" && boundSessionID != entry.SessionID {
		expected := entry.SessionID
		switched, err := r.asyncStore().Store().SwitchSession(ctx, sessionKey, boundSessionID, &expected, true)
		if err != nil {
			return nil, err
		}
		if switched != nil {
			entry = switched
		}
	}
	if boundSessionID != "" && boundSessionID != storedSessionID {
		r.syncTelegramTopicBinding(ctx, source, entry, "compression-tip-walk")
	}
	return entry, nil
}

// Consumes the auto reset and fresh reset flags and emits session start for a new session
func (r *GatewayRunner) hmwaOpenSession(ctx context.Context, entry *gateway.SessionEntry, sessionKey string, source *gateway.SessionSource) (bool, bool, error) {
	wasAutoReset := entry.WasAutoReset
	if wasAutoReset {
		r.clearConversationScope(ctx, sessionKey, "auto_reset")
		r.evictCachedAgent(ctx, sessionKey, false, false)
		entry.WasAutoReset = false
	}
	isFreshReset := entry.IsFreshReset
	sameStamp := entry.CreatedAt.Aware == entry.UpdatedAt.Aware && entry.CreatedAt.T.Equal(entry.UpdatedAt.T)
	isNewSession := sameStamp || wasAutoReset || isFreshReset
	if isFreshReset {
		entry.IsFreshReset = false
	}
	if isNewSession {
		if err := r.hooks.emit(ctx, "session:start", map[string]any{
			"platform": string(source.Platform), "user_id": optAny(source.UserID),
			"session_id": entry.SessionID, "session_key": sessionKey,
		}); err != nil {
			return wasAutoReset, isNewSession, err
		}
	}
	return wasAutoReset, isNewSession, nil
}
