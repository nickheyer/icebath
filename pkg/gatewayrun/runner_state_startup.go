package gatewayrun

// Attributes the bootstrap and startup modules set lazily, after the constructor
type runnerStartupState struct {
	startupCleanupOnly     bool
	startupParkedPlatforms bool
	// Gateway health OTLP export the startup started, nil until then and after shutdown
	gatewayHealthExportRuntime gatewayHealthExport
}
