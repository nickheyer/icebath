package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Setup beat card server request answered by the renderer, nil when nobody answered in time
func setupBeatRequest(ctx context.Context, sid string, payload map[string]any) *pyjson.Object {
	timeout := float64(tools.OnboardingCardTimeout)
	return sendResult(ctx, tuigateway.Send)("setup_beat", sid, objectOf(copyDict(payload)), &timeout, nil)
}
