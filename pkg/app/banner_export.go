package app

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Skill names grouped by category for the TUI server session info
func AvailableSkills(ctx context.Context) *pyjson.Object { return getAvailableSkills(ctx) }

// Commits behind of the background update check, nil while unknown, waiting at most the timeout
func UpdateResult(timeout time.Duration) *int64 { return getUpdateResult(timeout) }

// Unavailable enabled toolsets with their tools split into lazy and disabled names for the banner colours
func ToolsetAvailability(ctx context.Context, enabled []string) map[string]any {
	return computeToolsetAvailability(ctx, enabled)
}
