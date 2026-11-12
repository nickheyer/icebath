package provider

import "context"

// Loads the active profile aliases into the shared table unless it holds them or a caller seeded it
func EnsureDirectAliases(ctx context.Context) { ensureDirectAliases(ctx) }
