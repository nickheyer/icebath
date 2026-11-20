package media

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Downloads a provider image URL into the generated images cache with the sixty second and 25 MiB defaults
func SaveURLImage(ctx context.Context, env *tools.Env, rawURL, prefix string) (string, error) {
	return saveURLImage(ctx, env, rawURL, prefix, 0, 25*1024*1024)
}
