package commands

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestCommandRegistrySkillsDesktopMetaLimitsExecToReviewSubcommands(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	skills := Resolve("skills")
	if skills == nil {
		t.Fatal("skills not registered")
	}
	if skills.Desktop != "" {
		t.Fatalf("desktop %q", skills.Desktop)
	}
	want := map[string]any{"argument_mode": "options", "desktop": nil,
		"desktop_subcommands": []string{"pending", "approve", "reject", "diff", "approval"}}
	if got := DesktopMeta(skills); !reflect.DeepEqual(got, want) {
		t.Fatalf("meta %#v", got)
	}
	for _, sub := range skills.DesktopSubcommands {
		if !slices.Contains(skills.Subcommands, sub) {
			t.Fatalf("desktop subcommand %s outside %v", sub, skills.Subcommands)
		}
	}
}

func TestCommandRegistryEmptyDesktopSubcommandScopeSerializesAsDenyAll(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	d := &Def{Name: "demo", Description: "Demo", Category: "Session", Meta: Meta{DesktopSubcommands: []string{}}}
	want := map[string]any{"argument_mode": nil, "desktop": nil, "desktop_subcommands": []string{}}
	if got := DesktopMeta(d); !reflect.DeepEqual(got, want) {
		t.Fatalf("meta %#v", got)
	}
}

func TestCommandRegistryArgumentModeInfersTextFromAnyArgsHint(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, d := range []*Def{
		{Name: "demo", Description: "Demo", Category: "Session", ArgsHint: "<prompt>"},
		{Name: "ask", Description: "Ask", Category: "Session", ArgsHint: "<query>"},
		{Name: "note", Description: "Note", Category: "Session", ArgsHint: "[message]"},
	} {
		if got := InferArgumentMode(d); got != "text" {
			t.Fatalf("%s mode %q", d.Name, got)
		}
	}
}

func TestTelegramBotCommandsUnicodeDashesFoldedToHyphen(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	fake := &Def{Name: "dashy", Description: "does a — b – c", Category: "Session"}
	oldAvail, oldPlugins := gatewayAvailableCommands, iterPluginCommandEntries
	t.Cleanup(func() { gatewayAvailableCommands, iterPluginCommandEntries = oldAvail, oldPlugins })
	gatewayAvailableCommands = func(*Registry, config.Tree) []*Def { return []*Def{fake} }
	iterPluginCommandEntries = func(context.Context, *Registry) []PluginCommandEntry { return nil }
	got := TelegramBotCommands(context.Background(), Default, config.Tree{}, false)
	if !slices.Contains(got, NameDesc{"dashy", "does a - b - c"}) {
		t.Fatalf("got %v", got)
	}
}
