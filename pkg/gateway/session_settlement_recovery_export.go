package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Publishes a settled turn's completion frame under the stream lock the caller holds
func PublishCompletion(ctx context.Context, a *SessionAuthority, live *LiveSession, ref SessionRef, row, settled *pyjson.Object, response any, captured *pyjson.Object) error {
	return publishCompletion(ctx, a, live, ref, row, settled, response, captured)
}
