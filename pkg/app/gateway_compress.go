package app

import (
	"context"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Request token estimate of transcript plus the gateway session's tool schemas, zero for no messages
func gatewayManualRequestTokens(ctx context.Context, a *GatewayAgent, messages []map[string]any) int {
	if len(messages) == 0 {
		return 0
	}
	return provider.EstimateRequestTokens(ctx, agent.RowsToMessages(messages), "", a.Tools(), true)
}

// One manual compression of history on the gateway session, the owner committing and reporting it
func compressNowOnGateway(ctx context.Context, a *GatewayAgent, history []map[string]any, request agent.CompressRequest) (agent.CompressResult, error) {
	before := append([]map[string]any(nil), history...)
	beforeTokens := gatewayManualRequestTokens(ctx, a, before)
	head, tail := before, []map[string]any(nil)
	if request.Partial {
		head, tail = provider.SplitHistoryForPartialCompress(before, request.KeepLast)
		if len(tail) == 0 {
			head = before
		}
	}
	if request.Preview {
		report := provider.SummarizeCompressPreview(before, request.Partial, request.KeepLast, request.FocusTopic, beforeTokens)
		lines, _ := report["lines"].([]string)
		return agent.CompressResult{Status: "preview", BeforeMessages: before, AfterMessages: before, BeforeTokens: beforeTokens,
			AfterTokens: beforeTokens, Request: request, Lines: lines}, nil
	}
	var tailRows []map[string]any
	for _, m := range tail {
		row := copyMap(m)
		delete(row, state.DBPersistedMarker)
		if !pyrepr.Truthy(m[state.DBPersistedMarker]) {
			dropped, had := row["_row_id"]
			delete(row, "_row_id")
			if id, ok := pyIntValue(dropped); had && ok && id > 0 {
				var absorbed []any
				if existing, isList := row["_absorbed_row_ids"].([]any); isList {
					absorbed = append(absorbed, existing...)
				}
				found := false
				for _, v := range absorbed {
					if n, isInt := pyIntValue(v); isInt && n == id {
						found = true
					}
				}
				if !found {
					absorbed = append(absorbed, dropped)
				}
				row["_absorbed_row_ids"] = absorbed
			}
		}
		tailRows = append(tailRows, row)
	}
	compressed, _, err := a.compressContext(ctx, head, nil, request.FocusTopic, tailRows)
	if err != nil {
		return agent.CompressResult{}, err
	}
	if len(tail) > 0 {
		allStamped := true
		for _, row := range tailRows {
			if stamped, isBool := row[state.DBPersistedMarker].(bool); !isBool || !stamped {
				allStamped = false
			}
		}
		if !allStamped {
			compressed = provider.RejoinCompressedHeadAndTail(compressed, tail)
		}
	}
	afterTokens := gatewayManualRequestTokens(ctx, a, compressed)
	var summary agent.ManualCompressionFeedback
	if owner := a.CompressionSummary(); owner != nil {
		summary = agent.ManualCompressionFeedback{Noop: pyrepr.Truthy(owner["noop"]), Aborted: pyrepr.Truthy(owner["aborted"]),
			RefusedWouldGrow: pyrepr.Truthy(owner["refused_would_grow"]), FallbackUsed: pyrepr.Truthy(owner["fallback_used"]),
			Headline: strOrEmpty(owner["headline"]), TokenLine: strOrEmpty(owner["token_line"])}
		if note := strOrEmpty(owner["note"]); note != "" {
			summary.Note = &note
		}
	} else {
		summary = agent.SummarizeManualCompression(before, compressed, beforeTokens, afterTokens, nil)
	}
	return agent.CompressResult{Status: "compressed", BeforeMessages: before, AfterMessages: append([]map[string]any(nil), compressed...),
		BeforeTokens: beforeTokens, AfterTokens: afterTokens, Request: request, Summary: &summary}, nil
}
