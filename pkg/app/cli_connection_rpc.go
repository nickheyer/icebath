package app

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

// connectorRefusal is a connector RPC refusal, its message the gateway error text the card shows
type connectorRefusal struct{ message string }

func (r *connectorRefusal) Error() string { return r.message }

// Connector refusals of tui_gateway/connector_refusals.py
var (
	connectorInvalidAnswer        = &connectorRefusal{"Connection answer is invalid."}
	connectorUnknownOperation     = &connectorRefusal{"No open operation with that op_id."}
	connectorUnknownTargetRefusal = &connectorRefusal{"No such target on the open operation."}
	connectorsUnavailableRefusal  = &connectorRefusal{"Connectors are not available in this session."}
	connectorRequestFailed        = &connectorRefusal{"Connector request failed. Try again explicitly."}
	connectorNoOpenOperation      = &connectorRefusal{"No open connection operation for this session."}
	connectorReissueRefusals      = map[string]*connectorRefusal{
		tools.ConnUnknownTarget:  connectorUnknownTargetRefusal,
		tools.ConnMixedKinds:     {"One target kind per request."},
		tools.ConnLinkStillValid: {"Reopen the stored link."},
		tools.ConnSettled:        {"The operation has settled."},
		tools.ConnNotAllowed:     {"This target cannot be run again."},
		tools.ConnRefused:        {"The target cannot be run again."},
	}
)

// Refusal for a gateway authentication failure, the auth_refusal mapping
func connectorAuthRefusal(e *tools.ToolGatewayError) *connectorRefusal {
	switch {
	case e.Code == "no_access" || e.Code == "ORG_ACCESS_DENIED":
		return &connectorRefusal{"This account cannot manage connectors for this organization."}
	case e.Status == 401 || e.Code == "invalid_token" || e.Code == "INVALID_TOKEN" || e.Code == "NO_TOKEN":
		return &connectorRefusal{"Sign in to use connectors."}
	}
	return &connectorRefusal{"Connector access is not permitted for this account."}
}

// The _connector_guard of a connector RPC, an auth failure its refusal and any other failure a request failure
func connectorGuard(err error) error {
	if err == nil {
		return nil
	}
	var refusal *connectorRefusal
	if errors.As(err, &refusal) {
		return refusal
	}
	var gatewayErr *tools.ToolGatewayError
	if errors.As(err, &gatewayErr) && gatewayErr.Kind == "auth" {
		return connectorAuthRefusal(gatewayErr)
	}
	return connectorRequestFailed
}

// Exact keys of a forbid extra model, false when any other key is present
func connectionOnlyKeys(m map[string]any, allowed ...string) bool {
	for k := range m {
		ok := false
		for _, a := range allowed {
			if k == a {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// String or None field of a model, false for any other type
func connectionOptionalString(m map[string]any, key string) (string, bool, bool) {
	v, present := m[key]
	if !present || v == nil {
		return "", false, true
	}
	s, ok := v.(string)
	return s, true, ok
}

// The ConnectionAnswer model validated and dumped as JSON with None fields left out, false when it does not validate
func connectionAnswerJSON(result any) (string, bool) {
	m, ok := pyjson.Plain(result).(map[string]any)
	if !ok || !connectionOnlyKeys(m, "targets", "settled_by", "said") {
		return "", false
	}
	out := pyjson.NewObject()
	targets := []any{}
	if raw, present := m["targets"]; present {
		rows, ok := raw.([]any)
		if !ok {
			return "", false
		}
		for _, row := range rows {
			t, ok := row.(map[string]any)
			if !ok || !connectionOnlyKeys(t, "name", "status", "detail", "env") {
				return "", false
			}
			name, ok := t["name"].(string)
			if !ok {
				return "", false
			}
			status, ok := t["status"].(string)
			if !ok || (status != "approved" && status != "skipped") {
				return "", false
			}
			target := pyjson.NewObject()
			target.Set("name", name)
			target.Set("status", status)
			detail, hasDetail, ok := connectionOptionalString(t, "detail")
			if !ok {
				return "", false
			}
			if hasDetail {
				target.Set("detail", detail)
			}
			if rawEnv, present := t["env"]; present && rawEnv != nil {
				envMap, ok := rawEnv.(map[string]any)
				if !ok {
					return "", false
				}
				env := pyjson.NewObject()
				for k, v := range envMap {
					s, ok := v.(string)
					if !ok {
						return "", false
					}
					env.Set(k, s)
				}
				target.Set("env", env)
			}
			targets = append(targets, target)
		}
	}
	out.Set("targets", targets)
	settledBy, hasSettledBy, ok := connectionOptionalString(m, "settled_by")
	if !ok {
		return "", false
	}
	if hasSettledBy {
		switch settledBy {
		case "all_resolved", "continue", "deadline", "interrupt":
			out.Set("settled_by", settledBy)
		default:
			return "", false
		}
	}
	said, hasSaid, ok := connectionOptionalString(m, "said")
	if !ok {
		return "", false
	}
	if hasSaid {
		out.Set("said", said)
	}
	text, err := pyjson.Dumps(out, pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
	if err != nil {
		return "", false
	}
	return text, true
}

// Tool selection and connector availability the retry gate reads, swapped by tests as hermes patches model_tools and connectors
var (
	connectionSelectToolNames = func(ctx context.Context, env *tools.Env, enabled, disabled []string) map[string]bool {
		selected, _ := tools.Default.SelectToolNames(ctx, env, enabled, disabled, tools.SelectionContext{})
		return selected
	}
	connectionConnectorsAvailable = tools.ConnectorsAvailable
)

// Session key the running agent's connector operations are registered under
func connectionSessionKey(c *CLI) string {
	return tools.OperationSessionKey(c.agent.ToolEnv(), c.agent.SessionID())
}

// Answers the open connection operation of this session in process, the connection.respond method
func agentConnectionRespond(ctx context.Context, c *CLI, opID string, result map[string]any) error {
	raw, ok := connectionAnswerJSON(result)
	if !ok {
		return connectorInvalidAnswer
	}
	op := tools.LiveGet(connectionSessionKey(c), opID, "")
	if op == nil {
		return connectorUnknownOperation
	}
	_, err := mcp.Respond(ctx, op, raw)
	var illegal *tools.IllegalTransition
	if errors.As(err, &illegal) {
		return connectorInvalidAnswer
	}
	return connectorGuard(err)
}

// Re-mints the failed or expired row name on this session's open operation in process, the connectors.connect reconnect
func agentConnectionRetry(ctx context.Context, c *CLI, name string) error {
	env := c.agent.ToolEnv()
	selected := connectionSelectToolNames(ctx, env, c.agent.EnabledToolsets(), c.agent.DisabledToolsets())
	if !selected["manage_connections"] || !connectionConnectorsAvailable(env) {
		return connectorsUnavailableRefusal
	}
	op := tools.LiveCurrent(connectionSessionKey(c), "")
	if op == nil {
		return connectorNoOpenOperation
	}
	reason, err := mcp.Reissue(ctx, env, op, []string{name}, "")
	if err != nil {
		return connectorGuard(err)
	}
	if refusal, ok := connectorReissueRefusals[reason]; ok {
		return refusal
	}
	return nil
}
