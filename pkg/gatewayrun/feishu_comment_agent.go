package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/feishu"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Model and provider credentials of a comment agent turn, resolved the way gateway message handling resolves them
func resolveCommentModelAndRuntime(ctx context.Context) (string, map[string]any, error) {
	model := gateway.ResolveGatewayModel(ctx, gateway.LoadGatewayConfigMap(ctx, ""))
	kwargs, err := resolveRuntimeAgentKwargs(ctx)
	if err != nil {
		return "", nil, err
	}
	if p, _ := kwargs["provider"].(string); model == "" && p != "" {
		func() {
			defer func() { _ = recover() }()
			model = provider.GetDefaultModelForProvider(ctx, p)
		}()
	}
	kwargs["reasoning_config"] = config.ResolveReasoningConfig(gateway.LoadGatewayConfigMap(ctx, ""), model)
	return model, kwargs, nil
}

// Runs one Feishu comment agent turn with the feishu doc and drive toolsets, closing the agent afterwards
func RunFeishuCommentAgent(ctx context.Context, prompt string, hooks feishu.CommentAgentHooks) (*feishu.CommentAgentRun, error) {
	model, kwargs, err := resolveCommentModelAndRuntime(ctx)
	if err != nil {
		return nil, err
	}
	text := func(key string) string {
		if v := kwargs[key]; v != nil {
			return pyrepr.Str(v)
		}
		return ""
	}
	history := hooks.OnRuntime(feishu.CommentAgentRuntime{Model: model, Provider: text("provider"), BaseURL: text("base_url")})
	agentKwargs := map[string]any{"model": model, "quiet_mode": true, "skip_context_files": true, "skip_memory": true, "max_iterations": int64(15),
		"enabled_toolsets": []any{"feishu_doc", "feishu_drive"}}
	for _, k := range []string{"base_url", "api_key", "provider", "api_mode", "credential_pool", "reasoning_config"} {
		agentKwargs[k] = kwargs[k]
	}
	opts, err := agent.AIAgentOptionsFromKwargs(agentKwargs)
	if err != nil {
		return nil, err
	}
	a, err := agent.NewAIAgent(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = a.Close() }()
	hooks.OnRun()
	var conv []map[string]any
	if len(history) > 0 {
		conv = history
	}
	result, err := a.RunConversation(ctx, prompt, agent.RunConversationOptions{ConversationHistory: conv})
	if err != nil {
		return nil, err
	}
	run := &feishu.CommentAgentRun{}
	if s, ok := result["final_response"].(string); ok {
		run.FinalResponse = s
	}
	if n, err := pyint.Int64(result["api_calls"]); err == nil {
		run.APICalls = int(n)
	}
	if msgs, ok := result["messages"].([]map[string]any); ok {
		run.Messages = msgs
	} else if items, ok := result["messages"].([]any); ok {
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				run.Messages = append(run.Messages, m)
			}
		}
	}
	return run, nil
}
