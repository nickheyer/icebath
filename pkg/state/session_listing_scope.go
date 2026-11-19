package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

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
