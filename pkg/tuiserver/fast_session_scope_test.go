package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Session info of a priority tier agent on the given route
func fastInfo(t *testing.T, o agent.AIAgentOptions) map[string]any {
	t.Helper()
	o.ServiceTier, o.SessionID = "priority", "sess-key"
	if o.APIMode == "" {
		o.APIMode = "chat_completions"
	}
	o.APIKey, o.MaxIterations, o.QuietMode, o.SkipContextFiles, o.SkipMemory = "test", 5, true, true, true
	a, err := agent.NewAIAgent(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return sessionInfo(context.Background(), a, tuigateway.NewSession(map[string]any{"session_key": "k7", "agent": a}))
}

func TestSessionInfoFastFollowsTheRouteFirstPartyRouteReportsFast(t *testing.T) {
	info := fastInfo(t, agent.AIAgentOptions{Model: "gpt-5.4", Provider: "openai", BaseURL: "https://api.openai.com/v1"})
	if info["service_tier"] != "priority" || info["fast"] != true {
		t.Fatalf("tier %v fast %v", info["service_tier"], info["fast"])
	}
}

func TestSessionInfoFastFollowsTheRouteAnthropicRouteReadsTheAnthropicBaseURL(t *testing.T) {
	info := fastInfo(t, agent.AIAgentOptions{Model: "claude-opus-5", Provider: "anthropic", APIMode: "anthropic_messages"})
	if info["fast"] != true {
		t.Fatalf("fast %v", info["fast"])
	}
}

func TestSessionInfoFastFollowsTheRouteLocalModelKeepsTheTierButReportsNoFast(t *testing.T) {
	info := fastInfo(t, agent.AIAgentOptions{Model: "Qwen3.8-27B-UD-Q4_K_M", Provider: "llamacpp", BaseURL: "http://127.0.0.1:18434/v1"})
	if info["service_tier"] != "priority" || info["fast"] != false {
		t.Fatalf("tier %v fast %v", info["service_tier"], info["fast"])
	}
}

func TestSessionInfoFastFollowsTheRouteFastCapableModelBehindAProxyReportsNoFast(t *testing.T) {
	info := fastInfo(t, agent.AIAgentOptions{Model: "gpt-5.4", Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1"})
	if info["fast"] != false {
		t.Fatalf("fast %v", info["fast"])
	}
}

func TestSessionInfoFastFollowsTheRoutePendingSwitchIsJudgedByTheNewRoute(t *testing.T) {
	a, err := agent.NewAIAgent(context.Background(), agent.AIAgentOptions{ServiceTier: "priority", SessionID: "sess-key",
		APIMode: "chat_completions", Model: "gpt-5.4", Provider: "openai", BaseURL: "https://api.openai.com/v1",
		APIKey: "test", MaxIterations: 5, QuietMode: true, SkipContextFiles: true, SkipMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	session := tuigateway.NewSession(map[string]any{"session_key": "k8", "agent": a, "pending_model_switch": map[string]any{
		"display_model": "Qwen3.8-27B-UD-Q4_K_M", "display_provider": "llamacpp"}})
	if fast := sessionInfo(context.Background(), a, session)["fast"]; fast != false {
		t.Fatalf("local pending fast %v", fast)
	}
	session.Set("pending_model_switch", map[string]any{"display_model": "claude-opus-5", "display_provider": "anthropic"})
	if fast := sessionInfo(context.Background(), a, session)["fast"]; fast != true {
		t.Fatalf("anthropic pending fast %v", fast)
	}
}
