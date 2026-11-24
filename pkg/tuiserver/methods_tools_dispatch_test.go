package tuiserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestCommandCatalogListsPluginOwnedRegistryCommandsWithTheirSubcommands(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	prevRegistry, prevPlugins := CatalogRegistry, RegistryPluginCommands
	CatalogRegistry = func(context.Context) ([]CatalogCommand, map[string][]string) {
		return []CatalogCommand{{Name: "help", Category: "Info", Description: "Show help"}}, map[string][]string{}
	}
	RegistryPluginCommands = func(context.Context) ([]CatalogCommand, map[string][]string) {
		return []CatalogCommand{{Name: "boundfix", Category: "Plugins", Description: "Design fixture", Aliases: []string{"bf"}},
			{Name: "help", Category: "Plugins", Description: "shadowed"}}, map[string][]string{"/boundfix": {"audit", "polish"}}
	}
	t.Cleanup(func() { CatalogRegistry, RegistryPluginCommands = prevRegistry, prevPlugins })
	catalog, err := CommandCatalog(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pairs := fmt.Sprint(catalog.Pairs)
	if !strings.Contains(pairs, "[/boundfix Design fixture]") || strings.Contains(pairs, "shadowed") {
		t.Fatalf("pairs %s", pairs)
	}
	if bf := catalog.Canon["/bf"]; bf != "/boundfix" {
		t.Fatalf("alias canon %v", bf)
	}
	if subs := catalog.Sub["/boundfix"]; fmt.Sprint(subs) != "[audit polish]" {
		t.Fatalf("subcommands %v", subs)
	}
	listed := false
	for _, group := range catalog.Categories {
		if group.Name == "Plugin commands" {
			listed = strings.Contains(fmt.Sprint(group.Pairs), "[/boundfix Design fixture]")
		}
	}
	if !listed {
		t.Fatalf("categories %v", catalog.Categories)
	}
}
