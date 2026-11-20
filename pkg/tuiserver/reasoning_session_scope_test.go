package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
)

// Agent of the reasoning cases, glm-5 on zai unless overridden
func reasoningAgent(t *testing.T, reasoning map[string]any, o agent.AIAgentOptions) *agent.AIAgent {
	if o.Model == "" {
		o.Model = "glm-5"
	}
	if o.Provider == "" {
		o.Provider = "zai"
	}
	o.ReasoningConfig, o.SessionID = reasoning, "sess-key"
	return testAgent(t, o)
}

func TestSessionInfoReasoningEffortDisabledReportsNone(t *testing.T) {
	info := sessionInfo(context.Background(), reasoningAgent(t, map[string]any{"enabled": false}, agent.AIAgentOptions{}), nil)
	if info["reasoning_effort"] != "none" {
		t.Fatalf("reasoning_effort = %v", info["reasoning_effort"])
	}
}

func TestSessionInfoReasoningEffortEnabledReportsEffort(t *testing.T) {
	info := sessionInfo(context.Background(), reasoningAgent(t, map[string]any{"enabled": true, "effort": "high"}, agent.AIAgentOptions{}), nil)
	if info["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", info["reasoning_effort"])
	}
}

func TestSessionInfoReasoningEffortUnsetReportsEmpty(t *testing.T) {
	info := sessionInfo(context.Background(), reasoningAgent(t, nil, agent.AIAgentOptions{}), nil)
	if info["reasoning_effort"] != "" || info["reasoning_effort_wire"] != "" {
		t.Fatalf("reasoning_effort = %v wire = %v", info["reasoning_effort"], info["reasoning_effort_wire"])
	}
}

func TestSessionInfoReasoningEffortWireLevelIsWhatTheRouteActuallySends(t *testing.T) {
	ctx := context.Background()
	info := sessionInfo(ctx, reasoningAgent(t, map[string]any{"enabled": true, "effort": "ultra"}, agent.AIAgentOptions{}), nil)
	if info["reasoning_effort"] != "ultra" || info["reasoning_effort_wire"] != "max" {
		t.Fatalf("ultra = %v wire %v", info["reasoning_effort"], info["reasoning_effort_wire"])
	}
	if wire := sessionInfo(ctx, reasoningAgent(t, map[string]any{"enabled": true, "effort": "high"}, agent.AIAgentOptions{}), nil)["reasoning_effort_wire"]; wire != "high" {
		t.Fatalf("high wire = %v", wire)
	}
	if wire := sessionInfo(ctx, reasoningAgent(t, map[string]any{"enabled": false}, agent.AIAgentOptions{}), nil)["reasoning_effort_wire"]; wire != "" {
		t.Fatalf("disabled wire = %v", wire)
	}
	appServer := reasoningAgent(t, map[string]any{"enabled": true, "effort": "ultra"},
		agent.AIAgentOptions{Provider: "openai-codex", Model: "gpt-5.6-sol", APIMode: "codex_app_server"})
	if wire := sessionInfo(ctx, appServer, nil)["reasoning_effort_wire"]; wire != "ultra" {
		t.Fatalf("app server wire = %v", wire)
	}
}

func TestSessionNoneReachesDeepSeekWireSessionInfoEffortNoneWithoutEnabledReportsNone(t *testing.T) {
	info := sessionInfo(context.Background(), reasoningAgent(t, map[string]any{"effort": "none"}, agent.AIAgentOptions{}), nil)
	if info["reasoning_effort"] != "none" {
		t.Fatalf("reasoning_effort = %v", info["reasoning_effort"])
	}
}
