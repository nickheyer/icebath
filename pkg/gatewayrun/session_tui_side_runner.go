package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Final response of a side agent run, the whole result's text when it carries none
func sideFinalResponseText(result map[string]any) string {
	value, ok := result["final_response"]
	if !ok {
		return pyrepr.Str(result)
	}
	if value == nil {
		return ""
	}
	return pyrepr.Str(value)
}

// Background prompt run to completion by a fresh agent on the session's frozen route, closed afterwards
func (r *GatewayRunner) SideBackground(ctx context.Context, env *tools.Env, a *gateway.SessionAuthority, policy *gateway.LocalSessionPolicy, live *gateway.LiveSession, taskID, text string, hold func(agent any)) (string, error) {
	side, err := r.sideAgent(ctx, a, policy, live, taskID, env, nil)
	if err != nil {
		return "", err
	}
	hold(side)
	defer func() {
		defer func() { _ = recover() }()
		_ = side.Close()
	}()
	result, err := side.RunConversation(ctx, text, agent.RunConversationOptions{TaskID: taskID})
	if err != nil {
		return "", err
	}
	return sideFinalResponseText(result), nil
}

// Side question answered over the history snapshot on the session's frozen route
func (r *GatewayRunner) SideQuestion(ctx context.Context, a *gateway.SessionAuthority, ref gateway.SessionRef, policy *gateway.LocalSessionPolicy, text string, parent any, history []map[string]any, onFork func(fork any), shouldStop func() bool) (string, error) {
	model, runtime, err := resolvePolicyAgentRuntime(ctx, r, policy, true)
	if err != nil {
		return "", err
	}
	mainRuntime := map[string]any{"model": model, "session_id": ref.SessionID}
	for _, key := range []string{"provider", "base_url", "api_key", "api_mode"} {
		mainRuntime[key] = runtime[key]
	}
	opts := agent.NewSideQuestionOptions()
	opts.Parent, _ = parent.(*agent.AIAgent)
	opts.MainRuntime = mainRuntime
	opts.OnFork = func(fork *agent.AIAgent) {
		if fork == nil {
			onFork(nil)
			return
		}
		onFork(fork)
	}
	opts.ShouldStop = shouldStop
	return agent.AnswerSideQuestion(ctx, text, history, opts)
}

// Hidden terminal and file agent restarting the preview server, left open so the server it started keeps running
func (r *GatewayRunner) SidePreviewRestart(ctx context.Context, env *tools.Env, a *gateway.SessionAuthority, policy *gateway.LocalSessionPolicy, live *gateway.LiveSession, taskID, prompt string, history []map[string]any, progress func(text string), hold func(agent any)) (string, error) {
	side, err := r.sideAgent(ctx, a, policy, live, taskID, env, map[string]any{
		"enabled_toolsets": []string{"terminal", "file"}, "disabled_toolsets": nil, "session_db": nil, "skip_memory": true,
		"tool_start_callback": func(toolCallID, name string, args map[string]any) { progress("Running " + name) },
	})
	if err != nil {
		return "", err
	}
	hold(side)
	var priorHistory []map[string]any
	if len(history) > 0 {
		priorHistory = history
	}
	result, err := side.RunConversation(ctx, prompt, agent.RunConversationOptions{TaskID: taskID, ConversationHistory: priorHistory})
	if err != nil {
		return "", err
	}
	return sideFinalResponseText(result), nil
}
