package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python ValueError reasons a retry refuses with before any destructive mutation
var (
	ErrRetryNonText        = errors.New("retry does not support non-text content")
	ErrRetryMediaPart      = errors.New("retry does not support media or unknown content parts")
	ErrRetryAnnotatedPart  = errors.New("retry cannot losslessly flatten annotated text parts")
	ErrRetryPartWithoutTxt = errors.New("retry text parts must contain text")
	ErrRetryNoText         = errors.New("retry found no text to send")
)

var retryTextPartTypes = map[any]bool{"text": true, "input_text": true, "output_text": true}

// Lossless retry text of live user content, media and annotated or unknown parts failing closed
func RetryableUserText(content any) (string, error) {
	var parts []any
	switch c := content.(type) {
	case string:
		parts = []any{c}
	case []any:
		parts = c
	case []map[string]any:
		for _, p := range c {
			parts = append(parts, p)
		}
	default:
		return "", ErrRetryNonText
	}
	var chunks strings.Builder
	for _, part := range parts {
		if s, ok := part.(string); ok {
			chunks.WriteString(s)
			continue
		}
		p, ok := part.(map[string]any)
		if !ok {
			return "", ErrRetryNonText
		}
		if !retryTextPartTypes[p["type"]] {
			return "", ErrRetryMediaPart
		}
		for k := range p {
			if k != "type" && k != "text" {
				return "", ErrRetryAnnotatedPart
			}
		}
		text, isStr := p["text"].(string)
		if !isStr {
			return "", ErrRetryPartWithoutTxt
		}
		chunks.WriteString(text)
	}
	text := chunks.String()
	if pystr.Strip(text) == "" {
		return "", ErrRetryNoText
	}
	return text, nil
}

// Summarizable middle boundaries, an older assistant or tool bridge keeping the newest user ask out of the merge slot
func (c *ContextCompressor) compressWindow(messages []map[string]any) (int, int) {
	compressStart := c.AlignBoundaryForward(messages, c.protectHeadSize(messages))
	compressEnd := c.findTailCutByTokens(messages, compressStart, nil)
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
	working, pruned = c.pruneOldToolResults(messages, c.ProtectLastN, &tailTokens, pruneMinChars)
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
