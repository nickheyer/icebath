package plugins

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Relay rewritten tool arguments at the authorization boundary, a Relay failure returned
func relayApplyToolRequestIntercepts(ctx context.Context, sessionID, toolName string, args map[string]any) (map[string]any, error) {
	return tools.ApplyRelayToolRequestIntercepts(ctx, sessionID, toolName, args)
}
