package agent

import "context"

// ConvertToTrajectoryFormat renders a finished message history as the ShareGPT turns a trajectory file stores
func (a *AIAgent) ConvertToTrajectoryFormat(messages []map[string]any, userQuery string) ([]any, error) {
	return a.convertToTrajectoryFormat(messages, userQuery)
}

// CheckpointRequiredFromConfig reports whether compression.checkpoint_required is armed, for a caller that decides before its agent exists
func CheckpointRequiredFromConfig(ctx context.Context) bool {
	cfg, err := agentConfigLoader(ctx)
	if err != nil {
		return false
	}
	compression, ok := cfg["compression"].(map[string]any)
	return ok && isTruthyValue(compression["checkpoint_required"], false)
}
