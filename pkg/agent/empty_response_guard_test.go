package agent

import (
	"context"
	"math/big"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

type guardResponse struct {
	Usage any `json:"usage"`
}

func guardAgent() *AIAgent {
	return &AIAgent{model: "anthropic/claude-fable-5", provider: "nous", apiMode: "chat_completions"}
}

func guardUsageResponse(promptTokens, completionTokens int) guardResponse {
	return guardResponse{Usage: map[string]any{"prompt_tokens": promptTokens, "completion_tokens": completionTokens,
		"total_tokens": promptTokens + completionTokens}}
}

func zeroResponse() guardResponse { return guardUsageResponse(25_900, 0) }

func recordGuardStreak(a *AIAgent, responses []any, finishReasons []string, observed []bool) {
	for i, resp := range responses {
		reason, seen := "stop", false
		if finishReasons != nil {
			reason = finishReasons[i]
		}
		if observed != nil {
			seen = observed[i]
		}
		a.recordEmptyAttempt(context.Background(), reason, resp, seen)
		a.emptyContentRetries++
	}
}

func swapAttemptCost(t *testing.T, costs ...string) {
	t.Helper()
	prev := estimateEmptyAttemptCostFn
	i := 0
	estimateEmptyAttemptCostFn = func(context.Context, *AIAgent, any) *big.Rat {
		if len(costs) == 0 {
			return nil
		}
		r, _ := new(big.Rat).SetString(costs[min(i, len(costs)-1)])
		i++
		return r
	}
	t.Cleanup(func() { estimateEmptyAttemptCostFn = prev })
}

func ratOf(s string) *big.Rat { r, _ := new(big.Rat).SetString(s); return r }

func TestDeterministicEmpty(t *testing.T) {
	noUsage := guardResponse{}
	cases := []struct {
		name      string
		agent     func() *AIAgent
		responses []any
		reasons   []string
		observed  []bool
		want      bool
	}{
		{"two_zero_output_attempts_same_signature_is_deterministic", guardAgent, []any{zeroResponse(), zeroResponse()}, nil, nil, true},
		{"single_attempt_is_never_deterministic", guardAgent, []any{zeroResponse()}, nil, nil, false},
		{"missing_usage_without_observed_generation_is_deterministic", guardAgent, []any{noUsage, noUsage}, nil, []bool{false, false}, true},
		{"missing_usage_with_observed_reasoning_fails_open", guardAgent, []any{noUsage, noUsage}, nil, []bool{true, true}, false},
		{"mixed_usage_presence_fails_open", guardAgent, []any{zeroResponse(), noUsage}, nil, nil, false},
		{"nonzero_output_tokens_fails_open", guardAgent, []any{guardUsageResponse(25_900, 12), guardUsageResponse(25_900, 12)}, nil, nil, false},
		{"zero_then_nonzero_fails_open", guardAgent, []any{zeroResponse(), guardUsageResponse(25_900, 5)}, nil, nil, false},
		{"finish_reason_change_fails_open", guardAgent, []any{zeroResponse(), zeroResponse()}, []string{"stop", "length"}, nil, false},
		{"guard_disabled_via_config", func() *AIAgent {
			a, off := guardAgent(), false
			a.emptyGuardEnabled = &off
			return a
		}, []any{zeroResponse(), zeroResponse()}, nil, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := c.agent()
			recordGuardStreak(a, c.responses, c.reasons, c.observed)
			if got := a.deterministicEmpty(); got != c.want {
				t.Fatalf("deterministic %v want %v", got, c.want)
			}
		})
	}
}

func TestDeterministicEmptySignatureChangeResetsDeterminism(t *testing.T) {
	a := guardAgent()
	recordGuardStreak(a, []any{zeroResponse()}, nil, nil)
	a.model = "openai/gpt-5"
	recordGuardStreak(a, []any{zeroResponse()}, nil, nil)
	if a.deterministicEmpty() {
		t.Fatal("model change across the streak classified as deterministic")
	}
}

