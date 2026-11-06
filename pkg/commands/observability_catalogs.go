package commands

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
)

var builtinLookup = func() map[string]string {
	out := map[string]string{}
	for _, d := range Builtin {
		out[d.Name] = d.Name
		for _, a := range d.Aliases {
			out[a] = d.Name
		}
	}
	return out
}()

func init() {
	observability.SetCatalogSource("slash_command_names", builtinCommandNames)
	observability.ResolveCommandHook = resolveBuiltinCommand
}

// Names of every core slash command, the commands slash command metrics may name
func builtinCommandNames() ([]string, error) {
	names := make([]string, 0, len(Builtin))
	for _, d := range Builtin {
		names = append(names, d.Name)
	}
	return names, nil
}

// Canonical core command for a typed name or alias, leading slash optional, false when none matches
func resolveBuiltinCommand(name string) (string, bool) {
	canonical, ok := builtinLookup[strings.TrimLeft(pystr.Lower(name), "/")]
	return canonical, ok
}
