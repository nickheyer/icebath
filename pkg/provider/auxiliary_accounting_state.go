package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// Session store adapted to the provider usage row so the state DB records auxiliary usage
type stateAuxUsageRecorder struct {
	db *state.DB
}

// Recorder writing auxiliary usage rows into the session store, nil when there is no store
func AuxUsageRecorderFor(db *state.DB) AuxUsageRecorder {
	if db == nil {
		return nil
	}
	return stateAuxUsageRecorder{db: db}
}

func (r stateAuxUsageRecorder) RecordAuxiliaryUsage(ctx context.Context, sessionID, task string, usage AuxiliaryUsage) error {
	return r.db.RecordAuxiliaryUsage(ctx, sessionID, task, StateAuxiliaryUsage(usage))
}

// Session store row of one auxiliary call, an empty provider or base URL stored as NULL like None
func StateAuxiliaryUsage(u AuxiliaryUsage) state.AuxiliaryUsage {
	optional := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	return state.AuxiliaryUsage{Model: optional(u.Model), BillingProvider: optional(u.BillingProvider), BillingBaseURL: optional(u.BillingBaseURL),
		InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens), CacheReadTokens: int64(u.CacheReadTokens),
		CacheWriteTokens: int64(u.CacheWriteTokens), ReasoningTokens: int64(u.ReasoningTokens), EstimatedCostUSD: u.EstimatedCostUSD}
}
