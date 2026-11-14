package gateway_test

import (
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Installs the agent side error surface builder the process wiring assigns, for every case of the package
func init() {
	gateway.ErrorSurfaceFromResult = agent.BuildErrorSurfaceFromResult
	gateway.ExpandContextReferences = agent.ExpandGatewayContextReferences
}
