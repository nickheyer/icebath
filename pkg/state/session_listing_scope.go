package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Whether sessions.show_subagents of the store home is on
func ShowSubagentSessions(ctx context.Context, storeHome string) bool {
	return settings.SessionsShowSubagents.Get(home.WithOverride(ctx, storeHome))
}
