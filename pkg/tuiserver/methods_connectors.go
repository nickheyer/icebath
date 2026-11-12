package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Connector handler body answering a response or raising an error the guard maps to a refusal
type connectorBody func(ctx context.Context, rid any, params map[string]any) (map[string]any, error)

// Whether each account owned action of the Connectors page may create the guest identity
var accountCreatesGuest = map[string]bool{"status": false, "connect": true}

var connectorsRegistry = &handlerRegistry{}

func init() {
	connectorsRegistry.method("connectors.list", connectorGuard(func(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
		return connectorRPC(ctx, rid, params, "status")
	}))
	connectorsRegistry.method("connectors.connect", connectorGuard(func(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
		return connectorRPC(ctx, rid, params, "connect")
	}))
	connectorsRegistry.method("connectors.operation.status", connectorGuard(connectorsOperationStatus))
	connectorsRegistry.method("connectors.operation.wake", connectorGuard(connectorsOperationWake))
	connectorsRegistry.method("connection.respond", connectorGuard(connectionRespond))
	connectorsRegistry.install()
	names := make([]string, 0, len(connectorRPCMethods))
	for name := range connectorRPCMethods {
		names = append(names, name)
	}
	addLongHandlers(names...)
	installConnectionUpdateHook()
}

// Connector refusal carrying the wire value of its ConnectorErrorReason member in the error data
func connectorRPCError(rid any, code int, reason, message string) map[string]any {
	return rpcErr(rid, code, message, map[string]any{"reason": tuigateway.ConnectorErrorReason.Member(reason)})
}

// Refusal of one connector refusal spec
func connectorRefusal(rid any, spec tuigateway.ConnectorRefusalSpec) map[string]any {
	return connectorRPCError(rid, spec.Code, spec.Reason, spec.Message)
}

// Refusal for a gateway auth failure by its code and status
func connectorAuthError(rid any, failure *tools.ToolGatewayError) map[string]any {
	return connectorRefusal(rid, tuigateway.ConnectorAuthRefusal(failure.Code, failure.Status))
}

// Whether a raised failure is a profile or config outage the dispatcher answers instead of the guard
func connectorOutage(failure any) bool {
	err, isError := failure.(error)
	if !isError {
		return false
	}
	var profileErr *ProfileUnavailableError
	var backendErr *config.ConfigBackendUnavailable
	var sourceErr *provider.ConfigBackendUnavailableError
	return errors.As(err, &profileErr) || errors.As(err, &backendErr) || errors.As(err, &sourceErr)
}

// Refusal of a raised connector failure, an auth failure by its code and anything else as request failed
func connectorFailure(rid any, failure any) map[string]any {
	if err, isError := failure.(error); isError {
		var gatewayErr *tools.ToolGatewayError
		if errors.As(err, &gatewayErr) && gatewayErr.Kind == "auth" {
			return connectorAuthError(rid, gatewayErr)
		}
	}
	return connectorRefusal(rid, tuigateway.ConnectorRequestFailed)
}

// Handler that maps a raised failure to its connector refusal and lets a profile or config outage through
func connectorGuard(body connectorBody) Handler {
	return func(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
		defer func() {
			failure := recover()
			if failure == nil {
				return
			}
			if connectorOutage(failure) {
				panic(failure)
			}
			response = connectorFailure(rid, failure)
		}()
		response, err := body(ctx, rid, params)
		if err != nil {
			if connectorOutage(err) {
				panic(err)
			}
			return connectorFailure(rid, err)
		}
		return response
	}
}

// Text field of a validated contract instance, empty for None
func connectorText(instance *pydantic.Instance, name string) string {
	text, _ := instance.Get(name).(string)
	return text
}

// Requested connector names of a validated connect request
func connectorNames(request *pydantic.Instance) []string {
	var names []string
	for _, item := range pyjson.Plain(request.Get("connectors")).([]any) {
		names = append(names, item.(string))
	}
	return names
}

// Optional text field of a validated contract instance, nil for None
func connectorOptional(instance *pydantic.Instance, name string) *string {
	text, isText := instance.Get(name).(string)
	if !isText {
		return nil
	}
	return &text
}

