package gatewayrun

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
)

const switchKey = "agent:main:telegram:dm:c1"

// test_model_switch_persistence.py TestApplySessionModelOverride test_override_replaces_all_fields
func TestOverrideReplacesAllFields(t *testing.T) {
	r := &GatewayRunner{}
	r.sessionState(switchKey).conversation.modelOverride = map[string]any{
		"model": "gpt-5.4-turbo", "provider": "openrouter", "api_key": "or-key-123",
		"base_url": "https://openrouter.ai/api/v1", "api_mode": "chat_completions",
	}
	model, rt := r.applySessionModelOverride(context.Background(), switchKey, "anthropic/claude-sonnet-4", map[string]any{
		"provider": "anthropic", "api_key": "ant-key", "base_url": "https://api.anthropic.com", "api_mode": "anthropic_messages",
	})
	if model != "gpt-5.4-turbo" || rt["provider"] != "openrouter" || rt["api_key"] != "or-key-123" ||
		rt["base_url"] != "https://openrouter.ai/api/v1" || rt["api_mode"] != "chat_completions" {
		t.Fatalf("model %s runtime %v", model, rt)
	}
}

// test_model_switch_persistence.py TestApplySessionModelOverride test_no_override_returns_originals
func TestNoOverrideReturnsOriginals(t *testing.T) {
	r := &GatewayRunner{}
	orig := map[string]any{"provider": "anthropic", "api_key": "key", "base_url": "https://api.anthropic.com", "api_mode": "anthropic_messages"}
	runtime := map[string]any{}
	for k, v := range orig {
		runtime[k] = v
	}
	model, rt := r.applySessionModelOverride(context.Background(), switchKey, "anthropic/claude-sonnet-4", runtime)
	if model != "anthropic/claude-sonnet-4" || !reflect.DeepEqual(rt, orig) {
		t.Fatalf("model %s runtime %v", model, rt)
	}
}

// Real agent running the given model on a custom endpoint
func modelAgent(t *testing.T, model string) *agent.AIAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a, err := agent.NewAIAgent(context.Background(), agent.AIAgentOptions{
		Model: model, APIKey: "key", BaseURL: "http://127.0.0.1:9/v1", Provider: "custom",
		MaxIterations: 5, QuietMode: true, SkipContextFiles: true, SkipMemory: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// test_model_switch_persistence.py TestIsIntentionalModelSwitch test_matches_override
func TestMatchesOverride(t *testing.T) {
	r := &GatewayRunner{}
	a := modelAgent(t, "gpt-5.4")
	r.sessionState(switchKey).conversation.modelOverride = map[string]any{
		"model": a.Model(), "provider": "openai", "api_key": "key", "base_url": "", "api_mode": "chat_completions",
	}
	if !r.isIntentionalModelSwitch(switchKey, a, "openai/gpt-5") {
		t.Fatal("a model the session override names was treated as drift")
	}
}

// test_model_switch_persistence.py TestIsIntentionalModelSwitch test_plain_drift_is_not_intentional
func TestPlainDriftIsNotIntentional(t *testing.T) {
	r := &GatewayRunner{}
	if r.isIntentionalModelSwitch(switchKey, modelAgent(t, "fallback/model"), "primary/model") {
		t.Fatal("plain drift was treated as intentional")
	}
}

// test_model_switch_persistence.py TestOneTurnModelOverrideRestore test_restores_previous_override
func TestRestoresPreviousOverride(t *testing.T) {
	r := &GatewayRunner{}
	previous := map[string]any{"model": "old/model", "provider": "openrouter", "api_key": "old-key",
		"base_url": "https://openrouter.ai/api/v1", "api_mode": "chat_completions"}
	r.sessionState(switchKey).conversation.modelOverride = previous
	snapshot := r.snapshotSessionModelOverride(switchKey)
	r.sessionState(switchKey).conversation.modelOverride = map[string]any{"model": "temp/model", "provider": "anthropic"}
	r.restoreSessionModelOverride(context.Background(), switchKey, snapshot)
	if got := r.sessionState(switchKey).conversation.modelOverride; !reflect.DeepEqual(got, previous) {
		t.Fatalf("override %v", got)
	}
}

const (
	scopeKey   = "agent:main:telegram:dm:777"
	scopeOther = "agent:main:discord:dm:888"
)

// Runner whose conversation scoped state is populated for two sessions with a running turn on one
func scopedRunner(t *testing.T) *GatewayRunner {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	SlashConfirmClear = func(string) {}
	t.Cleanup(func() { SlashConfirmClear = nil })
	r := &GatewayRunner{}
	for _, key := range []string{scopeKey, scopeOther} {
		conv := &r.sessionState(key).conversation
		conv.modelOverride = map[string]any{"model": "m"}
		conv.oneTurnRestore = map[string]any{"had_override": false}
		conv.reasoningOverride = map[string]any{"effort": "high"}
		conv.lastResolvedModel = "m"
		conv.sidecarNotes = []string{"note"}
	}
	r.pendingModelNotes = map[string]string{scopeKey: "n", scopeOther: "n"}
	r.sessionStallNotified = map[string]bool{scopeKey: true, scopeOther: true}
	r.transcriptLagStreaks = map[string]int{scopeKey: 2, scopeOther: 2}
	r.sessionState(scopeKey).turn.agent = agentPendingSentinel
	r.sessionState(scopeKey).turn.startedTS = 1.0
	r.sessionState(scopeKey).persistent.runGeneration = 7
	return r
}

// test_conversation_scope_funnel.py test_funnel_leaves_turn_scoped_and_generation_state_alone
func TestFunnelLeavesTurnScopedAndGenerationStateAlone(t *testing.T) {
	r := scopedRunner(t)
	r.clearConversationScope(context.Background(), scopeKey, "test")
	state := r.peekSessionState(scopeKey)
	if state.turn.agent == nil || state.turn.startedTS != 1.0 || state.persistent.runGeneration != 7 {
		t.Fatalf("turn scoped state touched: %+v", state.turn)
	}
	if state.conversation.modelOverride != nil || state.conversation.lastResolvedModel != "" {
		t.Fatal("conversation scope survived the funnel")
	}
	if _, ok := r.pendingModelNotes[scopeKey]; ok {
		t.Fatal("the pending model note survived the funnel")
	}
	if r.peekSessionState(scopeOther).conversation.modelOverride == nil || r.pendingModelNotes[scopeOther] == "" {
		t.Fatal("the funnel cleared another session")
	}
}

// test_conversation_scope_funnel.py test_funnel_also_clears_boundary_security_state
func TestFunnelAlsoClearsBoundarySecurityState(t *testing.T) {
	r := scopedRunner(t)
	r.sessionState(scopeKey).persistent.approvals = map[string]any{"cmd": "rm -rf"}
	r.sessionState(scopeOther).persistent.approvals = map[string]any{}
	r.sessionState(scopeKey).persistent.updatePromptPending = true
	r.pendingSkillsReloadNotes = map[string]string{scopeKey: "note"}
	r.clearConversationScope(context.Background(), scopeKey, "test")
	if r.peekSessionState(scopeKey).persistent.approvals != nil || r.peekSessionState(scopeOther).persistent.approvals == nil {
		t.Fatal("approvals were not cleared for the boundary session only")
	}
	if r.peekSessionState(scopeKey).persistent.updatePromptPending {
		t.Fatal("the update prompt flag survived the boundary")
	}
	if _, ok := r.pendingSkillsReloadNotes[scopeKey]; ok {
		t.Fatal("the skills reload note survived the boundary")
	}
}
