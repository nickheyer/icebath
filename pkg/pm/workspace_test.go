package pm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Plain plugin directories as members that are their own identity
func wsMembers(dirs ...string) []MemberDir {
	out := []MemberDir{}
	for _, dir := range dirs {
		out = append(out, MemberDir{Identity: dir, Source: dir})
	}
	return out
}

// Rewrites a text file through edit
func wsEdit(t *testing.T, path string, edit func(string) string) {
	t.Helper()
	peWrite(t, path, edit(peRead(t, path)))
}

// Locks and syncs members into a fresh workspace over a new managed engine
func wsSync(t *testing.T, env map[string]string, members []MemberDir, extras []string, root, core, engine string) (*PythonEnvironment, error) {
	t.Helper()
	e := wsEngine(t, engine, env)
	return e, LockAndSync(context.Background(), members, extras, LockAndSyncOptions{Root: root, Source: core, Environment: e})
}

// Manifest of the single snapshotted member of a workspace
func wsMemberManifest(t *testing.T, root string) *OrderedObject {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "plugin-sources"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("plugin-sources of %s: %v %v", root, entries, err)
	}
	return wsTOML(t, filepath.Join(root, "plugin-sources", entries[0].Name(), "pyproject.toml"))
}

// Table of string lists as a plain map, nil when the value is no table
func wsTable(v any) map[string][]string {
	obj, ok := v.(*OrderedObject)
	if !ok {
		return nil
	}
	out := map[string][]string{}
	for _, key := range obj.Keys {
		out[key] = wsStrings(obj.Values[key])
	}
	return out
}

// Distribution names with a dist-info directory in a venv
func wsInstalled(t *testing.T, venv string) map[string]bool {
	t.Helper()
	site, err := SitePackages(venv)
	if err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(site, "*.dist-info"))
	out := map[string]bool{}
	for _, match := range matches {
		out[strings.Split(filepath.Base(match), "-")[0]] = true
	}
	return out
}

// Marker environment of the interpreter the layout installs with
func wsHostMarkers(t *testing.T) map[string]string {
	t.Helper()
	env, err := targetEnvironment(context.Background(), peHostPython(t))
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestWorkspacePreparationRefusesExistingWorkspaceWithoutMutatingIt(t *testing.T) {
	tmp, core, plugin, env := wsLayout(t)
	ctx := context.Background()
	root := filepath.Join(tmp, "workspace")
	opts := LockAndSyncOptions{Root: root, Source: core, Environment: wsEngine(t, filepath.Join(tmp, "env"), env)}
	if err := LockAndSync(ctx, wsMembers(plugin), []string{}, opts); err != nil {
		t.Fatal(err)
	}
	before := wsSnapshot(t, root)
	peWrite(t, filepath.Join(plugin, "pyproject.toml"), "changed after publication")
	err := LockAndSync(ctx, wsMembers(), []string{}, opts)
	var ie *InstallError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "fresh") {
		t.Fatalf("got %v", err)
	}
	if after := wsSnapshot(t, root); !reflect.DeepEqual(after, before) {
		t.Fatalf("workspace mutated")
	}
}

func TestWorkspaceMissingExplicitSeedCannotSilentlyResolveNewVersions(t *testing.T) {
	tmp, core, plugin, env := wsLayout(t)
	e := wsEngine(t, filepath.Join(tmp, "env"), env)
	err := LockAndSync(context.Background(), wsMembers(plugin), []string{}, LockAndSyncOptions{Root: filepath.Join(tmp, "workspace"),
		Source: core, SeedLock: filepath.Join(tmp, "missing.lock"), Environment: e})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(tmp, "env")); err == nil {
		t.Fatal("engine destination was created")
	}
}

