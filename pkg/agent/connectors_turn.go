package agent

import "github.com/nickheyer/icebath/pkg/tools"

// Card for an agent with a connection callback, side for a delegated side agent, link otherwise
func agentConnectionSurface(a *AIAgent) string {
	if a.connectionCallback != nil {
		return tools.SurfaceCard
	}
	if a.sideAgent {
		return tools.SurfaceSide
	}
	return tools.SurfaceLink
}
