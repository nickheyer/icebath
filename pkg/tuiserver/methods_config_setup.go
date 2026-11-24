package tuiserver

import ()

// Dedicated readiness executor and the probes in flight by kind and profile
var (
	readinessPool = newRPCWorkerPool(max(2, min(4, envInt("ICEBATH_TUI_RPC_POOL_WORKERS", 8))))
)

var configSetupRegistry = &handlerRegistry{}

func init() {
	registerAtExit(func() { readinessPool.Shutdown(true) })
	configSetupRegistry.install()
}