func TestWorkspaceTrustsCoreLockInsteadOfRecheckingReleaseAge(t *testing.T) {
	tmp, core, plugin, env := wsLayout(t)
	manifest := filepath.Join(core, "pyproject.toml")
	wsEdit(t, manifest, func(s string) string {
		return strings.Replace(s, "[tool.uv]\n", "[tool.uv]\nexclude-newer=\"3 days\"\n", 1) + "[tool.uv.exclude-newer-package]\nBase_Dep = false\n"
	})
	peRun(t, []string{peUV(t), "lock", "--python", peHostPython(t)}, core, env)
	registry := map[string]bool{}
	for _, p := range wsAt(wsTOML(t, filepath.Join(core, "uv.lock")), "package").([]any) {
		if source, ok := wsAt(p, "source").(*OrderedObject); ok {
			if _, ok := source.Values["registry"]; ok {
				registry[pyStr(wsAt(p, "name"))] = true
			}
		}
	}
	if !registry["base-dep"] || !registry["chosen-dep"] || registry["member-dep"] {
		t.Fatalf("core registry packages %v", registry)
	}
	root := filepath.Join(tmp, "workspace")
	e := wsEngine(t, filepath.Join(tmp, "env"), env)
	if err := LockAndSync(context.Background(), wsMembers(plugin), []string{}, LockAndSyncOptions{Root: root, Source: core,
		SeedLock: filepath.Join(core, "uv.lock"), Environment: e}); err != nil {
		t.Fatal(err)
	}
	policy := wsAt(wsTOML(t, filepath.Join(root, "pyproject.toml")), "tool", "uv").(*OrderedObject)
	for _, key := range []string{"exclude-newer", "exclude-newer-package", "constraint-dependencies"} {
		if _, ok := policy.Values[key]; ok {
			t.Fatalf("generated policy carries %s", key)
		}
	}
	if _, ok := wsLocked(t, filepath.Join(root, "uv.lock"))["member-dep"]; !ok {
		t.Fatal("member-dep not locked")
	}
}

func TestWorkspaceSolvesOnlyForTheInstallingInterpreter(t *testing.T) {
	tmp, core, plugin, env := wsLayout(t)
	wsEdit(t, filepath.Join(core, "pyproject.toml"), func(s string) string {
		return strings.Replace(s, "[tool.uv]\n", "[tool.uv]\nenvironments=[\"python_version >= '3.11'\"]\n", 1)
	})
	root := filepath.Join(tmp, "workspace")
	if _, err := wsSync(t, env, wsMembers(plugin), []string{}, root, core, filepath.Join(tmp, "env")); err != nil {
		t.Fatal(err)
	}
	environments := wsStrings(wsAt(wsTOML(t, filepath.Join(root, "pyproject.toml")), "tool", "uv", "environments"))
	if len(environments) != 1 {
		t.Fatalf("environments %v", environments)
	}
	req, err := ParseRequirement("x; " + environments[0])
	if err != nil {
		t.Fatal(err)
	}
	host := wsHostMarkers(t)
	with := func(key, value string) map[string]string {
		out := map[string]string{}
		for k, v := range host {
			out[k] = v
		}
		out[key] = value
		return out
	}
	type row struct {
		env  map[string]string
		want bool
	}
	rows := []row{{host, true}}
	for _, p := range []string{"android", "win32", "linux"} {
		if p != host["sys_platform"] {
			rows = append(rows, row{with("sys_platform", p), false})
		}
	}
	machine := "riscv64"
	if host["platform_machine"] == machine {
		machine = "s390x"
	}
	rows = append(rows, row{with("platform_machine", machine), false}, row{with("python_version", "3.10"), false})
	for _, r := range rows {
		if got, err := evaluateMarker(req.Marker, nil, r.env); err != nil || got != r.want {
			t.Fatalf("%s under %v = %v %v", environments[0], r.env, got, err)
		}
	}
}