// Owner instance of a validated connector request
func connectorOwner(request *pydantic.Instance) *pydantic.Instance {
	owner, _ := request.Get("owner").(*pydantic.Instance)
	return owner
}

// Whether the owner session is still the live record of sid in profileHome and not finalized
func connectorOwnerMatches(ctx context.Context, sid string, owner *tuigateway.Session, profileHome any) bool {
	_, current := currentSessionSteerAuthority(ctx, sid)
	return current == owner && !truthy(owner.Get("_finalized")) && pyjson.Equal(owner.Get("profile_home"), profileHome)
}

// Live session this transport steers for a session owner, refused when not owned or hosted on a compute host
func connectorSessionOwner(ctx context.Context, rid any, owner *pydantic.Instance) (*tuigateway.Session, map[string]any) {
	sid := connectorText(owner, "session_id")
	_, session := currentSessionSteerAuthority(ctx, sid)
	origin := connectorRPCOriginOf(ctx)
	if session == nil || truthy(session.Get("_finalized")) ||
		origin != nil && (origin.Session != session || !pyjson.Equal(origin.ProfileHome, session.Get("profile_home"))) {
		return nil, connectorRPCError(rid, 4001, "not_owner", "session not found or not owned by this transport")
	}
	if sessionUsesComputeHost(ctx, session, nil) {
		return nil, connectorRPCError(rid, 5033, "unsupported_runtime", "Connectors must be managed on the session's compute host.")
	}
	return session, nil
}

// Closed availability refusal for an account owner, nil when connectors are available
func accountGateClosed(env *tools.Env, rid any) map[string]any {
	if tools.ConnectorsAvailable(env) {
		return nil
	}
	return connectorRPCError(rid, 4031, "connectors_unavailable", "Connectors are not available.")
}

// Creates the guest identity for a Connectors page connect, its setup failure as a refusal
func accountIdentityError(env *tools.Env, rid any) map[string]any {
	if setupError := tools.EnsureGuestIdentity(env); setupError != "" {
		return connectorRPCError(rid, 5034, "connector_request_failed", setupError)
	}
	return nil
}

// Request validated against a contract, the invalid params refusal when it does not validate
func parseConnectorParams(rid any, params map[string]any, model *pydantic.Model) (*pydantic.Instance, map[string]any) {
	request, invalid := model.Validate(params)
	if invalid != nil {
		return nil, connectorRefusal(rid, tuigateway.ConnectorInvalidParams)
	}
	return request, nil
}

// Validated request and the owning live session, nil for an account owner
func connectorParams(ctx context.Context, rid any, params map[string]any, model *pydantic.Model) (*pydantic.Instance, *tuigateway.Session, map[string]any) {
	request, failure := parseConnectorParams(rid, params, model)
	if failure != nil {
		return nil, nil, failure
	}
	owner := connectorOwner(request)
	if owner.Get("type") == "account" {
		return request, nil, nil
	}
	session, failure := connectorSessionOwner(ctx, rid, owner)
	return request, session, failure
}

// Enabled and disabled toolsets of the session and its availability answer when connectors are off
func sessionConnectorGate(ctx context.Context, env *tools.Env, rid any, session *tuigateway.Session, action string) ([]string, []string, map[string]any) {
	var enabled, disabled []string
	if a := sessionAgent(session); a != nil {
		enabled, disabled = a.EnabledToolsets(), a.DisabledToolsets()
	} else {
		enabled = loadEnabledToolsets(ctx, resolveAgentPlatform(sessionSource(session)))
	}
	selected, _ := tools.Default.SelectToolNames(ctx, env, enabled, disabled, tools.SelectionContext{})
	if !selected["manage_connections"] || !tools.ConnectorsAvailable(env) {
		if action == "status" {
			return enabled, disabled, ok(rid, map[string]any{"available": false, "connectors": []any{}})
		}
		return enabled, disabled, connectorRefusal(rid, tuigateway.ConnectorConnectorsUnavailable)
	}
	return enabled, disabled, nil
}

