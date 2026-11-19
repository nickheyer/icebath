package provider

import (
	"github.com/nickheyer/icebath/pkg/state"
)

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
