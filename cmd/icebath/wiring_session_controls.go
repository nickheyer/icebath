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

// Registry of the builtin command table alone, the lookup hermes_cli resolve_command reads
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
		desc := d.Describe(ctx)
		if d.ArgsHint != "" {
			desc = i18n.T(ctx, "slash.shared.usage_suffix", "", map[string]any{"description": desc, "name": d.Name, "args_hint": d.ArgsHint})
		}
		rows = append(rows, tuiserver.CatalogCommand{Name: d.Name, Category: d.Category, Description: desc,
			Aliases: append([]string(nil), d.Aliases...), GatewayOnly: commands.MetaOf(d.Name).GatewayOnly,
			DesktopMeta: commands.DesktopMeta(d)})
	}
	return rows, commands.Subcommands(builtins)
}
