package gateway

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// One SQLite write a step sequence yields, for sequences built outside the package
type WriteStep = writeStep

// Runner that performs one yielded write, for sequences built outside the package
type StepRunner = stepRunner

// Sequence of write steps returning its final value, for sequences built outside the package
type WriteSteps = writeSteps

// API adapter surface the admission of a canonical API turn reads
type APITurnAdapter interface {
	PlatformAdapter
	EnsureSessionDB(ctx context.Context) (*state.SessionDB, error)
	ConfiguredModelRoute(alias any) map[string]any
	BindRunAdmissionAlias(key, admissionID string)
	ReleaseRunAdmissionAlias(key, admissionID string)
}

// API adapter whose run ids resolve to canonical API admissions
type APIRunAdmitter interface {
	RunAdmission(ctx context.Context, runID string) (*SessionAuthority, *pyjson.Object, error)
}

// Keyword arguments of one API turn, its settings holding exactly the setting keys the caller passed
type APITurn struct {
	UserMessage              any
	ConversationHistory      any
	SessionID                string
	GatewaySessionKey        string
	BindDeclaredConversation bool
	Settings                 map[string]any
	HistoryFromSession       bool
	RunOwnerScope            any
	TurnAuthor               any
	ResumeUnansweredTurn     bool
	RelayMetadata            any
	RequestID                string
	ActiveRunID              string
	WalkedFrom               string
	Observers                map[string]APIObserverCallback
	ApprovalNotifyCallback   func(data map[string]any) error
	ApprovalSessionKey       string
}

// One committed API admission with its authority, session and ledger row
type APIAdmission struct {
	Authority *SessionAuthority
	Ref       SessionRef
	Row       *pyjson.Object
}

// Admission itself, the first three members of any admission tuple a step sequence returns
func (x *APIAdmission) Admitted() *APIAdmission { return x }

// Result of a step sequence that ends in an API admission
type apiAdmitted interface{ Admitted() *APIAdmission }

// Retryable refusal of a request under a parked profile's prefix, nil for a served profile
func refuseParkedScope(ctx context.Context, runner any) error {
	name := servedProfileName(home.Dir(ctx))
	if profileParked(runner, name) {
		return NewParkedProfileRefusal(name)
	}
	return nil
}

// Active scope's authority whose store is the caller's, refused retryably while parked and else profile_mismatch
func RequireActiveAuthority(ctx context.Context, runner any, store func() (*state.SessionDB, error)) (*SessionAuthority, error) {
	authority, err := activeAuthority(ctx, runner)
	if err != nil {
		return nil, err
	}
	if authority == nil {
		if err := refuseParkedScope(ctx, runner); err != nil {
			return nil, err
		}
		return nil, runtimeRefusal("profile_mismatch")
	}
	if store != nil {
		db, err := store()
		if err != nil {
			return nil, err
		}
		if db != authority.DB {
			return nil, runtimeRefusal("profile_mismatch")
		}
	}
	return authority, nil
}

// Settings of an admission with every setting key present, None for the ones the caller did not pass
func apiTurnSettings(given map[string]any) *pyjson.Object {
	settings := pyjson.NewObject()
	for _, key := range []string{"ephemeral_system_prompt", "requested_model", "requested_provider", "model_options", "route",
		"session_model", "confirmed_runtime_lock", "requested_runtime", "route_source", "room_dispatch",
		"room_execution_policy", "session_history_delivery"} {
		settings.Set(key, given[key])
	}
	return settings
}

// Route mapping of a setting value, false for anything that is not a dict
func routeMapping(value any) (map[string]any, bool) {
	switch route := value.(type) {
	case map[string]any:
		return route, route != nil
	case *pyjson.Object:
		if route == nil {
			return nil, false
		}
		return pyjson.Plain(route).(map[string]any), true
	case map[string]string:
		if route == nil {
			return nil, false
		}
		out := make(map[string]any, len(route))
		for k, v := range route {
			out[k] = v
		}
		return out, true
	}
	return nil, false
}

