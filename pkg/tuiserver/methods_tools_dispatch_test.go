package tuiserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Message of the /init directive command.dispatch answers for a session
func dispatchInit(t *testing.T, sid string) string {
	t.Helper()
	result := resultOf(t, callMethod(t, "command.dispatch", map[string]any{"name": "init", "arg": "", "session_id": sid}))
	return result["message"].(string)
}

// Plugin owned registry command loading the named skill of the session home as the turn its handler hands over
func registerBoundSkillCommand(t *testing.T, name, skill string) *[]string {
	t.Helper()
	dir := filepath.Join(home.Dir(context.Background()), "skills", skill)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+skill+"\ndescription: bound fixture\n---\n\nBody marker of "+skill+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sessions []string
	commands.Register(commands.Def{Name: name, Plugin: "fixture", Handler: func(ctx context.Context, inv *commands.Invocation) error {
		host := inv.Host.(interface {
			SubmitUserTurn(context.Context, string) error
			TurnSession() (string, string)
		})
		taskID, platform := host.TurnSession()
		sessions = append(sessions, taskID+"|"+platform)
		msg, found, err := skills.NewScope(home.Dir(ctx), nil).BuildSkillInvocationMessage(ctx, "/"+skill, inv.Args, taskID, "")
		if err != nil || !found {
			return fmt.Errorf("skill %s found %v: %v", skill, found, err)
		}
		return host.SubmitUserTurn(ctx, msg)
	}})
	t.Cleanup(func() { commands.Default.Remove(name) })
	return &sessions
}

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
