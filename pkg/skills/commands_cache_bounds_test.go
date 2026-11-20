package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Git repo directory under root used as one project scope
func scopeRepo(t *testing.T, root, name string) string {
	t.Helper()
	repo := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return repo
}

// Lengths of the command, signature and interactive baseline tables and whether each holds key
func scopeTables(key skillCommandScope) ([3]int, [3]bool) {
	skillCmds.mu.Lock()
	defer skillCmds.mu.Unlock()
	_, a := skillCmds.byKey[key]
	_, b := skillCmds.sigByKey[key]
	_, c := skillCmds.lastInteractive[key]
	return [3]int{len(skillCmds.byKey), len(skillCmds.sigByKey), len(skillCmds.lastInteractive)}, [3]bool{a, b, c}
}

func TestSkillCommandScopesAreLRUBoundedAndRecentScopeStaysWarm(t *testing.T) {
	ctx := context.Background()
	s, _, _ := cmdScope(t)
	s.Platform = "local"
	root := t.TempDir()
	live := *s
	live.Cwd = scopeRepo(t, root, "repo0")
	liveCmds, err := live.GetInteractiveSkillCommands(ctx)
	if err != nil {
		t.Fatal(err)
	}
	liveID := commandsID(live.GetSkillCommands())
	var firstOther SkillCommands
	other := *s
	for i := 0; i < 200; i++ {
		if _, err := live.GetInteractiveSkillCommands(ctx); err != nil {
			t.Fatal(err)
		}
		other.Cwd = scopeRepo(t, root, "repo"+strconv.Itoa(i+1))
		if _, err := other.GetInteractiveSkillCommands(ctx); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstOther = other.GetSkillCommands()
		}
	}
	lens, has := scopeTables(live.commandScopeKey())
	for i, n := range lens {
		if n > 2*maxWatchedScopes || !has[i] {
			t.Fatalf("table %d holds %d scopes, live present %v", i, n, has[i])
		}
	}
	if commandsID(live.GetSkillCommands()) != liveID || len(liveCmds) != 0 {
		t.Fatal("live scope was evicted and rescanned")
	}
	other.Cwd = filepath.Join(root, "repo1")
	again := other.GetSkillCommands()
	if len(again) != 0 || commandsID(again) == commandsID(firstOther) {
		t.Fatal("long evicted scope served its stale map instead of rescanning")
	}
}

func TestSkillScopeUsedThroughDispatchKeepsItsReloadBaseline(t *testing.T) {
	ctx := context.Background()
	s, _, _ := cmdScope(t)
	pluginMD := filepath.Join(t.TempDir(), "stable", "SKILL.md")
	os.MkdirAll(filepath.Dir(pluginMD), 0o755)
	os.WriteFile(pluginMD, []byte("---\nname: stable\ndescription: same\n---\n\nBody.\n"), 0o644)
	withPluginHost(t, &fakePluginHost{skills: map[string]string{"plugin:stable": pluginMD}, meta: []map[string]any{{"name": "plugin:stable", "description": "same"}}})
	s.Platform = "local"
	root := t.TempDir()
	live := *s
	live.Cwd = scopeRepo(t, root, "live")
	if _, err := live.GetInteractiveSkillCommands(ctx); err != nil {
		t.Fatal(err)
	}
	other := *s
	var first skillCommandScope
	for i := 0; i < maxCachedScopes-1; i++ {
		other.Cwd = scopeRepo(t, root, "repo"+strconv.Itoa(i))
		if i == 0 {
			first = other.commandScopeKey()
		}
		if _, err := other.GetInteractiveSkillCommands(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := other.ReloadSkills(ctx); err != nil {
		t.Fatal(err)
	}
	live.GetSkillCommands()
	other.Cwd = scopeRepo(t, root, "new")
	if _, err := other.GetInteractiveSkillCommands(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := live.ReloadSkills(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(result["added"]) != "[]" || fmt.Sprint(result["unchanged"]) != "[plugin:stable]" {
		t.Fatalf("live baseline lost %v", result)
	}
	if _, has := scopeTables(first); has[2] {
		t.Fatal("least recently used baseline kept instead of the live one")
	}
}
