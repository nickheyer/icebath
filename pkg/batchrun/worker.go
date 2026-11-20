package batchrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// What one batch reports back to the run
type batchResult struct {
	batchNum         int
	processed        int
	skipped          int
	toolStats        *toolStats
	reasoning        reasoningStats
	discarded        int
	completedPrompts []int
}

// One row as a JSON line, raw UTF-8 unless the row holds text that is not valid UTF-8
func jsonLine(row *pyjson.Object) (string, error) {
	text, err := pyjson.Dumps(row, pyjson.EnsureASCII(false))
	if err != nil {
		return "", err
	}
	if !utf8.ValidString(text) {
		if text, err = pyjson.Dumps(row, pyjson.EnsureASCII(true)); err != nil {
			return "", err
		}
	}
	return text + "\n", nil
}

// Appends one JSON row and syncs it to disk so a crash never loses an acknowledged prompt
func appendJSONL(path string, row *pyjson.Object) (err error) {
	line, err := jsonLine(row)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	if _, err = f.WriteString(line); err != nil {
		return err
	}
	return f.Sync()
}

// Text of a dataset or trajectory value the way str renders it, stripped
func strippedText(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

// Human prompt text of a dataset or trajectory entry, from a flat prompt, from/value or role/content turns, or a discard tombstone
func entryPromptText(entry any) string {
	fields, ok := pyjson.Dict(entry)
	if !ok {
		return ""
	}
	if text := strippedText(fields["prompt"]); text != "" {
		return text
	}
	for _, key := range []string{"conversations", "messages"} {
		for _, item := range listItems(fields[key]) {
			msg, ok := pyjson.Dict(item)
			if !ok {
				continue
			}
			role := msg["role"]
			if !pyrepr.Truthy(role) {
				role = msg["from"]
			}
			if role != "user" && role != "human" {
				continue
			}
			content := msg["content"]
			if !pyrepr.Truthy(content) {
				content = msg["value"]
			}
			if text := strippedText(content); text != "" {
				return text
			}
		}
	}
	return ""
}

// Trajectory row of a finished prompt, tool statistics normalized to one schema across every row
func (r *Runner) trajectoryRow(result promptResult) *pyjson.Object {
	row := pyjson.NewObject()
	row.Set("prompt_index", result.promptIndex)
	row.Set("conversations", agent.RedactTrajectoryValue(result.trajectory))
	row.Set("metadata", result.metadata)
	row.Set("completed", result.completed)
	row.Set("partial", result.partial)
	row.Set("api_calls", result.apiCalls)
	row.Set("toolsets_used", stringList(result.toolsetsUsed))
	row.Set("tool_stats", normalizeToolStats(result.toolStats, r.allTools))
	row.Set("tool_error_counts", normalizeToolErrorCounts(result.toolStats, r.allTools))
	return row
}

// Runs the prompts of one batch in order, appending a row per finished prompt, and reports what the batch did
func (r *Runner) processBatch(ctx context.Context, w *workerState, batchNum int, batch []indexedEntry, completed map[int]bool) (batchResult, error) {
	r.printf("\n🔄 Batch %d: Starting (%d prompts)\n", batchNum, len(batch))
	outputFile := filepath.Join(r.outputDir, fmt.Sprintf("batch_%d.jsonl", batchNum))
	var pending []indexedEntry
	for _, item := range batch {
		if !completed[item.index] {
			pending = append(pending, item)
		}
	}
	result := batchResult{batchNum: batchNum, processed: len(pending), skipped: len(batch) - len(pending), toolStats: newToolStats()}
	if len(pending) == 0 {
		r.printf("✅ Batch %d: Already completed (skipping)\n", batchNum)
		return result, nil
	}
	r.printf("   Processing %d prompts (skipping %d already completed)\n", len(pending), result.skipped)
	for _, item := range pending {
		if r.agents.isStopping() || ctx.Err() != nil {
			return result, errStopped
		}
		prompt, err := r.processPrompt(ctx, w, item.index, item.entry, batchNum)
		if err != nil {
			return result, err
		}
		saved := prompt.success && len(prompt.trajectory) > 0
		if saved {
			if !prompt.hasReasoning {
				r.printf("   🚫 Prompt %d discarded (no reasoning in any turn)\n", item.index)
				result.discarded++
				result.completedPrompts = append(result.completedPrompts, item.index)
				tombstone := pyjson.ObjectOf("prompt_index", item.index, "discarded", "no_reasoning", "prompt", entryPromptText(item.entry))
				if err := appendJSONL(outputFile, tombstone); err != nil {
					return result, err
				}
				continue
			}
			if err := appendJSONL(outputFile, r.trajectoryRow(prompt)); err != nil {
				return result, err
			}
		}
		result.toolStats.merge(prompt.toolStats)
		result.reasoning.merge(prompt.reasoning)
		if !saved {
			r.printf("   ❌ Prompt %d failed (will retry on resume)\n", item.index)
			continue
		}
		result.completedPrompts = append(result.completedPrompts, item.index)
		status := "✅"
		if pyrepr.Truthy(prompt.partial) {
			status = "⚠️  partial"
		}
		r.printf("   %s Prompt %d completed\n", status, item.index)
	}
	r.printf("✅ Batch %d: Completed (%d prompts processed)\n", batchNum, len(pending))
	return result, nil
}

// Runs one batch with a panic of the batch code returned as an error carrying its stack
func (r *Runner) processBatchSafely(ctx context.Context, w *workerState, batchNum int, batch []indexedEntry, completed map[int]bool) (result batchResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &promptPanic{value: recovered, stack: debug.Stack()}
		}
	}()
	return r.processBatch(ctx, w, batchNum, batch, completed)
}

// Whether an error is the stop of an interrupted run rather than a failure
func isStopped(err error) bool { return errors.Is(err, errStopped) }
