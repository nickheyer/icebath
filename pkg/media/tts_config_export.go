package media

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Text to speech section of the active config with its defaults
func LoadTTSConfig(ctx context.Context, env *tools.Env) map[string]any {
	return loadTTSConfig(ctx, env)
}
