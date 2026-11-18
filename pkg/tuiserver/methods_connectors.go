package tuiserver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Connector handler body answering a response or raising an error the guard maps to a refusal
type connectorBody func(ctx context.Context, rid any, params map[string]any) (map[string]any, error)

var connectorsRegistry = &handlerRegistry{}

func init() {
	connectorsRegistry.method("connectors.connect", connectorGuard(connectorsConnect))
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
	return rpcErr(rid, code, message, map[string]any{"reason": tuihost.ConnectorErrorReason.Member(reason)})
}

// Refusal of one connector refusal spec
func connectorRefusal(rid any, spec tuihost.ConnectorRefusalSpec) map[string]any {
	return connectorRPCError(rid, spec.Code, spec.Reason, spec.Message)
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

// Refusal of a raised connector failure as request failed
func connectorFailure(rid any) map[string]any {
	return connectorRefusal(rid, tuihost.ConnectorRequestFailed)
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
			response = connectorFailure(rid)
		}()
		response, err := body(ctx, rid, params)
		if err != nil {
			if connectorOutage(err) {
				panic(err)
			}
			return connectorFailure(rid)
		}
		return response
	}
}

// Whether the owner session is still the live record of sid in profileHome and not finalized
func connectorOwnerMatches(ctx context.Context, sid string, owner *tuihost.Session, profileHome any) bool {
	_, current := currentSessionSteerAuthority(ctx, sid)
	return current == owner && !truthy(owner.Get("_finalized")) && pyjson.Equal(owner.Get("profile_home"), profileHome)
}

// Live session this transport steers for a session owner, refused when not owned
func connectorSessionOwner(ctx context.Context, rid any, sid string) (*tuihost.Session, map[string]any) {
	_, session := currentSessionSteerAuthority(ctx, sid)
	origin := connectorRPCOriginOf(ctx)
	if session == nil || truthy(session.Get("_finalized")) ||
		origin != nil && (origin.Session != session || !pyjson.Equal(origin.ProfileHome, session.Get("profile_home"))) {
		return nil, connectorRPCError(rid, 4001, "not_owner", "session not found or not owned by this transport")
	}
	return session, nil
}

// Params of a connector verb decoded into its type with the owning live session, refused when they do not fit or the session is not owned
func connectorParams[P, R any](ctx context.Context, rid any, params map[string]any, m tuigateway.Method[P, R], owner func(P) tuigateway.SessionOwner) (P, *tuihost.Session, map[string]any) {
	request, err := m.Params(params)
	if err != nil {
		return request, nil, connectorRefusal(rid, tuihost.ConnectorInvalidParams)
	}
	session, failure := connectorSessionOwner(ctx, rid, owner(request).SessionID)
	return request, session, failure
}

// Refusal while the session's tools leave out manage_connections, nil when it is selected
func sessionConnectorGate(ctx context.Context, env *tools.Env, rid any, session *tuihost.Session) map[string]any {
	var enabled, disabled []string
	if a := sessionAgent(session); a != nil {
		enabled, disabled = a.EnabledToolsets(), a.DisabledToolsets()
	} else {
		enabled = loadEnabledToolsets(ctx, resolveAgentPlatform(sessionSource(session)))
	}
	selected, _ := tools.Default.SelectToolNames(ctx, env, enabled, disabled, tools.SelectionContext{})
	if !selected["manage_connections"] {
		return connectorRefusal(rid, tuihost.ConnectorConnectorsUnavailable)
	}
	return nil
}

// Card retry of the session's open operation, gated on manage_connections and the steering owner
func sessionConnectorRetry(ctx context.Context, env *tools.Env, rid any, request tuigateway.ConnectorsConnectParams, session *tuihost.Session) (map[string]any, error) {
	if gate := sessionConnectorGate(ctx, env, rid, session); gate != nil {
		return gate, nil
	}
	if !connectorOwnerMatches(ctx, request.Owner.SessionID, session, session.Get("profile_home")) {
		return connectorRPCError(rid, 4001, "not_owner", "session ownership changed"), nil
	}
	operation := tools.LiveCurrent(strOr(session.Get("session_key")), strOr(session.Get("profile_home")))
	if operation == nil {
		return connectorRPCError(rid, 4004, "unknown_operation", "No open connection operation for this session."), nil
	}
	return connectorReissue(ctx, rid, operation, request.Connectors)
}

