package skills

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

func TestDoUninstallSkipConfirmBypassesInput(t *testing.T) {
	x, _, _ := newTestCLI(t)
	var removed []string
	x.seams.uninstallSkill = func(name string) (bool, string, error) {
		removed = append(removed, name)
		return true, "Removed", nil
	}
	if got, err := x.DoUninstall(context.Background(), "test-skill", nil, true, false); err != nil || got != HubOK {
		t.Fatal(got, err)
	}
	if len(removed) != 1 || removed[0] != "test-skill" {
		t.Fatalf("uninstall_skill calls %q", removed)
	}
}

func TestOptionalSkillSourceHonorsEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_OPTIONAL_SKILLS", dir)
	h, _ := newTestHub(t)
	if src := NewOptionalSkillSource(h, anonAuth(h)); src.tree.root != dir {
		t.Fatalf("optional dir %q want %q", src.tree.root, dir)
	}
}

func TestBuiltInNameCollisionIsVisibleInTheSkillsTable(t *testing.T) {
	RegisterCoreCommandResolver(func(name string) bool { return name == "handoff" || testCoreCommands[name] })
	t.Cleanup(func() { RegisterCoreCommandResolver(func(name string) bool { return testCoreCommands[name] }) })
	x, _, _ := newTestCLI(t)
	for _, n := range []string{"handoff", "tidy-notes"} {
		writeTestFile(t, filepath.Join(x.S.SkillsDir(), n, "SKILL.md"), "---\nname: "+n+"\ndescription: "+n+" skill.\n---\n# "+n+"\n")
	}
	sink := &bytes.Buffer{}
	if err := x.DoList(context.Background(), "all", false, &pyrich.Console{Out: sink, Width: 200}); err != nil {
		t.Fatal(err)
	}
	table := sink.String()
	note := "slash command /handoff unavailable — name taken by built-in; use /skill handoff"
	if !strings.Contains(table, note) || !strings.Contains(table, "tidy-notes") || strings.Count(table, "unavailable") != 1 {
		t.Fatal(table)
	}
}
