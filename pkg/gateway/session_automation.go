package gateway

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Longest card text a producer may attach to its admission
const automationDisplayTextLimit = 2000

// Text a folded completion row keeps once a later batch carries its content
const automationFoldedText = "[folded into a later completion batch]"

// End reasons of a session closed at a user boundary
var userBoundaryEndReasons = map[string]bool{"session_reset": true, "user_exit": true, "session_switch": true, "new_session": true}

var automationMetadataKeys = map[string]bool{"gateway_session_key": true, "gateway_session_id": true, "automation_identities": true,
	"turn_author": true, "notification_origin": true, "original_trigger_message_id": true, "notification_category": true,
	"display_kind": true, "display_text": true, "gateway_proven_session_id": true, "gateway_completion_ids": true, "heartbeat_tick": true}

// Adapter that vouches for a local source it registered
type sourceAuthorizer interface {
	authorizeSource(source *SessionSource) bool
}

// Automation descriptor of a payload, empty for every other input
func automationDescriptor(payload *pyjson.Object) *pyjson.Object {
	if local, _ := payload.Get("local_automation_v1"); pyTruthy(local) {
		if obj, ok := local.(*pyjson.Object); ok {
			return obj
		}
	}
	if native, _ := payload.Get("native_text_v1"); pyTruthy(native) {
		if envelope, ok := native.(*pyjson.Object); ok {
			if automation, _ := envelope.Get("automation"); pyTruthy(automation) {
				if obj, ok := automation.(*pyjson.Object); ok {
					return obj
				}
			}
		}
	}
	return pyjson.NewObject()
}

// Stored output of a liveness tick a later tick folds in, refused unless it is exactly output and omitted
func tickMetadata(metadata map[string]any) (*pyjson.Object, error) {
	out := pyjson.NewObject()
	tick, ok := metadata["heartbeat_tick"]
	if !ok || tick == nil {
		return out, nil
	}
	var output, omitted any
	var count int
	switch t := tick.(type) {
	case map[string]any:
		output, omitted, count = t["output"], t["omitted"], len(t)
		if _, has := t["output"]; !has {
			count = -1
		}
		if _, has := t["omitted"]; !has {
			count = -1
		}
	case *pyjson.Object:
		var hasOutput, hasOmitted bool
		output, hasOutput = t.Get("output")
		omitted, hasOmitted = t.Get("omitted")
		count = t.Len()
		if !hasOutput || !hasOmitted {
			count = -1
		}
	default:
		return nil, runtimeRefusal("invalid_params")
	}
	text, isText := output.(string)
	var n int64
	switch v := omitted.(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	default:
		return nil, runtimeRefusal("invalid_params")
	}
	if count != 2 || !isText || n < 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	if ordered, ok := tick.(*pyjson.Object); ok {
		out.Set("tick", ordered.Copy())
		return out, nil
	}
	out.Set("tick", pyjson.ObjectOf("output", text, "omitted", omitted))
	return out, nil
}

// Write guard cancelling the replaced queued tick in the replacement's own transaction
func replaceQueuedGuard(admissionID, sessionID string, replaced *[]string) func(ctx context.Context, conn state.SQLiteConn) error {
	return func(ctx context.Context, conn state.SQLiteConn) error {
		result, err := conn.ExecContext(ctx, "UPDATE session_admissions SET status='terminal',outcome='cancelled' "+
			"WHERE admission_id=? AND target_session_id=? AND status='queued'", admissionID, sessionID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return runtimeRefusal("stale_generation")
		}
		*replaced = []string{admissionID}
		return nil
	}
}

