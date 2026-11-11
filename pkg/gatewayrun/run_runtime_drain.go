package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Adapter whose public ingress callback can be withdrawn
type messageHandlerSetter interface {
	SetMessageHandler(handler gateway.MessageHandler)
}

// Withdraws admission before any wait and stops hosted rooms, keeping egress for admitted work
func (r *GatewayRunner) drainGatewayRuntime(ctx context.Context) {
	if r.sessionRuntimeDescriptor.load() == nil {
		return
	}
	r.draining.Store(true)
	r.sessionRuntimeDescriptor.update(func(descriptor map[string]any) {
		descriptor["state"] = "draining"
		descriptor["capabilities"] = []any{}
		if reason, ok := descriptor["drain_reason"].(string); ok && reason == gateway.RestartWaitReason {
			delete(descriptor, "drain_reason")
		}
	})
	gateway.StopHostedService(ctx, r, 5)
	for _, adapter := range r.adapters.All() {
		if setter, ok := adapter.(messageHandlerSetter); ok {
			setter.SetMessageHandler(nil)
		}
	}
	for _, adapters := range r.profileAdapters.All() {
		for _, adapter := range adapters.All() {
			if setter, ok := adapter.(messageHandlerSetter); ok {
				setter.SetMessageHandler(nil)
			}
		}
	}
}
