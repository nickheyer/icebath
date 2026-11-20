package mcp

import "context"

// Profile runtime scope of the tui gateway, set by the binary
var TUIProfileRuntimeScope func(ctx context.Context, profileHome string) context.Context