// Write guard folding a queued completion that is still the FIFO tail into the batch replacing it
func foldQueuedGuard(admissionID, sessionID string, replaced *[]string, skip []string) func(ctx context.Context, conn state.SQLiteConn) error {
	replace := replaceQueuedGuard(admissionID, sessionID, replaced)
	return func(ctx context.Context, conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT seq, payload_json FROM session_admissions WHERE admission_id=? "+
			"AND target_session_id=? AND status='queued'", admissionID, sessionID)
		if err != nil {
			return err
		}
		var seq int64
		var encoded string
		found := rows.Next()
		if found {
			if err := rows.Scan(&seq, &encoded); err != nil {
				rows.Close()
				return err
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if !found {
			return runtimeRefusal("stale_generation")
		}
		later, err := conn.QueryContext(ctx, "SELECT admission_id FROM session_admissions WHERE target_session_id=? "+
			"AND status='queued' AND seq>?", sessionID, seq)
		if err != nil {
			return err
		}
		behind := false
		for later.Next() {
			var other string
			if err := later.Scan(&other); err != nil {
				later.Close()
				return err
			}
			if !slices.Contains(skip, other) {
				behind = true
			}
		}
		if err := later.Close(); err != nil {
			return err
		}
		if behind {
			return runtimeRefusal("stale_generation")
		}
		if err := replace(ctx, conn); err != nil {
			return err
		}
		decoded, err := pyjson.Loads(encoded)
		if err != nil {
			return err
		}
		payload, ok := decoded.(*pyjson.Object)
		if !ok {
			return fmt.Errorf("'%s' object does not support item assignment", pyrepr.TypeName(decoded))
		}
		payload.Set("text", automationFoldedText)
		automationDescriptor(payload).Delete("identities")
		text, err := pyjson.Dumps(payload, pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, "UPDATE session_admissions SET payload_json=? WHERE admission_id=?", text, admissionID)
		return err
	}
}

// Whether the session or a delegate conversation it runs under is the route's live session or compressed into it
func ownedBy(ctx context.Context, db *state.SessionDB, sessionID, current string) (bool, error) {
	lineage := []string{sessionID}
	for {
		last := lineage[len(lineage)-1]
		if last == current {
			return true, nil
		}
		tip, err := db.GetCompressionTip(ctx, last)
		if err != nil {
			return false, err
		}
		if tip == current {
			return true, nil
		}
		row, err := db.GetSession(ctx, last)
		if err != nil {
			return false, err
		}
		parent, err := db.GetSessionModelConfigValue(ctx, last, "_delegate_from", nil)
		if err != nil {
			return false, err
		}
		if row != nil {
			ended, _ := row.Get("ended_at")
			reason, _ := row.Get("end_reason")
			if text, _ := reason.(string); pyTruthy(ended) && userBoundaryEndReasons[text] {
				return false, nil
			}
		}
		next, isText := parent.(string)
		if parent == nil || !isText || slices.Contains(lineage, next) {
			return false, nil
		}
		lineage = append(lineage, next)
	}
}

// Routing entry an automation event's route names, refused unless the event still owns that route's session
func automationOwner(ctx context.Context, runner AuthorityRunner, event *MessageEvent, a *SessionAuthority) (*SessionEntry, error) {
	route, _ := event.Metadata["gateway_session_key"].(string)
	if route == "" {
		generated, err := runner.SessionStore().generateSessionKey(ctx, event.Source, nil)
		if err != nil {
			return nil, err
		}
		route = generated
	}
	entry, err := routeEntry(ctx, a, route)
	if err != nil {
		return nil, err
	}
	if entry == nil || entry.Suspended {
		return nil, runtimeRefusal("not_found")
	}
	generated, err := runner.SessionStore().generateSessionKey(ctx, event.Source, nil)
	if err != nil {
		return nil, err
	}
	if generated != route {
		return nil, runtimeRefusal("admission_conflict")
	}
	expected, _ := event.Metadata["gateway_session_id"].(string)
	if expected == "" {
		return nil, runtimeRefusal("admission_conflict")
	}
	owned, err := ownedBy(ctx, a.DB, expected, entry.SessionID)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, runtimeRefusal("admission_conflict")
	}
	return entry, nil
}

// Trusted producer category of a notification, the default left out of old fingerprints
func automationNotificationMetadata(get func(key string) (any, bool)) (*pyjson.Object, error) {
	category, ok := get("notification_category")
	if !ok {
		category = "result"
	}
	if category != "result" && category != "diagnostic" {
		return nil, runtimeRefusal("invalid_params")
	}
	out := pyjson.NewObject()
	if category == "diagnostic" {
		out.Set("notification_category", category)
	}
	return out, nil
}

