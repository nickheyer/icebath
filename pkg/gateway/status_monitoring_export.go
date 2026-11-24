package gateway

import (
	"github.com/nickheyer/icebath/pkg/monitoring"
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Hands the runtime status reader, its derivations and the background work counters to the health export
func init() {
	monitoring.GatewayReadRuntimeStatus = func() *jsonutil.Object { return ReadRuntimeStatus("") }
	monitoring.GatewayDeriveBusy = DeriveGatewayBusy
	monitoring.GatewayDeriveDrainable = DeriveGatewayDrainable
	monitoring.AsyncDelegationActiveTaskCount = tools.AsyncDelegationActiveTaskCount
	monitoring.AsyncDelegationActiveCount = tools.AsyncDelegationActiveCount
	monitoring.ProcessRegistryCountRunning = func() int { return tools.DefaultProcessRegistry().CountRunning() }
}
