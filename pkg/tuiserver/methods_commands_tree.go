package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// CLI command tree of this binary with the plugin commands the process loaded for the scoped profile, assigned in cmd/icebath/wiring.go
var CLICommandTree func(ctx context.Context) tuigateway.CommandsTreeResult

// Command tree RPC of the stdio server, run in the profile scope its params name
func commandsTreeRPC(ctx context.Context, rid any, _ map[string]any) (map[string]any, error) {
	return ok(rid, tuigateway.Plain(CLICommandTree(ctx))), nil
}

var commandsTreeRegistry = &handlerRegistry{}

func init() {
	scopedRPC(commandsTreeRegistry, tuigateway.CommandsTree.Name(), 5020, rpcShape{}, commandsTreeRPC)
	commandsTreeRegistry.install()
}
