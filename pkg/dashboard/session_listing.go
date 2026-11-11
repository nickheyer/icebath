package dashboard

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

const subagentSource = "subagent"

// Whether the sessions.show_subagents key of a home's own config is truthy, false when it cannot be read
func showSubagentSessions(ctx context.Context, profileHome string) bool {
	homeCtx := home.WithOverride(ctx, profileHome)
	tree, err := config.LoadReadonly(homeCtx, home.Dir(homeCtx), config.EnvRefLookupContext(homeCtx))
	if err != nil {
		return false
	}
	sessions := tree["sessions"]
	if !pyTruthy(sessions) {
		return false
	}
	cfg, ok := sessions.(map[string]any)
	return ok && provider.IsTruthyValue(cfg["show_subagents"])
}

// Whether delegate runs join one human facing list and the exclusions that list keeps
func subagentListingScope(ctx context.Context, profileHome, source string, sources, excludeSources []string) (bool, []string) {
	if source != "" || len(sources) > 0 || (len(excludeSources) > 0 && !slices.Contains(excludeSources, subagentSource)) {
		return false, excludeSources
	}
	if !showSubagentSessions(ctx, profileHome) {
		return false, excludeSources
	}
	var kept []string
	for _, s := range excludeSources {
		if s != subagentSource {
			kept = append(kept, s)
		}
	}
	return true, kept
}
