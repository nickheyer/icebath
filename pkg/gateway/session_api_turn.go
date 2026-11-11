package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const apiSettingsPrefix = "gateway.api.settings.v1."

// Request owned sinks an API observer may register for its admission's execution
var apiObserverKeys = []string{"stream_delta_callback", "tool_start_callback", "tool_complete_callback",
	"reasoning_callback", "status_callback", "interim_assistant_callback"}

var apiToolSinks = map[string]bool{"tool_start_callback": true, "tool_complete_callback": true}

var apiSettingKeys = map[string]bool{"ephemeral_system_prompt": true, "requested_model": true, "requested_provider": true,
	"model_options": true, "route": true, "session_model": true, "confirmed_runtime_lock": true,
	"requested_runtime": true, "route_source": true, "room_dispatch": true, "room_execution_policy": true,
	"session_history_delivery": true}

var apiTurnFields = map[string]bool{"history": true, "settings": true, "turn_author": true, "media": true,
	"run_owner_scope": true, "browser_control": true, "relay_metadata": true, "resume_unanswered": true}

var apiOwnerScopeRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Message every API surface shows for an admission still queued when its owner retired
const admissionQueuedMessage = "Icebath is restarting. Your input is saved and will run once when it is back; do not resend it."

// Owner retired while this API admission was still queued, its input committed for the next owner
type AdmissionQueuedError struct{ *state.RuntimeStoreError }

func newAdmissionQueued() *AdmissionQueuedError {
	return &AdmissionQueuedError{state.NewRuntimeStoreError("admission_queued", nil, nil)}
}

func (e *AdmissionQueuedError) Error() string { return admissionQueuedMessage }

// Python class name of the error
func (e *AdmissionQueuedError) PyTypeName() string { return "AdmissionQueued" }

func (e *AdmissionQueuedError) Unwrap() error { return e.RuntimeStoreError }

// Saved API settings of the session, an empty mapping when none were committed
func apiSettings(ctx context.Context, a *SessionAuthority, ref SessionRef) (any, error) {
	var saved string
	var found bool
	err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var err error
		saved, found, err = metaValue(ctx, conn, apiSettingsPrefix+ref.SessionID)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return pyjson.NewObject(), nil
	}
	return pyjson.Loads(saved)
}

// Members iterating a decoded JSON value yields, the TypeError Python raises for a scalar
func pyIterMembers(value any) ([]any, error) {
	switch v := value.(type) {
	case *pyjson.Object:
		members := make([]any, len(v.Keys))
		for i, key := range v.Keys {
			members[i] = key
		}
		return members, nil
	case []any:
		return v, nil
	case string:
		members := []any{}
		for _, r := range v {
			members = append(members, string(r))
		}
		return members, nil
	}
	return nil, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(value) + "' object is not iterable"}
}

// Value of key in a decoded dict, the AttributeError Python raises calling get on anything else
func pyDictGet(value any, key string, def any) (any, error) {
	obj, ok := value.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(value) + "' object has no attribute 'get'"}
	}
	if item, present := obj.Get(key); present {
		return item, nil
	}
	return def, nil
}

// Refuses an api_turn_v1 envelope whose fields or shapes the turn cannot run
func checkAPITurnEnvelope(data any) error {
	members, err := pyIterMembers(data)
	if err != nil {
		return err
	}
	keys := map[any]bool{}
	for _, member := range members {
		switch member.(type) {
		case []any, *pyjson.Object:
			return &PyError{Kind: "TypeError", Msg: "unhashable type: '" + jsonValueTypeName(member) + "'"}
		}
		keys[member] = true
	}
	invalid := runtimeRefusal("invalid_params")
	for key := range keys {
		if name, ok := key.(string); !ok || !apiTurnFields[name] {
			return invalid
		}
	}
	resume, err := pyDictGet(data, "resume_unanswered", true)
	if err != nil {
		return err
	}
	if resume != true || !keys["history"] || !keys["settings"] {
		return invalid
	}
	relay, err := pyDictGet(data, "relay_metadata", pyjson.NewObject())
	if err != nil {
		return err
	}
	if _, isDict := relay.(*pyjson.Object); !isDict {
		return invalid
	}
	obj := data.(*pyjson.Object)
	history, _ := obj.Get("history")
	if _, isList := history.([]any); history != nil && !isList {
		return invalid
	}
	if scope, present := obj.Get("run_owner_scope"); present && !validOwnerScope(scope) {
		return invalid
	}
	settings, _ := obj.Get("settings")
	settingKeys, err := pyIterMembers(settings)
	if err != nil {
		return err
	}
	for _, key := range settingKeys {
		if name, ok := key.(string); !ok || !apiSettingKeys[name] {
			return invalid
		}
	}
	return nil
}

