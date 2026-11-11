package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runner deriving a background result's delivery identity, GatewayRunner._completion_delivery_identity
type completionIdentityRunner interface {
	CompletionDeliveryIdentity(ctx context.Context, event map[string]any) []any
}

// Runner injecting a formatted watch notification, GatewayRunner._inject_watch_notification, nil meaning None
type watchNotificationRunner interface {
	InjectWatchNotification(ctx context.Context, text string, event map[string]any) (*bool, error)
}

// Compact JSON text of a producer event's delivery identity, refused when the runner derives none
func producerIdentity(ctx context.Context, runner any, event map[string]any) (string, error) {
	deriver, ok := runner.(completionIdentityRunner)
	if !ok {
		return "", &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(runner) + "' object has no attribute '_completion_delivery_identity'"}
	}
	identity := deriver.CompletionDeliveryIdentity(ctx, event)
	if identity == nil {
		return "", runtimeRefusal("invalid_params")
	}
	return pyjson.Dumps(identity, pyjson.Separators(",", ":"))
}

// Text of an event field with a falsy value as empty
func eventText(event map[string]any, key string) string {
	if value := event[key]; pyrepr.Truthy(value) {
		return pyrepr.Str(value)
	}
	return ""
}

// Admission carrying a completion's identity on its route's owner, pending or settled, nil when none does
func completionAdmission(ctx context.Context, runner AuthorityRunner, event map[string]any) (*pyjson.Object, error) {
	a, err := activeAuthority(ctx, runner)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, nil
	}
	entry, err := routeEntry(ctx, a, eventText(event, "session_key"))
	if err != nil || entry == nil {
		return nil, err
	}
	identity, err := producerIdentity(ctx, runner, event)
	if err != nil {
		return nil, err
	}
	owner, err := routeOwnerOf(ctx, a, entry)
	if err != nil {
		return nil, err
	}
	rows, err := a.DB.ListSessionAdmissions(ctx, owner, false)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if descriptorHolds(automationDescriptor(rowPayload(row)), identity) {
			return row, nil
		}
	}
	return nil, nil
}

// Whether a descriptor names the identity among its batch identities or as its own
func descriptorHolds(descriptor *pyjson.Object, identity string) bool {
	identities, ok := descriptor.Get("identities")
	if !ok {
		identities = []any{descriptor.GetOr("identity", nil)}
	}
	items, _ := identities.([]any)
	for _, item := range items {
		if item == identity {
			return true
		}
	}
	return false
}

// Identity of two maps, the Python is test of a dict
func sameMap(a, b map[string]any) bool {
	return reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
}

// Prepares, formats and admits one liveness tick under the heartbeat lock, true admitted, nil dropped, false requeued
func deliverHeartbeat(ctx context.Context, runner AuthorityRunner, event map[string]any) (*bool, error) {
	lock := heartbeatLock(runner)
	if err := lock.Lock(ctx); err != nil {
		return nil, err
	}
	defer lock.Unlock()
	requeue := false
	for attempt := 0; attempt < 2; attempt++ {
		prepared, err := func() (map[string]any, error) {
			followed, err := followDetachedPin(ctx, runner, event)
			if err != nil {
				return nil, err
			}
			return prepareHeartbeat(ctx, runner, followed)
		}()
		if err != nil {
			slog.Error(fmt.Sprintf("Heartbeat preparation error; the tick is requeued: %s", err))
			return &requeue, nil
		}
		text := ""
		if prepared != nil {
			text = FormatGatewayProcessNotification(prepared)
		}
		if text == "" {
			return nil, nil
		}
		injector, ok := runner.(watchNotificationRunner)
		if !ok {
			return nil, &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(runner) + "' object has no attribute '_inject_watch_notification'"}
		}
		delivered, err := injector.InjectWatchNotification(ctx, text, prepared)
		if err != nil {
			slog.Error(fmt.Sprintf("Watch notification injection error: %s", err))
			return &requeue, nil
		}
		refused := delivered != nil && !*delivered
		if refused && !sameMap(prepared, event) {
			event["_refusal_permanent"] = prepared["_refusal_permanent"]
		}
		if !refused {
			return delivered, nil
		}
		claimed, err := replacedTickClaimed(ctx, runner, prepared)
		if err != nil {
			return nil, err
		}
		if !claimed {
			return delivered, nil
		}
	}
	return &requeue, nil
}

