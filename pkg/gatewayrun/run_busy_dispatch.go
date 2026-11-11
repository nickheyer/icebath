package gatewayrun

import (
	"context"
	"fmt"
	"slices"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Slash command handler reply, nil, a string or an ephemeral reply
type commandHandler func(ctx context.Context, event *gateway.MessageEvent) (any, error)

// Reply of a handler as the dispatcher returns it, a nil pointer reading as None
func commandReply(reply any) any {
	switch typed := reply.(type) {
	case *string:
		if typed == nil {
			return nil
		}
		return *typed
	case *gateway.EphemeralReply:
		if typed == nil {
			return nil
		}
	}
	return reply
}

// Handler of any reply type as a dispatcher handler
func handlerOf[T any](fn func(ctx context.Context, event *gateway.MessageEvent) (T, error)) commandHandler {
	return func(ctx context.Context, event *gateway.MessageEvent) (any, error) {
		reply, err := fn(ctx, event)
		if err != nil {
			return nil, err
		}
		return commandReply(reply), nil
	}
}

// Ordinary slash handlers shared by idle and busy dispatch, in their declared order
var plainCommands = []string{
	"status", "context", "restart", "approve", "deny", "pause", "discard", "agents", "bg", "btw",
	"kanban", "subgoal", "heartbeat", "busy", "yolo", "verbose", "footer", "help",
	"commands", "profile", "login", "update", "version", "autoprune",
}

// Slash handlers dispatched only on the idle path
var idleCommands = []string{
	"topic", "whoami", "platform", "stop", "reasoning", "memory", "skills", "fast",
	"approvals", "model", "codex-runtime", "personality", "suggestions", "save", "retry",
	"sethome", "compress", "usage", "topup", "insights", "reload-mcp", "reload-skills",
	"bundles", "debug", "title", "resume", "sessions", "branch", "rollback", "diff", "goal",
	"loop", "refine", "review", "voice", "curator",
}

// The handler method of a slash name, its handle name command method except the bg and sethome aliases
func (r *GatewayRunner) commandHandlerFor(name string) commandHandler {
	switch name {
	case "status":
		return handlerOf(r.handleStatusCommand)
	case "context":
		return handlerOf(r.handleContextCommand)
	case "restart":
		return handlerOf(r.handleRestartCommand)
	case "approve":
		return handlerOf(r.handleApproveCommand)
	case "deny":
		return handlerOf(r.handleDenyCommand)
	case "pause":
		return handlerOf(r.handlePauseCommand)
	case "discard":
		return handlerOf(r.handleDiscardCommand)
	case "agents":
		return handlerOf(r.handleAgentsCommand)
	case "bg":
		return handlerOf(r.handleBackgroundCommand)
	case "btw":
		return handlerOf(r.handleBtwCommand)
	case "kanban":
		return handlerOf(r.handleKanbanCommand)
	case "subgoal":
		return handlerOf(r.handleSubgoalCommand)
	case "heartbeat":
		return handlerOf(r.handleHeartbeatCommand)
	case "busy":
		return handlerOf(r.handleBusyCommand)
	case "yolo":
		return handlerOf(r.handleYoloCommand)
	case "verbose":
		return handlerOf(r.handleVerboseCommand)
	case "footer":
		return handlerOf(r.handleFooterCommand)
	case "help":
		return handlerOf(r.handleHelpCommand)
	case "commands":
		return handlerOf(r.handleCommandsCommand)
	case "profile":
		return handlerOf(r.handleProfileCommand)
	case "login":
		return handlerOf(r.handleLoginCommand)
	case "update":
		return handlerOf(r.handleUpdateCommand)
	case "version":
		return handlerOf(r.handleVersionCommand)
	case "autoprune":
		return handlerOf(r.handleAutopruneCommand)
	case "topic":
		return handlerOf(r.handleTopicCommand)
	case "whoami":
		return handlerOf(r.handleWhoamiCommand)
	case "platform":
		return handlerOf(r.handlePlatformCommand)
	case "stop":
		return handlerOf(r.handleStopCommand)
	case "reasoning":
		return handlerOf(r.handleReasoningCommand)
	case "memory":
		return handlerOf(r.handleMemoryCommand)
	case "skills":
		return handlerOf(r.handleSkillsCommand)
	case "fast":
		return handlerOf(r.handleFastCommand)
	case "approvals":
		return handlerOf(r.handleApprovalsCommand)
	case "model":
		return handlerOf(r.handleModelCommand)
	case "codex-runtime":
		return handlerOf(r.handleCodexRuntimeCommand)
	case "personality":
		return handlerOf(r.handlePersonalityCommand)
	case "suggestions":
		return handlerOf(r.handleSuggestionsCommand)
	case "save":
		return handlerOf(r.handleSaveCommand)
	case "retry":
		return handlerOf(r.handleRetryCommand)
	case "sethome":
		return handlerOf(r.handleSetHomeCommand)
	case "compress":
		return handlerOf(r.handleCompressCommand)
	case "usage":
		return handlerOf(r.handleUsageCommand)
	case "topup":
		return handlerOf(r.handleTopupCommand)
	case "insights":
		return handlerOf(r.handleInsightsCommand)
	case "reload-mcp":
		return handlerOf(r.handleReloadMCPCommand)
	case "reload-skills":
		return handlerOf(r.handleReloadSkillsCommand)
	case "bundles":
		return handlerOf(r.handleBundlesCommand)
	case "debug":
		return handlerOf(r.handleDebugCommand)
	case "title":
		return handlerOf(r.handleTitleCommand)
	case "resume":
		return handlerOf(r.handleResumeCommand)
	case "sessions":
		return handlerOf(r.handleSessionsCommand)
	case "branch":
		return handlerOf(r.handleBranchCommand)
	case "rollback":
		return handlerOf(r.handleRollbackCommand)
	case "diff":
		return handlerOf(r.handleDiffCommand)
	case "goal":
		return handlerOf(r.handleGoalCommand)
	case "loop":
		return handlerOf(r.handleLoopCommand)
	case "refine":
		return handlerOf(r.handleRefineCommand)
	case "review":
		return handlerOf(r.handleReviewCommand)
	case "voice":
		return handlerOf(r.handleVoiceCommand)
	case "curator":
		return handlerOf(r.handleCuratorCommand)
	}
	return nil
}

// Slash name to handler for each of names
func (r *GatewayRunner) commandHandlerTable(names []string) map[string]commandHandler {
	table := make(map[string]commandHandler, len(names))
	for _, name := range names {
		table[name] = r.commandHandlerFor(name)
	}
	return table
}

// Ordinary slash handlers shared by idle and busy dispatch
func (r *GatewayRunner) gatewayPlainCommandHandlers() map[string]commandHandler {
	return r.commandHandlerTable(plainCommands)
}

// Slash handlers dispatched only on the idle path
func (r *GatewayRunner) gatewayIdleCommandHandlers() map[string]commandHandler {
	return r.commandHandlerTable(idleCommands)
}

// Busy handler keys of a command definition with a mid run variant method
var busySpecialHandlers = map[string]bool{"start": true, "stop": true, "new": true, "queue": true, "steer": true, "egress": true, "goal": true, "loop": true}

// Busy handler keys whose mid run reply is a fixed rejection text key
var busyRejectText = map[string]string{"model": "gateway.busy.reject_model", "codex-runtime": "gateway.busy.reject_codex_runtime", "moa": "gateway.busy.reject_moa"}

// Registered command as mid run dispatch reads it
type busyCommandDef struct {
	name, busyPolicy, busyHandler string
}

// Mid run route of a command, special with its variant key, plain with its handler, or reject with its text key
func (r *GatewayRunner) busySlashRoute(def busyCommandDef) (string, string, commandHandler) {
	policy := def.busyPolicy
	if policy == "" {
		policy = "reject"
	}
	if def.busyHandler != "" {
		if busySpecialHandlers[def.busyHandler] {
			return "special", def.busyHandler, nil
		}
		if rejectKey, ok := busyRejectText[def.busyHandler]; ok {
			return "reject", rejectKey, nil
		}
	}
	if policy == "dispatch" || policy == "interrupt_then_dispatch" {
		if plain := r.gatewayPlainCommandHandlers()[def.name]; plain != nil {
			return "plain", "", plain
		}
		runLog().Warn(fmt.Sprintf("busy_policy=%s for /%s has no mid-run handler — falling back to busy-reject", policy, def.name))
	}
	return "reject", "", nil
}

// Dispatches a recognized slash command while an agent runs, its mid run variant, its handler or a rejection
func (r *GatewayRunner) dispatchBusySlashCommand(ctx context.Context, event *gateway.MessageEvent, def busyCommandDef, quickKey string, source *gateway.SessionSource) (any, error) {
	kind, target, plain := r.busySlashRoute(def)
	switch kind {
	case "special":
		switch target {
		case "start":
			return r.busyStartCommand(ctx, event, quickKey, source)
		case "stop":
			return r.busyStopCommand(ctx, event, quickKey, source)
		case "new":
			return r.busyNewCommand(ctx, event, quickKey, source)
		case "queue":
			return r.busyQueueCommand(ctx, event, quickKey, source)
		case "steer":
			return r.busySteerCommand(ctx, event, quickKey, source)
		case "egress":
			return r.busyEgressCommand(ctx, event, quickKey, source)
		case "goal":
			return r.busyGoalCommand(ctx, event, quickKey, source)
		case "loop":
			return r.busyLoopCommand(ctx, event, quickKey, source)
		}
	case "plain":
		if err := busyAccepted(ctx); err != nil {
			return nil, err
		}
		scoped, err := r.asyncProfileScopeForSource(ctx, source)
		if err != nil {
			return nil, err
		}
		return plain(scoped, event)
	}
	if target != "" {
		return i18n.T(ctx, target, "", nil), nil
	}
	return i18n.T(ctx, "gateway.busy.slash_rejected", "", map[string]any{"command": def.name}), nil
}

// Interrupts the running agent and clears the pending queue before resetting the session
func (r *GatewayRunner) busyNewCommand(ctx context.Context, event *gateway.MessageEvent, quickKey string, source *gateway.SessionSource) (any, error) {
	if err := busyAccepted(ctx); err != nil {
		return nil, err
	}
	if err := r.interruptAndClearSession(ctx, quickKey, source, interruptReasonReset, "new_command", true); err != nil {
		return nil, err
	}
	return handlerOf(r.handleResetCommand)(ctx, event)
}

// Runs a goal control verb mid run, rejecting new goal text
func (r *GatewayRunner) busyGoalCommand(ctx context.Context, event *gateway.MessageEvent, quickKey string, source *gateway.SessionSource) (any, error) {
	if goals.IsGoalControl(event.GetCommandArgs()) {
		if err := busyAccepted(ctx); err != nil {
			return nil, err
		}
		return handlerOf(r.handleGoalCommand)(ctx, event)
	}
	return i18n.T(ctx, "gateway.busy.reject_goal", "", nil), nil
}

// Runs a loop control verb mid run, rejecting a new loop
func (r *GatewayRunner) busyLoopCommand(ctx context.Context, event *gateway.MessageEvent, quickKey string, source *gateway.SessionSource) (any, error) {
	loopArg := pystr.Lower(pystr.Strip(event.GetCommandArgs()))
	if loopArg == "" || slices.Contains([]string{"status", "pause", "resume", "stop", "clear", "cancel", "help", "--help", "-h"}, loopArg) {
		if err := busyAccepted(ctx); err != nil {
			return nil, err
		}
		return handlerOf(r.handleLoopCommand)(ctx, event)
	}
	return i18n.T(ctx, "gateway.busy.reject_loop", "", nil), nil
}
