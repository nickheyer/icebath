// Package batchrun runs the agent over a JSONL prompt dataset in parallel workers and writes trajectories, a checkpoint and tool statistics
package batchrun

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Call, success and failure tally of one tool
type toolStat struct{ count, success, failure int }

// Per tool tallies in first call order
type toolStats struct {
	names  []string
	byTool map[string]*toolStat
}

// Empty tally set
func newToolStats() *toolStats { return &toolStats{byTool: map[string]*toolStat{}} }

// Tally of a tool, created at zero on its first use
func (t *toolStats) of(name string) *toolStat {
	stat, ok := t.byTool[name]
	if !ok {
		stat = &toolStat{}
		t.byTool[name] = stat
		t.names = append(t.names, name)
	}
	return stat
}

// Adds every tally of other into this set
func (t *toolStats) merge(other *toolStats) {
	if other == nil {
		return
	}
	for _, name := range other.names {
		from, into := other.byTool[name], t.of(name)
		into.count += from.count
		into.success += from.success
		into.failure += from.failure
	}
}

// Assistant turn counters of a run, a sample or a batch
type reasoningStats struct{ total, with, without int }

// Adds the turn counters of other into these
func (r *reasoningStats) merge(other reasoningStats) {
	r.total += other.total
	r.with += other.with
	r.without += other.without
}

// Counters as the JSON object the statistics file stores
func (r reasoningStats) object() *pyjson.Object {
	return pyjson.ObjectOf("total_assistant_turns", r.total, "turns_with_reasoning", r.with, "turns_without_reasoning", r.without)
}

// Items of a message list value, nil for anything that is not a list
func listItems(v any) []any {
	switch list := v.(type) {
	case []any:
		return list
	case []map[string]any:
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = item
		}
		return out
	}
	return nil
}

// Value under a required key, the missing key named the way a failed lookup reports it
func requiredKey(m map[string]any, key string) (any, error) {
	v, ok := m[key]
	if !ok {
		return nil, fmt.Errorf("'%s'", key)
	}
	return v, nil
}

// Judges one tool response, a JSON error or success false failing and plain text failing only when empty or led by Error:
func toolCallSucceeded(content any) bool {
	decoded := content
	if text, ok := content.(string); ok {
		parsed, err := pyjson.Loads(text)
		if err != nil {
			return text != "" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "error:")
		}
		decoded = parsed
	}
	fields, ok := pyjson.Dict(decoded)
	if !ok {
		return true
	}
	if fields["error"] != nil {
		return false
	}
	if inner, ok := pyjson.Dict(fields["content"]); ok && inner["error"] != nil {
		return false
	}
	return fields["success"] != false
}

// Per tool call counts and success or failure tallies of a message history
func extractToolStats(messages []map[string]any) (*toolStats, error) {
	stats := newToolStats()
	callNames := map[any]string{}
	for _, msg := range messages {
		role, err := requiredKey(msg, "role")
		if err != nil {
			return nil, err
		}
		switch {
		case role == "assistant" && pyrepr.Truthy(msg["tool_calls"]):
			for _, raw := range listItems(msg["tool_calls"]) {
				call, ok := pyjson.Dict(raw)
				if !ok || len(call) == 0 {
					continue
				}
				fnValue, err := requiredKey(call, "function")
				if err != nil {
					return nil, err
				}
				fn, ok := pyjson.Dict(fnValue)
				if !ok {
					return nil, fmt.Errorf("a tool call function is %s, expected a mapping", pyrepr.TypeName(fnValue))
				}
				nameValue, err := requiredKey(fn, "name")
				if err != nil {
					return nil, err
				}
				name := pyrepr.Str(nameValue)
				stats.of(name).count++
				id, err := requiredKey(call, "id")
				if err != nil {
					return nil, err
				}
				if key, ok := hashableKey(id); ok {
					callNames[key] = name
				}
			}
		case role == "tool":
			var id any = ""
			if v, ok := msg["tool_call_id"]; ok {
				id = v
			}
			var content any = ""
			if v, ok := msg["content"]; ok {
				content = v
			}
			succeeded := toolCallSucceeded(content)
			key, ok := hashableKey(id)
			if !ok {
				continue
			}
			if name, known := callNames[key]; known {
				if succeeded {
					stats.byTool[name].success++
				} else {
					stats.byTool[name].failure++
				}
			}
		}
	}
	return stats, nil
}