// Admission of one API turn as write steps, its media captured and released around the admission write
func AdmitAPISteps(ctx context.Context, adapter APITurnAdapter, turn *APITurn) WriteSteps {
	return func(run stepRunner) (any, error) {
		authority, err := RequireActiveAuthority(ctx, adapter.Base().GatewayRunner, func() (*state.SessionDB, error) {
			return adapter.EnsureSessionDB(ctx)
		})
		if err != nil {
			return nil, err
		}
		sid := turn.SessionID
		if sid == "" {
			sid = uuid4HexString()
		}
		declaredKey := ""
		if turn.BindDeclaredConversation {
			declaredKey = turn.GatewaySessionKey
		}
		if declaredKey != "" {
			declared, found, err := declaredAPISession(ctx, authority.DB, declaredKey)
			if err != nil {
				return nil, err
			}
			if found && declared != "" {
				sid = declared
			}
		}
		if sid, err = authority.LogicalOwner(ctx, sid); err != nil {
			return nil, err
		}
		if err := authority.requireAdmissionOpen(false); err != nil {
			return nil, err
		}
		settings := apiTurnSettings(turn.Settings)
		if route, ok := routeMapping(turn.Settings["route"]); ok && len(route) > 0 && pyrepr.Truthy(route["api_key"]) {
			configured := adapter.ConfiguredModelRoute(settings.GetOr("requested_model", nil))
			if configured == nil || !pyjson.Equal(configured, route) {
				return nil, runtimeRefusal("permission_denied")
			}
			stripped := pyjson.NewObject()
			for _, key := range sortedKeys(route) {
				if key != "api_key" {
					stripped.Set(key, route[key])
				}
			}
			settings.Set("route", stripped)
		}
		var history any
		if !turn.HistoryFromSession {
			history = turn.ConversationHistory
		}
		text, err := state.RuntimeJSON(pyjson.ObjectOf("text", turn.UserMessage, "api_turn_v1",
			pyjson.ObjectOf("history", history, "settings", settings)))
		if err != nil {
			return nil, err
		}
		decoded, err := pyjson.Loads(text)
		if err != nil {
			return nil, err
		}
		payload := decoded.(*pyjson.Object)
		data, _ := payload.Get("api_turn_v1")
		envelope := data.(*pyjson.Object)
		if turn.RunOwnerScope != nil {
			if !validOwnerScope(turn.RunOwnerScope) {
				return nil, runtimeRefusal("invalid_params")
			}
			envelope.Set("run_owner_scope", turn.RunOwnerScope)
		}
		existing, err := authority.DB.GetSession(ctx, sid)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if source := existing.GetOr("source", nil); source != "api_server" && source != "bot_room" {
				return nil, runtimeRefusal("permission_denied")
			}
		}
		if turn.TurnAuthor != nil {
			author := provider.ParseTurnAuthor(turn.TurnAuthor)
			if author == nil {
				return nil, runtimeRefusal("invalid_params")
			}
			envelope.Set("turn_author", author)
		}
		if turn.ResumeUnansweredTurn {
			envelope.Set("resume_unanswered", true)
		}
		if pyrepr.Truthy(turn.RelayMetadata) {
			envelope.Set("relay_metadata", turn.RelayMetadata)
		}
		requestID := turn.RequestID
		if requestID == "" {
			requestID = turn.ActiveRunID
		}
		if requestID == "" {
			requestID = uuid4HexString()
		}
		content, isList := turn.UserMessage.([]any)
		if !isList {
			return admitAPIPayload(ctx, run, authority, adapter, sid, requestID, payload, settings, declaredKey, turn)
		}
		leaseCtx, _, dropLease := captureLease(ctx, nil)
		media, err := commitAPIImages(leaseCtx, content)
		if err != nil {
			dropLease()
			return nil, err
		}
		envelope.Set("media", media)
		release := func(writeCtx context.Context) (any, error) {
			return releaseUnheldMedia(writeCtx, authority.DB, media, true)
		}
		admitted, err := admitAPIPayload(leaseCtx, run, authority, adapter, sid, requestID, payload, settings, declaredKey, turn)
		if err != nil {
			dropCaptureLease(leaseCtx, nil, nil)
			dropLease()
			if _, releaseErr := run(release); releaseErr != nil {
				return nil, releaseErr
			}
			return nil, err
		}
		dropLease()
		if _, err := run(release); err != nil {
			return nil, err
		}
		return admitted, nil
	}
}