// Whether the queued tick a prepared tick replaces was claimed or settled first
func replacedTickClaimed(ctx context.Context, runner AuthorityRunner, prepared map[string]any) (bool, error) {
	replaced := prepared["_replaces"]
	if !pyrepr.Truthy(replaced) {
		return false, nil
	}
	a, err := activeAuthority(ctx, runner)
	if err != nil {
		return false, err
	}
	if a == nil {
		return false, nil
	}
	row, err := a.DB.GetSessionAdmission(ctx, pyrepr.Str(replaced))
	if err != nil || row == nil {
		return false, err
	}
	return rowString(row, "status") != "queued", nil
}

// Admits a process notice, a completion and the withdrawal of its process's waiting ticks under the heartbeat lock
func admitProcessNotice(ctx context.Context, runner AuthorityRunner, a *SessionAuthority, adapter PlatformAdapter, event *MessageEvent, evt map[string]any) (AdmissionReceipt, error) {
	identity, err := producerIdentity(ctx, runner, evt)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	if evt["type"] == "heartbeat" {
		omitted := int64(0)
		if raw := evt["omitted"]; pyrepr.Truthy(raw) {
			if omitted, err = pyint.Int64(raw); err != nil {
				return AdmissionReceipt{}, err
			}
		}
		if event.Metadata == nil {
			event.Metadata = map[string]any{}
		}
		event.Metadata["heartbeat_tick"] = map[string]any{"output": eventText(evt, "output"), "omitted": omitted}
		if replaces := evt["_replaces"]; pyrepr.Truthy(replaces) {
			replaced := pyrepr.Str(replaces)
			return a.AdmitAutomation(ctx, adapter, event, identity, &replaced, nil)
		}
	}
	if evt["type"] != "completion" {
		return a.AdmitAutomation(ctx, adapter, event, identity, nil, nil)
	}
	lock := heartbeatLock(runner)
	if err := lock.Lock(ctx); err != nil {
		return AdmissionReceipt{}, err
	}
	defer lock.Unlock()
	receipt, err := admitFoldedCompletion(ctx, a, adapter, event, evt, identity)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	withdrawProcessTicks(ctx, runner, evt)
	return receipt, nil
}

// Completion folded with the one waiting at the FIFO tail, queued alone when that row was claimed first
func admitFoldedCompletion(ctx context.Context, a *SessionAuthority, adapter PlatformAdapter, event *MessageEvent, evt map[string]any, identity string) (AdmissionReceipt, error) {
	waiting, skip, err := waitingCompletion(ctx, a, evt, event)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	if waiting != nil {
		savedText := event.Text
		savedMetadata := make(map[string]any, len(event.Metadata))
		for key, value := range event.Metadata {
			savedMetadata[key] = value
		}
		foldCompletion(event, waiting, identity)
		replaced := rowString(waiting, "admission_id")
		receipt, err := a.AdmitAutomation(ctx, adapter, event, identity, &replaced, skip)
		if err == nil {
			return receipt, nil
		}
		if !isRuntimeRefusal(err, "stale_generation") {
			return AdmissionReceipt{}, err
		}
		event.Text, event.Metadata = savedText, savedMetadata
	}
	return a.AdmitAutomation(ctx, adapter, event, identity, nil, nil)
}

// Route's completion row ending the queue and the finishing process's tick ids it skips
func waitingCompletion(ctx context.Context, a *SessionAuthority, evt map[string]any, event *MessageEvent) (*pyjson.Object, []string, error) {
	entry, err := routeEntry(ctx, a, eventText(evt, "session_key"))
	if err != nil || entry == nil {
		return nil, nil, err
	}
	owner, err := routeOwnerOf(ctx, a, entry)
	if err != nil {
		return nil, nil, err
	}
	rows, err := a.DB.ListSessionAdmissions(ctx, owner, true)
	if err != nil {
		return nil, nil, err
	}
	var queued []*pyjson.Object
	for _, row := range rows {
		if rowString(row, "status") == "queued" {
			queued = append(queued, row)
		}
	}
	process := eventText(evt, "session_id")
	skip := []string{}
	for len(queued) > 0 && queuedTick(queued[len(queued)-1], process, entry.SessionKey) {
		skip = append(skip, rowString(queued[len(queued)-1], "admission_id"))
		queued = queued[:len(queued)-1]
	}
	if len(queued) == 0 {
		return nil, nil, nil
	}
	tail := queued[len(queued)-1]
	if rowString(tail, "principal_id") != "automation:"+entry.SessionKey {
		return nil, nil, nil
	}
	parsed := automationIdentitiesOf([]any{tail.GetOr("request_id", nil)})
	if len(parsed) != 1 || parsed[0][0] != "completion" {
		return nil, nil, nil
	}
	descriptor := automationDescriptor(rowPayload(tail))
	category := func(get func(string) (any, bool)) any {
		if value, ok := get("notification_category"); ok {
			return value
		}
		return "result"
	}
	metadata := metadataGetter(event.Metadata)
	if !pyjson.Equal(category(descriptor.Get), category(metadata)) ||
		!pyjson.Equal(descriptor.GetOr("display_kind", nil), event.Metadata["display_kind"]) {
		return nil, nil, nil
	}
	return tail, skip, nil
}