// Tool call id as a map key, false for a value that cannot key a map
func hashableKey(v any) (any, bool) {
	switch v.(type) {
	case nil, string, bool, int, int64, float64:
		return v, true
	}
	return nil, false
}

// Whether an assistant turn carries a reasoning scratchpad in its content or native reasoning text
func turnHasReasoning(msg map[string]any) (bool, error) {
	if content, ok := msg["content"].(string); ok && strings.Contains(content, "<REASONING_SCRATCHPAD>") {
		return true, nil
	}
	reasoning := msg["reasoning"]
	if !pyrepr.Truthy(reasoning) {
		return false, nil
	}
	text, ok := reasoning.(string)
	if !ok {
		return false, fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(reasoning))
	}
	return strings.TrimSpace(text) != "", nil
}

// Assistant turns with and without reasoning and whether any turn reasoned
func extractReasoningStats(messages []map[string]any) (reasoningStats, bool, error) {
	var stats reasoningStats
	for _, msg := range messages {
		if msg["role"] != "assistant" {
			continue
		}
		stats.total++
		has, err := turnHasReasoning(msg)
		if err != nil {
			return reasoningStats{}, false, err
		}
		if has {
			stats.with++
		}
	}
	stats.without = stats.total - stats.with
	return stats, stats.with > 0, nil
}

// One count, success and failure object in the order the trajectory rows store
func (s *toolStat) object() *pyjson.Object {
	return pyjson.ObjectOf("count", s.count, "success", s.success, "failure", s.failure)
}

// Every known tool with zero defaults for a consistent row schema, then any tool outside the known set
func normalizeToolStats(stats *toolStats, allTools []string) *pyjson.Object {
	out := pyjson.NewObject()
	for _, name := range allTools {
		if stat, ok := stats.byTool[name]; ok {
			out.Set(name, stat.object())
		} else {
			out.Set(name, (&toolStat{}).object())
		}
	}
	for _, name := range stats.names {
		if _, known := out.Get(name); !known {
			out.Set(name, stats.byTool[name].object())
		}
	}
	return out
}

// Failure count of every known tool with zero defaults, then any tool outside the known set
func normalizeToolErrorCounts(stats *toolStats, allTools []string) *pyjson.Object {
	out := pyjson.NewObject()
	for _, name := range allTools {
		failures := 0
		if stat, ok := stats.byTool[name]; ok {
			failures = stat.failure
		}
		out.Set(name, failures)
	}
	for _, name := range stats.names {
		if _, known := out.Get(name); !known {
			out.Set(name, stats.byTool[name].failure)
		}
	}
	return out
}

// Value rounded to digits decimal places the way round does, ties resolved on the exact binary value
func roundTo(value float64, digits int) float64 {
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(value, 'f', digits, 64), 64)
	if err != nil {
		return value
	}
	return rounded
}

// Success and failure percentages of a tally over its judged calls, zero when none was judged
func (s *toolStat) rates() (float64, float64) {
	judged := s.success + s.failure
	if judged == 0 {
		return 0, 0
	}
	return roundTo(float64(s.success)/float64(judged)*100, 2), roundTo(float64(s.failure)/float64(judged)*100, 2)
}

// Run totals per tool with their success and failure rates, in first call order
func (t *toolStats) statistics() *pyjson.Object {
	out := pyjson.NewObject()
	for _, name := range t.names {
		stat := t.byTool[name]
		success, failure := stat.rates()
		entry := stat.object()
		entry.Set("success_rate", success)
		entry.Set("failure_rate", failure)
		out.Set(name, entry)
	}
	return out
}

// Tool names by descending call count, equal counts keeping first call order
func (t *toolStats) byCount() []string {
	names := append([]string{}, t.names...)
	sort.SliceStable(names, func(i, j int) bool { return t.byTool[names[i]].count > t.byTool[names[j]].count })
	return names
}
