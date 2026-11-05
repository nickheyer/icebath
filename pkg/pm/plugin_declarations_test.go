package pm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPluginDeclPolicySurvivesCrossProfileResolution(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "modern"}[modern], func(t *testing.T) {
			tmp := isolateHome(t)
			wsHost(t, "")
			root := DependencyHomeRoot(context.Background())
			sibling := filepath.Join(root, "profiles", "work")
			t.Setenv("ICEBATH_HOME", sibling)
			if got := DependencyHomeRoot(context.Background()); got != root {
				t.Fatalf("profile home resolves to %s", got)
			}
			core, wheels := filepath.Join(tmp, "core"), filepath.Join(tmp, "wheels")
			peWheel(t, wheels, "fixturedep", "1.0", nil, nil)
			wheelsJSON, _ := json.Marshal(filepath.ToSlash(wheels))
			peWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"core\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
				"[tool.uv]\npackage=false\nno-index=true\nfind-links=["+string(wheelsJSON)+"]\n")
			plugin, external := filepath.Join(root, "plugins", "member"), filepath.Join(sibling, "plugins", "sidecar")
			peWrite(t, filepath.Join(root, "config.yaml"), "plugins:\n  enabled: [member]\n")
			peWrite(t, filepath.Join(sibling, "config.yaml"), "memory:\n  provider: sidecar\n")
			peWrite(t, filepath.Join(external, "plugin.yaml"), "name: sidecar\npython_runtime: external\npython_dependencies: [\"impossible==99\"]\n")
			peWrite(t, filepath.Join(external, "pyproject.toml"), "[project]\nname=\"sidecar\"\nversion=\"1\"\ndependencies=[\"impossible==99\"]\n")
			specs := []string{"fixturedep>=1,<2", "icebath-agent>=0.1,<1", "remote @ https://example.invalid/unreviewed.whl", `missing-other-python; python_version < "3.0"`}
			listed := specs
			if modern {
				listed = []string{"ignored==99"}
			}
			listedJSON, _ := json.Marshal(listed)
			peWrite(t, filepath.Join(plugin, "plugin.yaml"), "name: member\npython_dependencies: "+string(listedJSON)+"\n")
			if modern {
				specsJSON, _ := json.Marshal(specs)
				peWrite(t, filepath.Join(plugin, "pyproject.toml"), "[project]\nname=\"member\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
					"dependencies="+string(specsJSON)+"\n[tool.uv]\npackage=false\n")
			}
			before := map[string]string{}
			for _, dir := range []string{plugin, external} {
				for rel, body := range wsSnapshot(t, dir) {
					before[filepath.Join(dir, rel)] = body
				}
			}
			members, err := EnabledMemberDirs(context.Background(), MemberSelection{})
			if err != nil || !reflect.DeepEqual(members, []string{plugin}) {
				t.Fatalf("members %v %v", members, err)
			}
			if d, err := ReadPythonDeclaration(external); err != nil || !d.External() {
				t.Fatalf("sidecar %v %v", d, err)
			}
			declaration, err := ReadPythonDeclaration(plugin)
			if err != nil || !reflect.DeepEqual(declaration.Requirements, specs) {
				t.Fatalf("requirements %v %v", declaration, err)
			}
			if bad, err := UnsupportedRequirements(declaration.Requirements); err != nil || !reflect.DeepEqual(bad, specs[2:3]) {
				t.Fatalf("unsupported %v %v", bad, err)
			}
			env := map[string]string{}
			for k, v := range pyEnviron() {
				env[k] = v
			}
			environment := &PythonEnvironment{UV: peUV(t), Python: peHostPython(t), Destination: filepath.Join(tmp, "env"),
				Cache: filepath.Join(tmp, "cache"), Env: env, Offline: true}
			snapshot := filepath.Join(tmp, "snapshot")
			if err := LockAndSync(context.Background(), MemberPaths(members), []string{}, LockAndSyncOptions{Root: snapshot, Source: core, Environment: environment}); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(VenvPython(environment.Destination), "-I", "-c", "import fixturedep; print(fixturedep.__version__)").Output()
			if err != nil || strings.TrimSpace(string(out)) != "1.0" {
				t.Fatalf("probe %q %v", out, err)
			}
			relative := wsStrings(wsAt(wsTOML(t, filepath.Join(snapshot, "pyproject.toml")), "tool", "uv", "workspace", "members"))
			if len(relative) != 1 {
				t.Fatalf("members %v", relative)
			}
			generated := wsStrings(wsAt(wsTOML(t, filepath.Join(snapshot, relative[0], "pyproject.toml")), "project", "dependencies"))
			if !reflect.DeepEqual(generated, []string{specs[0], specs[3]}) {
				t.Fatalf("target markers must reach uv, unsafe and self requirements must not: %v", generated)
			}
			for path, body := range before {
				if data, _ := os.ReadFile(path); string(data) != body {
					t.Fatalf("%s changed", path)
				}
			}
		})
	}
}