// Batch header of completions folded while the session was busy
const foldHeader = "[IMPORTANT: %d background process results arrived while this session was busy. " +
	"Treat them as one batch and give one consolidated response; preserve failures and actionable results.]"

// Batch header an already folded text starts with
var foldHeaderPattern = regexp.MustCompile(`^\[IMPORTANT: \d+ background process results arrived while this session was busy\.[^\]]*\]\n\n`)

// Folds the waiting completion's text and identities into the event as one batch
func foldCompletion(event *MessageEvent, waiting *pyjson.Object, identity string) {
	descriptor := automationDescriptor(rowPayload(waiting))
	set := map[string]bool{}
	if earlier, _ := descriptor.GetOr("identities", nil).([]any); pyrepr.Truthy(earlier) {
		for _, item := range earlier {
			set[pyrepr.Str(item)] = true
		}
	} else {
		set[rowString(waiting, "request_id")] = true
	}
	if current := automationIdentityList(event.Metadata["automation_identities"]); len(current) > 0 {
		for _, item := range current {
			set[item] = true
		}
	} else {
		set[identity] = true
	}
	identities := make([]string, 0, len(set))
	for item := range set {
		identities = append(identities, item)
	}
	sort.Strings(identities)
	body := func(text string) string {
		text = pystr.Rstrip(text)
		text = pystr.Rstrip(strings.TrimSuffix(text, InternalNotificationFooter))
		if loc := foldHeaderPattern.FindStringIndex(text); loc != nil {
			return text[loc[1]:]
		}
		return text
	}
	waitingText := pyrepr.Str(rowPayload(waiting).GetOr("text", nil))
	event.Text = MarkInternalNotification(strings.Join([]string{fmt.Sprintf(foldHeader, len(identities)), body(waitingText), body(event.Text)}, "\n\n"))
	listed := make([]any, len(identities))
	for i, item := range identities {
		listed[i] = item
	}
	event.Metadata["automation_identities"] = listed
	if _, shown := event.Metadata["display_text"]; shown {
		blanks := make([]map[string]any, len(identities))
		for i := range blanks {
			blanks[i] = map[string]any{}
		}
		event.Metadata["display_text"] = tools.ProcessCompletionDisplayText(blanks)
	}
}

// Strings of an automation identities metadata value
func automationIdentityList(value any) []string {
	var out []string
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			out = append(out, pyrepr.Str(item))
		}
	case []string:
		out = append(out, typed...)
	}
	return out
}

