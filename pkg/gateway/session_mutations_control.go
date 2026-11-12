package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Controls that leave the transcript alone and authorize on the lineage owner
var ownerAuthorizedOperations = map[string]bool{"rename": true, "archive": true, "sidebar": true, "delete": true}

// Model and reasoning picks a messaging chat keeps as its own override
var chatPicks = map[string]bool{"model": true, "reasoning": true}

// Whether a mutation is a pick on a messaging chat, which has no frozen policy
func isChatPick(live *LiveSession, operation string) bool {
	return chatPicks[operation] && live != nil && live.Source != nil && live.Source.Platform != PlatformLocal
}

// History control actor of a principal
func historyActor(actor Principal) state.HistoryActor {
	caps := make([]string, 0, len(actor.Capabilities))
	for capability, held := range actor.Capabilities {
		if held {
			caps = append(caps, capability)
		}
	}
	sort.Strings(caps)
	return state.HistoryActor{ProfileID: actor.ProfileID, Subject: actor.Subject, Capabilities: caps}
}

// Refuses a cold history claim the actor has no import receipt or ownership for
func requireHistoryClaim(ctx context.Context, a *SessionAuthority, conn state.SQLiteConn, actor Principal, sid string) error {
	row, err := state.ConnObject(ctx, conn, "SELECT * FROM sessions WHERE id=?", sid)
	if err != nil || row == nil {
		return err
	}
	if pyTruthy(rowValue(row, "session_key")) || pyTruthy(rowValue(row, "chat_id")) || pyTruthy(rowValue(row, "origin_json")) {
		return nil
	}
	var bound int
	switch err := conn.QueryRowContext(ctx, "SELECT 1 FROM state_meta WHERE key=?", state.BindingPrefix+sid).Scan(&bound); {
	case err == nil:
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	own := a.DB.OwnProfileName()
	if own == "" {
		own = "default"
	}
	profile := rowValue(row, "profile_name")
	ownProfile := profile == nil || profile == "" || profile == own
	owner, err := state.OwnerUser(row)
	if err != nil {
		return err
	}
	if ownProfile && pyTruthy(owner) && SameOwner(rowValue(row, "user_id"), actor.Subject) {
		return nil
	}
	a.mu.Lock()
	legacy, ok := a.nativeLegacyTransports[actor.TransportID]
	a.mu.Unlock()
	if !ok || !samePrincipal(legacy, actor) {
		return runtimeRefusal("permission_denied")
	}
	return nil
}

// Import authorization, the imported session ids the write must claim
func authorizeImport(a *SessionAuthority, actor Principal, ref SessionRef, params *pyjson.Object) ([]string, error) {
	if actor.ProfileID != a.ProfileID || ref.ProfileID != a.ProfileID {
		return nil, runtimeRefusal("profile_mismatch")
	}
	if !actor.Capabilities["session:create"] {
		return nil, runtimeRefusal("permission_denied")
	}
	payload, _ := params.Get("payload")
	if err := state.ValidateAction("import", payload); err != nil {
		return nil, err
	}
	sessions, err := pyItem(payload, "sessions")
	if err != nil {
		return nil, err
	}
	list, _ := pyjson.Plain(sessions).([]any)
	normalized, problems, err := a.DB.ValidateImportPayload(list)
	if err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	ids := make([]string, len(normalized))
	for i, item := range normalized {
		id, err := pyItem(item, "session", "id")
		if err != nil {
			return nil, err
		}
		ids[i] = pyrepr.Str(id)
	}
	return ids, nil
}

// Control authorization, true when the session is cold history the write must claim
func authorizeControl(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *pyjson.Object, owner *SessionRef) (bool, error) {
	if actor.ProfileID != a.ProfileID || ref.ProfileID != a.ProfileID {
		return false, runtimeRefusal("profile_mismatch")
	}
	if !actor.Capabilities["session:control"] {
		return false, runtimeRefusal("permission_denied")
	}
	held, err := a.DB.HasMutationReceipt(ctx, actor.Subject, ref.SessionID, rowString(params, "request_id"))
	if err != nil || held {
		return false, err
	}
	if owner != nil {
		ref = *owner
	} else if ref, err = ownerRef(ctx, a, ref, params); err != nil {
		return false, err
	}
	cold := false
	if _, live := a.live(ref.SessionID); !live {
		err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			if err := requireHistoryClaim(ctx, a, conn, actor, ref.SessionID); err != nil {
				return err
			}
			var err error
			cold, err = state.AuthorizeHistory(ctx, conn, historyActor(actor), ref.SessionID, false)
			return err
		})
		if err != nil {
			return false, err
		}
	}
	if cold {
		return true, nil
	}
	operation, _ := params.Get("operation")
	flagging := operation == "delete" || operation == "archive" || operation == "sidebar"
	if flagging {
		if err := a.Authorize(ctx, actor, ref, "session:read"); err != nil {
			return false, err
		}
	}
	if operation == "delete" {
		if err := requireHistoryLeaf(ctx, a, ref.SessionID); err != nil {
			return false, err
		}
	}
	live, ok := a.live(ref.SessionID)
	if !ok && flagging {
		return false, &pyKeyError{ref.SessionID}
	}
	if !(flagging && isHistoryMount(live)) {
		if err := a.Authorize(ctx, actor, ref, "session:control"); err != nil {
			return false, err
		}
	}
	return false, nil
}

