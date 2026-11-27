package engine

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/provider"
)

const (
	// Smallest window a session runs on, and the window a model whose size nobody knows is taken to have
	minimumWindow = 64_000
	// Windows below this trigger at no less than their share
	smallWindow        = 512_000
	smallWindowPercent = 0.75
	// Share of the input budget the trigger never goes over, so a small window still fires
	triggerCeiling = 0.85
	// Share of the window the protected tail may take at most
	tailMaxContextFraction = 0.20
	// Lean tail: 2.5 percent of the window between the floor and the cap, held to half the trigger
	leanTailFloorTokens = 10_000
	leanTailCapTokens   = 25_000
	tailMaxThreshold    = 0.50
	minSummaryTokens    = 2000
	summaryRatio        = 0.20
	// Most tokens a summary may take
	summaryTokensCeiling = 10_000
)

// Trigger share of a window: models under 512K trigger at no less than 75 percent
func effectivePercent(window int, percent float64) float64 {
	if window != 0 && window < smallWindow {
		return max(percent, smallWindowPercent)
	}
	return percent
}

// Window less the reply's reservation, the whole window when that leaves nothing
func inputWindow(window, reserve int) int {
	if w := window - reserve; w > 0 {
		return w
	}
	return window
}

// Trigger in tokens: the share of the input window, floored at the smallest window and capped so a small window still fires
func thresholdTokens(window int, percent float64, reserve int) int {
	input := inputWindow(window, reserve)
	share := int(float64(input) * percent)
	floored := max(share, minimumWindow)
	ceiling := int(float64(input) * triggerCeiling)
	if input > 0 && floored > share && floored > ceiling {
		floored = max(share, ceiling)
	}
	if input > 0 && floored >= input {
		return max(1, min(ceiling, input-1))
	}
	return floored
}

// Lean tail budget: 2.5 percent of the window inside the floor and the cap, held to half the trigger
func leanTailBudget(window, threshold int) int {
	floor, term := leanTailFloorTokens, min(leanTailCapTokens, int(float64(window)*0.025))
	if threshold > 0 {
		share := int(float64(threshold) * tailMaxThreshold)
		floor = min(floor, share)
		if threshold > minSummaryTokens {
			term = min(term, share)
		}
	}
	return max(floor, term)
}

// Trigger share of the model from the table: the longest key the model holds wins, a provider:model key over a bare one; the default
// when none does
func resolveModelThreshold(model string, table map[string]float64, def float64, providerName string) float64 {
	if len(table) == 0 || model == "" {
		return def
	}
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	best, bestLen, bestScope, found := "", 0, 0, false
	for key := range table {
		length, scope, ok := keyRank(key, model, providerName)
		if !ok {
			continue
		}
		if !found || length > bestLen || length == bestLen && (scope > bestScope || scope == bestScope && key > best) {
			best, bestLen, bestScope, found = key, length, scope, true
		}
	}
	if !found {
		return def
	}
	return table[best]
}

// Length of the part of a key the model holds and whether it names a provider, false when the key does not apply
func keyRank(key, model, providerName string) (int, int, bool) {
	scope, part, scoped := strings.Cut(key, ":")
	if !scoped {
		return utf8.RuneCountInString(key), 0, strings.Contains(model, key)
	}
	if strings.ToLower(strings.TrimSpace(scope)) == providerName && strings.Contains(model, part) {
		return utf8.RuneCountInString(part), 1, true
	}
	return 0, 0, false
}

// Model name without its vendor, lower case
func bare(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	return m[strings.LastIndex(m, "/")+1:]
}

// Codex models whose 900K variant exists, and the dated snapshots of some of them
var (
	codexLongBases = map[string]bool{"gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true, "gpt-6-sol": true, "gpt-6-luna": true,
		"gpt-5.4": true, "gpt-daybreak-blue-latest": true, "gpt-6-astra": true, "gpt-6.1-sol": true}
	codexSnapshotBases = []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-sol", "gpt-6-luna"}
	snapshotDate       = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// Whether a Codex model is the 900K variant of an eligible one
func codexLongVariant(slug string) bool {
	base, ok := strings.CutSuffix(slug, "-900k")
	if !ok {
		return false
	}
	if codexLongBases[base] {
		return true
	}
	for _, prefix := range codexSnapshotBases {
		if rest, ok := strings.CutPrefix(base, prefix+"-"); ok && snapshotDate.MatchString(rest) {
			return true
		}
	}
	return false
}

// Whether the route is a Codex model capped at 272K whose trigger is raised: gpt-5.4 to 5.6, gpt-6 Sol, Luna and Astra on Codex
func codexAutoraised(route provider.Route) bool {
	if route.Spec != "openai-codex" && route.Provider != "openai-codex" {
		return false
	}
	slug := bare(route.Model)
	if codexLongVariant(slug) {
		return false
	}
	if strings.Contains(slug, "astra") {
		return !strings.Contains(slug, "900k")
	}
	if slug == "gpt-daybreak-blue-latest" {
		return true
	}
	for _, family := range []string{"gpt-5.4", "gpt-5.5", "gpt-5.6", "gpt-6-sol", "gpt-6.1-sol", "gpt-6-luna"} {
		if slug == family || strings.HasPrefix(slug, family+"-") || strings.HasPrefix(slug, family+".") {
			return true
		}
	}
	return false
}

// Whether the route is Codex's Spark model
func codexSpark(route provider.Route) bool {
	return (route.Spec == "openai-codex" || route.Provider == "openai-codex") && bare(route.Model) == "gpt-5.3-codex-spark"
}

// Trigger share a model of the route needs, over the setting's; false when the setting stands
func modelThreshold(route provider.Route, autoraise bool) (float64, bool) {
	switch {
	case bare(route.Model) == "trinity-large-thinking":
		return 0.75, true
	case autoraise && codexAutoraised(route):
		return 0.85, true
	case codexSpark(route):
		return 0.70, true
	}
	return 0, false
}