// Keys of a map in sorted order
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Exact retry of a retired admission, else the session bound and the turn admitted with its settings committed
func admitAPIPayload(ctx context.Context, run stepRunner, authority *SessionAuthority, adapter APITurnAdapter, sid, requestID string,
	payload, settings *pyjson.Object, declaredKey string, turn *APITurn) (*APIAdmission, error) {
	row, err := authority.DB.RetryTerminalAdmission(ctx, state.SessionInput{Epoch: authority.Epoch, PrincipalID: "api",
		SessionID: sid, RequestID: requestID, Payload: payload})
	if err != nil {
		return nil, err
	}
	if row != nil {
		if err := checkAPISettings(ctx, adapter, settings); err != nil {
			return nil, err
		}
		return &APIAdmission{Authority: authority, Ref: SessionRef{authority.ProfileID, sid}, Row: row}, nil
	}
	var hosted map[string]any
	if dispatch, ok := routeMapping(turn.Settings["room_dispatch"]); ok {
		hosted = dispatch
	}
	bound, err := bindAPISteps(ctx, authority, sid, apiBindOptions{HostedDispatch: hosted, DeclaredKey: declaredKey,
		CallerKey: turn.GatewaySessionKey, WalkedFrom: turn.WalkedFrom})(run)
	if err != nil {
		return nil, err
	}
	ref := bound.(SessionRef)
	if _, err := checkAPITurn(ctx, authority, ref, payload); err != nil {
		return nil, err
	}
	data, _ := payload.Get("api_turn_v1")
	committed, _ := data.(*pyjson.Object).Get("settings")
	clientKey := turn.RequestID != ""
	admitted, err := run(func(writeCtx context.Context) (any, error) {
		return authority.DB.AdmitSessionInput(writeCtx, state.SessionInput{Epoch: authority.Epoch, PrincipalID: "api",
			SessionID: sid, RequestID: requestID, Payload: payload,
			AuthorizeWrite: authority.admissionGate(func(gateCtx context.Context, conn state.SQLiteConn) error {
				return admitTarget(gateCtx, sid, requestID, committed, conn, clientKey)
			}, false)})
	})
	if err != nil {
		return nil, err
	}
	return &APIAdmission{Authority: authority, Ref: ref, Row: admitted.(*pyjson.Object)}, nil
}

// One target per request id, the session's settings committed inside the admission's own transaction
func admitTarget(ctx context.Context, sessionID, requestID string, settings any, conn state.SQLiteConn, clientKey bool) error {
	if err := oneTarget(ctx, sessionID, requestID, conn, clientKey); err != nil {
		return err
	}
	encoded, err := state.RuntimeJSON(settings)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "INSERT INTO state_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		apiSettingsPrefix+sessionID, encoded)
	return err
}

// Refuses a request id another session's live admission or retired tombstone already spent
func oneTarget(ctx context.Context, sessionID, requestID string, conn state.SQLiteConn, clientKey bool) error {
	found, err := state.QueryRowObject(ctx, conn, "SELECT 1 AS one FROM session_admissions WHERE principal_id='api' AND request_id=? "+
		"AND target_session_id!=? LIMIT 1", requestID, sessionID)
	if err != nil {
		return err
	}
	if found != nil {
		return runtimeRefusal("admission_conflict")
	}
	if !clientKey {
		return nil
	}
	rows, err := state.TerminalAPIAdmissions(ctx, conn, requestID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		target, err := pyItem(row, "target_session_id")
		if err != nil {
			return err
		}
		if !pyjson.Equal(target, sessionID) {
			return runtimeRefusal("admission_conflict")
		}
	}
	return nil
}

