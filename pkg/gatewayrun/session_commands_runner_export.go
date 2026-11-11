package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Operator slash access refusal text for the command, empty when allowed
func (r *GatewayRunner) LocalSlashAccess(ctx context.Context, source *gateway.SessionSource, canonical string) string {
	return r.checkSlashAccess(ctx, source, canonical)
}

// Runs an exec quick command in a local session's folder with a sanitized env and redacted output
func (r *GatewayRunner) LocalExecQuickCommand(ctx context.Context, command, shell, cwd string) (string, error) {
	return RunGatewayExecQuickCommand(ctx, tools.ToolEnvFrom(ctx), shell, cwd)
}

// Runs a plugin command handler under the session env of the source with the argument as typed
func (r *GatewayRunner) LocalPluginCommand(ctx context.Context, handler plugins.PluginCommandHandler, event *gateway.MessageEvent, source *gateway.SessionSource, entry *gateway.SessionEntry, args string) (any, error) {
	reply, err := r.hmRunPluginHandler(ctx, handler, event, source, entry, &args)
	if err != nil || reply == nil {
		return nil, err
	}
	return *reply, nil
}

// Mid run dispatch of a recognized command, its variant, its handler or the rejection text
func (r *GatewayRunner) LocalBusySlashCommand(ctx context.Context, event *gateway.MessageEvent, def gateway.RegistryCommand, quickKey string, source *gateway.SessionSource) (any, error) {
	reply, err := r.dispatchBusySlashCommand(ctx, event, busyCommandDef{name: def.Name, busyPolicy: def.BusyPolicy, busyHandler: def.BusyHandler}, quickKey, source)
	return commandReply(reply), err
}

// Idle path rewrite of a prompt command, false with the rewritten event text when it becomes a turn
func (r *GatewayRunner) LocalPromptCommand(ctx context.Context, canonical string, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	var run func(context.Context, *gateway.MessageEvent, *gateway.SessionSource, string) (bool, any, error)
	switch canonical {
	case "plan":
		run = r.hmCmdPlan
	case "learn":
		run = r.hmCmdLearn
	case "init":
		run = r.hmCmdInit
	case "moa":
		run = r.hmCmdMoa
	case "blueprint":
		run = r.hmCmdBlueprint
	case "egress":
		run = r.hmCmdEgress
	default:
		return false, nil, &missingRunnerMethod{"_hm_cmd_" + canonical}
	}
	handled, reply, err := run(ctx, event, source, quickKey)
	return handled, commandReply(reply), err
}

// Slash handler of a canonical name as the handler table resolves it, nil when the runner has none
func (r *GatewayRunner) LocalCommandHandler(canonical string) func(ctx context.Context, event *gateway.MessageEvent) (any, error) {
	handler := r.commandHandlerTable([]string{canonical})[canonical]
	if handler == nil {
		return nil
	}
	return func(ctx context.Context, event *gateway.MessageEvent) (any, error) {
		reply, err := handler(ctx, event)
		return commandReply(reply), err
	}
}

// Value and global flag of reasoning command arguments
func (r *GatewayRunner) ParseReasoningCommandArgs(arg string) (string, bool) {
	return parseReasoningCommandArgs(arg)
}

// Goal manager and session entry of the event's session
func (r *GatewayRunner) GoalManagerForEvent(ctx context.Context, event *gateway.MessageEvent) (*goals.GoalManager, *gateway.SessionEntry) {
	return r.getGoalManagerForEvent(ctx, event)
}

// Python AttributeError of a runner method a prompt command names that the runner lacks
type missingRunnerMethod struct{ name string }

func (e *missingRunnerMethod) Error() string {
	return "AttributeError: 'GatewayRunner' object has no attribute '" + e.name + "'"
}

// Goal turn budget of the user config, the default when unset or invalid
func (r *GatewayRunner) GoalMaxTurnsFromConfig(ctx context.Context) int64 {
	return r.goalMaxTurnsFromConfig(ctx)
}