// Whether a named target of the open operation belongs to another account than the request names
func otherAccountTarget(operation *tools.ConnectionOperation, request *pydantic.Instance, names []string) bool {
	alias, connectionID := request.Get("alias"), request.Get("connection_id")
	if alias == nil && connectionID == nil {
		return false
	}
	for _, name := range names {
		target := operation.Target(name)
		if target == nil {
			continue
		}
		if alias != nil && (target.Alias == nil || *target.Alias != alias) {
			return true
		}
		if connectionID != nil && !(target.RepairID != nil && *target.RepairID == connectionID) &&
			!(target.ConnectionID != "" && target.ConnectionID == connectionID) {
			return true
		}
	}
	return false
}

// Status read through manage_connections or the retry of the open operation for a session owner
func sessionConnectorRPC(ctx context.Context, env *tools.Env, rid any, request *pydantic.Instance, session *tuigateway.Session, action string) (map[string]any, error) {
	sid := connectorText(connectorOwner(request), "session_id")
	enabled, disabled, gate := sessionConnectorGate(ctx, env, rid, session, action)
	if gate != nil {
		return gate, nil
	}
	if !connectorOwnerMatches(ctx, sid, session, session.Get("profile_home")) {
		return connectorRPCError(rid, 4001, "not_owner", "session ownership changed"), nil
	}
	toolAction := action
	if action == "connect" && truthy(request.Get("reconnect")) {
		toolAction = "reconnect"
	}
	args := map[string]any{"action": toolAction}
	sessionKey := strOr(session.Get("session_key"))
	if action == "connect" {
		names := connectorNames(request)
		args["connectors"] = names
		operation := tools.LiveCurrent(sessionKey, strOr(session.Get("profile_home")))
		if operation == nil {
			return connectorRPCError(rid, 4004, "unknown_operation", "No open connection operation for this session."), nil
		}
		if otherAccountTarget(operation, request, names) {
			return connectorRefusal(rid, tuigateway.ConnectorUnknownTargetRefusal), nil
		}
		return connectorReissue(ctx, env, rid, operation, names, "")
	}
	agentSessionID := sessionKey
	if a := sessionAgent(session); a != nil && a.SessionID() != "" {
		agentSessionID = a.SessionID()
	}
	raw := tools.DispatcherOf(ctx).DispatchWith(ctx, &tools.Call{Name: "manage_connections", Args: args, TaskID: sessionKey,
		SessionID: agentSessionID, ToolCallID: "connector-ui-" + strings.ReplaceAll(uuid.NewString(), "-", ""), Env: env},
		tools.DispatchOptions{EnabledToolsets: enabled, DisabledToolsets: disabled})
	decoded, err := pyjson.Loads(raw)
	if err != nil {
		return nil, err
	}
	data, isDict := decoded.(*pyjson.Object)
	if !isDict {
		return connectorRefusal(rid, tuigateway.ConnectorRequestFailed), nil
	}
	if _, failed := data.Get("error"); failed {
		return connectorRefusal(rid, tuigateway.ConnectorRequestFailed), nil
	}
	if action == "status" {
		rows, isList := data.GetOr("connectors", nil).([]any)
		if !isList {
			return connectorRPCError(rid, 5034, "invalid_connector_response", "Connector service returned an invalid response."), nil
		}
		for _, row := range rows {
			if _, rowDict := row.(*pyjson.Object); !rowDict {
				return connectorRPCError(rid, 5034, "invalid_connector_response", "Connector service returned an invalid response."), nil
			}
		}
		connectors, err := connectorRows(rows)
		if err != nil {
			return nil, err
		}
		return ok(rid, map[string]any{"available": true, "connectors": connectors}), nil
	}
	if _, isList := data.GetOr("targets", nil).([]any); !isList {
		return connectorRPCError(rid, 5034, "invalid_connector_response", "Connector service returned no authorization results."), nil
	}
	return ok(rid, pyjson.Plain(tuigateway.ConnectorUIPayload(data))), nil
}

// Gateway list rows typed as connector list items and dumped by field name for a renderer
func connectorRows(rows []any) ([]any, error) {
	dumped := make([]any, 0, len(rows))
	for _, row := range rows {
		item, invalid := tools.ConnectorListItem.Validate(row)
		if invalid != nil {
			return nil, invalid
		}
		dumped = append(dumped, item.Dump(pydantic.DumpOptions{}))
	}
	return pyjson.Plain(tuigateway.ConnectorUIPayload(dumped)).([]any), nil
}

