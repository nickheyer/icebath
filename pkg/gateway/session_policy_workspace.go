package gateway

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Registers the frozen workspace as the docker terminal cwd of the turn task, unbinding it off a host mount
func BindTurnWorkspace(env *tools.Env, policy *LocalSessionPolicy, taskID string) error {
	if policy == nil || taskID == "" {
		return nil
	}
	decoded, err := jsonutil.Decode(policy.TerminalJSON)
	if err != nil {
		return err
	}
	obj, isDict := decoded.(*jsonutil.Object)
	if !isDict {
		return &PyError{Kind: "AttributeError", Msg: "the turn workspace record is " + errchain.Kind(decoded) + ", expected a mapping"}
	}
	terminal := jsonutil.Plain(obj).(map[string]any)
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
