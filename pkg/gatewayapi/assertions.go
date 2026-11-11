package gatewayapi

import "github.com/nickheyer/icebath/pkg/gateway"

// Surfaces other packages reach the API server adapter through, checked at compile time
var (
	_ gateway.PlatformAdapter                             = (*APIServerAdapter)(nil)
	_ gateway.DelegationDeliveryStore                     = (*APIServerAdapter)(nil)
	_ gateway.SelfPostEndpoint                            = (*APIServerAdapter)(nil)
	_ gateway.InternalSessionTurnRunner                   = (*APIServerAdapter)(nil)
	_ gateway.APITurnAdapter                              = (*APIServerAdapter)(nil)
	_ gateway.APIRunAdmitter                              = (*APIServerAdapter)(nil)
	_ interface{ ActiveAgentWorkCount() int }             = (*APIServerAdapter)(nil)
	_ interface{ InterruptActiveRuns(reason string) int } = (*APIServerAdapter)(nil)
	_ interface{ MarkShutdownRequested() int }            = (*APIServerAdapter)(nil)
)