// Drain of an admission whose caller stopped waiting, scheduled anyway while it is still queued
func scheduleOrphan(ctx context.Context, result any) {
	held, ok := result.(apiAdmitted)
	if !ok {
		return
	}
	admitted := held.Admitted()
	if rowString(admitted.Row, "status") != AdmissionQueued {
		return
	}
	if err := scheduleSession(ctx, admitted.Authority, admitted.Ref); err != nil {
		slog.Warn(fmt.Sprintf("orphaned API admission %s could not be scheduled", admitted.Ref.SessionID), "error", err)
	}
}

// Drives an API admission step sequence off the caller in admission order, an orphaned admission still drained
func RunAPIStepsOffLoop(ctx context.Context, adapter APITurnAdapter, steps WriteSteps) (any, error) {
	authority, err := RequireActiveAuthority(ctx, adapter.Base().GatewayRunner, nil)
	if err != nil {
		return nil, err
	}
	detached := context.WithoutCancel(ctx)
	return runStepsOffLoop(ctx, authority, steps, func(result any) { scheduleOrphan(detached, result) })
}

// Admits one API turn with every SQLite write off the caller, in admission order
func AdmitAPITurnAsync(ctx context.Context, adapter APITurnAdapter, turn *APITurn) (*APIAdmission, error) {
	result, err := RunAPIStepsOffLoop(ctx, adapter, AdmitAPISteps(context.WithoutCancel(ctx), adapter, turn))
	if err != nil {
		return nil, err
	}
	return result.(*APIAdmission), nil
}

// Admits one API turn inline, each write run by the caller
func AdmitAPITurn(ctx context.Context, adapter APITurnAdapter, turn *APITurn) (*APIAdmission, error) {
	result, err := runSteps(ctx, AdmitAPISteps(ctx, adapter, turn))
	if err != nil {
		return nil, err
	}
	return result.(*APIAdmission), nil
}

// Admits and observes one API turn, a streaming caller's run id bound to the admission's shared approval prompt
func RunAPITurn(ctx context.Context, adapter APITurnAdapter, turn *APITurn) (map[string]any, map[string]any, error) {
	admitted, err := AdmitAPITurnAsync(ctx, adapter, turn)
	if err != nil {
		return nil, nil, err
	}
	if turn.ApprovalNotifyCallback == nil || turn.ApprovalSessionKey == "" {
		return ObserveAPITurn(ctx, admitted, turn.Observers)
	}
	admissionID := rowString(admitted.Row, "admission_id")
	adapter.BindRunAdmissionAlias(turn.ApprovalSessionKey, admissionID)
	defer adapter.ReleaseRunAdmissionAlias(turn.ApprovalSessionKey, admissionID)
	sink := func(eventType string, prompt *pyjson.Object) error {
		if eventType != "approval.request" {
			return nil
		}
		choices, _ := prompt.Get("choices")
		data := map[string]any{}
		for _, key := range prompt.Keys {
			if key != "kind" && key != "prompt_id" && key != "choices" {
				data[key] = prompt.Vals[key]
			}
		}
		promptID, err := pyItem(prompt, "prompt_id")
		if err != nil {
			return err
		}
		allowSession, err := apiChoicesContain(choices, "session")
		if err != nil {
			return err
		}
		allowPermanent, err := apiChoicesContain(choices, "always")
		if err != nil {
			return err
		}
		data["request_id"], data["allow_session"], data["allow_permanent"] = promptID, allowSession, allowPermanent
		return turn.ApprovalNotifyCallback(data)
	}
	release, err := ObserveAPIControls(ctx, admitted, sink)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	return ObserveAPITurn(ctx, admitted, turn.Observers)
}

// Whether a falsy or collection value contains item, the TypeError Python raises for a scalar
func apiChoicesContain(collection any, item string) (bool, error) {
	if !pyrepr.Truthy(collection) {
		return false, nil
	}
	if text, isText := collection.(string); isText {
		return strings.Contains(text, item), nil
	}
	members, err := pyIterMembers(collection)
	if err != nil {
		return false, err
	}
	for _, member := range members {
		if member == any(item) {
			return true, nil
		}
	}
	return false, nil
}

