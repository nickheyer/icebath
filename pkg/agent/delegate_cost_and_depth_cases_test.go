package agent

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Child of test_delegate_cost_footer.py _make_mock_child, returned from the child constructor seam
func costFooterChild(t *testing.T, env *tools.Env, cost float64, costStatus string) *AIAgent {
	t.Helper()
	child, _ := cannedDelegateChild(t, env, "done")
	child.sessionPromptTokens, child.sessionCompletionTokens = 100, 50
	child.sessionEstimatedCostUSD, child.sessionCostStatus = cost, costStatus
	child.model, child.sessionID = "anthropic/claude-sonnet-4", "child-session"
	prev := newChildAIAgentFn
	newChildAIAgentFn = func(context.Context, AIAgentOptions) (*AIAgent, error) { return child, nil }
	t.Cleanup(func() { newChildAIAgentFn = prev })
	return child
}

// Runs one delegation over the canned child and returns the parent and the parsed result
func runCostFooter(t *testing.T, cost float64, costStatus string) (*AIAgent, *pyjson.Object) {
	t.Helper()
	parent := batchValidationParent(t)
	parent.sessionEstimatedCostUSD, parent.sessionCostSource, parent.sessionCostStatus = 0.0, "none", "unknown"
	costFooterChild(t, parent.env, cost, costStatus)
	out, err := delegateTask(context.Background(), parent, map[string]any{"goal": "Test per-delegation cost"})
	return parent, dispatchPayload(t, out, err)
}

func firstResult(t *testing.T, payload *pyjson.Object) *pyjson.Object {
	t.Helper()
	results := batchResults(t, payload)
	if len(results) == 0 {
		t.Fatalf("payload %v", payload)
	}
	return results[0].(*pyjson.Object)
}

func TestCostInResultEntryEntryCarriesCostUsdAndStatus(t *testing.T) {
	_, payload := runCostFooter(t, 0.1234567, "estimated")
	entry := firstResult(t, payload)
	cost, ok := entry.Get("cost_usd")
	if !ok || math.Abs(cost.(float64)-0.123457) > 1e-6 || entryText(entry, "cost_status") != "estimated" {
		t.Fatalf("entry %v", entry)
	}
}

func TestCostInResultEntryInternalChildCostFieldStillStripped(t *testing.T) {
	_, payload := runCostFooter(t, 0.25, "estimated")
	for _, raw := range batchResults(t, payload) {
		entry := raw.(*pyjson.Object)
		if entryHas(entry, "_child_cost_usd") || entryHas(entry, "_child_role") {
			t.Fatalf("entry %v", entry)
		}
	}
}

func TestCostInResultEntryParentRollupUnchanged(t *testing.T) {
	parent, _ := runCostFooter(t, 0.25, "estimated")
	if math.Abs(parent.sessionEstimatedCostUSD-0.25) > 1e-6 || parent.sessionCostSource != "subagent" || parent.sessionCostStatus != "estimated" {
		t.Fatalf("parent cost %v source %q status %q", parent.sessionEstimatedCostUSD, parent.sessionCostSource, parent.sessionCostStatus)
	}
}

func TestCostInResultEntryNonNumericChildCostDegradesToZero(t *testing.T) {
	_, payload := runCostFooter(t, 0.0, "estimated")
	if cost, _ := firstResult(t, payload).Get("cost_usd"); cost != 0.0 {
		t.Fatalf("cost_usd %v", cost)
	}
}

// test_delegate_depth_prompt.py test_dispatched_child_prompt_matches_depth_capability
func TestDispatchedChildPromptMatchesDepthCapability(t *testing.T) {
	for _, legacyRole := range []any{nil, "leaf", "orchestrator"} {
		for _, tc := range []struct {
			parentDepth, maxDepth int
			enabled               bool
		}{{1, 5, true}, {1, 3, true}, {2, 3, true}, {1, 5, false}} {
			t.Run(fmt.Sprintf("%v_%d_%d_%v", legacyRole, tc.parentDepth, tc.maxDepth, tc.enabled), func(t *testing.T) {
				env := delegationTestEnv(t, 0, false)
				writeDelegationBlock(t, env.Home, fmt.Sprintf("  max_spawn_depth: %d\n  orchestrator_enabled: %v\n", tc.maxDepth, tc.enabled))
				parent := &AIAgent{sessionID: "parent-session", env: env, delegateDepth: tc.parentDepth, enabledToolsets: []string{"terminal", "file", "delegation"},
					disabledToolsets: []string{}, requestOverrides: map[string]any{}, provider: "custom", baseURL: "https://fixture.invalid/v1",
					apiKey: "fixture-only", apiMode: "chat_completions", model: "fixture", platform: "cli"}
				child, canned := cannedDelegateChild(t, env, "done")
				child.model = "fixture"
				var built []AIAgentOptions
				prev := newChildAIAgentFn
				newChildAIAgentFn = func(_ context.Context, o AIAgentOptions) (*AIAgent, error) {
					built = append(built, o)
					return child, nil
				}
				t.Cleanup(func() { newChildAIAgentFn = prev })
				background := false
				raw := tools.DelegateTask(tools.WithToolEnv(context.Background(), env), env, tools.DelegateTaskRequest{
					Tasks: []any{map[string]any{"goal": "Inspect fixture"}}, Background: &background, Role: legacyRole, ParentAgent: parent,
				})
				result := dispatchPayload(t, raw, nil)
				if len(canned.calls) != 1 || entryHas(result, "error") || len(built) != 1 {
					t.Fatalf("calls %d result %v built %d", len(canned.calls), result, len(built))
				}
				prompt := built[0].EphemeralSystemPrompt
				depth := tc.parentDepth + 1
				canDelegate := tc.enabled && depth < tc.maxDepth
				wantRole := "leaf"
				if canDelegate {
					wantRole = "orchestrator"
				}
				if child.delegateDepth != depth || child.delegateRole != wantRole {
					t.Fatalf("depth %d role %q", child.delegateDepth, child.delegateRole)
				}
				hasDelegation := false
				for _, ts := range built[0].EnabledToolsets {
					hasDelegation = hasDelegation || ts == "delegation"
				}
				if hasDelegation != canDelegate || strings.Contains(prompt, "CAN spawn your own subagents") != canDelegate {
					t.Fatalf("delegation toolset %v prompt %q", hasDelegation, prompt)
				}
				if strings.Contains(prompt, "role=") || strings.Contains(prompt, "`role`") || strings.Contains(prompt, "Default is 'leaf'") {
					t.Fatalf("prompt teaches a role knob %q", prompt)
				}
				if !canDelegate {
					return
				}
				if !strings.Contains(prompt, "automatically") || !strings.Contains(prompt, "depth") {
					t.Fatalf("prompt %q", prompt)
				}
				if depth+1 >= tc.maxDepth {
					if !strings.Contains(prompt, "children MUST be leaves") {
						t.Fatalf("prompt %q", prompt)
					}
				} else if !strings.Contains(prompt, "children can themselves delegate because depth remains") || strings.Contains(prompt, "orchestrators or leaves") {
					t.Fatalf("prompt %q", prompt)
				}
			})
		}
	}
}