// Card a producer named for its admission, both keys or neither with a kind from the closed producer set
func automationDisplayMetadata(get func(key string) (any, bool)) (*pyjson.Object, error) {
	kind, hasKind := get("display_kind")
	text, hasText := get("display_text")
	out := pyjson.NewObject()
	if !hasKind && !hasText {
		return out, nil
	}
	kindText, isKind := kind.(string)
	body, isBody := text.(string)
	if !isKind || !ProducerCardKinds[kindText] || !isBody || pystr.Strip(body) == "" || pystr.Len(body) > automationDisplayTextLimit {
		return nil, runtimeRefusal("invalid_params")
	}
	out.Set("display_kind", kindText)
	out.Set("display_text", body)
	return out, nil
}

// Lookup over an event's metadata mapping
func metadataGetter(metadata map[string]any) func(key string) (any, bool) {
	return func(key string) (any, bool) {
		value, ok := metadata[key]
		return value, ok
	}
}

// Route's last committed native envelope carrying connector provenance, nil while no human message was admitted
func committedRouteEnvelope(ctx context.Context, a *SessionAuthority, entry *SessionEntry) (*pyjson.Object, error) {
	owner, err := a.LogicalOwner(ctx, entry.SessionID)
	if err != nil {
		return nil, err
	}
	prior, err := a.DB.ListSessionAdmissions(ctx, owner, false)
	if err != nil {
		return nil, err
	}
	for i := len(prior) - 1; i >= 0; i-- {
		payloadValue, _ := prior[i].Get("payload")
		payload, _ := payloadValue.(*pyjson.Object)
		if payload == nil {
			continue
		}
		native, ok := payload.Get("native_text_v1")
		if !ok {
			continue
		}
		envelope, _ := native.(*pyjson.Object)
		if envelope == nil {
			return nil, nil
		}
		if _, ok := envelope.Get("provenance"); !ok {
			return nil, nil
		}
		return envelope, nil
	}
	return nil, nil
}

