package skills

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"

	"github.com/nickheyer/icebath/pkg/internal/pyrich"
)

type uninstallCall struct {
	name                    string
	skipConfirm, deferCache bool
}

func recordUninstalls(x *HubCLI) *[]uninstallCall {
	calls := &[]uninstallCall{}
	x.seams.uninstall = func(_ context.Context, name string, _ *pyrich.Console, skipConfirm, deferCache bool) (HubOutcome, error) {
		*calls = append(*calls, uninstallCall{name, skipConfirm, deferCache})
		return HubNoop, nil
	}
	return calls
}

func TestSlashUninstallWithoutFlagsStillSkipsConfirm(t *testing.T) {
	x, _, _ := newTestCLI(t)
	calls := recordUninstalls(x)
	if err := x.HandleSkillsSlash(context.Background(), "/skills uninstall test-skill", nil); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !(*calls)[0].skipConfirm {
		t.Fatalf("%+v", *calls)
	}
}

func TestSlashUninstallDefaultDefersCacheInvalidation(t *testing.T) {
	x, _, _ := newTestCLI(t)
	calls := recordUninstalls(x)
	if err := x.HandleSkillsSlash(context.Background(), "/skills uninstall test-skill", nil); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !(*calls)[0].deferCache {
		t.Fatalf("%+v", *calls)
	}
}

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

func TestRegistryIDsAreExactlyTheHubRouterAdapters(t *testing.T) {
	h, _ := newTestHub(t)
	ids := observability.Set()
	for _, s := range h.createSourceRouter(nil) {
		ids = ids.Union(observability.Set(s.SourceID()))
	}
	if !reflect.DeepEqual(ids, observability.ExtensionRegistryIDs) {
		t.Fatalf("router ids %v, contract %v", ids, observability.ExtensionRegistryIDs)
	}
	if !reflect.DeepEqual(observability.ExtensionRegistries, observability.ExtensionRegistryIDs.Union(observability.Set("none", "other", "unresolved"))) {
		t.Fatal(observability.ExtensionRegistries)
	}
}

func TestAnUnservedIdentifierNamesTheRegistryItsAdapterAccepts(t *testing.T) {
	cases := [][2]string{{"skills.sh/acme/x", "skills-sh"}, {"skils-sh/acme/x", "skills-sh"}, {"OFFICIAL/x/y", "official"},
		{"lobehub/x", "lobehub"}, {"well-known:https://h/x", "well-known"}, {"https://h/.well-known/skills/x", "well-known"},
		{"https://h/a/SKILL.md", "url"}, {"acme-corp/skills/x", "unresolved"}}
	for _, c := range cases {
		if got := registryFromIdentifier(c[0]); got != c[1] {
			t.Errorf("registryFromIdentifier(%q) = %q want %q", c[0], got, c[1])
		}
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
