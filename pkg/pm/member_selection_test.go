package pm

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Generated member of a plugin under a fresh generation root
func msMember(t *testing.T, plugin, root string) string {
	t.Helper()
	sttMkdir(t, root)
	member, err := workspaceMember(plugin, root, plugin, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return member
}

func TestMemberSelectionScopedMembersRespectDisabledAndKeepOtherProfiles(t *testing.T) {
	tmp := isolateHome(t)
	wsHost(t, "")
	home := filepath.Join(tmp, ".icebath")
	current := filepath.Join(home, "plugins", "group", "plugin")
	otherHome := filepath.Join(home, "profiles", "other")
	other := filepath.Join(otherHome, "plugins", "same")
	for _, member := range []string{current, other} {
		peWrite(t, filepath.Join(member, "pyproject.toml"), "[project]\nname=\"test\"\nversion=\"1\"\ndependencies=[\"requests>=2\"]\n")
	}
	peWrite(t, filepath.Join(home, "config.yaml"), "plugins:\n  enabled: [group/plugin]\n  disabled: []\n")
	peWrite(t, filepath.Join(otherHome, "config.yaml"), "plugins:\n  enabled: [same]\n")
	members, err := EnabledMemberDirs(context.Background(), MemberSelection{})
	sort.Strings(members)
	want := []string{current, other}
	sort.Strings(want)
	if err != nil || !reflect.DeepEqual(members, want) {
		t.Fatalf("members %v %v", members, err)
	}
	peWrite(t, filepath.Join(home, "config.yaml"), "plugins:\n  enabled: [group/plugin]\n  disabled: [group/plugin]\n")
	members, err = EnabledMemberDirs(context.Background(), MemberSelection{})
	if err != nil || !reflect.DeepEqual(members, []string{other}) {
		t.Fatalf("members after disabling %v %v", members, err)
	}
}

func TestMemberSelectionManifestOnlyMemberIsNamedAfterItsPluginDirAndStaysUnique(t *testing.T) {
	tmp := t.TempDir()
	var members, names []string
	for _, home := range []string{"home-a", "home-b"} {
		plugin := filepath.Join(tmp, home, "plugins", "My Plugin!")
		peWrite(t, filepath.Join(plugin, "plugin.yaml"), "name: my-plugin\npip_dependencies: [left-pad-py]\n")
		member := msMember(t, plugin, filepath.Join(tmp, "gen-"+home))
		members = append(members, member)
		names = append(names, pyrepr.Str(wsAt(wsTOML(t, filepath.Join(member, "pyproject.toml")), "project", "name")))
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "icebath-plugin-my-plugin-") {
			t.Fatalf("names %v", names)
		}
	}
	if names[0] == names[1] || filepath.Base(members[0]) == filepath.Base(members[1]) {
		t.Fatalf("members not unique %v %v", names, members)
	}
}

func TestMemberSelectionPyprojectMemberIsRenamedByItsKeyAndStaysUnique(t *testing.T) {
	tmp := t.TempDir()
	var members, names []string
	for _, home := range []string{"home-a", "home-b"} {
		plugin := filepath.Join(tmp, home, "plugins", "hindsight")
		peWrite(t, filepath.Join(plugin, "pyproject.toml"),
			"[project]\nname = \"icebath-plugin-hindsight\"\nversion = \"1.0.0\"\ndependencies = [\"hindsight-client>=0.10.1\"]\n")
		member := msMember(t, plugin, filepath.Join(tmp, "gen-"+home))
		members = append(members, member)
		names = append(names, pyrepr.Str(wsAt(wsTOML(t, filepath.Join(member, "pyproject.toml")), "project", "name")))
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "icebath-plugin-hindsight-") {
			t.Fatalf("names %v", names)
		}
	}
	if names[0] == names[1] {
		t.Fatalf("names not unique %v", names)
	}
	document := wsTOML(t, filepath.Join(members[0], "pyproject.toml"))
	if wsAt(document, "project", "version") != "1.0.0" || !reflect.DeepEqual(wsStrings(wsAt(document, "project", "dependencies")), []string{"hindsight-client>=0.10.1"}) {
		t.Fatalf("project %v", wsAt(document, "project"))
	}
}