// Sorted distinct identities an automation event carries, nil when it names none
func automationIdentities(metadata map[string]any) []any {
	var names []string
	switch list := metadata["automation_identities"].(type) {
	case []any:
		for _, item := range list {
			names = append(names, pyrepr.Str(item))
		}
	case []string:
		names = append(names, list...)
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	names = slices.Compact(names)
	out := make([]any, len(names))
	for i, name := range names {
		out[i] = name
	}
	return out
}

// Envelope without its retained media, what a producer wake borrows from the committed human turn
func envelopeWithoutMedia(text string, envelope *pyjson.Object) *pyjson.Object {
	borrowed := pyjson.NewObject()
	for _, key := range envelope.Keys {
		if key != "media" {
			value, _ := envelope.Get(key)
			borrowed.Set(key, value)
		}
	}
	return pyjson.ObjectOf("text", text, "native_text_v1", borrowed)
}

// Payload and routing entry of a producer's automation input, bound to the route's committed provenance
func snapshotAutomation(ctx context.Context, a *SessionAuthority, adapter PlatformAdapter, event *MessageEvent, identity string) (*pyjson.Object, *SessionEntry, error) {
	runner := a.Runner
	if !event.Internal || event.MessageType != MessageTypeText || event.IsCommand() || identity == "" ||
		len(event.MediaURLs) > 0 || len(event.PromptResponse) > 0 || event.Source.Platform == PlatformAPIServer {
		return nil, nil, runtimeRefusal("invalid_params")
	}
	for key := range event.Metadata {
		if !automationMetadataKeys[key] {
			return nil, nil, runtimeRefusal("invalid_params")
		}
	}
	notification := pyjson.NewObject()
	for _, build := range []func() (*pyjson.Object, error){
		func() (*pyjson.Object, error) { return automationNotificationMetadata(metadataGetter(event.Metadata)) },
		func() (*pyjson.Object, error) { return automationDisplayMetadata(metadataGetter(event.Metadata)) },
		func() (*pyjson.Object, error) { return tickMetadata(event.Metadata) },
	} {
		part, err := build()
		if err != nil {
			return nil, nil, err
		}
		notification.Update(part)
	}
	entry, err := automationOwner(ctx, runner, event, a)
	if err != nil {
		return nil, nil, err
	}
	if event.Source.Platform == PlatformLocal || detachedRouteOf(event.Source) != nil {
		return snapshotLocalAutomation(ctx, a, adapter, event, identity, entry)
	}
	owner, err := a.LogicalOwner(ctx, entry.SessionID)
	if err != nil {
		return nil, nil, err
	}
	envelope, err := committedRouteEnvelope(ctx, a, entry)
	if err != nil {
		return nil, nil, err
	}
	if envelope == nil {
		return nil, nil, runtimeRefusal("not_found")
	}
	restored, err := restoreNative(ctx, envelopeWithoutMedia(event.Text, envelope), runner)
	if err != nil {
		return nil, nil, err
	}
	restoredKey, err := runner.SessionStore().generateSessionKey(ctx, restored.Source, nil)
	if err != nil {
		return nil, nil, err
	}
	if restoredKey != entry.SessionKey || runner.AdapterForSource(ctx, restored.Source) != adapter {
		return nil, nil, runtimeRefusal("admission_conflict")
	}
	provenanceValue, _ := envelope.Get("provenance")
	provenance, _ := pyjson.Clone(provenanceValue).(*pyjson.Object)
	sourceValue, _ := envelope.Get("source")
	source, _ := pyjson.Clone(sourceValue).(*pyjson.Object)
	if source == nil {
		return nil, nil, &pyKeyError{"source"}
	}
	if provenance == nil {
		return nil, nil, &pyKeyError{"provenance"}
	}
	source.Delete("message_id")
	if _, relayed := provenance.Get("relay"); relayed {
		if provenance, err = resealRelay(ctx, adapter, source, provenance); err != nil {
			return nil, nil, err
		}
	}
	automation := pyjson.ObjectOf("identity", identity, "owner", owner)
	automation.Update(notification)
	if heartbeat := event.HeartbeatSessionID(); heartbeat != nil && *heartbeat != "" {
		automation.Set("heartbeat", *heartbeat)
	}
	if identities := automationIdentities(event.Metadata); identities != nil {
		automation.Set("identities", identities)
	}
	committed := pyjson.ObjectOf("source", source, "route", entry.SessionKey, "timestamp", "1970-01-01T00:00:00+00:00",
		"event", pyjson.ObjectOf("message_id", identity), "provenance", provenance, "automation", automation)
	return pyjson.ObjectOf("text", event.Text, "native_text_v1", committed), entry, nil
}

// Session and live entry a local automation lands on, refused unless its adapter vouches for the live source
func localAutomationOwner(ctx context.Context, a *SessionAuthority, adapter PlatformAdapter, event *MessageEvent, entry *SessionEntry) (SessionRef, *LiveSession, error) {
	authorizer, canAuthorize := adapter.(sourceAuthorizer)
	if detachedRouteOf(event.Source) != nil {
		ref, live, found := detachedLive(a, entry.SessionKey)
		if found && live.Source == event.Source && !canAuthorize {
			return SessionRef{}, nil, fmt.Errorf("'%s' object has no attribute 'authorize_source'", pyrepr.TypeName(adapter))
		}
		if !found || live.Source != event.Source || !authorizer.authorizeSource(event.Source) ||
			a.Runner.AdapterForSource(ctx, live.Source) != adapter {
			return SessionRef{}, nil, runtimeRefusal("permission_denied")
		}
		return ref, live, nil
	}
	ref, err := restoreLocalSession(ctx, a, event.Source.ChatID)
	if err != nil {
		return SessionRef{}, nil, err
	}
	live, _ := a.live(ref.SessionID)
	if live != nil && live.Source == event.Source && !canAuthorize {
		return SessionRef{}, nil, fmt.Errorf("'%s' object has no attribute 'authorize_source'", pyrepr.TypeName(adapter))
	}
	if live == nil || live.Source != event.Source || !authorizer.authorizeSource(event.Source) ||
		a.Runner.AdapterForSource(ctx, live.Source) != adapter {
		return SessionRef{}, nil, runtimeRefusal("permission_denied")
	}
	return ref, live, nil
}

// Payload and routing entry of an automation input a local session or a detached conversation takes
func snapshotLocalAutomation(ctx context.Context, a *SessionAuthority, adapter PlatformAdapter, event *MessageEvent, identity string, entry *SessionEntry) (*pyjson.Object, *SessionEntry, error) {
	ref, _, err := localAutomationOwner(ctx, a, adapter, event, entry)
	if err != nil {
		return nil, nil, err
	}
	descriptor := pyjson.ObjectOf("identity", identity, "owner", ref.SessionID, "route", entry.SessionKey, "target", entry.SessionID)
	for _, build := range []func() (*pyjson.Object, error){
		func() (*pyjson.Object, error) { return automationNotificationMetadata(metadataGetter(event.Metadata)) },
		func() (*pyjson.Object, error) { return automationDisplayMetadata(metadataGetter(event.Metadata)) },
		func() (*pyjson.Object, error) { return tickMetadata(event.Metadata) },
	} {
		part, err := build()
		if err != nil {
			return nil, nil, err
		}
		descriptor.Update(part)
	}
	if author := event.Metadata["turn_author"]; author != nil {
		parsed := provider.ParseTurnAuthor(author)
		if parsed == nil {
			return nil, nil, runtimeRefusal("invalid_params")
		}
		descriptor.Set("turn_author", parsed)
	}
	if identities := automationIdentities(event.Metadata); identities != nil {
		descriptor.Set("identities", identities)
	}
	if heartbeat := event.HeartbeatSessionID(); heartbeat != nil && *heartbeat != "" {
		descriptor.Set("heartbeat", *heartbeat)
	}
	return pyjson.ObjectOf("text", event.Text, "local_automation_v1", descriptor), entry, nil
}

// Refuses a queued local automation row whose owner, route, identity or target no longer match its session
func checkLocalAutomation(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object, route string) error {
	live, _ := a.live(ref.SessionID)
	if route == "" && live != nil {
		route = live.Route
	}
	payloadValue, _ := row.Get("payload")
	payload, _ := payloadValue.(*pyjson.Object)
	if payload == nil {
		return &pyKeyError{"local_automation_v1"}
	}
	descriptorValue, ok := payload.Get("local_automation_v1")
	if !ok {
		return &pyKeyError{"local_automation_v1"}
	}
	descriptor, _ := descriptorValue.(*pyjson.Object)
	if descriptor == nil {
		return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(descriptorValue))
	}
	descriptorRoute, _ := descriptor.Get("route")
	admitted := route
	if ownDetachedKey(descriptorRoute, ref.SessionID) {
		admitted = descriptorRoute.(string)
	}
	_, hasText := payload.Get("text")
	if route == "" || payload.Len() != 2 || !hasText || rowString(row, "principal_id") != "automation:"+admitted {
		return runtimeRefusal("permission_denied")
	}
	for _, check := range []struct{ key, want string }{{"owner", ref.SessionID}, {"route", admitted}, {"identity", rowString(row, "request_id")}} {
		value, ok := descriptor.Get(check.key)
		if !ok {
			return &pyKeyError{check.key}
		}
		if value != check.want {
			return runtimeRefusal("permission_denied")
		}
	}
	entry, err := routeEntry(ctx, a, route)
	if err != nil {
		return err
	}
	if entry == nil || entry.Suspended {
		return runtimeRefusal("admission_conflict")
	}
	target, ok := descriptor.Get("target")
	if !ok {
		return &pyKeyError{"target"}
	}
	if entry.SessionID != target {
		tip, err := a.DB.GetCompressionTip(ctx, pyrepr.Str(target))
		if err != nil {
			return err
		}
		if tip != entry.SessionID {
			return runtimeRefusal("admission_conflict")
		}
	}
	return nil
}

