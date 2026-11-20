package commands

import (
	"context"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

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