// Runs the named failed rows of the session's open operation again, a connect without reconnect refused
func connectorsConnect(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	request, session, failure := connectorParams(ctx, rid, params, tuigateway.ConnectorsConnect, func(p tuigateway.ConnectorsConnectParams) tuigateway.SessionOwner { return p.Owner })
	if failure != nil {
		return failure, nil
	}
	if !request.Reconnect {
		return connectorRefusal(rid, tuihost.ConnectorInvalidParams), nil
	}
	sid := request.Owner.SessionID
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
		return sessionConnectorRetry(bound, env, rid, request, session)
	}()
	if err != nil {
		return nil, err
	}
	if !connectorOwnerMatches(runtimeCtx, sid, session, profileHome) {
		return connectorRPCError(rid, 4001, "not_owner", "session ownership changed"), nil
	}
	return result, nil
}

// Re runs the named failed rows of an operation, a known refusal reason answered as its refusal
func connectorReissue(ctx context.Context, rid any, operation *tools.ConnectionOperation, names []string) (map[string]any, error) {
	reason, err := mcp.Reissue(ctx, operation, names)
	if err != nil {
		return nil, err
	}
	if spec, known := tuihost.ConnectorReissueRefusals[reason]; known {
		return connectorRefusal(rid, spec), nil
	}
	return ok(rid, connectorOperationView(operation)), nil
}

// Open operation of the owning session named by the request, refused when unknown
func connectorOperationForRequest(rid any, opID string, session *tuihost.Session) (*tools.ConnectionOperation, map[string]any) {
	operation := tools.LiveGet(strOr(session.Get("session_key")), opID, strOr(session.Get("profile_home")))
	if operation == nil {
		return nil, connectorRefusal(rid, tuihost.ConnectorUnknownOperation)
	}
	return operation, nil
}

// Live snapshot of one open session operation
func connectorsOperationStatus(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	request, session, failure := connectorParams(ctx, rid, params, tuigateway.ConnectorsOperationStatus, operationOwner)
	if failure != nil {
		return failure, nil
	}
	operation, failure := connectorOperationForRequest(rid, request.OpID, session)
	if failure != nil {
		return failure, nil
	}
	return ok(rid, connectorOperationView(operation)), nil
}

// Wakes the watcher of one open operation so it reads the accounts now
func connectorsOperationWake(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	request, session, failure := connectorParams(ctx, rid, params, tuigateway.ConnectorsOperationWake, operationOwner)
	if failure != nil {
		return failure, nil
	}
	operation, failure := connectorOperationForRequest(rid, request.OpID, session)
	if failure != nil {
		return failure, nil
	}
	operation.Wake.Set()
	return ok(rid, map[string]any{"status": "ok"}), nil
}

// Owner of a status or wake request
func operationOwner(p tuigateway.ConnectionOperationParams) tuigateway.SessionOwner { return p.Owner }

// Applies the card's per target answer and optional Continue to the open operation
func connectionRespond(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	request, session, failure := connectorParams(ctx, rid, params, tuigateway.ConnectionRespond, func(p tuigateway.ConnectionRespondParams) tuigateway.SessionOwner { return p.Owner })
	if failure != nil {
		return failure, nil
	}
	operation, failure := connectorOperationForRequest(rid, request.OpID, session)
	if failure != nil {
		return failure, nil
	}
	scopeHome := strOr(session.Get("profile_home"))
	if scopeHome == "" {
		scopeHome = importedHome()
	}
	return applyConnectionAnswer(profileRuntimeScope(ctx, scopeHome, true), rid, request.Result, operation)
}

// Hands the answer to the operation, an illegal transition refused as an invalid answer
func applyConnectionAnswer(ctx context.Context, rid any, answer tuigateway.ConnectionAnswer, operation *tools.ConnectionOperation) (map[string]any, error) {
	raw, err := json.Marshal(tuigateway.Plain(answer))
	if err != nil {
		return nil, err
	}
	settled, err := mcp.Respond(ctx, operation, string(raw))
	var illegal *tools.IllegalTransition
	if errors.As(err, &illegal) {
		return connectorRefusal(rid, tuihost.ConnectorInvalidAnswer), nil
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
	return pyjson.Plain(tuihost.ConnectorUIPayload(connectorSnapshotView(operation.Snapshot(true)))).(map[string]any)
}

// Emits one transition or settlement of an operation to its owning session
func connectionUpdate(operation *tools.ConnectionOperation, change, snapshot *pyjson.Object) {
	payload := connectorSnapshotView(snapshot)
	if change != nil {
		for _, key := range change.Keys {
			payload.Set(key, change.Vals[key])
		}
	}
	sid := ""
	for _, live := range tuihost.LiveSessions() {
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
	tuihost.Emit(context.Background(), "connection.update", sid, pyjson.Plain(tuihost.ConnectorUIPayload(payload)).(map[string]any))
}

var connectionUpdateHookOnce sync.Once

// Installs the operation change hook once
func installConnectionUpdateHook() {
	connectionUpdateHookOnce.Do(func() { tools.OperationOnChange = connectionUpdate })
}
