package main

import (
	"context"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Builtin registry rows with their localized usage descriptions and the builtin subcommand table
func catalogRegistry(ctx context.Context) ([]tuiserver.CatalogCommand, map[string][]string) {
	builtins := commands.NewRegistry()
	commands.Install(builtins)
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