// Liveness tick to admit replacing a queued tick of its process, nil once its completion is in
func prepareHeartbeat(ctx context.Context, runner AuthorityRunner, event map[string]any) (map[string]any, error) {
	a, err := activeAuthority(ctx, runner)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return event, nil
	}
	entry, err := routeEntry(ctx, a, eventText(event, "session_key"))
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return event, nil
	}
	owner, err := routeOwnerOf(ctx, a, entry)
	if err != nil {
		return nil, err
	}
	process := eventText(event, "session_id")
	completed, err := processCompleted(ctx, runner, a, owner, event)
	if err != nil {
		return nil, err
	}
	if completed {
		slog.Debug(fmt.Sprintf("Dropped heartbeat %s of process %s: its completion is already admitted", pyrepr.Str(event["event_id"]), process))
		return nil, nil
	}
	identity, err := producerIdentity(ctx, runner, event)
	if err != nil {
		return nil, err
	}
	found, err := a.DB.FindSessionAdmission(ctx, "automation:"+entry.SessionKey, owner, identity)
	if err != nil {
		return nil, err
	}
	if found != nil {
		return nil, nil
	}
	pending, err := a.DB.ListSessionAdmissions(ctx, owner, true)
	if err != nil {
		return nil, err
	}
	var waiting *pyjson.Object
	for _, row := range pending {
		if queuedTick(row, process, entry.SessionKey) {
			waiting = row
			break
		}
	}
	if waiting == nil {
		return event, nil
	}
	earlier, isDict := automationDescriptor(rowPayload(waiting)).GetOr("tick", nil).(*pyjson.Object)
	if !isDict {
		return event, nil
	}
	plainEarlier, _ := pyjson.Plain(earlier).(map[string]any)
	out := make(map[string]any, len(event)+3)
	for key, value := range event {
		out[key] = value
	}
	for key, value := range tools.JoinHeartbeatOutputs(plainEarlier, event) {
		out[key] = value
	}
	out["_replaces"] = rowString(waiting, "admission_id")
	return out, nil
}

// Cancels a finished process's ticks still queued in its session, never failing since the completion is already in
func withdrawProcessTicks(ctx context.Context, runner AuthorityRunner, event map[string]any) {
	if err := withdrawTicks(ctx, runner, event); err != nil {
		slog.Warn(fmt.Sprintf("Could not withdraw the waiting heartbeats of finished process %s: %s", pyrepr.Str(event["session_id"]), err))
	}
}