// Commits an automation input into its owner's FIFO, a superseded queued row cancelled in the same write
func admitAutomation(ctx context.Context, a *SessionAuthority, adapter PlatformAdapter, event *MessageEvent, identity string, replaces *string, fold []string) (AdmissionReceipt, error) {
	if err := a.requireAdmissionOpen(false); err != nil {
		return AdmissionReceipt{}, err
	}
	payload, entry, err := snapshotAutomation(ctx, a, adapter, event, identity)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	sid := rowString(automationDescriptor(payload), "owner")
	ref := SessionRef{a.ProfileID, sid}
	source := event.Source
	if native, ok := payload.Get("native_text_v1"); ok && native != nil {
		provenance, _ := native.(*pyjson.Object).Get("provenance")
		if obj, isObj := provenance.(*pyjson.Object); isObj {
			if _, relayed := obj.Get("relay"); relayed {
				if source, err = restoreNativeSource(ctx, payload, a.Runner); err != nil {
					return AdmissionReceipt{}, err
				}
			}
		}
	}
	a.mu.Lock()
	if _, ok := a.Sessions[ref.SessionID]; !ok {
		a.Sessions[ref.SessionID] = NewLiveSession(source, entry.SessionKey)
	}
	a.mu.Unlock()
	var replaced []string
	var authorize func(ctx context.Context, conn state.SQLiteConn) error
	if replaces != nil && *replaces != "" {
		if fold == nil {
			authorize = replaceQueuedGuard(*replaces, ref.SessionID, &replaced)
		} else {
			authorize = foldQueuedGuard(*replaces, ref.SessionID, &replaced, fold)
		}
	}
	committed, _ := pyjson.Clone(payload).(*pyjson.Object)
	var scheduleErr error
	result, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		return a.DB.AdmitSessionInput(taskCtx, state.SessionInput{Epoch: a.Epoch, PrincipalID: "automation:" + entry.SessionKey,
			SessionID: ref.SessionID, RequestID: identity, Payload: committed, AuthorizeWrite: a.admissionGate(authorize, false)})
	}, func(taskCtx context.Context, written any) (any, error) {
		if replaces != nil && len(replaced) > 0 {
			if err := a.settleCancelled(taskCtx, ref, *replaces, "cancelled"); err != nil {
				return nil, err
			}
		}
		return a.admitted(taskCtx, ref, event, written.(*pyjson.Object))
	}, true, func(any) { scheduleErr = a.scheduleAdmitted(ctx, ref) })
	if err == nil {
		err = scheduleErr
	}
	if err != nil {
		return AdmissionReceipt{}, err
	}
	return result.(AdmissionReceipt), nil
}