// Settled result and usage of the admission, observing it through its waiter until the drain settles it
func ObserveAPITurn(ctx context.Context, admitted *APIAdmission, callbacks map[string]APIObserverCallback) (map[string]any, map[string]any, error) {
	a, ref, row := admitted.Authority, admitted.Ref, admitted.Row
	admissionID := rowString(row, "admission_id")
	current, err := a.DB.GetSessionAdmission(ctx, admissionID)
	if err != nil {
		return nil, nil, err
	}
	if current == nil {
		current = row
	}
	switch rowString(current, "status") {
	case AdmissionUnknown:
		return nil, nil, runtimeRefusal("unknown_execution")
	case AdmissionTerminal:
		result, usage, err := settledAPIResult(ctx, a, admissionID)
		if err != nil {
			return nil, nil, err
		}
		if callback := callbacks["stream_delta_callback"]; callback != nil {
			text := result["final_response"]
			if !pyrepr.Truthy(text) {
				text = ""
			}
			if err := callback([]any{text}, nil); err != nil {
				return nil, nil, err
			}
		}
		return result, usage, nil
	}
	a.mu.Lock()
	waiter := a.waiters[admissionID]
	if waiter == nil {
		waiter = newAdmissionWaiter()
		a.waiters[admissionID] = waiter
	}
	a.mu.Unlock()
	observer := &APIObserver{Callbacks: map[string]APIObserverCallback{}}
	structured := false
	for _, key := range apiObserverKeys {
		if callback := callbacks[key]; callback != nil {
			observer.Callbacks[key] = callback
			structured = structured || apiToolSinks[key]
		}
	}
	if progress := callbacks["tool_progress_callback"]; progress != nil && !structured {
		observer.Callbacks["tool_progress_callback"] = progress
	}
	unregister := registerAPIObserver(a, admissionID, observer)
	err = func() error {
		defer unregister()
		err := a.publishPending(ctx, ref)
		if err == nil {
			err = scheduleSession(ctx, a, ref)
		}
		if err == nil {
			_, err = waiter.wait(ctx)
		}
		var refusal *state.RuntimeStoreError
		if err != nil && errors.As(err, &refusal) && refusal.Reason == "runtime_draining" {
			latest, readErr := a.DB.GetSessionAdmission(context.WithoutCancel(ctx), admissionID)
			if readErr != nil {
				return readErr
			}
			if latest != nil && rowString(latest, "status") == AdmissionQueued {
				return newAdmissionQueued()
			}
		}
		return err
	}()
	if err != nil {
		return nil, nil, err
	}
	return settledAPIResult(ctx, a, admissionID)
}

// Mapping of a decoded dict with its values kept as decoded
func topLevelMap(value any) (map[string]any, error) {
	switch v := value.(type) {
	case *pyjson.Object:
		out := make(map[string]any, v.Len())
		for _, key := range v.Keys {
			out[key] = v.Vals[key]
		}
		return out, nil
	case map[string]any:
		return v, nil
	}
	return nil, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(value) + "' object is not a mapping"}
}

// Saved result and usage of a settled admission, an interrupted empty reply for a cancelled one
func settledAPIResult(ctx context.Context, a *SessionAuthority, admissionID string) (map[string]any, map[string]any, error) {
	saved, err := admissionResult(ctx, a.DB, admissionID)
	if err != nil {
		return nil, nil, err
	}
	if saved == nil {
		current, err := a.DB.GetSessionAdmission(ctx, admissionID)
		if err != nil {
			return nil, nil, err
		}
		if current == nil {
			return nil, nil, &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
		}
		outcome, err := pyItem(current, "outcome")
		if err != nil {
			return nil, nil, err
		}
		if outcome == "cancelled" {
			return map[string]any{"final_response": "", "interrupted": true, "completed": false}, map[string]any{}, nil
		}
		return nil, nil, runtimeRefusal("unknown_execution")
	}
	rawResult, err := pyItem(saved, "result")
	if err != nil {
		return nil, nil, err
	}
	rawUsage, err := pyItem(saved, "usage")
	if err != nil {
		return nil, nil, err
	}
	result, err := topLevelMap(rawResult)
	if err != nil {
		return nil, nil, err
	}
	usage, err := topLevelMap(rawUsage)
	if err != nil {
		return nil, nil, err
	}
	return result, usage, nil
}

