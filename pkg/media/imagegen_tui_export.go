package media

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Whether any image generation backend is configured for the environment
func CheckImageGenerationRequirements(env *tools.Env) bool {
	return checkImageGenerationRequirements(env)
}

// Result JSON of the full image_generate dispatcher for a request outside any agent turn
func HandleImageGenerate(ctx context.Context, env *tools.Env, args map[string]any) string {
	return handleImageGenerate(ctx, &tools.Call{Name: "image_generate", Args: args, Env: env})
}
