package gateway

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
)

// Verbs of a session's connection card
var connectionMethods = map[string]bool{"connection.respond": true, "connectors.operation.status": true,
	"connectors.operation.wake": true, "connectors.connect": true}

// Refusal on the legacy connector handlers' wire shape, a store refusal carrying their code and message
type ConnectorRefusal struct {
	*state.RuntimeStoreError
	code    int
	message string
}

func (r *ConnectorRefusal) WireCode() int       { return r.code }
func (r *ConnectorRefusal) WireMessage() string { return r.message }
func (r *ConnectorRefusal) Unwrap() error       { return r.RuntimeStoreError }

// Store refusal of a connector refusal spec with the cause that raised it
func connectorRefusal(spec tuihost.ConnectorRefusalSpec, cause error) *ConnectorRefusal {
	detail := spec.Message
	refusal := state.NewRuntimeStoreError(string(spec.Reason), nil, &detail)
	refusal.Cause = cause
	return &ConnectorRefusal{RuntimeStoreError: refusal, code: spec.Code, message: spec.Message}
}

// Session runner binding a turn's session variables for a retry under the session's own context
type sessionEnvScoper interface {
	SessionEnvScope(ctx context.Context, sc *SessionContext, body func(env *tools.Env) error) error
}

// Whether params name a session owner, the only owner a connection card verb has
func sessionOwned(params *jsonutil.Object) bool {
	owner, ok := params.ValueOr("owner", nil).(*jsonutil.Object)
	return ok && anyutil.Equal(owner.ValueOr("type", nil), "session")
}

// Connection card verbs of a connection keyed by method name
func connectionHandlers(c *AuthorityConnection) map[string]controlHandler {
	out := map[string]controlHandler{}
	for method := range connectionMethods {
		out[method] = func(ctx context.Context, ref SessionRef, params *jsonutil.Object) (any, error) {
			return handleConnection(ctx, c, method, params)
		}
	}
	return out
}

// Open operation of a live session as its connection.request payload, nil without one
func pendingConnection(a *SessionAuthority, sessionID string) (*jsonutil.Object, error) {
	live, err := liveSession(a, sessionID)
	if err != nil {
		return nil, err
	}
	operation := tools.LiveCurrent(live.Route, a.ProfileID)
	if operation == nil {
		return nil, nil
	}
	return operation.RequestPayload(), nil
}

// Decoded params of a connector verb, its owner and the operation, answer or connector names it carries
type connectionVerb struct {
	owner      tuigateway.SessionOwner
	opID       string
	answer     tuigateway.ConnectionAnswer
	connectors []string
	reconnect  bool
}

// Params of a connector verb decoded into its declared type, refused as invalid params when they do not fit
func connectionRequest(method string, params *jsonutil.Object) (connectionVerb, error) {
	var verb connectionVerb
	var err error
	switch method {
	case tuigateway.ConnectionRespond.Name():
		var p tuigateway.ConnectionRespondParams
		p, err = tuigateway.ConnectionRespond.Params(params)
		verb = connectionVerb{owner: p.Owner, opID: p.OpID, answer: p.Result}
	case tuigateway.ConnectorsConnect.Name():
		var p tuigateway.ConnectorsConnectParams
		p, err = tuigateway.ConnectorsConnect.Params(params)
		verb = connectionVerb{owner: p.Owner, connectors: p.Connectors, reconnect: p.Reconnect}
	case tuigateway.ConnectorsOperationWake.Name():
		var p tuigateway.ConnectionOperationParams
		p, err = tuigateway.ConnectorsOperationWake.Params(params)
		verb = connectionVerb{owner: p.Owner, opID: p.OpID}
	default:
		var p tuigateway.ConnectionOperationParams
		p, err = tuigateway.ConnectorsOperationStatus.Params(params)
		verb = connectionVerb{owner: p.Owner, opID: p.OpID}
	}
	if err != nil {
		return verb, connectorRefusal(tuihost.ConnectorInvalidParams, err)
	}
	return verb, nil
}

// Card view of an operation's snapshot with its settled flag
func connectionView(operation *tools.ConnectionOperation) any {
	snapshot := operation.Snapshot(false).Clone()
	snapshot.Set("settled", snapshot.ValueOr("settled_at", nil) != nil)
	return tuihost.ConnectorUIPayload(snapshot)
}