// Session a control authorizes on, the lineage owner for metadata or delete and the lane root for a chat pick
func ownerRef(ctx context.Context, a *SessionAuthority, ref SessionRef, params *pyjson.Object) (SessionRef, error) {
	operation := rowString(params, "operation")
	if chatPicks[operation] {
		return messagingOwnerRef(ctx, a, ref)
	}
	if !ownerAuthorizedOperations[operation] {
		return ref, nil
	}
	var owner string
	if err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var err error
		owner, err = state.LocalLineageOwner(ctx, conn, ref.SessionID)
		return err
	}); err != nil {
		return SessionRef{}, err
	}
	if owner == ref.SessionID && operation == "delete" {
		return messagingOwnerRef(ctx, a, ref)
	}
	if owner == ref.SessionID {
		return ref, nil
	}
	return SessionRef{ProfileID: ref.ProfileID, SessionID: owner}, nil
}

// Action specific session mutation shared by the socket and authenticated HTTP, owned by a tracked task
func mutateSession(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *pyjson.Object) (*pyjson.Object, error) {
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, err
	}
	task := trackMutation(ctx, a, func(taskCtx context.Context) (any, error) {
		return ownedMutation(taskCtx, a, actor, ref, params)
	})
	result, err := task.wait()
	if err != nil {
		return nil, err
	}
	out, _ := result.(*pyjson.Object)
	return out, nil
}

// Refusal reason of a mutation error, session_busy for a guard refusal without one
func mutationRefusalReason(err error) (string, bool) {
	if state.IsMutationGuardRefusal(err) {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return refusal.Reason, true
		}
		return "session_busy", true
	}
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		return refusal.Reason, true
	}
	return "", false
}

