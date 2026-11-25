package gateway

import (
	"context"
	"slices"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

var notedProducers = map[string]bool{"async_delegation": true, "completion": true, "watch_match": true, "watch_disabled": true}

var alwaysNotedProducers = map[string]bool{"cron": true}

// Runner that recounts a cached agent's messages after the owner wrote its row
type agentCacheRefresher interface {
	RefreshAgentCacheMessageCount(ctx context.Context, route, target string) error
}

// Producer kind an automation identity names, empty for anything that is no identity list
func completionProducer(identity any) string {
	text, ok := identity.(string)
	if !ok {
		return ""
	}
	decoded, err := jsonutil.Decode(text)
	if err != nil {
		return ""
	}
	switch list := decoded.(type) {
	case []any:
		if len(list) == 0 {
			return ""
		}
		kind, _ := list[0].(string)
		return kind
	case string:
		if list == "" {
			return ""
		}
		return string([]rune(list)[:1])
	}
	return ""
}

// Local automation descriptor of a row, nil for any other row
func completionDescriptor(row *jsonutil.Object) *jsonutil.Object {
	if row == nil {
		return nil
	}
	value, _ := rowPayload(row).Get("local_automation_v1")
	descriptor, _ := value.(*jsonutil.Object)
	return descriptor
}

// True for a local completion admission that is a note when it runs with no viewer attached
func notedCompletion(row *jsonutil.Object) bool {
	descriptor := completionDescriptor(row)
	if descriptor == nil {
		return false
	}
	identity, _ := descriptor.Get("identity")
	producer := completionProducer(identity)
	return notedProducers[producer] || alwaysNotedProducers[producer]
}

// Makes a finalized row live again for a turn, a detached conversation's row or a local session's receipt tip
func reopenForTurn(ctx context.Context, a *SessionAuthority, ref SessionRef) error {
	live, ok := a.live(ref.SessionID)
	reopen := reopenLocalSession
	if ok && liveDetached(live) {
		reopen = reopenAPISession
	}
	_, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		return nil, reopen(taskCtx, a, ref)
	}, nil, false, nil)
	return err
}

// Records an unviewed completion in the session's history without a model call, false when it runs as a turn
func noteIfUnviewed(ctx context.Context, a *SessionAuthority, ref SessionRef, row *jsonutil.Object) (bool, error) {
	if !notedCompletion(row) {
		return false, nil
	}
	live, ok := a.live(ref.SessionID)
	descriptor := completionDescriptor(row)
	identity, _ := descriptor.Get("identity")
	if !ok || (hasViewers(a, ref.SessionID) && !alwaysNotedProducers[completionProducer(identity)]) {
		return false, reopenForTurn(ctx, a, ref)
	}
	names := []string{anyutil.String(identity)}
	if identities, _ := descriptor.Get("identities"); anyutil.IsTrue(identities) {
		if list, isList := identities.([]any); isList {
			for _, item := range list {
				names = append(names, anyutil.String(item))
			}
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	noted := make([]any, len(names))
	for i, name := range names {
		noted[i] = name
	}
	metadata := jsonutil.ObjectOf("completion_note", noted)
	for _, key := range []string{"display_text", "notification_category"} {
		if value, has := descriptor.Get(key); has {
			metadata.Set(key, value)
		}
	}
	target, err := viewerPhysicalTarget(ctx, a, ref.SessionID)
	if err != nil {
		return false, err
	}
	kind := "internal_notification"
	if value, has := descriptor.Get("display_kind"); has {
		kind = anyutil.String(value)
	}
	text, _ := rowPayload(row).Get("text")
	if _, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		return a.DB.AppendCompletionNote(taskCtx, target, rowString(row, "admission_id"), anyutil.String(text), kind, metadata)
	}, nil, false, nil); err != nil {
		return false, err
	}
	if refresher, canRefresh := a.Runner.(agentCacheRefresher); canRefresh {
		a.mu.Lock()
		route := live.Route
		a.mu.Unlock()
		if err := refresher.RefreshAgentCacheMessageCount(ctx, route, target); err != nil {
			return false, err
		}
	}
	return true, nil
}
