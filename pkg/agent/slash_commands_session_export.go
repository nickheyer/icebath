package agent

import "context"

// Forced compaction of the live context under the task id, the manual compress of a codex app server session
func (a *AIAgent) ForceCompressContext(ctx context.Context, messages []map[string]any, systemMessage, taskID string) ([]map[string]any, string, error) {
	return a.compressContext(ctx, messages, &systemMessage, compressContextOptions{Force: true, TaskID: taskID})
}

// The api_content sidecar of a message, false when absent or not a string
func ExtractAPIContentSidecar(msg map[string]any) (string, bool) {
	return extractAPIContentSidecar(msg)
}

// Percent the latest compaction saved, the context command's last savings line
func (c *ContextCompressor) LastCompressionSavingsPct() float64 { return c.lastCompressionSavingsPct }