// Mutation under the owner's gates, a model or reasoning pick refused only as busy deferred to the next turn
func ownedMutation(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *pyjson.Object) (*pyjson.Object, error) {
	operation := rowString(params, "operation")
	_, isLive := a.live(ref.SessionID)
	if _, hasRequest := params.Get("request_id"); (mutationPolicy[operation] || operation == "compress" || operation == "reset" || operation == "rewind") && !isLive && hasRequest {
		if _, err := authorizeControl(ctx, a, actor, ref, params, nil); err != nil {
			return nil, err
		}
	}
	owner, err := ownerRef(ctx, a, ref, params)
	if err != nil {
		return nil, err
	}
	live, ok := a.live(owner.SessionID)
	if !ok {
		return mutateSessionChecked(ctx, a, actor, ref, params, nil, &owner)
	}
	var result *pyjson.Object
	if operation == "compress" {
		live.claimGate.Lock()
		result, err = mutateSessionChecked(ctx, a, actor, ref, params, &live.mutationLock, &owner)
		live.claimGate.Unlock()
	} else {
		live.mutationLock.Lock()
		result, err = mutateSessionChecked(ctx, a, actor, ref, params, nil, &owner)
		live.mutationLock.Unlock()
		if err != nil {
			reason, refused := mutationRefusalReason(err)
			if !refused || (operation != "model" && operation != "reasoning") || reason != "session_busy" {
				return nil, err
			}
			payload, _ := params.Get("payload")
			picked, _ := pyjson.AsObject(payload)
			revision, _ := params.Get("expected_revision")
			generation, _ := params.Get("expected_generation")
			return deferPick(ctx, a, actor.Subject, ref, deferredPickRequest{Operation: operation, RequestID: rowString(params, "request_id"),
				Payload: picked, ExpectedRevision: revision, ExpectedGeneration: generation})
		}
	}
	if err != nil {
		return nil, err
	}
	if (mutationPolicy[operation] || operation == "compress" || operation == "reset") && !isChatPick(live, operation) {
		if err := scheduleSession(ctx, a, ref); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Validated and authorized mutation handed to the writer, a messaging chat pick applied to its own override
func mutateSessionChecked(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *pyjson.Object, commitLock sync.Locker, owner *SessionRef) (*pyjson.Object, error) {
	fields := map[string]bool{}
	for _, key := range params.Keys {
		if key != "expected_generation" {
			fields[key] = true
		}
	}
	if len(fields) != 5 || !fields["session_id"] || !fields["request_id"] || !fields["expected_revision"] || !fields["operation"] || !fields["payload"] {
		return nil, runtimeRefusal("invalid_params")
	}
	if sid, _ := params.Get("session_id"); sid != ref.SessionID {
		return nil, runtimeRefusal("invalid_params")
	}
	rawOperation, _ := params.Get("operation")
	operation, isText := rawOperation.(string)
	generation, _ := params.Get("expected_generation")
	if !isText || (mutationPolicy[operation] && generation == nil) {
		return nil, runtimeRefusal("invalid_params")
	}
	rawPayload, _ := params.Get("payload")
	payload, editorServers, err := privateMutationPayload(operation, rawPayload)
	if err != nil {
		return nil, err
	}
	params = preparedWith(params, "payload", payload)
	cold := false
	var imported []string
	if operation == "import" {
		if imported, err = authorizeImport(a, actor, ref, params); err != nil {
			return nil, err
		}
	} else if cold, err = authorizeControl(ctx, a, actor, ref, params, owner); err != nil {
		return nil, err
	}
	if operation == "branch" && !actor.Capabilities["session:create"] {
		return nil, runtimeRefusal("permission_denied")
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, err
	}
	target := ref
	if owner != nil {
		target = *owner
	}
	live, _ := a.live(target.SessionID)
	if isChatPick(live, operation) {
		return applyChatPick(ctx, a, ref, target, live, params, actor.Subject)
	}
	var authorizeWrite func(ctx context.Context, conn state.SQLiteConn) error
	if cold || operation == "import" {
		authorizeWrite = func(ctx context.Context, conn state.SQLiteConn) error {
			if operation == "import" {
				return state.ImportHistoryControl(ctx, conn, historyActor(actor), imported)
			}
			ok, err := state.AuthorizeHistory(ctx, conn, historyActor(actor), ref.SessionID, true)
			if err != nil {
				return err
			}
			if !ok {
				return runtimeRefusal("permission_denied")
			}
			return nil
		}
	}
	revision, ok := pathIndex(rowValue(params, "expected_revision"))
	if !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	write := mutationParams{SessionID: ref.SessionID, RequestID: rowString(params, "request_id"), ExpectedRevision: int64(revision), Operation: operation, Payload: payload}
	if generation != nil {
		g, ok := pathIndex(generation)
		if !ok {
			return nil, runtimeRefusal("invalid_params")
		}
		fenced := int64(g)
		write.ExpectedGeneration = &fenced
	}
	return writeMutation(ctx, a, ref, write, editorServers, actor.Subject, authorizeWrite, commitLock)
}

// Current route and persistence policy of a model switch, shared with the runner's model commands
type ModelSwitchContext struct {
	SessionKey       string
	Source           *SessionSource
	ConfigPath       string
	PersistGlobal    bool
	OneTurn          bool
	ReasoningEffort  string
	RestoreSnapshot  map[string]any
	CurrentModel     string
	CurrentProvider  string
	RouteProvider    any
	CurrentBaseURL   string
	CurrentAPIKey    string
	UserProviders    any
	CustomProviders  any
	ExcludedProvider []any
}

// Fills the current route from the config file, the defaults kept when it cannot be read
func (c *ModelSwitchContext) readConfig(ctx context.Context) {
	defer func() { _ = recover() }()
	cfg := loadGatewayConfig(ctx, c.ConfigPath)
	if len(cfg) == 0 {
		return
	}
	if model, ok := cfg["model"].(map[string]any); ok {
		c.CurrentModel = pyrepr.Str(pyrepr.Or(model["default"], ""))
		if value, has := model["provider"]; has {
			c.CurrentProvider = pyrepr.Str(value)
		}
		c.RouteProvider = model["provider"]
		c.CurrentBaseURL = pyrepr.Str(pyrepr.Or(model["base_url"], ""))
	}
	c.UserProviders = cfg["providers"]
	c.CustomProviders = provider.CompatibleCustomProviders(cfg)
	if catalog, ok := cfg["model_catalog"].(map[string]any); ok {
		if excluded, ok := catalog["excluded_providers"].([]any); ok {
			c.ExcludedProvider = excluded
		}
	}
}

// Session model override superseding the configured route
func (c *ModelSwitchContext) applyOverride(override map[string]any) {
	if len(override) == 0 {
		return
	}
	if v, has := override["model"]; has {
		c.CurrentModel = pyrepr.Str(v)
	}
	if v, has := override["provider"]; has {
		c.CurrentProvider, c.RouteProvider = pyrepr.Str(v), v
	}
	if v, has := override["base_url"]; has {
		c.CurrentBaseURL = pyrepr.Str(v)
	}
	if v, has := override["api_key"]; has {
		c.CurrentAPIKey = pyrepr.Str(v)
	}
}

// Runner model switch surface a messaging chat pick drives
type chatModelRunner interface {
	reasoningSelectionRunner
	HandleModelReset(ctx context.Context, event *MessageEvent, sw *ModelSwitchContext) (*string, error)
	RehydrateSessionModelOverride(ctx context.Context, sessionKey string)
	SessionModelOverride(sessionKey string) map[string]any
	PerformModelSwitch(ctx context.Context, sw *ModelSwitchContext, rawInput, explicitProvider string, source *SessionSource) (*provider.ModelSwitchResult, string, error)
	RecordModelSwitch(ctx context.Context, result *provider.ModelSwitchResult, sw *ModelSwitchContext, source *SessionSource, oneTurn, picker bool) string
	ModelSwitchLock() *sync.Mutex
}

// Model switch surface of the runner, an AttributeError naming the attribute the operation reads first
func chatRunner(a *SessionAuthority, operation string) (chatModelRunner, error) {
	runner, ok := a.Runner.(chatModelRunner)
	if !ok {
		attribute := "_model_switch_lock"
		if operation == "reasoning" {
			attribute = "_apply_reasoning_selection"
		}
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '%s'", a.Runner, attribute)}
	}
	return runner, nil
}

// Config key of a platform, cli for local
func platformConfigKey(platform Platform) string {
	if platform == PlatformLocal {
		return "cli"
	}
	return string(platform)
}

// Applies a reasoning level as the chat's own reasoning command would, a nil effort resetting it, the reply returned
func applyChatReasoning(ctx context.Context, runner chatModelRunner, live *LiveSession, effort any) string {
	value := "reset"
	if effort != nil {
		value = pyrepr.Str(effort)
	}
	return runner.ApplyReasoningSelection(ctx, live.Route, platformConfigKey(live.Source.Platform), value, false)
}

// Chat's model command resolved and guarded like a local pick then committed as its session override
func pickChatModel(ctx context.Context, a *SessionAuthority, runner chatModelRunner, owner SessionRef, live *LiveSession, payload *pyjson.Object, row *pyjson.Object) (*pyjson.Object, error) {
	sw := &ModelSwitchContext{SessionKey: live.Route, Source: live.Source, ConfigPath: filepath.Join(home.Dir(ctx), "config.yaml"), CurrentProvider: "openrouter"}
	sw.readConfig(ctx)
	reset, err := isModelReset(payload)
	if err != nil {
		return nil, err
	}
	if reset {
		note, err := runner.HandleModelReset(ctx, nil, sw)
		if err != nil {
			return nil, err
		}
		info, err := messagingRouteInfo(ctx, a.Runner, live)
		if err != nil {
			return nil, err
		}
		model, _ := info.Get("model")
		var reply any
		if note != nil {
			reply = *note
		}
		return pyjson.ObjectOf("model", model, "note", reply), nil
	}
	runner.RehydrateSessionModelOverride(ctx, live.Route)
	sw.applyOverride(runner.SessionModelOverride(live.Route))
	raw, _ := payload.Get("model")
	explicit := ""
	if value, has := payload.Get("provider"); has {
		explicit = pyrepr.Str(value)
	}
	result, problem, err := runner.PerformModelSwitch(ctx, sw, pyrepr.Str(raw), explicit, live.Source)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, state.NewRuntimeStoreError("model_resolution_failed", nil, &problem)
	}
	snapshot := pyjson.ObjectOf("target", owner.SessionID, "target_revision", rowValue(row, "runtime_revision"))
	current := switchRoute{Provider: sw.CurrentProvider, Model: sw.CurrentModel, BaseURL: sw.CurrentBaseURL, APIKey: sw.CurrentAPIKey}
	refusal, err := selectionGuardRefusal(ctx, a, live, payload, snapshot, current, *result)
	if err != nil || refusal != nil {
		return refusal, err
	}
	runner.RecordModelSwitch(ctx, result, sw, live.Source, false, true)
	fields := pyjson.ObjectOf("model", result.NewModel, "provider", result.TargetProvider)
	if effort, has := payload.Get("reasoning"); has {
		applyChatReasoning(ctx, runner, live, effort)
		fields.Set("effort", effort)
	}
	return fields, nil
}

// Applies a model or reasoning mutation to the messaging chat mounted on owner, fenced and receipted on that mount
func applyChatPick(ctx context.Context, a *SessionAuthority, ref, owner SessionRef, live *LiveSession, params *pyjson.Object, principalID string) (*pyjson.Object, error) {
	operation := rowString(params, "operation")
	rawPayload, _ := params.Get("payload")
	if err := state.ValidateAction(operation, rawPayload); err != nil {
		return nil, err
	}
	payload, _ := pyjson.AsObject(rawPayload)
	key, digest, err := state.ChatPickKey(principalID, owner.SessionID, params)
	if err != nil {
		return nil, err
	}
	prepared, err := a.DB.PrepareChatPick(ctx, a.Epoch, key, digest, owner.SessionID, params)
	if err != nil {
		return nil, err
	}
	if receipt, has := prepared.Get("receipt"); has {
		out, _ := receipt.(*pyjson.Object)
		return out, nil
	}
	rawRow, _ := prepared.Get("row")
	row, _ := pyjson.AsObject(rawRow)
	runner, err := chatRunner(a, operation)
	if err != nil {
		return nil, err
	}
	var fields *pyjson.Object
	if operation == "reasoning" {
		effort, _ := payload.Get("effort")
		applyChatReasoning(ctx, runner, live, effort)
		fields = pyjson.ObjectOf("effort", effort)
	} else {
		fields, err = func() (*pyjson.Object, error) {
			lock := runner.ModelSwitchLock()
			lock.Lock()
			defer lock.Unlock()
			return pickChatModel(ctx, a, runner, owner, live, payload, row)
		}()
		if err != nil {
			return nil, err
		}
	}
	if rowString(fields, "status") == "confirmation_required" {
		return preparedWith(pyjson.ObjectOf("session_id", ref.SessionID, "operation", operation), flattenObject(fields)...), nil
	}
	result := preparedWith(pyjson.ObjectOf("session_id", ref.SessionID, "operation", operation, "revision", rowValue(row, "runtime_revision"),
		"execution_generation", rowValue(row, "runtime_generation")), flattenObject(fields)...)
	committed, err := a.DB.CommitChatPick(ctx, a.Epoch, key, digest, owner.SessionID, params, result)
	if err != nil {
		return nil, err
	}
	info, err := localSessionInfo(ctx, a, owner)
	if err != nil {
		return nil, err
	}
	live.EventStream.Publish(owner.SessionID, info, "session.info", nil)
	out, _ := committed.(*pyjson.Object)
	return out, nil
}
