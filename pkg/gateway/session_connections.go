package gateway

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pydantic"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
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
func connectorRefusal(spec tuigateway.ConnectorRefusalSpec, cause error) *ConnectorRefusal {
	detail := spec.Message
	refusal := state.NewRuntimeStoreError(spec.Reason, nil, &detail)
	refusal.Cause = cause
	return &ConnectorRefusal{RuntimeStoreError: refusal, code: spec.Code, message: spec.Message}
}

// Session runner binding a turn's session variables for a retry under the session's own context
type sessionEnvScoper interface {
	SessionEnvScope(ctx context.Context, sc *SessionContext, body func(env *tools.Env) error) error
}

// Whether params name a session owner, the card of a session rather than the account's Connectors page
func sessionOwned(params *pyjson.Object) bool {
	owner, ok := params.GetOr("owner", nil).(*pyjson.Object)
	return ok && pyjson.Equal(owner.GetOr("type", nil), "session")
}

// Connection card verbs of a connection keyed by method name
func connectionHandlers(c *AuthorityConnection) map[string]controlHandler {
	out := map[string]controlHandler{}
	for method := range connectionMethods {
		out[method] = func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return handleConnection(ctx, c, method, params)
		}
	}
	return out
}

// Open operation of a live session as its connection.request payload, nil without one
func pendingConnection(a *SessionAuthority, sessionID string) (*pyjson.Object, error) {
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

// Validated envelope of a connector verb and the separately validated answer of connection.respond
func connectionRequest(method string, params *pyjson.Object) (*pydantic.Instance, *pydantic.Instance, error) {
	respond := method == "connection.respond"
	contract := tuigateway.ConnectionOperationParams
	if !respond {
		contract = tuigateway.Methods[method].Params
	}
	envelope := map[string]any{}
	for _, key := range params.Keys {
		if respond && key == "result" {
			continue
		}
		envelope[key] = pyjson.Plain(params.GetOr(key, nil))
	}
	request, invalid := contract.Validate(envelope)
	if invalid != nil {
		return nil, nil, connectorRefusal(tuigateway.ConnectorInvalidParams, invalid)
	}
	if !respond {
		return request, nil, nil
	}
	answer, invalid := tuigateway.ConnectionAnswer.Validate(pyjson.Plain(params.GetOr("result", nil)))
	if invalid != nil {
		return nil, nil, connectorRefusal(tuigateway.ConnectorInvalidAnswer, invalid)
	}
	return request, answer, nil
}

// Card view of an operation's snapshot with its settled flag
func connectionView(operation *tools.ConnectionOperation) any {
	snapshot := operation.Snapshot(false).Copy()
	snapshot.Set("settled", snapshot.GetOr("settled_at", nil) != nil)
	return tuigateway.ConnectorUIPayload(snapshot)
}

// Text field of a validated contract instance, empty for None
func instanceText(instance *pydantic.Instance, name string) string {
	text, _ := instance.Get(name).(string)
	return text
}

// Answers, polls, wakes or retries the connection operation of one session the caller may drive
func handleConnection(ctx context.Context, c *AuthorityConnection, method string, params *pyjson.Object) (any, error) {
	request, answer, err := connectionRequest(method, params)
	if err != nil {
		return nil, err
	}
	a := c.authority
	owner, _ := request.Get("owner").(*pydantic.Instance)
	ref := SessionRef{ProfileID: a.ProfileID, SessionID: instanceText(owner, "session_id")}
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
		operation = tools.LiveGet(live.Route, instanceText(request, "op_id"), a.ProfileID)
	}
	if operation == nil {
		return nil, connectorRefusal(tuigateway.ConnectorUnknownOperation, nil)
	}
	switch method {
	case "connectors.operation.status":
		return connectionView(operation), nil
	case "connectors.operation.wake":
		operation.Wake.Set()
		return pyjson.ObjectOf("status", "ok"), nil
	case "connection.respond":
		return forwardOffThread(ctx, func() (*pyjson.Object, error) { return respondConnection(ctx, operation, answer) })
	}
	return forwardOffThread(ctx, func() (any, error) { return retryConnection(ctx, a, ref, operation, request) })
}

