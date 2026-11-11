package tools

import "context"

// Plugin terminal backends visible in scope sorted by name
func ListTerminalEnvProviders(ctx context.Context, scope string) []BackendProvider {
	return terminalEnvRegistry.ListProviders(ctx, scope)
}