func TestWorkspacePluginLocalGroupsDoNotConstrainRuntimeWorkspace(t *testing.T) {
	tmp, core, plugin, env := wsLayout(t)
	ctx := context.Background()
	metadata := filepath.Join(plugin, "pyproject.toml")
	wsEdit(t, metadata, func(s string) string {
		return s + "\n[project.optional-dependencies]\nfeature=[\"chosen-dep==1.0\"]\n" +
			"[dependency-groups]\ndev=[\"base-dep==2.0\"]\nlint=[{include-group=\"dev\"}]\n" +
			"[tool.uv.dependency-groups]\ndev={requires-python=\">=3.11\"}\n"
	})
	wsEdit(t, metadata, func(s string) string {
		return strings.Replace(s, "[tool.uv]\n", "[tool.uv]\ndefault-groups=[\"dev\"]\ndev-dependencies=[\"other-dep==2.0\"]\n"+
			"conflicts=[[{group=\"dev\"},{extra=\"feature\"}]]\n", 1)
	})
	original := peRead(t, metadata)
	root := filepath.Join(tmp, "runtime-workspace")
	if _, err := wsSync(t, env, wsMembers(plugin), []string{"feature"}, root, core, filepath.Join(tmp, "runtime-env")); err != nil {
		t.Fatal(err)
	}
	projected := wsMemberManifest(t, root)
	if _, ok := projected.Values["dependency-groups"]; ok {
		t.Fatal("snapshot kept dependency-groups")
	}
	if got := wsStrings(wsAt(projected, "project", "dependencies")); !reflect.DeepEqual(got, []string{"member-dep==1.0"}) {
		t.Fatalf("dependencies %v", got)
	}
	if got := wsTable(wsAt(projected, "project", "optional-dependencies")); !reflect.DeepEqual(got, map[string][]string{"feature": {"chosen-dep==1.0"}}) {
		t.Fatalf("optional %v", got)
	}
	for _, key := range []string{"default-groups", "dev-dependencies", "dependency-groups", "conflicts"} {
		if _, ok := wsAt(projected, "tool", "uv").(*OrderedObject).Values[key]; ok {
			t.Fatalf("snapshot tool.uv kept %s", key)
		}
	}
	if peRead(t, metadata) != original {
		t.Fatal("plugin manifest mutated")
	}
	locked := wsLocked(t, filepath.Join(root, "uv.lock"))
	if _, ok := locked["member-dep"]; locked["base-dep"] != "1.0" || !ok {
		t.Fatalf("locked %v", locked)
	}

	wsEdit(t, metadata, func(s string) string {
		return s + "\n[build-system]\nrequires=[]\nbuild-backend=\"local_backend\"\n"
	})
	buildableOriginal := peRead(t, metadata)
	buildable, err := workspaceMember(plugin, filepath.Join(tmp, "buildable"), plugin, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	built := wsTOML(t, filepath.Join(buildable, "pyproject.toml"))
	if _, ok := built.Values["dependency-groups"]; ok || pyStr(wsAt(built, "project", "name")) != "plugin-member" ||
		!reflect.DeepEqual(wsStrings(wsAt(built, "project", "dependencies")), []string{"member-dep==1.0"}) {
		t.Fatalf("buildable member %v", built.Values)
	}
	if peRead(t, metadata) != buildableOriginal {
		t.Fatal("plugin manifest mutated by the buildable member")
	}

	wsEdit(t, metadata, func(s string) string {
		return strings.ReplaceAll(s, "dependencies=[\"member-dep==1.0\"]", "dependencies=[\"base-dep==2.0\"]")
	})
	e := wsEngine(t, filepath.Join(tmp, "conflicting-env"), env)
	err = LockAndSync(ctx, wsMembers(plugin), []string{}, LockAndSyncOptions{Root: filepath.Join(tmp, "conflicting-runtime"), Source: core, Environment: e})
	var conflict *ResolutionConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("got %v", err)
	}
}

func TestWorkspaceUnselectedPluginExtrasDoNotConstrainRuntimeWorkspace(t *testing.T) {
	tmp, core, plugin, env := wsLayout(t)
	wsEdit(t, filepath.Join(plugin, "pyproject.toml"), func(s string) string {
		return s + "\n[project.optional-dependencies]\ndev=[\"base-dep==2.0\"]\nfeature=[\"chosen-dep==1.0\"]\n"
	})
	solve := func(name string, extras []string) (any, map[string]string) {
		root := filepath.Join(tmp, name)
		if _, err := wsSync(t, env, wsMembers(plugin), extras, root, core, filepath.Join(tmp, name+"-env")); err != nil {
			t.Fatal(err)
		}
		return wsAt(wsMemberManifest(t, root), "project", "optional-dependencies"), wsLocked(t, filepath.Join(root, "uv.lock"))
	}
	optional, locked := solve("unselected", []string{})
	if _, ok := locked["member-dep"]; optional != nil || locked["base-dep"] != "1.0" || !ok {
		t.Fatalf("unselected %v %v", optional, locked)
	}
	optional, locked = solve("selected", []string{"feature"})
	if !reflect.DeepEqual(wsTable(optional), map[string][]string{"feature": {"chosen-dep==1.0"}}) || locked["base-dep"] != "1.0" {
		t.Fatalf("selected %v %v", wsTable(optional), locked)
	}
}