// Settles the operation with the client's answer, an illegal transition refused as an invalid answer
func respondConnection(ctx context.Context, operation *tools.ConnectionOperation, answer *pydantic.Instance) (*pyjson.Object, error) {
	settled, err := mcp.Respond(ctx, operation, answer.DumpJSON(pydantic.DumpOptions{ExcludeNone: true}))
	var illegal *tools.IllegalTransition
	if errors.As(err, &illegal) {
		return nil, connectorRefusal(tuigateway.ConnectorInvalidAnswer, err)
	}
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("status", "ok", "settled", settled), nil
}

// The card's Try again under the session's own context, re-minting only the named targets of this account
func retryConnection(ctx context.Context, a *SessionAuthority, ref SessionRef, operation *tools.ConnectionOperation, request *pydantic.Instance) (any, error) {
	if !pyrepr.Truthy(request.Get("reconnect")) {
		return nil, connectorRefusal(tuigateway.ConnectorInvalidParams, nil)
	}
	names := []string{}
	for _, item := range pyjson.Plain(request.Get("connectors")).([]any) {
		names = append(names, pyrepr.Str(item))
	}
	alias, connectionID := request.Get("alias"), request.Get("connection_id")
	for _, name := range names {
		target := operation.Target(name)
		if target == nil {
			continue
		}
		aliasText, _ := alias.(string)
		cid, _ := connectionID.(string)
		if (alias != nil && (target.Alias == nil || *target.Alias != aliasText)) ||
			(connectionID != nil && !(target.RepairID != nil && *target.RepairID == cid) && !(target.ConnectionID != "" && target.ConnectionID == cid)) {
			return nil, connectorRefusal(tuigateway.ConnectorUnknownTargetRefusal, nil)
		}
	}
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
	session := &SessionContext{Source: live.Source, ConnectedPlatforms: []Platform{}, HomeChannels: pydict.New[Platform, *HomeChannel](),
		SessionKey: live.Route, SessionID: target}
	var reason string
	err = scoper.SessionEnvScope(ctx, session, func(env *tools.Env) error {
		if err := requireConnectors(ctx, a, ref, env); err != nil {
			return err
		}
		var reissueErr error
		reason, reissueErr = mcp.Reissue(ctx, env, operation, names, "")
		if reissueErr == nil {
			return nil
		}
		var refusal *state.RuntimeStoreError
		if errors.As(reissueErr, &refusal) {
			return reissueErr
		}
		var gatewayErr *tools.ToolGatewayError
		if errors.As(reissueErr, &gatewayErr) && gatewayErr.Kind == "auth" {
			return connectorRefusal(tuigateway.ConnectorAuthRefusal(gatewayErr.Code, gatewayErr.Status), reissueErr)
		}
		return connectorRefusal(tuigateway.ConnectorRequestFailed, reissueErr)
	})
	if err != nil {
		return nil, err
	}
	if reason != "" {
		spec, known := tuigateway.ConnectorReissueRefusals[reason]
		if !known {
			spec = tuigateway.ConnectorReissueRefused
		}
		return nil, connectorRefusal(spec, nil)
	}
	return connectionView(operation), nil
}

// Refuses a retry unless connectors are on and the session's tools include manage_connections
func requireConnectors(ctx context.Context, a *SessionAuthority, ref SessionRef, env *tools.Env) error {
	enabled, disabled, err := sessionToolsets(ctx, a, ref)
	if err != nil {
		return err
	}
	selected, _ := tools.Default.SelectToolNames(ctx, env, enabled, disabled, tools.SelectionContext{})
	if !selected["manage_connections"] || !tools.ConnectorsAvailable(env) {
		return connectorRefusal(tuigateway.ConnectorConnectorsUnavailable, nil)
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
	if !pyrepr.Truthy(agentSection) {
		agentSection = map[string]any{}
	}
	raw, err := pyGet(agentSection, "disabled_toolsets", nil)
	if err != nil {
		return nil, nil, err
	}
	platform := "cli"
	if policy != nil {
		platform = policy.Platform
	}
	return enabled, tools.SessionDisabledToolsets(tools.ParseConfigStringList(raw), platform, policy != nil && policy.Source == "oneshot"), nil
}
