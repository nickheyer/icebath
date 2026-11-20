package agent

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// SummaryDispatchCompressor is the compressor state the summary route pin and window dispatch read and write
type SummaryDispatchCompressor interface {
	SummaryModel() string
	SummaryModelFallenBack() bool
	MainRoute() (providerName, model, baseURL, apiKey, apiMode string)
	SetLastSummaryError(text string)
	ActiveCompressionTelemetry() *pyjson.Object
	DeriveAutoFocusTopic(messages []map[string]any) *string
	GenerateWindowSummary(ctx context.Context, turns []map[string]any, focusTopic *string, memoryContext string) (*string, error)
	RestoreSummaryState(previousSummary *string, hasUserTurn *bool)
}

// CooldownBypassSummarizer is the summary generator form that accepts the bypass_cooldown keyword
type CooldownBypassSummarizer interface {
	GenerateWindowSummaryBypassingCooldown(ctx context.Context, turns []map[string]any, focusTopic *string, memoryContext string, bypassCooldown bool) (*string, error)
}

// Pins the summary route onto the call keywords, a stall fallback pin replacing the whole route
func ApplySummaryRoute(c SummaryDispatchCompressor, callKwargs map[string]any, pinned map[string]any) {
	if len(pinned) > 0 {
		for _, key := range []string{"provider", "model", "base_url", "api_key", "api_mode"} {
			delete(callKwargs, key)
		}
		for k, v := range pinned {
			callKwargs[k] = v
		}
		return
	}
	if model := c.SummaryModel(); model != "" {
		callKwargs["model"] = model
		return
	}
	if !c.SummaryModelFallenBack() {
		return
	}
	task := callKwargs["task"]
	if !pyrepr.Truthy(task) {
		task = "compression"
	}
	callKwargs["task"] = provider.NewAuthoritativeAuxiliaryTask(task)
	providerName, model, baseURL, apiKey, apiMode := c.MainRoute()
	for _, kv := range [][2]string{{"provider", providerName}, {"model", model}, {"base_url", baseURL}, {"api_key", apiKey}, {"api_mode", apiMode}} {
		if kv[1] != "" {
			callKwargs[kv[0]] = kv[1]
		}
	}
}

// Runs the summary LLM over the window, a cancellation first rolling back the handoff scan self heal
func SummarizeWindow(ctx context.Context, c SummaryDispatchCompressor, messages, turnsToSummarize []map[string]any, scan HandoffScan,
	focusTopic *string, memoryContext string, bypassCooldown bool) (*string, error) {
	if takeDeterministicSummaryPin(ctx) {
		if err := raiseIfStaleAttempt(ctx, c); err != nil {
			return nil, err
		}
		c.SetLastSummaryError("summary model stalled on every route; deterministic fallback summary inserted")
		if telemetry := c.ActiveCompressionTelemetry(); telemetry != nil {
			telemetry.Set("failure_class", "stall_deterministic_fallback")
		}
		return nil, nil
	}
	focus := focusTopic
	if focus == nil || *focus == "" {
		focus = c.DeriveAutoFocusTopic(messages)
	}
	var summary *string
	var err error
	if bypass, ok := c.(CooldownBypassSummarizer); ok {
		summary, err = bypass.GenerateWindowSummaryBypassingCooldown(ctx, turnsToSummarize, focus, memoryContext, bypassCooldown)
	} else {
		summary, err = c.GenerateWindowSummary(ctx, turnsToSummarize, focus, memoryContext)
	}
	var cancelled *provider.AuxiliaryExplicitCancellation
	if errors.As(err, &cancelled) && callerAttemptIsCurrent(ctx, c) {
		c.RestoreSummaryState(scan.PreviousSummaryBefore, scan.HasUserTurnBefore)
	}
	return summary, err
}
