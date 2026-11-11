package gatewayrun

// Drains and detaches the gateway health export once, a shutdown failure debug logged
func shutdownGatewayHealthExport(r *GatewayRunner) {
	runtime := r.gatewayHealthExportRuntime
	if runtime == nil {
		return
	}
	r.gatewayHealthExportRuntime = nil
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("gateway health OTLP export shutdown failed", "exc_info", rec)
		}
	}()
	runtime.Shutdown()
}

// Started gateway health OTLP export that drains and detaches on shutdown
type gatewayHealthExport interface {
	Shutdown()
}
