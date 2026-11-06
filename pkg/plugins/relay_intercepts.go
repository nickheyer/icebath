package plugins

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Relay rewritten tool arguments at the authorization boundary, a Relay failure propagating as a panic
func relayApplyToolRequestIntercepts(sessionID, toolName string, args map[string]any) map[string]any {
	out, err := tools.ApplyRelayToolRequestIntercepts(context.Background(), sessionID, toolName, args)
	if err != nil {
		panic(err)
	}
	return out
}