// Adapter that may run an admitted API turn for the session, refusing a draining or malformed one
func checkAPITurn(ctx context.Context, a *SessionAuthority, ref SessionRef, payload *pyjson.Object) (PlatformAdapter, error) {
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, &pyKeyError{ref.SessionID}
	}
	if live.Source == nil {
		return nil, &PyError{Kind: "AttributeError", Msg: "'NoneType' object has no attribute 'platform'"}
	}
	if live.Source.Platform == PlatformAPIServer {
		if err := restoreAPISession(ctx, a, ref.SessionID); err != nil {
			return nil, err
		}
	}
	adapter, err := a.Runner.AdapterForSource(ctx, live.Source)
	if err != nil {
		return nil, err
	}
	if adapter == nil || adapter.Base() == nil || adapter.Base().GatewayRunner != any(a.Runner) {
		return nil, runtimeRefusal("runtime_draining")
	}
	data, hasData := payload.Get("api_turn_v1")
	if hasData {
		if err := checkAPITurnEnvelope(data); err != nil {
			return nil, err
		}
	}
	var settings any
	if hasData {
		settings, _ = data.(*pyjson.Object).Get("settings")
	}
	if !pyrepr.Truthy(settings) {
		saved, err := apiSettings(ctx, a, ref)
		if err != nil {
			return nil, err
		}
		settings = saved
	}
	if err := checkAPISettings(ctx, adapter, settings); err != nil {
		return nil, err
	}
	return adapter, nil
}

// Whether value is a 64 digit lowercase hex owner scope
func validOwnerScope(value any) bool {
	text, ok := value.(string)
	return ok && apiOwnerScopeRE.MatchString(text)
}

// Session context fields an executing API admission binds beyond the shared source, empty outside one
func APISessionVars(ctx context.Context) (map[string]any, error) {
	current := APIExecution(ctx)
	if current == nil {
		return map[string]any{}, nil
	}
	settings, _ := current.Get("settings")
	delivery, err := pyDictGet(settings, "session_history_delivery", nil)
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(delivery) {
		delivery = ""
	}
	return map[string]any{"session_history_delivery": delivery}, nil
}

// Refuses room settings unless the policy rides the dispatch it was verified for against the current catalog
func checkAPISettings(ctx context.Context, adapter PlatformAdapter, settings any) error {
	dispatch, err := pyDictGet(settings, "room_dispatch", nil)
	if err != nil {
		return err
	}
	policy, _ := pyDictGet(settings, "room_execution_policy", nil)
	if dispatch == nil && policy != nil {
		return runtimeRefusal("permission_denied")
	}
	if dispatch == nil {
		return nil
	}
	bound, err := HostedMemberDispatchFromMapping(dispatch)
	if err != nil {
		return err
	}
	local, err := LocalAuthorityGatewayID(ctx)
	if err != nil {
		return err
	}
	if bound.TargetInstallID != local {
		return runtimeRefusal("permission_denied")
	}
	_, catalog, err := localRoomCatalog(ctx, adapter, bound.TargetProfile, bound.TargetInstallID)
	if err != nil {
		return err
	}
	current, err := GatewayRoomCatalogFromMapping(catalog)
	if err != nil {
		return err
	}
	if current.CatalogDigest != bound.CapabilityDigest || !pyjson.Equal(current.ExecutionPolicy.AsMapping(), policy) {
		return runtimeRefusal("permission_denied")
	}
	return nil
}

