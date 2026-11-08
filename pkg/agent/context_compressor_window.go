package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// Python ValueError reasons a retry refuses with before any destructive mutation
var (
	ErrRetryNonText        = state.ErrRetryNonText
	ErrRetryMediaPart      = state.ErrRetryMediaPart
	ErrRetryAnnotatedPart  = state.ErrRetryAnnotatedPart
	ErrRetryPartWithoutTxt = state.ErrRetryPartWithoutTxt
	ErrRetryNoText         = state.ErrRetryNoText
)

// Lossless retry text of live user content, media and annotated or unknown parts failing closed
func RetryableUserText(content any) (string, error) { return state.RetryableUserText(content) }

// Summarizable middle boundaries, an older assistant or tool bridge keeping the newest user ask out of the merge slot
func (c *ContextCompressor) compressWindow(messages []map[string]any) (int, int) {
	compressStart := c.AlignBoundaryForward(messages, c.protectHeadSize(messages))
	compressEnd := compressorFindTailCutByTokens(c, messages, compressStart, nil)
	latestActionable := c.FindLastUserMessageIdx(messages, 0)
	if compressEnd == latestActionable {
		bridge := latestActionable - 1
		var bridgeRole any
		if bridge >= 0 {
			bridgeRole = messages[bridge]["role"]
		}
		if bridgeRole == "tool" {
			bridge = c.alignBoundaryBackward(messages, latestActionable)
		} else if bridgeRole != "assistant" {
			bridge = -1
		}
		if bridge >= TailCutProgressFloor(c, messages, compressStart, bridge) {
			compressEnd = bridge
		}
	}
	return compressStart, compressEnd
}

// Phases one and two of compress, a pruned copy without blank echoes and the window indexing into it
func (c *ContextCompressor) planCompactionWindow(messages []map[string]any) (working []map[string]any, pruned, start, end int) {
	tailTokens := c.TailTokenBudget()
	working, pruned = compressorPruneScan(c, messages, c.ProtectLastN, &tailTokens, pruneMinChars)
	working = c.dropBlankEchoes(working)
	start, end = c.compressWindow(working)
	return working, pruned, start, end
}

// Background prepared summary for this window, nil for fresh compactions or once a memory provider supplies context
func (c *ContextCompressor) takePreparedSummary(ctx context.Context, messages []map[string]any, compressStart int, fresh, providerContext bool) *PreparedSummary {
	if c.preparedCompaction == nil {
		return nil
	}
	if providerContext {
		c.preparedCompaction.StopForProviderContext(ctx)
	}
	return c.preparedCompaction.Take(ctx, messages, c.sessionID, compressStart, !(fresh || providerContext))
}

// Adopts a prepared summary as an inline summary call would have left it and returns the handoff text
func (c *ContextCompressor) adoptPreparedSummary(ctx context.Context, prepared *PreparedSummary) string {
	AdoptPreparedSummary(ctx, c, prepared)
	return prepared.Summary
}