func TestPluginDeclInvalidRequirementRefusedAtDeclarationBoundary(t *testing.T) {
	for _, modern := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "plugin.yaml")
		body := "python_dependencies: [\"not a requirement >=\"]\n"
		if modern {
			path, body = filepath.Join(dir, "pyproject.toml"), "[project]\ndependencies=[\"not a requirement >=\"]\n"
		}
		peWrite(t, path, body)
		_, err := ReadPythonDeclaration(dir)
		var invalid *InvalidRequirementError
		if !errors.As(err, &invalid) {
			t.Fatalf("modern=%v accepted %v", modern, err)
		}
		if data, _ := os.ReadFile(path); string(data) != body {
			t.Fatal("declaration changed")
		}
	}
}

func TestPluginDeclOnlyAPyprojectThatInstallsSomethingNeedsConsent(t *testing.T) {
	cases := []struct {
		id, body string
		installs bool
	}{
		{"empty", "dependencies = []\n", false},
		{"dependencies", "dependencies = [\"fixturedep>=1,<2\"]\n", true},
		{"buildable", "dependencies = []\n[build-system]\nrequires = [\"setuptools\"]\nbuild-backend = \"setuptools.build_meta\"\n", true},
		{"dependency-groups", "dependencies = []\n[dependency-groups]\ndev = [\"fixturedep\"]\n", true},
		{"extras", "dependencies = []\n[project.optional-dependencies]\nextra = [\"fixturedep\"]\n", true},
	}
	for _, c := range cases {
		dir := t.TempDir()
		peWrite(t, filepath.Join(dir, "plugin.yaml"), "name: probe\n")
		peWrite(t, filepath.Join(dir, "pyproject.toml"), "[project]\nname = \"probe\"\nversion = \"1\"\n"+c.body)
		d, err := ReadPythonDeclaration(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := d.InstallsPackages(); err != nil || got != c.installs {
			t.Fatalf("%s installs %v %v", c.id, got, err)
		}
	}
}

func TestPluginDeclToolingOnlyPyprojectUsesTheManifestDeclaration(t *testing.T) {
	for _, project := range []string{"[tool.ruff]\ntarget-version = 'py311'\n", "project = \"tooling label\"\n[tool.ruff]\ntarget-version = \"py311\"\n"} {
		dir := t.TempDir()
		peWrite(t, filepath.Join(dir, "pyproject.toml"), project)
		peWrite(t, filepath.Join(dir, "plugin.yaml"), "name: tooling-only\npip_dependencies: ['fixturedep>=1,<2']\n")
		d, err := ReadPythonDeclaration(dir)
		if err != nil {
			t.Fatal(err)
		}
		if d.Pyproject != "" || !reflect.DeepEqual(d.Requirements, []string{"fixturedep>=1,<2"}) {
			t.Fatalf("declaration %+v", d)
		}
		if want := []string{filepath.Join(dir, "plugin.yaml"), filepath.Join(dir, "pyproject.toml")}; !reflect.DeepEqual(d.Files, want) {
			t.Fatalf("files %v", d.Files)
		}
	}
}

func TestPluginDeclManifestVersionAndPythonFit(t *testing.T) {
	wsHost(t, "1.2.0")
	cases := []struct {
		manifest map[string]any
		want     string
	}{
		{map[string]any{}, ""},
		{map[string]any{"manifest_version": 2}, ""},
		{map[string]any{"manifest_version": "x"}, "Plugin 'p' has invalid manifest_version 'x' (expected an integer)."},
		{map[string]any{"manifest_version": " 3 "}, "Plugin 'p' requires manifest_version  3 , but this installer only supports up to 2."},
		{map[string]any{"requires_icebath": ">=9"}, "Plugin 'p' requires icebath >=9, running 1.2.0"},
	}
	for _, c := range cases {
		if got, err := ManifestVersionError(c.manifest, "p"); err != nil || got != c.want {
			t.Fatalf("%v gave %q %v", c.manifest, got, err)
		}
	}
	requires := ">=3.12"
	d := &PythonDeclaration{Requirements: []string{"x"}, Manifest: map[string]any{}, RequiresPython: &requires}
	if got, _ := d.PythonError("3.11.9"); got != "requires Python >=3.12, but icebath runs on Python 3.11.9" {
		t.Fatalf("python error %q", got)
	}
	if got, _ := d.PythonError("3.13.0rc1"); got != "" {
		t.Fatalf("prerelease refused %q", got)
	}
}
