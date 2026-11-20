package main

import (
	"context"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Registry of the builtin command table alone, the lookup reads
var builtinCommands = sync.OnceValue(func() *commands.Registry {
	r := commands.NewRegistry()
	commands.Install(r)
	return r
})

// Builtin registry command of a typed name or alias, leading slash optional
func resolveRegistryCommand(name string) (gateway.RegistryCommand, bool) {
	d := builtinCommands().Lookup(strings.TrimLeft(strings.ToLower(name), "/"))
	if d == nil {
		return gateway.RegistryCommand{}, false
	}
	return gateway.RegistryCommand{Name: d.Name, BusyPolicy: string(d.BusyPolicy), BusyHandler: d.BusyHandler}, true
}

// Builtin registry rows with their localized usage descriptions and the builtin subcommand table
func catalogRegistry(ctx context.Context) ([]tuiserver.CatalogCommand, map[string][]string) {
	builtins := builtinCommands()
	rows := make([]tuiserver.CatalogCommand, 0, len(commands.Builtin))
	for i := range commands.Builtin {
		d := &commands.Builtin[i]
		if !commands.SlashLinked(d.Name) {
			continue
		}
		desc := d.Describe(ctx)
		if d.ArgsHint != "" {
			desc = i18n.T(ctx, "slash.shared.usage_suffix", "", map[string]any{"description": desc, "name": d.Name, "args_hint": d.ArgsHint})
		}
		rows = append(rows, tuiserver.CatalogCommand{Name: d.Name, Category: d.Category, Description: desc,
			Aliases: append([]string(nil), d.Aliases...), GatewayOnly: commands.MetaOf(d.Name).GatewayOnly})
	}
	return rows, commands.Subcommands(builtins)
}

// Plugin owned commands of the process registry with their localized descriptions and subcommand table
func registryPluginCommands(ctx context.Context) ([]tuiserver.CatalogCommand, map[string][]string) {
	defs := commands.PluginEntries(commands.Default)
	rows := make([]tuiserver.CatalogCommand, 0, len(defs))
	subcommands := map[string][]string{}
	for _, d := range defs {
		rows = append(rows, tuiserver.CatalogCommand{Name: d.Name, Category: d.Category, Description: d.Describe(ctx),
			Aliases: append([]string(nil), d.Aliases...)})
		if len(d.Subcommands) > 0 {
			subcommands["/"+d.Name] = append([]string(nil), d.Subcommands...)
		}
	}
	return rows, subcommands
}
