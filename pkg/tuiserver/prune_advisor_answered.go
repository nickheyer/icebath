package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Profile home the advisor reads, the active home when none is given
func pruneAdvisorHome(ctx context.Context, profileHome string) string {
	if profileHome != "" {
		return profileHome
	}
	return home.Dir(ctx)
}

// Sessions block of the effective user config without defaults, ok false when the config cannot be read
func pruneAdvisorEffectiveSessions(ctx context.Context, profileHome string) (map[string]any, bool) {
	cfg, err := config.LoadUserEffectiveFailClosed(filepath.Join(pruneAdvisorHome(ctx, profileHome), "config.yaml"), os.LookupEnv)
	if err != nil {
		logger().Debug("auto-prune advice: config unreadable: " + err.Error())
		return nil, false
	}
	sessions, isDict := cfg["sessions"].(map[string]any)
	if !isDict {
		return map[string]any{}, true
	}
	return sessions, true
}

// True while neither the user config nor the managed layer sets sessions auto_prune
func pruneAdvisorUndecided(ctx context.Context, profileHome string) bool {
	sessions, ok := pruneAdvisorEffectiveSessions(ctx, profileHome)
	if !ok {
		return false
	}
	_, set := sessions["auto_prune"]
	return !set
}

// True while a later answer snooze of the auto prune question has not expired
func pruneAdvisorSnoozed(ctx context.Context, profileHome string, now time.Time) bool {
	raw, err := os.ReadFile(filepath.Join(pruneAdvisorHome(ctx, profileHome), ".auto_prune_prompt.json"))
	if err != nil {
		return false
	}
	parsed, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff"))
	if err != nil {
		return false
	}
	doc, isDict := parsed.(map[string]any)
	if !isDict {
		return false
	}
	until, present := doc["snoozed_until"]
	if !present {
		until = 0
	}
	limit, err := pyint.Float(until)
	if err != nil {
		return false
	}
	return float64(now.UnixNano())/1e9 < limit
}

// True once the auto prune question is settled by a saved answer or a live snooze
func pruneAdvisorAnswered(ctx context.Context, profileHome string) bool {
	return !pruneAdvisorUndecided(ctx, profileHome) || pruneAdvisorSnoozed(ctx, profileHome, time.Now())
}
