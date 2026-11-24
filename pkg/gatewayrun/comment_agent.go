package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
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
	kwargs["reasoning_config"] = settings.ReasoningConfig(gateway.SettingsContext(ctx), model)
	return model, kwargs, nil
}

// Model and provider route a comment agent turn runs on
type CommentAgentRuntime struct {
	Model    string
	Provider string
	BaseURL  string
}

// Outcome of one comment agent turn
type CommentAgentRun struct {
	FinalResponse string
	APICalls      int
	Messages      []map[string]any
}

// Runs one platform comment agent turn on toolsets, onRuntime returning the history and onRun marking the start
func RunCommentAgent(ctx context.Context, prompt string, toolsets []any, onRuntime func(CommentAgentRuntime) []map[string]any, onRun func()) (*CommentAgentRun, error) {
	model, kwargs, err := resolveCommentModelAndRuntime(ctx)
	if err != nil {
		return nil, err
	}
	text := func(key string) string {
		if v := kwargs[key]; v != nil {
			return anyutil.String(v)
		}
		return ""
	}
	history := onRuntime(CommentAgentRuntime{Model: model, Provider: text("provider"), BaseURL: text("base_url")})
	// The memory provider is skipped unless compression.checkpoint_required needs it to checkpoint with, scoped as a fork
	agentKwargs := map[string]any{"model": model, "quiet_mode": true, "skip_context_files": true,
		"skip_memory": !agent.CheckpointRequiredFromConfig(ctx), "memory_agent_context": "subagent", "max_iterations": int64(15),
		"enabled_toolsets": toolsets}
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
	onRun()
	var conv []map[string]any
	if len(history) > 0 {
		conv = history
	}
	result, err := a.RunConversation(ctx, prompt, agent.RunConversationOptions{ConversationHistory: conv})
	if err != nil {
		return nil, err
	}
	run := &CommentAgentRun{}
	if s, ok := result["final_response"].(string); ok {
		run.FinalResponse = s
	}
	if n, err := anyutil.ToInt(result["api_calls"]); err == nil {
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
