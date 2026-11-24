package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/settings"
)

// ConvertToTrajectoryFormat renders a finished message history as the ShareGPT turns a trajectory file stores
func (a *AIAgent) ConvertToTrajectoryFormat(messages []map[string]any, userQuery string) ([]any, error) {
	return a.convertToTrajectoryFormat(messages, userQuery)
}

// CheckpointRequiredFromConfig reports whether compression.checkpoint_required is armed for ctx, for a caller that decides before its agent exists
func CheckpointRequiredFromConfig(ctx context.Context) bool {
	return settings.CompressionCheckpointRequired.Get(ctx)
}
