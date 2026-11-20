package acp

import (
	"context"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/state"
)

// Rough token estimate of a request made of messages, a system prompt and tool schemas
var estimateRequestTokensRough = func(ctx context.Context, messages []map[string]any, systemPrompt string, toolDefs []map[string]any) int {
	rows := make([]*state.Message, len(messages))
	for i, m := range messages {
		rows[i] = state.MessageFromDict(m)
	}
	return provider.EstimateRequestTokens(ctx, rows, systemPrompt, toolDefs, true)
}

// Version provenance of this install
var getVersionInfo = updaterecord.GetVersionInfo