// Connectors page list read with the stored bearer, never creating an identity
func accountConnectorList(ctx context.Context, env *tools.Env, rid any) (map[string]any, error) {
	rows := []any{}
	if tools.ReadNousAccessToken(env) != "" {
		client, err := tools.NewManagedClient(env, false, tools.InitiatorAgentTool, nil)
		if err != nil {
			return nil, err
		}
		items, err := client.ListConnectors(ctx, 0)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			rows = append(rows, item)
		}
	}
	tools.NoteConnectedAppsSeen(ctx, env, rows)
	connectors, err := connectorRows(rows)
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"available": true, "connectors": connectors}), nil
}

// Connectors page connect finding or starting the account operation and answering its first snapshot
func accountConnectorConnect(ctx context.Context, env *tools.Env, rid any, request *pydantic.Instance) (map[string]any, error) {
	action := "connect"
	if truthy(request.Get("reconnect")) {
		action = "reconnect"
	}
	names := connectorNames(request)
	start, err := tools.FindOrStartAccountOperation(ctx, env, names, action, accountHome(request),
		connectorOptional(request, "alias"), connectorOptional(request, "connection_id"), tools.InitiatorDesktopPage)
	var mismatch *tools.AccountOperationMismatch
	if errors.As(err, &mismatch) {
		return connectorRefusal(rid, tuigateway.ConnectorInvalidParams), nil
	}
	if err != nil {
		return nil, err
	}
	if !start.Started {
		dead := false
		for _, name := range names {
			target := start.Operation.Target(name)
			if target == nil {
				return nil, fmt.Errorf("'NoneType' object has no attribute 'state'")
			}
			if state := start.Operation.StateOf(target); state == tools.TargetFailed || state == tools.TargetExpired {
				dead = true
			}
		}
		if dead {
			return connectorReissue(ctx, env, rid, start.Operation, names, tools.InitiatorDesktopPage)
		}
		return ok(rid, connectorOperationView(start.Operation)), nil
	}
	if !tools.WaitForAccountPrepare(start) {
		return ok(rid, connectorOperationView(start.Operation)), nil
	}
	if start.Failed() {
		return connectorRefusal(rid, tuigateway.ConnectorRequestFailed), nil
	}
	return ok(rid, connectorOperationView(start.Operation)), nil
}

// List or connect for an account owner under the Connectors page scope, else for the owning session
func connectorRPC(ctx context.Context, rid any, params map[string]any, action string) (map[string]any, error) {
	model := tuigateway.ConnectorsConnectParams
	if action == "status" {
		model = tuigateway.ConnectorsListParams
	}
	request, session, failure := connectorParams(ctx, rid, params, model)
	if failure != nil {
		return failure, nil
	}
	if connectorOwner(request).Get("type") == "account" {
		var response map[string]any
		var err error
		accountScope(ctx, request, func(scoped context.Context, env *tools.Env) {
			if closed := accountGateClosed(env, rid); closed != nil {
				if action == "status" {
					response = ok(rid, map[string]any{"available": false, "connectors": []any{}})
				} else {
					response = closed
				}
				return
			}
			if accountCreatesGuest[action] {
				if setupError := accountIdentityError(env, rid); setupError != nil {
					response = setupError
					return
				}
			}
			if action == "status" {
				response, err = accountConnectorList(scoped, env, rid)
			} else {
				response, err = accountConnectorConnect(scoped, env, rid, request)
			}
		})
		return response, err
	}
	sid := connectorText(connectorOwner(request), "session_id")
	runtimeCtx := withRuntimeSessionRecord(ctx, session)
	profileHome := session.Get("profile_home")
	scopeHome := strOr(profileHome)
	if scopeHome == "" {
		scopeHome = importedHome()
	}
	scoped := profileRuntimeScope(runtimeCtx, scopeHome, true)
	cwd := sessionCwd(scoped, session)
	bound, env := setSessionContext(scoped, strOr(session.Get("session_key")), &cwd, sid)
	result, err := func() (map[string]any, error) {
		defer clearSessionContext(env)
		return sessionConnectorRPC(bound, env, rid, request, session, action)
	}()
	if err != nil {
		return nil, err
	}
	if !connectorOwnerMatches(runtimeCtx, sid, session, profileHome) {
		return connectorRPCError(rid, 4001, "not_owner", "session ownership changed"), nil
	}
	return result, nil
}

