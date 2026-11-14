package commands

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
)

func TestSlashCommandMetricsNameEveryCoreCommandAndAliasByItsRegistryName(t *testing.T) {
	observability.ResetCatalogCaches()
	for _, d := range Builtin {
		if got := observability.SlashCommandMetricName(context.Background(), "/"+d.Name); got != d.Name {
			t.Fatalf("%s reads %s", d.Name, got)
		}
		for _, a := range d.Aliases {
			if got := observability.SlashCommandMetricName(context.Background(), a+" extra args"); got != d.Name {
				t.Fatalf("alias %s of %s reads %s", a, d.Name, got)
			}
		}
	}
	for raw, want := range map[string]string{"/RESET": "new", "  /Help  ": "help", "/acme-internal-command": "unknown", "/": "unknown"} {
		if got := observability.SlashCommandMetricName(context.Background(), raw); got != want {
			t.Fatalf("%q reads %s, want %s", raw, got, want)
		}
	}
	if canonical, ok := resolveBuiltinCommand("//Reset"); !ok || canonical != "new" {
		t.Fatalf("resolve %q %v", canonical, ok)
	}
	if names, _ := builtinCommandNames(); len(names) != len(Builtin) || names[0] != Builtin[0].Name {
		t.Fatalf("names %v", names)
	}
}
