package media

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Download policy of one save_url call made on behalf of a hosted plugin
type SaveURLOptions = saveURLOptions

// Streams a provider URL into the media cache of env under the given policy and returns the cached path
func SaveURL(ctx context.Context, env *tools.Env, kind, rawURL string, o SaveURLOptions) (string, error) {
	return saveURL(ctx, env, kind, rawURL, o)
}
