package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// CLI command tree of this binary with the plugin commands the process already loaded, built again only when that set changes
func cliCommandTree(ctx context.Context) tuigateway.CommandsTreeResult {
	memoryCommands := memory.DiscoverPluginCLICommands(ctx, processToolsEnv(ctx))
	managerCommands := plugins.GetPluginManager(ctx).CLICommands()
	var keys []string
	for _, info := range memoryCommands {
		keys = append(keys, info.Plugin+"/"+info.Name)
	}
	for name, info := range managerCommands {
		keys = append(keys, cmp.Or(info.Plugin, info.PluginKey)+"/"+name)
	}
	slices.Sort(keys)
	return commands.CachedTree(strings.Join(keys, ","), func() *commands.CLI {
		cli := commands.NewCLI()
		if failure := attachPluginCommands(cli, memoryCommands, managerCommands); failure != nil {
			pkgLog().Debug(fmt.Sprintf("Command tree stops at a plugin command whose setup failed: %v", failure))
		}
		return cli
	})
}