func TestDeterministicEmptyNewStreakClearsHistory(t *testing.T) {
	a := guardAgent()
	recordGuardStreak(a, []any{zeroResponse(), zeroResponse()}, nil, nil)
	if !a.deterministicEmpty() {
		t.Fatal("first streak not deterministic")
	}
	a.emptyContentRetries = 0
	recordGuardStreak(a, []any{zeroResponse()}, nil, nil)
	if len(a.emptyAttemptHistory) != 1 || a.deterministicEmpty() {
		t.Fatalf("new streak kept %d attempts", len(a.emptyAttemptHistory))
	}
}

func TestDeterministicEmptyReasoningTokensCountAsGeneration(t *testing.T) {
	reasoningOnly := func() any {
		return guardResponse{Usage: map[string]any{"prompt_tokens": 25_900, "completion_tokens": 0, "total_tokens": 25_900,
			"completion_tokens_details": map[string]any{"reasoning_tokens": 128}}}
	}
	a := guardAgent()
	if present, zero := a.emptyZeroOutput(reasoningOnly()); !present || zero {
		t.Fatalf("present %v zero %v", present, zero)
	}
	recordGuardStreak(a, []any{reasoningOnly(), reasoningOnly()}, nil, nil)
	if a.deterministicEmpty() {
		t.Fatal("reasoning only streak classified as deterministic")
	}
}

func TestEmptyRetryBudget(t *testing.T) {
	off := false
	five := guardDecimal{amount: ratOf("5.00")}
	cases := []struct {
		name  string
		costs []string
		setup func(a *AIAgent)
		want  int
	}{
		{"default_budget_when_cost_unknown", nil, nil, defaultEmptyRetryBudget},
		{"reduced_budget_above_threshold", []string{"0.80"}, nil, reducedEmptyRetryBudget},
		{"default_budget_below_threshold", []string{"0.01"}, nil, defaultEmptyRetryBudget},
		{"custom_threshold_config", []string{"0.80"}, func(a *AIAgent) { a.emptyGuardCostThresholdUSD = &five }, defaultEmptyRetryBudget},
		{"guard_disabled_keeps_default_budget", []string{"9.99"}, func(a *AIAgent) { a.emptyGuardEnabled = &off }, defaultEmptyRetryBudget},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			swapAttemptCost(t, c.costs...)
			a := guardAgent()
			if c.setup != nil {
				c.setup(a)
			}
			if got := a.emptyRetryBudget(t.Context(), zeroResponse()); got != c.want {
				t.Fatalf("budget %d want %d", got, c.want)
			}
		})
	}
}

func TestEmptyRetryBudgetBadThresholdAttrFallsBack(t *testing.T) {
	a := guardAgent()
	a.emptyGuardCostThresholdUSD = &guardDecimal{amount: big.NewRat(-1, 1)}
	if got := a.emptyGuardThreshold(); got.amount.Cmp(big.NewRat(1, 4)) != 0 {
		t.Fatalf("negative threshold kept %v", got.amount)
	}
	a.emptyGuardCostThresholdUSD = &guardDecimal{nan: true}
	if got := a.emptyGuardThreshold(); got.amount.Cmp(big.NewRat(1, 4)) != 0 {
		t.Fatalf("NaN threshold kept %v", got)
	}
	if got := guardAgent().emptyGuardThreshold(); got.amount.Cmp(big.NewRat(1, 4)) != 0 {
		t.Fatalf("absent threshold gave %v", got.amount)
	}
}

func TestEmptyRetryBudgetPricingExceptionFailsOpen(t *testing.T) {
	a := &AIAgent{apiMode: "chat_completions"}
	if got := a.emptyRetryBudget(t.Context(), guardResponse{Usage: struct{}{}}); got != defaultEmptyRetryBudget {
		t.Fatalf("garbage usage tightened the budget to %d", got)
	}
}

