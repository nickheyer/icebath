package tools

// RegisterTerminalBackendScoped adds a plugin backend to one profile scope, empty for the global layer
func RegisterTerminalBackendScoped(p BackendProvider, scope string) error {
	return terminalEnvRegistry.Register(p, scope)
}
