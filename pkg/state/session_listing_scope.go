package state

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// SubagentSource is the session source of delegate runs
const SubagentSource = "subagent"

var listingTruthyStrings = map[string]bool{"1": true, "true": true, "yes": true, "on": true}

// Truthy flag the shared way, a string by its stripped lowercase spelling and anything else by its truth value
func listingTruthyValue(v any) bool {
	if s, ok := v.(string); ok {
		return listingTruthyStrings[pystr.Lower(pystr.Strip(s))]
	}
	return pyTruthy(v)
}

// Whether sessions.show_subagents of the store home's own config is truthy, false when that config cannot be read
func ShowSubagentSessions(ctx context.Context, storeHome string) bool {
	homeCtx := home.WithOverride(ctx, storeHome)
	tree, err := config.LoadReadonly(homeCtx, home.Dir(homeCtx), config.EnvRefLookupContext(homeCtx))
	if err != nil {
		return false
	}
	var sessions any = map[string]any{}
	if pyTruthy(tree["sessions"]) {
		sessions = tree["sessions"]
	}
	cfg, ok := sessions.(map[string]any)
	return ok && listingTruthyValue(cfg["show_subagents"])
}

// Whether delegate runs join one human facing list and the exclusions that list keeps, nil when none remain
func SubagentListingScope(ctx context.Context, storeHome, source string, sources, excludeSources []string) (bool, []string) {
	if source != "" || len(sources) > 0 || (len(excludeSources) > 0 && !slices.Contains(excludeSources, SubagentSource)) {
		return false, excludeSources
	}
	if !ShowSubagentSessions(ctx, storeHome) {
		return false, excludeSources
	}
	var kept []string
	for _, s := range excludeSources {
		if s != SubagentSource {
			kept = append(kept, s)
		}
	}
	return true, kept
}