// Answers, polls, wakes or retries the connection operation of one session the caller may drive
func handleConnection(ctx context.Context, c *AuthorityConnection, method string, params *jsonutil.Object) (any, error) {
	verb, err := connectionRequest(method, params)
	if err != nil {
		return nil, err
	}
	a := c.authority
	ref := SessionRef{ProfileID: a.ProfileID, SessionID: verb.owner.SessionID}
	capability := "session:control"
	if method == "connectors.operation.status" {
		capability = "session:read"
	}
	if err := authorizeBusy(ctx, a, c.actor, ref, params, capability); err != nil {
		return nil, err
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, runtimeRefusal("not_found")
	}
	var operation *tools.ConnectionOperation
	if method == "connectors.connect" {
		operation = tools.LiveCurrent(live.Route, a.ProfileID)
	} else {
		operation = tools.LiveGet(live.Route, verb.opID, a.ProfileID)
	}
	if operation == nil {
		return nil, connectorRefusal(tuihost.ConnectorUnknownOperation, nil)
	}
	switch method {
	case "connectors.operation.status":
		return connectionView(operation), nil
	case "connectors.operation.wake":
		operation.Wake.Set()
		return jsonutil.ObjectOf("status", "ok"), nil
	case "connection.respond":
		return forwardOffThread(ctx, func() (*jsonutil.Object, error) { return respondConnection(ctx, operation, verb.answer) })
	}
	return forwardOffThread(ctx, func() (any, error) { return retryConnection(ctx, a, ref, operation, verb) })
}

// Settles the operation with the client's answer, an illegal transition refused as an invalid answer
func respondConnection(ctx context.Context, operation *tools.ConnectionOperation, answer tuigateway.ConnectionAnswer) (*jsonutil.Object, error) {
	raw, err := json.Marshal(tuigateway.Plain(answer))
	if err != nil {
		return nil, err
	}
	settled, err := mcp.Respond(ctx, operation, string(raw))
	var illegal *tools.IllegalTransition
	if errors.As(err, &illegal) {
		return nil, connectorRefusal(tuihost.ConnectorInvalidAnswer, err)
	}
	if err != nil {
		return nil, err
	}
	return jsonutil.ObjectOf("status", "ok", "settled", settled), nil
}

// The card's Try again under the session's own context, re-running only the named targets
func retryConnection(ctx context.Context, a *SessionAuthority, ref SessionRef, operation *tools.ConnectionOperation, verb connectionVerb) (any, error) {
	if !verb.reconnect {
		return nil, connectorRefusal(tuihost.ConnectorInvalidParams, nil)
	}
	names := append([]string{}, verb.connectors...)
	scoper, ok := a.Runner.(sessionEnvScoper)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: "runner has no attribute '_session_env_scope'"}
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	target, err := physicalTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	session := &SessionContext{Source: live.Source, ConnectedPlatforms: []Platform{}, HomeChannels: orderedutil.NewMap[Platform, *HomeChannel](),
		SessionKey: live.Route, SessionID: target}
	var reason string
	err = scoper.SessionEnvScope(ctx, session, func(env *tools.Env) error {
		if err := requireConnectors(ctx, a, ref, env); err != nil {
			return err
		}
		var reissueErr error
		reason, reissueErr = mcp.Reissue(ctx, operation, names)
		if reissueErr == nil {
			return nil
		}
		var refusal *state.RuntimeStoreError
		if errors.As(reissueErr, &refusal) {
			return reissueErr
		}
		return connectorRefusal(tuihost.ConnectorRequestFailed, reissueErr)
	})
	if err != nil {
		return nil, err
	}
	if reason != "" {
		spec, known := tuihost.ConnectorReissueRefusals[reason]
		if !known {
			spec = tuihost.ConnectorReissueRefused
		}
		return nil, connectorRefusal(spec, nil)
	}
	return connectionView(operation), nil
}

// Refuses a retry unless the session's tools include manage_connections
func requireConnectors(ctx context.Context, a *SessionAuthority, ref SessionRef, env *tools.Env) error {
	enabled, disabled, err := sessionToolsets(ctx, a, ref)
	if err != nil {
		return err
	}
	selected, _ := tools.Default.SelectToolNames(ctx, env, enabled, disabled, tools.SelectionContext{})
	if !selected["manage_connections"] {
		return connectorRefusal(tuihost.ConnectorConnectorsUnavailable, nil)
	}
	return nil
}

// Enabled and disabled toolsets of the session's resident agent or else its frozen policy
func sessionToolsets(ctx context.Context, a *SessionAuthority, ref SessionRef) ([]string, []string, error) {
	agent, err := a.agent(ref)
	if err != nil {
		return nil, nil, err
	}
	if !nilAgent(agent) {
		if carrier, ok := agent.(toolsetCarrier); ok {
			return carrier.EnabledToolsets(), carrier.DisabledToolsets(), nil
		}
		return nil, nil, nil
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, nil, err
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, nil, err
	}
	cfg := map[string]any{}
	var enabled []string
	if policy != nil {
		if cfg, err = policy.config(ctx, a, false); err != nil {
			return nil, nil, err
		}
		if enabled, err = policy.currentToolsets(ctx, cfg); err != nil {
			return nil, nil, err
		}
	}
	agentSection := cfg["agent"]
	if !anyutil.IsTrue(agentSection) {
		agentSection = map[string]any{}
	}
	raw, err := pyGet(agentSection, "disabled_toolsets", nil)
	if err != nil {
		return nil, nil, err
	}
	return enabled, tools.SessionDisabledToolsets(tools.ParseConfigStringList(raw), policy != nil && policy.Source == "oneshot"), nil
}
