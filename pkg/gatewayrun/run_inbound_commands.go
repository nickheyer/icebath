package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Registered command of a typed name with its canonical name, the typed name itself when unregistered
func canonCommand(cmd string) (bool, string) {
	if cmd == "" || ResolveCommand == nil {
		return false, cmd
	}
	if name, ok := ResolveCommand(cmd); ok {
		return true, name
	}
	return false, cmd
}

// Resolves the slash command through aliases, the access gate and the command hooks, handled replies returned as is
func (r *GatewayRunner) hmResolveCommand(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, *string, string, string) {
	command, _ := event.GetCommand()
	known, canonical := canonCommand(command)
	if command != "" && !known {
		if raw, ok := r.hmQuickCommands()[command]; ok && raw != nil {
			if qcmd, isMap := raw.(map[string]any); isMap && qcmd["type"] == "alias" {
				if newCommand, expanded := hmExpandAliasQuickCommand(event, qcmd); expanded {
					command = newCommand
					_, canonical = canonCommand(command)
				}
			}
		}
	}
	if !(command != "" && canonical != "" && IsGatewayKnownCommand(canonical)) {
		return false, nil, command, canonical
	}
	if denied := r.checkSlashAccess(ctx, source, canonical); denied != "" {
		return true, &denied, command, canonical
	}
	handled, result, newCommand := r.hmCommandHooks(ctx, event, source, quickKey, command, canonical)
	if handled {
		return true, result, command, canonical
	}
	if newCommand != nil {
		command = *newCommand
		_, canonical = canonCommand(command)
	}
	return false, nil, command, canonical
}

// Ignores the platform start ping
func (r *GatewayRunner) hmCmdStart(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	runLog().Info(fmt.Sprintf("Ignoring /start platform ping for session %s", quickKey))
	return true, "", nil
}

// Replies with the egress proxy status
func (r *GatewayRunner) hmCmdEgress(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	text, err := provider.FormatEgressStatusText(home.Dir(ctx), false)
	if err != nil {
		return true, nil, err
	}
	return true, text, nil
}

// Acks then rewrites the turn to the built prompt for the agent, a failing builder replying a retry hint
func (r *GatewayRunner) hmRewriteTurnToPrompt(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, name, ack string, build func() string) (bool, any, error) {
	r.sendCommandAck(ctx, source, ack, name)
	built, failed := func() (text string, failed bool) {
		defer func() {
			if rec := recover(); rec != nil {
				failed = true
			}
		}()
		return build(), false
	}()
	if failed {
		return true, i18n.T(ctx, "gateway.prompt_command.start_failed", "", map[string]any{"command": name}), nil
	}
	event.Text = built
	return false, nil, nil
}

// Rewrites the turn to the learn prompt for the described or recent work
func (r *GatewayRunner) hmCmdLearn(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	req := pystr.Strip(event.GetCommandArgs())
	ack := i18n.T(ctx, "gateway.learn.ack_conversation", "", nil)
	if req != "" {
		ack = i18n.T(ctx, "gateway.learn.ack_described", "", nil)
	}
	return r.hmRewriteTurnToPrompt(ctx, event, source, "learn", ack, func() string { return memory.BuildLearnPrompt(req) })
}

// Rewrites the turn to the plan prompt for the given task or the conversation
func (r *GatewayRunner) hmCmdPlan(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	task := pystr.Strip(event.GetCommandArgs())
	ack := i18n.T(ctx, "gateway.plan.ack_conversation", "", nil)
	if task != "" {
		shown := task
		if runes := []rune(task); len(runes) > 80 {
			shown = string(runes[:80]) + "…"
		}
		ack = i18n.T(ctx, "gateway.plan.ack_task", "", map[string]any{"task": shown})
	}
	return r.hmRewriteTurnToPrompt(ctx, event, source, "plan", ack, func() string { return provider.BuildPlanPrompt(task) })
}

// Rewrites the turn to the init prompt of the session's active directory, acking generate or update
func (r *GatewayRunner) hmCmdInit(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	sessionKey := quickKey
	if sessionKey == "" {
		key, err := r.sessionKeyForSource(ctx, source)
		if err != nil {
			return true, i18n.T(ctx, "gateway.init.start_failed", "", nil), nil
		}
		sessionKey = key
	}
	prompt, failed := func() (text string, failed bool) {
		defer func() {
			if rec := recover(); rec != nil {
				failed = true
			}
		}()
		return BuildInitPromptForCwd(tools.ToolEnvFrom(ctx), "", pystr.Strip(event.GetCommandArgs()), &sessionKey), false
	}()
	if failed {
		return true, i18n.T(ctx, "gateway.init.start_failed", "", nil), nil
	}
	ack := i18n.T(ctx, "gateway.init.ack_generate", "", nil)
	if strings.Contains(prompt, "UPDATE the existing AGENTS.md") {
		ack = i18n.T(ctx, "gateway.init.ack_update", "", nil)
	}
	r.sendCommandAck(ctx, source, ack, "init")
	event.Text = prompt
	return false, nil, nil
}

// Replies with the blueprint text, or rewrites the turn to a matched blueprint's seed after acking it
func (r *GatewayRunner) hmCmdBlueprint(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	result := r.handleBlueprintCommand(ctx, event)
	if result.AgentSeed == nil || *result.AgentSeed == "" {
		if result.Text == "" {
			return true, nil, nil
		}
		return true, result.Text, nil
	}
	if result.Text != "" {
		r.sendCommandAck(ctx, source, result.Text, "blueprint")
	}
	event.Text = *result.AgentSeed
	return false, nil, nil
}

// Sends the queued payload as a regular turn on the idle path
func (r *GatewayRunner) hmCmdQueue(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	return hmSendPayloadAsTurn(event, i18n.T(ctx, "gateway.queue.usage", "", nil))
}

// Sends the steer payload as a regular turn on the idle path
func (r *GatewayRunner) hmCmdSteer(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	return hmSendPayloadAsTurn(event, i18n.T(ctx, "gateway.steer.usage_idle", "", nil))
}

// Strips the command prefix so the payload runs as a user turn, the usage hint for an empty payload
func hmSendPayloadAsTurn(event *gateway.MessageEvent, usage string) (bool, any, error) {
	payload := pystr.Strip(event.GetCommandArgs())
	if payload == "" {
		return true, usage, nil
	}
	event.Text = payload
	return false, nil, nil
}

// Runs one prompt through the default mixture of agents preset with the prior model restored after the turn
func (r *GatewayRunner) hmCmdMoa(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	payload := pystr.Strip(event.GetCommandArgs())
	if payload == "" {
		return true, provider.MoAUsage(), nil
	}
	var raw any = map[string]any{}
	if cfg := r.readUserConfig(ctx); cfg != nil {
		raw = cfg["moa"]
	}
	moaCfg := provider.NormalizeMoAConfig(raw)
	failed := func() (failed bool) {
		defer func() {
			if rec := recover(); rec != nil {
				failed = true
			}
		}()
		event.Text = payload
		state := r.sessionState(quickKey)
		r.claimOneTurnRestore(quickKey, nil)
		state.conversation.modelOverride = map[string]any{
			"provider": "moa", "model": moaCfg["default_preset"], "base_url": "moa://local",
			"api_key": "moa-virtual-provider", "api_mode": "chat_completions",
		}
		r.evictCachedAgent(ctx, quickKey, false, false)
		return false
	}()
	if failed {
		return true, i18n.T(ctx, "gateway.moa.prepare_failed", "", nil), nil
	}
	return false, nil, nil
}