// Re runs the named dead rows of an operation, a known refusal reason answered as its refusal
func connectorReissue(ctx context.Context, env *tools.Env, rid any, operation *tools.ConnectionOperation, names []string, initiator string) (map[string]any, error) {
	reason, err := mcp.Reissue(ctx, env, operation, names, initiator)
	if err != nil {
		return nil, err
	}
	if spec, known := tuigateway.ConnectorReissueRefusals[reason]; known {
		return connectorRefusal(rid, spec), nil
	}
	return ok(rid, connectorOperationView(operation)), nil
}

// Profile home of an account request, empty for the launch profile, raising an unknown profile
func accountHome(request *pydantic.Instance) string {
	profileDir, err := profileHome(connectorText(request, "profile"))
	if err != nil {
		panic(err)
	}
	return profileDir
}

// Runs body with the session variables of the desktop Connectors page bound for a sessionless request
func desktopPageVars(ctx context.Context, body func(ctx context.Context, env *tools.Env)) {
	cronSession := ""
	env := &tools.Env{Home: home.Dir(ctx), Values: &sync.Map{}, TerminalScope: tools.GetTerminalScope(ctx)}
	tools.SetSessionVars(env, tools.SessionVars{Source: "desktop", Profile: currentProfileName(ctx), CronSession: &cronSession})
	defer clearSessionContext(env)
	body(tools.WithToolEnv(ctx, env), env)
}

// Runs body in the runtime scope of the account request's profile with the Connectors page variables bound
func accountScope(ctx context.Context, request *pydantic.Instance, body func(ctx context.Context, env *tools.Env)) {
	desktopPageVars(profileRuntimeScope(ctx, accountHome(request), true), body)
}

// Open operation named by the request for its session or account owner, refused when unknown
func connectorOperationForRequest(ctx context.Context, rid any, request *pydantic.Instance, session *tuigateway.Session) (*tools.ConnectionOperation, map[string]any) {
	var operation *tools.ConnectionOperation
	opID := connectorText(request, "op_id")
	if connectorOwner(request).Get("type") == "session" {
		operation = tools.LiveGet(strOr(session.Get("session_key")), opID, strOr(session.Get("profile_home")))
	} else {
		var closed map[string]any
		accountScope(ctx, request, func(_ context.Context, env *tools.Env) {
			if closed = accountGateClosed(env, rid); closed == nil {
				operation = tools.LiveGetByOpID(opID, accountHome(request))
			}
		})
		if closed != nil {
			return nil, closed
		}
	}
	if operation == nil {
		return nil, connectorRefusal(rid, tuigateway.ConnectorUnknownOperation)
	}
	return operation, nil
}

// Live snapshot of one open session or account operation
func connectorsOperationStatus(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	request, session, failure := connectorParams(ctx, rid, params, tuigateway.ConnectionOperationParams)
	if failure != nil {
		return failure, nil
	}
	operation, failure := connectorOperationForRequest(ctx, rid, request, session)
	if failure != nil {
		return failure, nil
	}
	return ok(rid, connectorOperationView(operation)), nil
}

// Wakes the watcher of one open operation so it reads the accounts now
func connectorsOperationWake(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	request, session, failure := connectorParams(ctx, rid, params, tuigateway.ConnectionOperationParams)
	if failure != nil {
		return failure, nil
	}
	operation, failure := connectorOperationForRequest(ctx, rid, request, session)
	if failure != nil {
		return failure, nil
	}
	operation.Wake.Set()
	return ok(rid, map[string]any{"status": "ok"}), nil
}