// Withdrawal body of a finished process's queued ticks
func withdrawTicks(ctx context.Context, runner AuthorityRunner, event map[string]any) error {
	a, err := activeAuthority(ctx, runner)
	if err != nil {
		return err
	}
	if a == nil {
		return nil
	}
	entry, err := routeEntry(ctx, a, eventText(event, "session_key"))
	if err != nil || entry == nil {
		return err
	}
	identity, err := producerIdentity(ctx, runner, event)
	if err != nil {
		return err
	}
	raws := []any{identity}
	if extra, ok := event["_automation_identities"].([]any); ok {
		raws = append(raws, extra...)
	}
	finished := map[string]bool{}
	for _, parsed := range automationIdentitiesOf(raws) {
		if parsed[0] == "completion" && len(parsed) > 1 {
			finished[pyrepr.Str(parsed[1])] = true
		}
	}
	owner, err := routeOwnerOf(ctx, a, entry)
	if err != nil {
		return err
	}
	pending, err := a.DB.ListSessionAdmissions(ctx, owner, true)
	if err != nil {
		return err
	}
	for _, row := range pending {
		for process := range finished {
			if queuedTick(row, process, entry.SessionKey) {
				if _, err := withdrawQueued(ctx, a, row); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// Per runner lock pairing a tick's check and admit with a completion's admit and withdraw
var heartbeatLocks sync.Map

// Heartbeat lock of a runner, created on first use
func heartbeatLock(runner any) *asyncLock {
	lock, _ := heartbeatLocks.LoadOrStore(runner, &asyncLock{})
	return lock.(*asyncLock)
}

// Whether the completion of a tick's process incarnation is admitted into the owner, alone or inside a batch
func processCompleted(ctx context.Context, runner any, a *SessionAuthority, owner string, event map[string]any) (bool, error) {
	deriver, ok := runner.(completionIdentityRunner)
	if !ok {
		return false, &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(runner) + "' object has no attribute '_completion_delivery_identity'"}
	}
	completion := deriver.CompletionDeliveryIdentity(ctx, map[string]any{"type": "completion", "session_id": event["session_id"], "started_at": event["started_at"]})
	if completion == nil {
		return false, nil
	}
	identity, err := pyjson.Dumps(completion, pyjson.Separators(",", ":"))
	if err != nil {
		return false, err
	}
	direct := false
	if err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		row, err := state.ConnRow(ctx, conn, "SELECT 1 FROM session_admissions WHERE target_session_id=? AND request_id=?", owner, identity)
		direct = row != nil
		return err
	}); err != nil {
		return false, err
	}
	if direct {
		return true, nil
	}
	rows, err := a.DB.ListSessionAdmissions(ctx, owner, false)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if items, _ := automationDescriptor(rowPayload(row)).GetOr("identities", nil).([]any); pyrepr.Truthy(items) {
			for _, item := range items {
				if item == identity {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// Non empty list identities parsed from request id texts, unparsable ones skipped
func automationIdentitiesOf(raws []any) [][]any {
	var parsed [][]any
	for _, raw := range raws {
		text, isText := raw.(string)
		if !isText {
			continue
		}
		decoded, err := pyjson.Loads(text)
		if err != nil {
			continue
		}
		if list, ok := decoded.([]any); ok && len(list) > 0 {
			parsed = append(parsed, list)
		}
	}
	return parsed
}

// Whether a row is the route producer's own queued liveness tick of the process
func queuedTick(row *pyjson.Object, process, route string) bool {
	if rowString(row, "status") != "queued" || rowString(row, "principal_id") != "automation:"+route {
		return false
	}
	for _, identity := range automationIdentitiesOf([]any{row.GetOr("request_id", nil)}) {
		if len(identity) == 3 && identity[0] == "heartbeat" && identity[1] == process {
			return true
		}
	}
	return false
}

// Cancels the gateway's own queued automation row as its principal with operator access, false when claimed first
func withdrawQueued(ctx context.Context, a *SessionAuthority, row *pyjson.Object) (bool, error) {
	actor := Principal{Subject: rowString(row, "principal_id"), ProfileID: a.ProfileID,
		Capabilities: map[string]bool{"session:submit": true, "session:control": true, "session:operator": true}, TransportID: "producer-withdraw"}
	receipt, err := a.CancelQueued(ctx, actor, SessionRef{a.ProfileID, rowString(row, "target_session_id")}, rowString(row, "admission_id"))
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return false, nil
		}
		return false, err
	}
	return receipt.Outcome != nil && *receipt.Outcome == "cancelled", nil
}

// Logical owner of a route's admissions, a local route's chat id or the root of its compressed session
func routeOwnerOf(ctx context.Context, a *SessionAuthority, entry *SessionEntry) (string, error) {
	if entry.Origin != nil && entry.Origin.Platform == PlatformLocal {
		return entry.Origin.ChatID, nil
	}
	return a.LogicalOwner(ctx, entry.SessionID)
}

// Important message of a watch pattern event, empty for an event type the gateway does not format
func FormatGatewayProcessNotification(evt map[string]any) string {
	get := func(key string, def any) any {
		if value, ok := evt[key]; ok {
			return value
		}
		return def
	}
	evtType := get("type", "completion")
	sid, cmd := get("session_id", "unknown"), get("command", "unknown")
	switch evtType {
	case "watch_disabled", "watch_overflow_tripped", "watch_overflow_released":
		return fmt.Sprintf("[IMPORTANT: %s]", pyrepr.Str(get("message", "")))
	case "watch_match":
		text := fmt.Sprintf("[IMPORTANT: Background process %s matched watch pattern \"%s\".\nCommand: %s\nMatched output:\n%s",
			pyrepr.Str(sid), pyrepr.Str(get("pattern", "?")), pyrepr.Str(cmd), pyrepr.Str(get("output", "")))
		if suppressed := get("suppressed", 0); pyrepr.Truthy(suppressed) {
			text += fmt.Sprintf("\n(%s earlier matches were suppressed by rate limit)", pyrepr.Str(suppressed))
		}
		return text + "]"
	case "async_delegation", "heartbeat":
		return tools.FormatProcessNotification(evt)
	}
	return ""
}

// One liveness tick delivered under the heartbeat lock, for the runner's watch notification drain
func DeliverHeartbeat(ctx context.Context, runner AuthorityRunner, event map[string]any) (*bool, error) {
	return deliverHeartbeat(ctx, runner, event)
}

// Process notice admission, for the runner's watch notification injection
func AdmitProcessNotice(ctx context.Context, runner AuthorityRunner, a *SessionAuthority, adapter PlatformAdapter, event *MessageEvent, evt map[string]any) (AdmissionReceipt, error) {
	return admitProcessNotice(ctx, runner, a, adapter, event, evt)
}

// Admission carrying a completion's identity, for the runner's completion pre flight
func CompletionAdmission(ctx context.Context, runner AuthorityRunner, event map[string]any) (*pyjson.Object, error) {
	return completionAdmission(ctx, runner, event)
}

// Compact JSON delivery identity of a producer event, for the runner's notification code
func ProducerIdentity(ctx context.Context, runner any, event map[string]any) (string, error) {
	return producerIdentity(ctx, runner, event)
}
