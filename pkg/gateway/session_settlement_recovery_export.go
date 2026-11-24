package gateway

import (
	"context"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Publishes a settled turn's completion frame under the stream lock the caller holds
func PublishCompletion(ctx context.Context, a *SessionAuthority, live *LiveSession, ref SessionRef, row, settled *jsonutil.Object, response any, captured *jsonutil.Object) error {
	return publishCompletion(ctx, a, live, ref, row, settled, response, captured)
}