func TestStreakCost(t *testing.T) {
	t.Run("streak_cost_accumulates", func(t *testing.T) {
		swapAttemptCost(t, "1.10", "1.23")
		a := guardAgent()
		recordGuardStreak(a, []any{zeroResponse(), zeroResponse()}, nil, nil)
		if got := a.streakCostUSD(); got == nil || got.Cmp(ratOf("2.33")) != 0 {
			t.Fatalf("streak cost %v", got)
		}
	})
	t.Run("streak_cost_none_when_unknown", func(t *testing.T) {
		swapAttemptCost(t)
		a := guardAgent()
		recordGuardStreak(a, []any{zeroResponse(), zeroResponse()}, nil, nil)
		if got := a.streakCostUSD(); got != nil {
			t.Fatalf("streak cost %v", got)
		}
	})
	t.Run("streak_cost_resets_on_new_streak", func(t *testing.T) {
		swapAttemptCost(t, "1.00")
		a := guardAgent()
		recordGuardStreak(a, []any{zeroResponse(), zeroResponse()}, nil, nil)
		if got := a.streakCostUSD(); got == nil || got.Cmp(ratOf("2.00")) != 0 {
			t.Fatalf("streak cost %v", got)
		}
		a.emptyContentRetries = 0
		a.recordEmptyAttempt(t.Context(), "stop", zeroResponse(), false)
		if got := a.streakCostUSD(); got == nil || got.Cmp(ratOf("1.00")) != 0 {
			t.Fatalf("new streak cost %v", got)
		}
	})
}

func TestZeroOutputExtraction(t *testing.T) {
	anthropic := guardAgent()
	anthropic.apiMode = "anthropic_messages"
	cases := []struct {
		name          string
		agent         *AIAgent
		response      any
		present, zero bool
	}{
		{"openai_shape_zero_completion", guardAgent(), guardUsageResponse(25_900, 0), true, true},
		{"openai_shape_with_completion", guardAgent(), guardUsageResponse(25_900, 9), true, false},
		{"no_usage", guardAgent(), guardResponse{}, false, false},
		{"anthropic_shape_zero_output", anthropic, guardResponse{Usage: map[string]any{"input_tokens": 25_900, "output_tokens": 0,
			"cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}}, true, true},
		{"all_zero_usage_object_fails_open", guardAgent(), guardResponse{Usage: struct{ Unset any }{}}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if present, zero := c.agent.emptyZeroOutput(c.response); present != c.present || zero != c.zero {
				t.Fatalf("present %v zero %v", present, zero)
			}
		})
	}
}

func TestResolveGuardSettings(t *testing.T) {
	isDefault := func(d guardDecimal) bool { return d.inf == 0 && !d.nan && d.amount.Cmp(big.NewRat(1, 4)) == 0 }
	for _, raw := range []any{nil, "nope"} {
		if enabled, threshold := resolveGuardSettings(raw); !enabled || !isDefault(threshold) {
			t.Fatalf("section %v gave %v %v", raw, enabled, threshold)
		}
	}
	if enabled, _ := resolveGuardSettings(map[string]any{"enabled": false}); enabled {
		t.Fatal("enabled false kept the guard on")
	}
	if enabled, _ := resolveGuardSettings(map[string]any{"enabled": "false"}); enabled {
		t.Fatal("yaml string false kept the guard on")
	}
	if enabled, _ := resolveGuardSettings(map[string]any{"enabled": "true"}); !enabled {
		t.Fatal("yaml string true turned the guard off")
	}
	if _, threshold := resolveGuardSettings(map[string]any{"cost_threshold_usd": 5}); threshold.amount.Cmp(big.NewRat(5, 1)) != 0 {
		t.Fatalf("int threshold %v", threshold.amount)
	}
	if _, threshold := resolveGuardSettings(map[string]any{"cost_threshold_usd": "1.50"}); threshold.amount.Cmp(ratOf("1.50")) != 0 {
		t.Fatalf("string threshold %v", threshold.amount)
	}
	if _, threshold := resolveGuardSettings(map[string]any{"cost_threshold_usd": 0.1}); threshold.amount.Cmp(ratOf("0.1")) != 0 {
		t.Fatalf("float threshold %v", threshold.amount)
	}
	for _, bad := range []any{"banana", -1, true, "1/2", "nan"} {
		if _, threshold := resolveGuardSettings(map[string]any{"cost_threshold_usd": bad}); !isDefault(threshold) {
			t.Fatalf("bad threshold %v kept %v", bad, threshold)
		}
	}
	defaults := config.Defaults()["agent"].(map[string]any)["empty_response_guard"]
	if enabled, threshold := resolveGuardSettings(defaults); !enabled || !isDefault(threshold) {
		t.Fatalf("shipped defaults resolve to %v %v", enabled, threshold)
	}
}
