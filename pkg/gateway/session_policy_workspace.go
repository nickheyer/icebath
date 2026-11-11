package gateway

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Registers the frozen workspace as the docker terminal cwd of the turn task, unbinding it off a host mount
func BindTurnWorkspace(env *tools.Env, policy *LocalSessionPolicy, taskID string) error {
	if policy == nil || taskID == "" {
		return nil
	}
	decoded, err := pyjson.Loads(policy.TerminalJSON)
	if err != nil {
		return err
	}
	obj, isDict := decoded.(*pyjson.Object)
	if !isDict {
		return &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(decoded) + "' object has no attribute 'get'"}
	}
	terminal := pyjson.Plain(obj).(map[string]any)
	if strings.ToLower(strings.TrimSpace(routeStr(terminal["TERMINAL_ENV"]))) != "docker" || !terminalUsesHostCwd(terminal) {
		tools.UnbindTurnWorkspace(env, taskID)
		return nil
	}
	current, err := tools.ResolveTaskOverrides(env, taskID)
	if err != nil {
		return err
	}
	if cwd, ok := current["cwd"].(string); ok && cwd == policy.Cwd && current["cwd_source"] == "session" {
		return nil
	}
	overrides := make(map[string]any, len(current)+3)
	for k, v := range current {
		overrides[k] = v
	}
	overrides["cwd"] = policy.Cwd
	overrides["cwd_source"] = "session"
	overrides["workspace_key"] = tools.WorkspaceKeyFor(policy.Cwd)
	return tools.RegisterTaskEnvOverrides(env, taskID, overrides)
}