// Commits a producer wake for a source's route into the authority FIFO, false when nothing there can take it
func admitRouteWake(ctx context.Context, runner AuthorityRunner, adapter PlatformAdapter, source *SessionSource, text, kind string, metadata map[string]any) (bool, error) {
	a := activeAuthority(runner)
	key, err := runnerSessionKeyForSource(ctx, runner, source)
	if err != nil {
		return false, err
	}
	if err := restoreCompletionRoute(ctx, runner, key); err != nil {
		return false, err
	}
	if a == nil {
		return false, nil
	}
	entry, err := routeEntry(ctx, a, key)
	if err != nil {
		return false, err
	}
	if entry == nil {
		return false, nil
	}
	encoded, err := pyjson.Dumps([]any{kind, key, uuid4HexString()}, pyjson.Separators(",", ":"))
	if err != nil {
		return false, err
	}
	identity := encoded
	wake := func(origin *SessionSource) *MessageEvent {
		event := NewMessageEvent(text)
		event.Source, event.Internal, event.MessageID = origin, true, &identity
		event.Metadata = map[string]any{"gateway_session_key": key, "gateway_session_id": entry.SessionID}
		for name, value := range metadata {
			event.Metadata[name] = value
		}
		return event
	}
	if source.Platform == PlatformLocal || detachedRouteOf(source) != nil {
		_, live, found := detachedLive(a, key)
		if !found {
			live, _ = a.live(source.ChatID)
		}
		if live == nil || live.Route != key {
			return false, nil
		}
		if _, err := admitAutomation(ctx, a, adapter, wake(live.Source), identity, nil, nil); err != nil {
			return false, err
		}
		return true, nil
	}
	envelope, err := committedRouteEnvelope(ctx, a, entry)
	if err != nil {
		return false, err
	}
	if envelope == nil {
		return false, nil
	}
	restored, err := restoreNative(ctx, envelopeWithoutMedia(text, envelope), runner)
	if err != nil {
		return false, err
	}
	if _, err := admitAutomation(ctx, a, adapter, wake(restored.Source), identity, nil, nil); err != nil {
		return false, err
	}
	return true, nil
}

// Async result card of an automation admission for its claim's start frame, empty for every other input
func automationAdmissionDisplay(payload *pyjson.Object) *pyjson.Object {
	descriptor := automationDescriptor(payload)
	kind, _ := descriptor.Get("display_kind")
	text, _ := descriptor.Get("display_text")
	body, isText := text.(string)
	if !pyTruthy(kind) || !isText || pystr.Strip(body) == "" {
		return pyjson.NewObject()
	}
	said := ""
	if value, _ := payload.Get("text"); pyTruthy(value) {
		said = pyrepr.Str(value)
	}
	card := pyjson.ObjectOf("display_kind", kind, "display_text", body, "text", said)
	if category, _ := descriptor.Get("notification_category"); category == "diagnostic" {
		card.Set("notification_category", "diagnostic")
	}
	return card
}