// Context bound to the room execution policy of the executing API admission, unchanged when it carries none
func APIPolicyScope(ctx context.Context) (context.Context, error) {
	current := APIExecution(ctx)
	if current == nil {
		return ctx, nil
	}
	settings, _ := current.Get("settings")
	policy, err := pyDictGet(settings, "room_execution_policy", nil)
	if err != nil || policy == nil {
		return ctx, err
	}
	bound, err := RoomExecutionPolicyFromMapping(policy)
	if err != nil {
		return nil, err
	}
	return BindRoomExecutionPolicy(ctx, bound), nil
}

// Execution state of one admitted API turn, its adapter, settings, history and content
func prepareAPIExecution(ctx context.Context, a *SessionAuthority, ref SessionRef, payload *pyjson.Object) (*pyjson.Object, error) {
	adapter, err := checkAPITurn(ctx, a, ref, payload)
	if err != nil {
		return nil, err
	}
	rawData, _ := payload.Get("api_turn_v1")
	data, _ := rawData.(*pyjson.Object)
	hasData := pyrepr.Truthy(rawData)
	var settings any
	if hasData {
		var present bool
		if settings, present = data.Get("settings"); !present {
			return nil, &pyKeyError{"settings"}
		}
	} else if settings, err = apiSettings(ctx, a, ref); err != nil {
		return nil, err
	}
	content, present := payload.Get("text")
	if !present {
		return nil, &pyKeyError{"text"}
	}
	if parts, isList := content.([]any); hasData && isList {
		media, _ := data.Get("media")
		mediaList, _ := media.([]any)
		if pyrepr.Truthy(media) && mediaList == nil {
			return nil, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(media) + "' object is not iterable"}
		}
		restored, err := restoreAPIImages(ctx, parts, mediaList)
		if err != nil {
			return nil, err
		}
		content = restored
	}
	var history, author, relay any
	resume := false
	if hasData {
		var present bool
		if history, present = data.Get("history"); !present {
			return nil, &pyKeyError{"history"}
		}
		author, _ = data.Get("turn_author")
		relay, _ = data.Get("relay_metadata")
		resumeValue, _ := data.Get("resume_unanswered")
		resume = pyrepr.Truthy(resumeValue)
	}
	return pyjson.ObjectOf("adapter", adapter, "settings", settings, "history", history, "content", content,
		"turn_author", author, "relay_metadata", relay, "resume_unanswered", resume), nil
}

type apiExecutionKey struct{}

// Context carrying prepared as the executing API admission, nil clearing it
func bindAPIExecution(ctx context.Context, prepared *pyjson.Object) context.Context {
	return context.WithValue(ctx, apiExecutionKey{}, prepared)
}

// Executing API admission the context carries, nil outside one
func APIExecution(ctx context.Context) *pyjson.Object {
	prepared, _ := ctx.Value(apiExecutionKey{}).(*pyjson.Object)
	return prepared
}

// Context carrying prepared as the executing API admission, for the runner and its cases
func BindAPIExecution(ctx context.Context, prepared *pyjson.Object) context.Context {
	return bindAPIExecution(ctx, prepared)
}

// One request owned API sink, args positional and kwargs by name as the Python callback receives them
type APIObserverCallback func(args []any, kwargs map[string]any) error

// Sinks one API request registered for its admission's execution, keyed by callback name
type APIObserver struct {
	Callbacks map[string]APIObserverCallback
}

// Observers registered for the session's executing admission
func apiObservers(a *SessionAuthority, sessionID string) ([]*APIObserver, error) {
	live, ok := a.live(sessionID)
	if !ok {
		return nil, &pyKeyError{sessionID}
	}
	var admissionID any
	if execution := live.EventStream.Execution; execution != nil && execution.Len() > 0 {
		admissionID, _ = execution.Get("admission_id")
	}
	key, isKey := admissionID.(string)
	if !isKey {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*APIObserver(nil), a.apiObservers[key]...), nil
}

