package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Whether the last compression compacted the session in place instead of rotating it
func (a *AIAgent) LastCompactionInPlace() bool { return a.lastCompactionInPlace }

// Prefers in place compaction over minting a continuation child session
func (a *AIAgent) SetCompressionInPlace(on bool) { a.compressionInPlace = on }

// Stamps the surface the agent runs for
func (a *AIAgent) SetPlatform(platform string) { a.platform = platform }

// Compresses messages under the gateway's commit fence for one task, the gateway hygiene entry
func (a *AIAgent) CompressContextUnderFence(ctx context.Context, messages []map[string]any, approxTokens int, fence *CompressionCommitFence, taskID, trigger string) ([]map[string]any, error) {
	system := ""
	out, _, err := a.compressContext(ctx, messages, &system, compressContextOptions{ApproxTokens: &approxTokens, CommitFence: fence, TaskID: taskID, Trigger: &trigger})
	return out, err
}

// Whether the last compress aborted with nothing dropped
func (c *ContextCompressor) LastCompressAborted() bool { return c.lastCompressAborted }

// Seeds the cached system prompt the next build reuses
func (a *AIAgent) SetCachedSystemPrompt(prompt string) { a.cachedSystemPrompt = prompt }

// Stamps the activity clock with a provenance and no forced persist
func (a *AIAgent) TouchActivityWithProvenance(desc, provenance string) {
	a.touchActivity(desc, provenance, false)
}

// Rough tokens of dict message rows at the image cost bound on ctx
func EstimateMessagesTokensRough(ctx context.Context, rows []map[string]any) int {
	return provider.EstimateRowsTokens(rows, provider.CurrentImageTokenCost(ctx))
}

// True when a compression dropped rows or cut tokens by more than five percent
func CompressionMadeProgress(origLen, newLen, origTokens, newTokens int) bool {
	return compressionMadeProgress(origLen, newLen, origTokens, newTokens)
}

// Error text of the last failed summary, nil when none was recorded
func (c *ContextCompressor) LastSummaryErrorValue() *string { return c.lastSummaryError }
