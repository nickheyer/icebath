package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Read only preflight of a queued automation row, its owner, route and adapter checked, returning its source and route
func checkAutomationRoute(ctx context.Context, runner AuthorityRunner, payload *jsonutil.Object, sessionID string, availableSource *SessionSource, adapter PlatformAdapter) (*SessionSource, string, error) {
	event, err := restoreNative(ctx, payload, runner)
	if err != nil {
		return nil, "", err
	}
	envelope, err := nativeEnvelope(payload)
	if err != nil {
		return nil, "", err
	}
	authority, err := activeAuthority(ctx, runner)
	if err != nil {
		return nil, "", err
	}
	if authority == nil {
		return nil, "", runtimeRefusal("not_found")
	}
	entry, err := automationOwner(ctx, runner, event, authority)
	if err != nil {
		return nil, "", err
	}
	if entry.SessionID != sessionID {
		return nil, "", runtimeRefusal("admission_conflict")
	}
	rawAutomation, found := envelope.Get("automation")
	if !found {
		return nil, "", &keyError{"automation"}
	}
	automation, ok := rawAutomation.(*jsonutil.Object)
	if !ok {
		return nil, "", &errorutil.Error{Kind: "TypeError", Msg: "the stored automation is " + errchain.Kind(rawAutomation) + ", expected a mapping"}
	}
	owner, found := automation.Get("owner")
	if !found {
		return nil, "", &keyError{"owner"}
	}
	logical, err := authority.LogicalOwner(ctx, sessionID)
	if err != nil {
		return nil, "", err
	}
	if !anyutil.Equal(owner, logical) {
		return nil, "", runtimeRefusal("admission_conflict")
	}
	key, err := runner.SessionStore().generateSessionKey(ctx, availableSource, nil)
	if err != nil {
		return nil, "", err
	}
	if key != entry.SessionKey || adapter == nil {
		return nil, "", runtimeRefusal("admission_conflict")
	}
	if owner, err := runner.AdapterForSource(ctx, event.Source); err != nil {
		return nil, "", err
	} else if owner != adapter {
		return nil, "", runtimeRefusal("admission_conflict")
	}
	return event.Source, entry.SessionKey, nil
}

// Internal event of a committed local automation row on its live session's current route
func restoreLocalAutomation(ctx context.Context, a *SessionAuthority, ref SessionRef, row *jsonutil.Object) (*MessageEvent, error) {
	if err := checkLocalAutomation(ctx, a, ref, row, ""); err != nil {
		return nil, err
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, &keyError{ref.SessionID}
	}
	payloadValue, _ := row.Get("payload")
	payload := payloadValue.(*jsonutil.Object)
	descriptorValue, _ := payload.Get("local_automation_v1")
	descriptor := descriptorValue.(*jsonutil.Object)
	entry, err := routeEntry(ctx, a, live.Route)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, &errorutil.Error{Kind: "AttributeError", Msg: "the automation has no session to restore into"}
	}
	text, _ := payload.Get("text")
	identity, _ := descriptor.Get("identity")
	messageID := anyutil.String(identity)
	event := NewMessageEvent(anyutil.String(text))
	event.Source, event.Internal, event.MessageID = live.Source, true, &messageID
	event.Metadata = map[string]any{"gateway_session_key": live.Route, "gateway_session_id": entry.SessionID}
	get := func(key string) (any, bool) { return descriptor.Get(key) }
	for _, build := range []func(func(string) (any, bool)) (*jsonutil.Object, error){automationNotificationMetadata, AutomationDisplayMetadata} {
		part, err := build(get)
		if err != nil {
			return nil, err
		}
		for _, key := range part.Keys() {
			event.Metadata[key], _ = part.Get(key)
		}
	}
	if author, _ := descriptor.Get("turn_author"); author != nil {
		event.Metadata["turn_author"] = any(anyutil.Clone(author))
	}
	if heartbeat, _ := descriptor.Get("heartbeat"); anyutil.IsTrue(heartbeat) {
		id := anyutil.String(heartbeat)
		event.SetHeartbeatSessionID(&id)
	}
	return event, nil
}
