package provider

import (
	"sort"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Rank of each value LM Studio accepts, weakest to strongest
var lmStudioRank = map[string]int{"minimal": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4}

func lmStudioEffort(t *testing.T, cfg map[string]any, allowed []string) (string, bool) {
	t.Helper()
	return resolveLMStudioEffort(reasoningFromRequest(reasoningFromConfig(cfg)), allowed)
}

func TestEffortLadderIsMonotonic(t *testing.T) {
	var ranks []int
	resolved := map[string]string{}
	for _, effort := range config.ValidReasoningEfforts {
		value, ok := lmStudioEffort(t, map[string]any{"enabled": true, "effort": effort}, nil)
		if !ok {
			t.Fatalf("%s resolved to an omitted field", effort)
		}
		rank, known := lmStudioRank[value]
		if !known {
			t.Fatalf("%s resolved to %q outside the LM Studio vocabulary", effort, value)
		}
		ranks = append(ranks, rank)
		resolved[effort] = value
	}
	if !sort.IntsAreSorted(ranks) {
		t.Fatalf("ladder inverts: %v", resolved)
	}
}

func TestClampedEffortIsStillCheckedAgainstAllowedOptions(t *testing.T) {
	for _, effort := range []string{"max", "ultra"} {
		t.Run(effort, func(t *testing.T) {
			if value, ok := lmStudioEffort(t, map[string]any{"enabled": true, "effort": effort}, []string{"off", "minimal", "low"}); ok {
				t.Fatalf("%s resolved to %q against a model that publishes no xhigh", effort, value)
			}
			if value, ok := lmStudioEffort(t, map[string]any{"enabled": true, "effort": effort}, []string{"low", "medium", "high", "xhigh"}); !ok || value != "xhigh" {
				t.Fatalf("%s resolved to %q %v, want xhigh", effort, value, ok)
			}
		})
	}
}

func TestClampDoesNotRewritePublishedAllowedOptions(t *testing.T) {
	if value, ok := lmStudioEffort(t, map[string]any{"enabled": true, "effort": "max"}, []string{"max"}); ok {
		t.Fatalf("max resolved to %q against allowed_options [max]", value)
	}
}
