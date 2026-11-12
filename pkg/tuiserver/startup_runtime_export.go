package tuiserver

import "context"

// Startup model and explicit provider a TUI session builds on
func ResolveStartupRuntime(ctx context.Context) (string, string) { return resolveStartupRuntime(ctx) }
