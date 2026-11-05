package tools

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Shared metrics mark name for one file edit tool call
const fileEditMetricName = "icebath.file_edit"

// Values key carrying the session's memory write origin, background reviews never meter their edits
const writeOriginValuesKey = "write_origin"

// Write origin the review and curator forks tag themselves with
const backgroundReviewOrigin = "background_review"

// The patch tool's mode argument mapped to the metric's mode bucket
var fileEditModes = map[string]string{"replace": "replace", "patch": "v4a", "whole_file": "whole_file"}

// editProbe collects which strategies landed and why none did during one metered edit tool call
type editProbe struct {
	strategies []string
	misses     []string
}

// Records the strategy that landed a replacement or the miss reason, a no op on a nil probe
func (p *editProbe) noteEditMatch(strategy, miss string) {
	if p == nil {
		return
	}
	if strategy != "" {
		p.strategies = append(p.strategies, strategy)
	} else if miss != "" {
		p.misses = append(p.misses, miss)
	}
}

// Shared metrics are opted in through telemetry.shared_metrics.enabled
func sharedMetricsEnabled(env *Env) bool {
	return cfgBool(env, "telemetry.shared_metrics.enabled", false)
}

// True when the session's write origin marks a background review or curator fork
func isBackgroundReview(env *Env) bool {
	if env == nil || env.Values == nil {
		return false
	}
	origin, _ := env.Values.Load(writeOriginValuesKey)
	return origin == backgroundReviewOrigin
}

// Position of each strategy in the matcher chain, higher meaning looser
func fuzzyStrategyRank() map[string]int {
	rank := map[string]int{}
	for i, st := range fuzzyStrategies {
		rank[st.name] = i
	}
	return rank
}

// Parsed tool result object, empty when the result is not a JSON object
func parsedResultObject(result string) map[string]any {
	decoded, err := pyjson.DecodeWTF8([]byte(result))
	if err != nil {
		return map[string]any{}
	}
	data, _ := decoded.(map[string]any)
	return data
}

// Metric fields describing how one patch or write_file call landed
func fileEditFields(tool, mode, result string, probe *editProbe) map[string]any {
	data := parsedResultObject(result)
	_, hasSuccess := data["success"]
	errValue, _ := data["error"]
	succeeded := data["success"] == true || (len(data) > 0 && !hasSuccess && !truthy(errValue))
	outcome := "failed"
	switch {
	case succeeded:
		outcome = "applied"
		if data["no_change"] == true {
			outcome = "already_applied"
		}
	case containsString(probe.misses, "ambiguous"):
		outcome = "ambiguous"
	case containsString(probe.misses, "no_match"):
		outcome = "no_match"
	}
	strategy := "none"
	if outcome == "applied" && len(probe.strategies) > 0 {
		rank := fuzzyStrategyRank()
		best := -2
		for _, name := range probe.strategies {
			r, ok := rank[name]
			if !ok {
				r = -1
			}
			if r > best {
				best, strategy = r, name
			}
		}
		if _, known := rank[strategy]; !known {
			strategy = "none"
		}
	}
	modeValue, ok := fileEditModes[strings.ToLower(strings.TrimSpace(mode))]
	if !ok {
		modeValue = "replace"
	}
	return map[string]any{"match_strategy": strategy, "mode": modeValue, "outcome": outcome, "tool": tool}
}

// Runs one patch or write_file call with a probe and emits the file edit metric, result returned unchanged
func recordFileEdit(env *Env, tool, mode string, call func(probe *editProbe) string) string {
	if !sharedMetricsEnabled(env) || isBackgroundReview(env) {
		return call(nil)
	}
	probe := &editProbe{}
	result := call(probe)
	data := fileEditFields(tool, mode, result, probe)
	data["name"] = fileEditMetricName
	env.Emit(Event{Kind: "metric", Tool: tool, Data: data})
	return result
}