func TestWorkspacePluginExtrasItsOwnRequirementsNameStayInTheSnapshot(t *testing.T) {
	tmp, _, plugin, _ := wsLayout(t)
	wsEdit(t, filepath.Join(plugin, "pyproject.toml"), func(s string) string {
		return strings.ReplaceAll(s, "dependencies=[\"member-dep==1.0\"]", "dependencies=[\"member-dep==1.0\", \"Plugin_Member[Runtime]\"]") +
			"\n[project.optional-dependencies]\nruntime=[\"plugin-member[extra-io]\"]\nextra_io=[\"other-dep==1.0\"]\n" +
			"dev=[\"base-dep==2.0\"]\n[build-system]\nrequires=[]\nbuild-backend=\"local_backend\"\n"
	})
	member, err := workspaceMember(plugin, filepath.Join(tmp, "buildable"), plugin, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := wsTable(wsAt(wsTOML(t, filepath.Join(member, "pyproject.toml")), "project", "optional-dependencies"))
	if !reflect.DeepEqual(got, map[string][]string{"runtime": {"plugin-member[extra-io]"}, "extra_io": {"other-dep==1.0"}}) {
		t.Fatalf("optional %v", got)
	}
}

func TestWorkspacePluginExtrasNamedOnlyForAnotherPlatformDoNotConstrainTheLock(t *testing.T) {
	tmp, core, plugin, env := wsLayout(t)
	other := "win32"
	if wsHostMarkers(t)["sys_platform"] == other {
		other = "linux"
	}
	wsEdit(t, filepath.Join(plugin, "pyproject.toml"), func(s string) string {
		return s + "\n[project.optional-dependencies]\n" +
			"runtime=[\"plugin-member[other]; sys_platform == '" + other + "'\", \"chosen-dep==1.0\"]\n" +
			"other=[\"base-dep==2.0\"]\n"
	})
	root := filepath.Join(tmp, "workspace")
	if _, err := wsSync(t, env, wsMembers(plugin), []string{"runtime"}, root, core, filepath.Join(tmp, "env")); err != nil {
		t.Fatal(err)
	}
	declared, ok := wsAt(wsMemberManifest(t, root), "project", "optional-dependencies", "other").([]any)
	if !ok || len(declared) != 0 {
		t.Fatalf("other extra %v", declared)
	}
	locked := wsLocked(t, filepath.Join(root, "uv.lock"))
	if _, ok := locked["chosen-dep"]; locked["base-dep"] != "1.0" || !ok {
		t.Fatalf("locked %v", locked)
	}
}

func TestWorkspaceSourcesOfPrunedPluginExtrasAndGroupsLeaveWithThem(t *testing.T) {
	tmp, core, plugin, env := wsLayout(t)
	wheelPath := func(name string) string {
		return filepath.ToSlash(filepath.Join(tmp, "wheels", name+"-1.0-py3-none-any.whl"))
	}
	quoted := func(s string) string {
		data, _ := json.Marshal(s)
		return string(data)
	}
	wheel, chosen, memberWheel := quoted(wheelPath("other_dep")), quoted(wheelPath("chosen_dep")), quoted(wheelPath("member_dep"))
	wsEdit(t, filepath.Join(plugin, "pyproject.toml"), func(s string) string {
		return s + "\n[project.optional-dependencies]\nfeature=[\"other-dep==1.0\"]\ndev=[\"other-dep==1.0\"]\n" +
			"lint=[\"chosen-dep==1.0\"]\n[dependency-groups]\ntest=[\"chosen-dep==1.0\"]\n" +
			"[tool.uv.sources]\nother-dep=[{path=" + wheel + ",extra=\"feature\"},{path=" + wheel + ",extra=\"dev\"}]\n" +
			"chosen-dep=[{path=" + chosen + ",extra=\"lint\"},{path=" + chosen + ",group=\"test\"}]\n" +
			"member-dep={path=" + memberWheel + "}\n"
	})
	root := filepath.Join(tmp, "workspace")
	if _, err := wsSync(t, env, wsMembers(plugin), []string{"feature"}, root, core, filepath.Join(tmp, "env")); err != nil {
		t.Fatal(err)
	}
	sources := wsAt(wsMemberManifest(t, root), "tool", "uv", "sources").(*OrderedObject)
	otherSources, _ := sources.Values["other-dep"].([]any)
	if len(otherSources) != 1 || pyStr(wsAt(otherSources[0], "path")) != wheelPath("other_dep") ||
		pyStr(wsAt(otherSources[0], "extra")) != "feature" || len(otherSources[0].(*OrderedObject).Keys) != 2 {
		t.Fatalf("other-dep sources %v", otherSources)
	}
	if _, ok := sources.Values["chosen-dep"]; ok || wsAt(sources, "member-dep", "path") == nil {
		t.Fatalf("sources %v", sources.Keys)
	}
	locked := wsLocked(t, filepath.Join(root, "uv.lock"))
	for _, name := range []string{"other-dep", "member-dep"} {
		if _, ok := locked[name]; !ok {
			t.Fatalf("%s not locked in %v", name, locked)
		}
	}
}

func TestWorkspaceBaseSelfReferenceGatedOnASelectedExtraKeepsItsExtra(t *testing.T) {
	tmp, _, plugin, _ := wsLayout(t)
	wsEdit(t, filepath.Join(plugin, "pyproject.toml"), func(s string) string {
		return strings.ReplaceAll(s, "dependencies=[\"member-dep==1.0\"]",
			"dependencies=[\"member-dep==1.0\", \"plugin-member[feature]; extra == 'runtime'\", \"plugin-member[deep]; extra == 'feature'\"]") +
			"\n[project.optional-dependencies]\nruntime=[]\nfeature=[\"chosen-dep==1.0\"]\ndeep=[\"other-dep==1.0\"]\ndev=[\"base-dep==2.0\"]\n" +
			"[build-system]\nrequires=[]\nbuild-backend=\"local_backend\"\n"
	})
	member, err := workspaceMember(plugin, filepath.Join(tmp, "buildable"), plugin, []string{"runtime"}, wsHostMarkers(t))
	if err != nil {
		t.Fatal(err)
	}
	got := wsTable(wsAt(wsTOML(t, filepath.Join(member, "pyproject.toml")), "project", "optional-dependencies"))
	want := map[string][]string{"runtime": {}, "feature": {"chosen-dep==1.0"}, "deep": {"other-dep==1.0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("optional %v", got)
	}
}

func TestWorkspaceVirtualPluginSelfReferenceInstallsItsOwnExtraAfterTheRename(t *testing.T) {
	tmp, core, plugin, env := wsLayout(t)
	wsEdit(t, filepath.Join(plugin, "pyproject.toml"), func(s string) string {
		s = strings.ReplaceAll(s, "name=\"plugin-member\"", "name=\"other-dep\"")
		return strings.ReplaceAll(s, "dependencies=[\"member-dep==1.0\"]", "dependencies=[\"member-dep==1.0\", \"Other_Dep[Runtime]>=0\"]") +
			"\n[project.optional-dependencies]\nruntime=[\"chosen-dep==1.0\"]\n"
	})
	root := filepath.Join(tmp, "workspace")
	e, err := wsSync(t, env, wsMembers(plugin), []string{}, root, core, filepath.Join(tmp, "env"))
	if err != nil {
		t.Fatal(err)
	}
	if installed := wsInstalled(t, e.Destination); !installed["chosen_dep"] || installed["other_dep"] {
		t.Fatalf("installed %v", installed)
	}
	project := wsAt(wsMemberManifest(t, root), "project")
	name := pyStr(wsAt(project, "name"))
	if got := wsStrings(wsAt(project, "dependencies")); !reflect.DeepEqual(got, []string{"member-dep==1.0", name + "[Runtime]>=0"}) {
		t.Fatalf("dependencies %v", got)
	}
}

func TestWorkspaceVirtualPluginSelfReferencesFollowTheRenameEverywhere(t *testing.T) {
	tmp, _, plugin, _ := wsLayout(t)
	wsEdit(t, filepath.Join(plugin, "pyproject.toml"), func(s string) string {
		s = strings.ReplaceAll(s, "dependencies=[\"member-dep==1.0\"]", "dependencies=[\"member-dep==1.0\", \"plugin_member[a]\"]")
		s = strings.Replace(s, "[tool.uv]\n", "[tool.uv]\nconflicts=[[{package=\"Plugin-Member\",extra=\"a\"},{extra=\"b\"}]]\n", 1)
		return s + "\n[project.optional-dependencies]\na=[\"plugin-member[b] ; python_version >= '3'\"]\n" +
			"b=[\"chosen-dep==1.0\"]\nunused=[\"base-dep==2.0\"]\n"
	})
	member, err := workspaceMember(plugin, filepath.Join(tmp, "virtual"), plugin, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	document := wsTOML(t, filepath.Join(member, "pyproject.toml"))
	name := pyStr(wsAt(document, "project", "name"))
	if !strings.HasPrefix(name, "icebath-plugin-member-") {
		t.Fatalf("name %s", name)
	}
	if got := wsStrings(wsAt(document, "project", "dependencies")); !reflect.DeepEqual(got, []string{"member-dep==1.0", name + "[a]"}) {
		t.Fatalf("dependencies %v", got)
	}
	want := map[string][]string{"a": {name + "[b] ; python_version >= '3'"}, "b": {"chosen-dep==1.0"}}
	if got := wsTable(wsAt(document, "project", "optional-dependencies")); !reflect.DeepEqual(got, want) {
		t.Fatalf("optional %v", got)
	}
	conflicts, _ := wsAt(document, "tool", "uv", "conflicts").([]any)
	encoded, _ := json.Marshal(conflicts)
	var decoded any
	json.Unmarshal(encoded, &decoded)
	if want := []any{[]any{map[string]any{"package": name, "extra": "a"}, map[string]any{"extra": "b"}}}; !reflect.DeepEqual(decoded, want) {
		t.Fatalf("conflicts %s", encoded)
	}
}

func TestWorkspaceClassifyNetworkFailureStaysGeneric(t *testing.T) {
	var conflict *ResolutionConflict
	if errors.As(ClassifyUVFailure("lock", 1, "error: Failed to fetch https://pypi.org (timed out)"), &conflict) {
		t.Fatal("network failure classified as a conflict")
	}
}

func TestWorkspaceCoreOnlyGenerationCarriesNoReleaseAgeCutoff(t *testing.T) {
	tmp, core, _, _ := wsLayout(t)
	wsEdit(t, filepath.Join(core, "pyproject.toml"), func(s string) string {
		return strings.Replace(s, "[tool.uv]\n", "[tool.uv]\nexclude-newer = \"3 days\"\n", 1)
	})
	root := filepath.Join(tmp, "workspace")
	if err := generatePyproject(context.Background(), wsMembers(), root, core, peHostPython(t), []string{}); err != nil {
		t.Fatal(err)
	}
	generated := peRead(t, filepath.Join(root, "pyproject.toml"))
	if strings.Contains(generated, "exclude-newer") || !strings.Contains(generated, "[tool.uv]") {
		t.Fatalf("generated %s", generated)
	}
}

func TestWorkspaceUndatedIndexFailureNamesTheIndexNotAConflict(t *testing.T) {
	seeded := "  x No solution found when resolving dependencies:\n" +
		"  Because resvg-py==0.4.0 has no publish time and hermes-agent depends on resvg-py==0.4.0, " +
		"we can conclude that hermes-agent's requirements are unsatisfiable."
	cold := "warning: cffi-2.0.0.tar.gz is missing an upload date, but user provided: 2026-09-25T00:00:00Z\n" +
		"  x No solution found when resolving dependencies:\n" +
		"  Because there are no versions of cffi and you require cffi>=2.0.0, we can conclude that " +
		"your requirements are unsatisfiable.\n" +
		"hint: `cffi` was filtered by `exclude-newer` to only include packages uploaded before 2026-09-25"
	var conflict *ResolutionConflict
	for _, output := range []string{seeded, cold} {
		err := ClassifyUVFailure("lock", 1, output)
		var ie *InstallError
		if errors.As(err, &conflict) || !errors.As(err, &ie) || !strings.Contains(ie.Remedy, "upload times") || !strings.Contains(ie.Remedy, "pypi.org") {
			t.Fatalf("%q classified %#v", output, err)
		}
	}
	mixed := strings.Split(cold, "\n")[0] + "\n  x No solution found: Because a==1 depends on b<2 and c==1 depends on b>=2"
	if !errors.As(ClassifyUVFailure("lock", 1, mixed), &conflict) {
		t.Fatal("a real conflict on a partly undated mirror is not a conflict")
	}
}

// Replaces both uv process seams with one recording runner
func wsFakeUV(t *testing.T, run func(command []string, cwd string, env map[string]string) uvResult) {
	t.Helper()
	peSwap(t, &uvCaptureRunner, func(ctx context.Context, command []string, cwd string, env map[string]string, timeout time.Duration) (uvResult, error) {
		return run(command, cwd, env), nil
	})
	peSwap(t, &uvStreamRunner, func(ctx context.Context, command []string, cwd string, env map[string]string, timeout time.Duration, write func(string)) (uvResult, error) {
		return run(command, cwd, env), nil
	})
}

func TestWorkspaceSyncFailureIsNeverAConflict(t *testing.T) {
	tmp, core, _, env := wsLayout(t)
	e := wsEngine(t, filepath.Join(tmp, "candidate"), env)
	wsFakeUV(t, func(command []string, cwd string, env map[string]string) uvResult {
		return uvResult{Code: 1, Stderr: "Failed to download wheel"}
	})
	err := LockAndSync(context.Background(), wsMembers(), []string{}, LockAndSyncOptions{Root: filepath.Join(tmp, "workspace"), Source: core, Environment: e, Frozen: true})
	var ie *InstallError
	var conflict *ResolutionConflict
	if !errors.As(err, &ie) || errors.As(err, &conflict) {
		t.Fatalf("got %#v", err)
	}
}

func TestWorkspaceStagingRootAndEnvAreHonoredWithoutLiveMutation(t *testing.T) {
	tmp, core, _, _ := wsLayout(t)
	staging := filepath.Join(tmp, "staging-ws")
	t.Setenv("PM_WORKSPACE_TEST_SENTINEL", "live")
	e := wsEngine(t, filepath.Join(tmp, "staging-venv"), map[string]string{"PATH": "/staged/bin", "PM_WORKSPACE_TEST_SENTINEL": "staged"})
	t.Setenv("PATH", t.TempDir())
	type call struct {
		command []string
		cwd     string
		env     map[string]string
	}
	var seen []call
	wsFakeUV(t, func(command []string, cwd string, env map[string]string) uvResult {
		seen = append(seen, call{command, cwd, env})
		return uvResult{}
	})
	if err := LockAndSync(context.Background(), wsMembers(), []string{}, LockAndSyncOptions{Root: staging, Source: core, Environment: e}); err != nil {
		t.Fatal(err)
	}
	var stages []string
	for _, c := range seen {
		stages = append(stages, c.command[1])
		if c.command[0] != e.UV || c.cwd != staging || c.env["PM_WORKSPACE_TEST_SENTINEL"] != "staged" || c.env["UV_CACHE_DIR"] != e.Cache ||
			c.env["UV_PROJECT_ENVIRONMENT"] != e.Destination || c.env["UV_PYTHON"] != e.Python {
			t.Fatalf("call %v in %s env %v", c.command, c.cwd, c.env)
		}
	}
	if !reflect.DeepEqual(stages, []string{"lock", "sync"}) {
		t.Fatalf("stages %v", stages)
	}
	if os.Getenv("PM_WORKSPACE_TEST_SENTINEL") != "live" {
		t.Fatal("live environment mutated")
	}
}

func TestWorkspaceSelectedPluginExtraNamingCoreInstallsTheWorkspaceCore(t *testing.T) {
	tmp, core, plugin, env := wsLayout(t)
	peWrite(t, filepath.Join(plugin, "pyproject.toml"), "[project]\nname=\"core-pinning-plugin\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
		"dependencies=[\"member-dep==1.0\"]\n[project.optional-dependencies]\n"+
		"chosen=[\"Construction_Root[other] @ git+https://example.invalid/hermes-agent"+
		"@0000000000000000000000000000000000000000\"]\n[tool.uv]\npackage=false\n")
	e, err := wsSync(t, env, wsMembers(plugin), []string{"chosen"}, filepath.Join(tmp, "workspace"), core, filepath.Join(tmp, "env"))
	if err != nil {
		t.Fatal(err)
	}
	if installed := wsInstalled(t, e.Destination); !installed["member_dep"] || !installed["other_dep"] {
		t.Fatalf("installed %v", installed)
	}
}
