package tuiserver

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Checkpoint enablement, the launch flag first and then the file checkpoints setting
func resolveCheckpointsEnabled(ctx context.Context) bool {
	if value, set := os.LookupEnv("ICEBATH_TUI_CHECKPOINTS"); set {
		return provider.IsTruthyValue(value)
	}
	return settings.CheckpointsEnabled.Get(ctx)
}
