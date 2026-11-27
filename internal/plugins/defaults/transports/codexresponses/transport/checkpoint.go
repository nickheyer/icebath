package transport

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// Tokens of the person's messages and of the summaries before the newest compaction checkpoint that are sent after it
const (
	retainedUsers     = 64000
	retainedSummaries = 32000
	charsPerToken     = 4
)

// Start of a summary icebath left in the conversation when it compacted it itself
const summaryStart = "[CONTEXT COMPACTION"

// Input built around the newest compaction checkpoint the provider handed back: the checkpoints, then the newest messages of the person and
// summaries from before them within their budgets, then everything after; the input as it is when it holds no checkpoint
func pruneCheckpointed(input []any) []any {
	last := -1
	for i, entry := range input {
		if checkpoint(entry) {
			last = i
		}
	}
	if last < 0 {
		return input
	}
	start := last
	for start > 0 && checkpoint(input[start-1]) {
		start--
	}
	users, summaries := retainedUsers, retainedSummaries
	var kept []any
	seen := map[string]bool{}
	for i := start - 1; i >= 0; i-- {
		entry, ok := input[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := entry["role"].(string)
		text := entryText(entry["content"])
		switch {
		case strings.HasPrefix(strings.TrimSpace(text), summaryStart):
			cost := max(1, utf8.RuneCountInString(text)/charsPerToken)
			if summaries > 0 && cost <= summaries && !seen[text] {
				seen[text] = true
				kept = append(kept, entry)
				summaries -= cost
			}
		case role == "user" && users > 0:
			cost := max(1, utf8.RuneCountInString(text)/charsPerToken)
			if cost <= users {
				kept = append(kept, entry)
				users -= cost
				continue
			}
			head := []rune(text)[:users*charsPerToken]
			if strings.TrimSpace(string(head)) != "" {
				kept = append(kept, map[string]any{"role": "user", "content": []map[string]any{{"type": "input_text", "text": string(head)}}})
			}
			users = 0
		}
	}
	out := make([]any, 0, len(input))
	out = append(out, input[start:last+1]...)
	for i := len(kept) - 1; i >= 0; i-- {
		out = append(out, kept[i])
	}
	out = append(out, input[last+1:]...)
	slog.Debug(fmt.Sprintf("Pruned pre-checkpoint items: %d input -> %d retained (user_rem=%d, summary_rem=%d)", len(input), len(out), users, summaries))
	return out
}

// Whether an input entry is a compaction checkpoint
func checkpoint(entry any) bool {
	switch e := entry.(type) {
	case json.RawMessage:
		var probe struct {
			Type string `json:"type"`
		}
		return json.Unmarshal(e, &probe) == nil && probe.Type == "compaction"
	case map[string]any:
		return e["type"] == "compaction"
	}
	return false
}

// Text of an entry's content, its text parts joined
func entryText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []map[string]any:
		var parts []string
		for _, part := range c {
			if text, ok := part["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
