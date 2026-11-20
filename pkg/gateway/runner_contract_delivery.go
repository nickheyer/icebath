package gateway

import "context"

// Profile aware fail closed adapter lookup of a runner that never borrows the default bot, gateway/authz_mixin.py _authorization_adapter
type AuthorizationAdapterRunner interface {
	AuthorizationAdapter(ctx context.Context, platform Platform, profile *string) PlatformAdapter
}

// Runner methods that adapter delivery, plugin messages, parked refusals and the gateway loop callers reach at runtime
type RunnerDeliveryContract interface {
	FailedObligationRedeliverer
	FloodRedeliveryScheduler
	SourceProfileResolver
	DeliveryAdapterRunner
	threadMetadataRunner
	PluginMessageRunner
	GatewayLoopRunner
	AuthorizationAdapterRunner
}