// Control events an API observer projects from the session stream
var apiControlEvents = map[string]bool{"approval.request": true, "approval.settled": true, "clarify.request": true, "clarify.settled": true}

// Projects the admission's approval and clarify prompts to sink until the returned call, replaying pending ones first
func ObserveAPIControls(ctx context.Context, admitted *APIAdmission, sink func(eventType string, prompt *pyjson.Object) error) (func(), error) {
	a, ref, row := admitted.Authority, admitted.Ref, admitted.Row
	if err := restoreRetired(ctx, a, ref.SessionID); err != nil {
		return nil, err
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return func() {}, nil
	}
	events := live.EventStream
	admissionID := rowString(row, "admission_id")
	observer := &SessionEventObserver{Observe: func(frame *pyjson.Object) {
		rawParams, err := pyItem(frame, "params")
		if err != nil {
			panic(err)
		}
		params, isDict := rawParams.(*pyjson.Object)
		if !isDict {
			panic(&PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(rawParams) + "' object has no attribute 'get'"})
		}
		eventType, _ := params.GetOr("type", nil).(string)
		if params.GetOr("admission_id", nil) != any(admissionID) || !apiControlEvents[eventType] {
			return
		}
		payload, _ := params.GetOr("payload", nil).(*pyjson.Object)
		if !pyrepr.Truthy(payload) {
			payload = pyjson.NewObject()
		}
		if err := sink(eventType, payload); err != nil {
			panic(err)
		}
	}}
	err := func() error {
		events.Lock.Lock()
		defer events.Lock.Unlock()
		events.Observers[observer] = true
		current, err := a.DB.GetSessionAdmission(ctx, admissionID)
		if err != nil {
			return err
		}
		if current == nil {
			current = row
		}
		if rowString(current, "status") != AdmissionStarted {
			return nil
		}
		generation, err := pyItem(current, "generation")
		if err != nil {
			return err
		}
		var stamp *int64
		if value, isInt := generation.(int64); isInt {
			stamp = &value
		}
		for _, prompt := range live.controls.snapshotObjects(ref.SessionID, stamp) {
			rawKind, err := pyItem(prompt, "kind")
			if err != nil {
				return err
			}
			kind, isText := rawKind.(string)
			if !isText {
				return &PyError{Kind: "TypeError", Msg: "unsupported operand type(s) for +: '" + jsonValueTypeName(rawKind) + "' and 'str'"}
			}
			if err := sink(kind+".request", prompt); err != nil {
				return err
			}
		}
		return nil
	}()
	release := func() {
		events.Lock.Lock()
		defer events.Lock.Unlock()
		delete(events.Observers, observer)
	}
	if err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// Pending prompts after a snapshot settles stale ones, each an ordered copy of its prompt
func (c *PendingControls) snapshotObjects(sessionID string, generation *int64) []*pyjson.Object {
	c.events.Lock.Lock()
	defer c.events.Lock.Unlock()
	c.snapshot(sessionID, generation)
	out := make([]*pyjson.Object, 0, len(c.order))
	for _, promptID := range c.order {
		out = append(out, pyjson.Clone(c.pending[promptID].prompt).(*pyjson.Object))
	}
	return out
}

// Whether a caller scope owns one canonical API admission, failing closed on every unanswered question
func OwnsAPIRun(ctx context.Context, adapter APIRunAdmitter, runID, ownerScope string) (bool, error) {
	if !validOwnerScope(ownerScope) {
		return false, nil
	}
	_, row, err := adapter.RunAdmission(ctx, runID)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return false, nil
		}
		return false, err
	}
	if row == nil {
		return false, nil
	}
	stored, err := pyItem(row, "payload", "api_turn_v1", "run_owner_scope")
	if err != nil {
		return false, nil
	}
	text, _ := stored.(string)
	return validOwnerScope(stored) && hmac.Equal([]byte(text), []byte(ownerScope)), nil
}
