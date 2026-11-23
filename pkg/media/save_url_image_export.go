package media

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Downloads an image URL into the generated images cache under the full SSRF guard with the sixty second and 25 MiB defaults
func SaveURLImage(ctx context.Context, env *tools.Env, rawURL, prefix string) (string, error) {
	return saveURLImage(ctx, env, rawURL, prefix, 0, 25*1024*1024, "")
}
