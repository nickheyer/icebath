package provider

import (
	"context"
	"fmt"
	"log/slog"
)

// AuxiliaryUsage is one auxiliary call's token and cost row for a session's usage ledger
type AuxiliaryUsage struct {
	Model            string
	BillingProvider  string
	BillingBaseURL   string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
	EstimatedCostUSD *float64
}

// AuxUsageRecorder is the session store that keeps auxiliary usage rows per session and task
type AuxUsageRecorder interface {
	RecordAuxiliaryUsage(ctx context.Context, sessionID, task string, usage AuxiliaryUsage) error
}

type auxAccountingKey struct{}

type auxAccountingScope struct {
	db        AuxUsageRecorder
	sessionID string
}

// MoA advisor and aggregator usage the conversation loop already folds into its own counters
var auxAccountingExcludedTasks = map[string]bool{"moa_reference": true, "moa_aggregator": true}

// Context carrying the active session's accounting handles, cleared when either handle is missing
func SetAccountingContext(ctx context.Context, db AuxUsageRecorder, sessionID string) context.Context {
	if db == nil || sessionID == "" {
		return context.WithValue(ctx, auxAccountingKey{}, (*auxAccountingScope)(nil))
	}
	return context.WithValue(ctx, auxAccountingKey{}, &auxAccountingScope{db: db, sessionID: sessionID})
}

// Records an auxiliary response's usage against the ambient session, never failing the auxiliary call
func RecordAuxUsage(ctx context.Context, response map[string]any, task, provider, baseURL string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Aux usage recording failed (non-fatal)", "err", fmt.Sprint(r))
		}
	}()
	if task == "" || auxAccountingExcludedTasks[task] {
		return
	}
	scope, _ := ctx.Value(auxAccountingKey{}).(*auxAccountingScope)
	if scope == nil {
		return
	}
	rawUsage, has := response["usage"]
	if !has || rawUsage == nil {
		return
	}
	tier, _ := response["service_tier"].(string)
	usage := WithServedServiceTier(NormalizeUsage(rawUsage, provider, ""), tier)
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheReadTokens == 0 && usage.CacheWriteTokens == 0 && usage.ReasoningTokens == 0 {
		return
	}
	model := ""
	if v := response["model"]; auxTruthy(v) {
		model = auxPyStr(v)
	}
	if model == "" {
		model = "unknown"
	}
	row := AuxiliaryUsage{Model: model, BillingProvider: provider, BillingBaseURL: baseURL, InputTokens: usage.InputTokens,
		OutputTokens: usage.OutputTokens, CacheReadTokens: usage.CacheReadTokens, CacheWriteTokens: usage.CacheWriteTokens,
		ReasoningTokens: usage.ReasoningTokens}
	row.EstimatedCostUSD = auxEstimateCost(ctx, model, usage, provider, baseURL)
	if err := scope.db.RecordAuxiliaryUsage(ctx, scope.sessionID, task, row); err != nil {
		slog.Debug("Aux usage recording failed (non-fatal)", "err", err)
	}
}

// Estimated USD cost of the usage, nil when pricing is unknown or estimation fails
func auxEstimateCost(ctx context.Context, model string, usage CanonicalUsage, provider, baseURL string) (out *float64) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Aux usage cost estimation failed", "err", fmt.Sprint(r))
			out = nil
		}
	}()
	cost := EstimateUsageCost(ctx, model, usage, provider, baseURL, "")
	if cost.AmountUSD == nil {
		return nil
	}
	f, _ := cost.AmountUSD.Float64()
	return &f
}