func TestMemberSelectionBuildablePyprojectMemberKeepsItsDeclaredName(t *testing.T) {
	tmp := t.TempDir()
	plugin := filepath.Join(tmp, "home", "plugins", "replay")
	text := "[project]\nname = \"replay-plugin\"\nversion = \"1.0\"\n[build-system]\nrequires = []\nbuild-backend = \"backend\"\n"
	peWrite(t, filepath.Join(plugin, "pyproject.toml"), text)
	member := msMember(t, plugin, filepath.Join(tmp, "gen"))
	if got := peRead(t, filepath.Join(member, "pyproject.toml")); got != text {
		t.Fatalf("buildable member rewritten %q", got)
	}
}

func TestMemberSelectionVirtualMemberWithoutProjectMetadataGetsAVersion(t *testing.T) {
	tmp := t.TempDir()
	plugin := filepath.Join(tmp, "home", "plugins", "icebath-lcm")
	peWrite(t, filepath.Join(plugin, "pyproject.toml"), "[tool.ruff]\ntarget-version = \"py311\"\n")
	member := msMember(t, plugin, filepath.Join(tmp, "gen"))
	if version := wsAt(wsTOML(t, filepath.Join(member, "pyproject.toml")), "project", "version"); version != "0.0.0" {
		t.Fatalf("version %v", version)
	}
}

func TestMemberSelectionVirtualMemberDeclaringDynamicVersionIsLeftAlone(t *testing.T) {
	tmp := t.TempDir()
	plugin := filepath.Join(tmp, "home", "plugins", "dynver")
	peWrite(t, filepath.Join(plugin, "pyproject.toml"), "[project]\nname = \"dynver\"\ndynamic = [\"version\"]\n")
	member := msMember(t, plugin, filepath.Join(tmp, "gen"))
	project, _ := wsAt(wsTOML(t, filepath.Join(member, "pyproject.toml")), "project").(*OrderedObject)
	if _, has := project.Values["version"]; has || !reflect.DeepEqual(wsStrings(project.Values["dynamic"]), []string{"version"}) {
		t.Fatalf("project %v", project.Values)
	}
}

func TestMemberSelectionBuildableMemberWithoutAVersionGetsOneAndKeepsItsName(t *testing.T) {
	tmp := t.TempDir()
	plugin := filepath.Join(tmp, "home", "plugins", "noversion")
	peWrite(t, filepath.Join(plugin, "pyproject.toml"),
		"[project]\nname = \"noversion-plugin\"\n[build-system]\nrequires = []\nbuild-backend = \"backend\"\n")
	member := msMember(t, plugin, filepath.Join(tmp, "gen"))
	project, _ := wsAt(wsTOML(t, filepath.Join(member, "pyproject.toml")), "project").(*OrderedObject)
	if !reflect.DeepEqual(project.Keys, []string{"name", "version"}) || project.Values["name"] != "noversion-plugin" || project.Values["version"] != "0.0.0" {
		t.Fatalf("project %v %v", project.Keys, project.Values)
	}
}

func TestMemberSelectionToolingOnlyPyprojectWithDevGroupStagesTheManifestDependencies(t *testing.T) {
	tmp := t.TempDir()
	plugin := filepath.Join(tmp, "home", "plugins", "icebath-lcm")
	peWrite(t, filepath.Join(plugin, "plugin.yaml"), "name: icebath-lcm\npip_dependencies: ['fixturedep>=1,<2']\n")
	peWrite(t, filepath.Join(plugin, "pyproject.toml"), "[tool.ruff]\ntarget-version = \"py311\"\n[dependency-groups]\ndev = [\"ruff\"]\n")
	document := wsTOML(t, filepath.Join(msMember(t, plugin, filepath.Join(tmp, "gen")), "pyproject.toml"))
	if !reflect.DeepEqual(wsStrings(wsAt(document, "project", "dependencies")), []string{"fixturedep>=1,<2"}) || wsAt(document, "dependency-groups") != nil {
		t.Fatalf("document %v", document.Keys)
	}
}