// Runs every observer's callback for key, logging and skipping one that fails, true when any accepted
func notifyObservers(a *SessionAuthority, sessionID, key string, args []any, kwargs map[string]any) (bool, error) {
	observers, err := apiObservers(a, sessionID)
	if err != nil {
		return false, err
	}
	accepted := false
	for _, observer := range observers {
		callback := observer.Callbacks[key]
		if callback == nil {
			continue
		}
		if err := callback(args, kwargs); err != nil {
			slog.Warn(fmt.Sprintf("API observer %s failed for %s", key, sessionID), "error", err)
			continue
		}
		accepted = true
	}
	return accepted, nil
}

// Registers observer for an admission and returns the call that removes it again
func registerAPIObserver(a *SessionAuthority, admissionID string, observer *APIObserver) func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.apiObservers == nil {
		a.apiObservers = map[string][]*APIObserver{}
	}
	a.apiObservers[admissionID] = append(a.apiObservers[admissionID], observer)
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		registered := a.apiObservers[admissionID]
		for i, existing := range registered {
			if existing == observer {
				registered = append(registered[:i:i], registered[i+1:]...)
				break
			}
		}
		if len(registered) == 0 {
			delete(a.apiObservers, admissionID)
		} else {
			a.apiObservers[admissionID] = registered
		}
	}
}

// Hands a message delta's text to the admission's stream observers, a None segment boundary staying viewer only
func publishAPIEvent(a *SessionAuthority, sessionID, eventType string, payload any) (bool, error) {
	if eventType != "message.delta" {
		return false, nil
	}
	var text any
	switch p := payload.(type) {
	case *pyjson.Object:
		value, present := p.Get("text")
		if !present {
			return false, &pyKeyError{"text"}
		}
		text = value
	case map[string]any:
		value, present := p["text"]
		if !present {
			return false, &pyKeyError{"text"}
		}
		text = value
	default:
		return false, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(payload) + "' object is not subscriptable"}
	}
	if text == nil {
		return false, nil
	}
	return notifyObservers(a, sessionID, "stream_delta_callback", []any{text}, nil)
}

// Producer emitted tool progress event for the admission's progress only observers, fenced to the running claim
func PublishAPIToolProgress(ctx context.Context, a *SessionAuthority, sessionID string, generation int64, args []any, kwargs map[string]any) error {
	live, ok := a.live(sessionID)
	if !ok {
		return &pyKeyError{sessionID}
	}
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	if err := a.checkApprovalGeneration(ctx, sessionID, generation); err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return nil
		}
		return err
	}
	_, err := notifyObservers(a, sessionID, "tool_progress_callback", args, kwargs)
	return err
}

// Real tool arguments and results for the admission's API observers, never part of the viewer stream
func PublishAPIToolEvent(ctx context.Context, a *SessionAuthority, sessionID string, generation int64, eventType, callID, toolName string, args, result any) error {
	live, ok := a.live(sessionID)
	if !ok {
		return &pyKeyError{sessionID}
	}
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	if err := a.checkApprovalGeneration(ctx, sessionID, generation); err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return nil
		}
		return err
	}
	if !pyrepr.Truthy(args) {
		args = pyjson.NewObject()
	}
	var err error
	switch eventType {
	case "tool.start":
		_, err = notifyObservers(a, sessionID, "tool_start_callback", []any{callID, toolName, args}, nil)
	case "tool.complete":
		_, err = notifyObservers(a, sessionID, "tool_complete_callback", []any{callID, toolName, args, result}, nil)
	}
	return err
}

// Executing API admission when its settings carry a confirmed Browser model lock, else nil
func APIRuntimeLocked(ctx context.Context) (*pyjson.Object, error) {
	current := APIExecution(ctx)
	if current == nil {
		return nil, nil
	}
	settings, _ := current.Get("settings")
	locked, err := pyDictGet(settings, "confirmed_runtime_lock", nil)
	if err != nil || !pyrepr.Truthy(locked) {
		return nil, err
	}
	return current, nil
}