// Applies the card's per target answer and optional Continue to the open operation
func connectionRespond(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	envelope := make(map[string]any, len(params))
	for key, value := range params {
		if key != "result" {
			envelope[key] = value
		}
	}
	request, session, failure := connectorParams(ctx, rid, envelope, tuigateway.ConnectionOperationParams)
	if failure != nil {
		return failure, nil
	}
	answer, invalid := tuigateway.ConnectionAnswer.Validate(params["result"])
	if invalid != nil {
		return connectorRefusal(rid, tuigateway.ConnectorInvalidAnswer), nil
	}
	operation, failure := connectorOperationForRequest(ctx, rid, request, session)
	if failure != nil {
		return failure, nil
	}
	if connectorOwner(request).Get("type") == "account" {
		var response map[string]any
		var err error
		accountScope(ctx, request, func(scoped context.Context, _ *tools.Env) {
			response, err = applyConnectionAnswer(scoped, rid, answer, operation)
		})
		return response, err
	}
	scopeHome := strOr(session.Get("profile_home"))
	if scopeHome == "" {
		scopeHome = importedHome()
	}
	return applyConnectionAnswer(profileRuntimeScope(ctx, scopeHome, true), rid, answer, operation)
}

// Hands the answer to the operation, an illegal transition refused as an invalid answer
func applyConnectionAnswer(ctx context.Context, rid any, answer *pydantic.Instance, operation *tools.ConnectionOperation) (map[string]any, error) {
	settled, err := mcp.Respond(ctx, operation, answer.DumpJSON(pydantic.DumpOptions{ExcludeNone: true}))
	var illegal *tools.IllegalTransition
	if errors.As(err, &illegal) {
		return connectorRefusal(rid, tuigateway.ConnectorInvalidAnswer), nil
	}
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"status": "ok", "settled": settled}), nil
}

// Snapshot with its settled flag
func connectorSnapshotView(snapshot *pyjson.Object) *pyjson.Object {
	view := snapshot.Copy()
	view.Set("settled", view.GetOr("settled_at", nil) != nil)
	return view
}

// Redacted settled view of an operation's live snapshot as a plain result
func connectorOperationView(operation *tools.ConnectionOperation) map[string]any {
	return pyjson.Plain(tuigateway.ConnectorUIPayload(connectorSnapshotView(operation.Snapshot(true)))).(map[string]any)
}

// Emits one transition or settlement of an operation to its owning session, or to every client for an account
func connectionUpdate(operation *tools.ConnectionOperation, change, snapshot *pyjson.Object) {
	payload := connectorSnapshotView(snapshot)
	if change != nil {
		for _, key := range change.Keys {
			payload.Set(key, change.Vals[key])
		}
	}
	if strings.HasPrefix(operation.SessionKey, "account:") {
		targets, _ := payload.GetOr("targets", nil).([]any)
		stripped := make([]any, 0, len(targets))
		for _, item := range targets {
			target, isDict := item.(*pyjson.Object)
			if !isDict {
				stripped = append(stripped, item)
				continue
			}
			kept := pyjson.NewObject()
			for _, key := range target.Keys {
				if key != "connect_url" && key != "connection_id" {
					kept.Set(key, target.Vals[key])
				}
			}
			stripped = append(stripped, kept)
		}
		payload.Set("targets", stripped)
		payload.Set("owner", pyjson.ObjectOf("type", "account"))
		tuigateway.BroadcastGlobalEvent("connection.update", pyjson.Plain(tuigateway.ConnectorUIPayload(payload)).(map[string]any))
		return
	}
	sid := ""
	for _, live := range tuigateway.LiveSessions() {
		profileDir := strOr(live.Session.Get("profile_home"))
		if profileDir == "" {
			profileDir = home.Process()
		}
		if live.Session.Get("session_key") == operation.SessionKey && home.Key(profileDir) == operation.ProfileKey {
			sid = live.SID
			break
		}
	}
	if sid == "" {
		return
	}
	payload.Set("owner", pyjson.ObjectOf("type", "session", "session_id", sid))
	tuigateway.Emit(context.Background(), "connection.update", sid, pyjson.Plain(tuigateway.ConnectorUIPayload(payload)).(map[string]any))
}

var connectionUpdateHookOnce sync.Once

// Installs the operation change hook once
func installConnectionUpdateHook() {
	connectionUpdateHookOnce.Do(func() { tools.OperationOnChange = connectionUpdate })
}
