package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/internal/config"
)

func setup(t *testing.T) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.New(dir)
	cfg.Register(config.Sections{{Key: "app", Global: true, Title: "App", Settings: []config.Setting{
		{Key: "app.skin", Title: "Skin", Default: "dark"},
	}}})
	cfg.Register(config.Sections{{Key: "agent", Title: "Agent", Settings: []config.Setting{
		{Key: "agent.model", Title: "Model", Default: "", Env: []string{"ICEBATH_TEST_MODEL"}},
		{Key: "agent.max_turns", Title: "Max", Default: 0},
	}}})
	cfg.Register(config.Sections{{Key: "agent.approvals", Parent: "agent", Title: "Approvals", Settings: []config.Setting{
		{Key: "agent.approvals.mode", Title: "Mode", Default: "manual"},
	}}})
	if err := cfg.Load(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Use("default"); err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

func TestScopes(t *testing.T) {
	cfg, dir := setup(t)
	if err := cfg.Set("app.skin", "light"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("agent.max_turns", 5); err != nil {
		t.Fatal(err)
	}
	shared, _ := os.ReadFile(filepath.Join(dir, "config.yaml"))
	own, _ := os.ReadFile(filepath.Join(dir, "profiles", "default", "config.yaml"))
	if !strings.Contains(string(shared), "skin: light") || strings.Contains(string(shared), "max_turns") {
		t.Fatalf("shared file holds %q", shared)
	}
	if !strings.Contains(string(own), "max_turns: 5") || strings.Contains(string(own), "skin") {
		t.Fatalf("profile file holds %q", own)
	}
	if err := cfg.Use("work"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.GetInt("agent.max_turns"); got != 0 {
		t.Fatalf("another profile sees max_turns %d", got)
	}
	if got := cfg.GetString("app.skin"); got != "light" {
		t.Fatalf("another profile sees skin %q", got)
	}
}

func TestEnvironmentDefaultsAndRemoval(t *testing.T) {
	cfg, _ := setup(t)
	if got := cfg.GetString("agent.approvals.mode"); got != "manual" {
		t.Fatalf("default is %q", got)
	}
	if err := cfg.Set("agent.model", "a"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_TEST_MODEL", "b")
	if got := cfg.GetString("agent.model"); got != "b" {
		t.Fatalf("environment gives %q", got)
	}
	view := cfg.View("agent")
	if err := view.Remove(); err != nil {
		t.Fatal(err)
	}
	if view.Exists() {
		t.Fatal("agent is still kept after its removal")
	}
}

func TestKeysFollowTheTree(t *testing.T) {
	for name, section := range map[string]config.Section{
		"dotted top":       {Key: "a.b"},
		"not under parent": {Key: "other.b", Parent: "agent"},
		"two names below":  {Key: "agent.b.c", Parent: "agent"},
		"unknown parent":   {Key: "nothing.b", Parent: "nothing"},
		"global child":     {Key: "agent.b", Parent: "agent", Global: true},
		"setting outside":  {Key: "agent.b", Parent: "agent", Settings: []config.Setting{{Key: "agent.c"}}},
		"twice":            {Key: "agent.approvals", Parent: "agent"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, _ := setup(t)
			defer func() {
				if recover() == nil {
					t.Fatal("registered without a panic")
				}
			}()
			cfg.Register(config.Sections{section})
		})
	}
}

type moving struct{}

// One section for each table under items, made from what is stored only
func (moving) Sections(values *config.Config) []config.Section {
	var sections []config.Section
	for _, name := range values.View("items").Names() {
		sections = append(sections, config.Section{Key: "items." + name, Parent: "items", Title: name, Settings: []config.Setting{
			{Key: "items." + name + ".size", Title: "Size", Default: 3},
		}})
	}
	return sections
}

func TestSectionsThatComeAndGo(t *testing.T) {
	cfg, _ := setup(t)
	cfg.Register(config.Sections{{Key: "items", Title: "Items"}})
	cfg.Register(moving{})
	if _, _, ok := cfg.Setting("items.one.size"); ok {
		t.Fatal("setting of an item that isn't there")
	}
	if err := cfg.View("items").View("one").Set("name", "One"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.GetInt("items.one.size"); got != 3 {
		t.Fatalf("default of an item is %d", got)
	}
	if _, ok := cfg.Section("items.one"); !ok {
		t.Fatal("no section for the item")
	}
}

func TestProfilesAtOnce(t *testing.T) {
	cfg, _ := setup(t)
	cfg.Register(config.Sections{{Key: "items", Title: "Items"}})
	cfg.Register(moving{})
	work := cfg.In("work")
	if err := work.Set("agent.max_turns", 7); err != nil {
		t.Fatal(err)
	}
	if err := work.View("items").View("two").Set("name", "Two"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.GetInt("agent.max_turns"); got != 0 {
		t.Fatalf("the profile in use sees %d", got)
	}
	if got := work.GetInt("agent.max_turns"); got != 7 {
		t.Fatalf("work sees %d", got)
	}
	if _, ok := cfg.Section("items.two"); ok {
		t.Fatal("the profile in use has the item of work")
	}
	if got := work.GetInt("items.two.size"); got != 3 {
		t.Fatalf("the item of work has size %d", got)
	}
	if err := cfg.Use("work"); err != nil {
		t.Fatal(err)
	}
	if got := cfg.GetInt("agent.max_turns"); got != 7 || cfg.In("default").GetInt("agent.max_turns") != 0 {
		t.Fatal("switching the profile in use mixed up the profiles")
	}
}
