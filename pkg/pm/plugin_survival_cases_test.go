package pm

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// A wrapper root without dependency keys never joins, nor do its nested or external sidecar pyprojects
func TestSurvivalSidecarNoRootPyprojectExcludesNestedAndExternal(t *testing.T) {
	tmp := isolateHome(t)
	wsHost(t, "")
	home := filepath.Join(tmp, ".icebath")
	svWriteEnabled(t, home, []string{"mnemosyne-wrapper"}, "")
	plugins := filepath.Join(home, "plugins")
	wrapper := filepath.Join(plugins, "mnemosyne-wrapper")
	peWrite(t, filepath.Join(wrapper, "plugin.yaml"), "name: mnemosyne-wrapper\n")
	peWrite(t, filepath.Join(wrapper, "mnemosyne-wrapper.json"), "{\"wrapper\": true}\n")
	peWrite(t, filepath.Join(wrapper, "runtime", "pyproject.toml"), "[project]\nname = \"mnemosyne-runtime\"\nversion = \"1.0.0\"\n")
	peWrite(t, filepath.Join(plugins, ".mnemosyne-sidecar", "pyproject.toml"), "[project]\nname = \"mnemosyne-sidecar\"\nversion = \"1.0.0\"\n")

	candidate, err := declaresPackages(wrapper)
	if err != nil || candidate {
		t.Fatalf("wrapper root is a member candidate %v %v", candidate, err)
	}
	members, err := EnabledMemberDirs(context.Background(), MemberSelection{})
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if name := filepath.Base(member); name == "mnemosyne-wrapper" || name == "mnemosyne-sidecar" || strings.Contains(member, "runtime") {
			t.Fatalf("sidecar joined the union %v", members)
		}
	}
}

// A plugin our version identity rejects sits out with config untouched and rejoins once the verdict flips
func TestSurvivalPluginOurVersionRejectsSitsOutWithoutBeingDisabled(t *testing.T) {
	tmp, home := svAdmissionEnv(t)
	ctx := context.Background()
	core := filepath.Join(tmp, "core")
	wsHost(t, "1.0.0")
	for _, name := range []string{"fits", "needs-newer"} {
		svInstallingMember(t, filepath.Join(home, "plugins", name), name, filepath.Join(tmp, "wheels"), "")
	}
	manifest := filepath.Join(home, "plugins", "needs-newer", "plugin.yaml")
	peWrite(t, manifest, "name: needs-newer\nrequires_icebath: '>=999'\n")
	svWriteEnabled(t, home, []string{"fits", "needs-newer"}, "")
	before := peRead(t, filepath.Join(home, "config.yaml"))

	svEngineCurrent(t, core, nil, false)
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	if after := peRead(t, filepath.Join(home, "config.yaml")); after != before {
		t.Fatalf("config changed %q", after)
	}
	warnings, _ := json.Marshal(LatestReceipt(ctx)["warnings"])
	if !strings.Contains(string(warnings), "Left plugin 'needs-newer'") {
		t.Fatalf("receipt warnings %s", warnings)
	}
	if strings.Contains(svWorkspaceManifest(t), "needs-newer") {
		t.Fatal("misfit joined the workspace")
	}
	svEngineCurrent(t, core, nil, true)

	peWrite(t, manifest, "name: needs-newer\nrequires_icebath: '>=0'\n")
	svEngineCurrent(t, core, nil, false)
	if err := syncVenv(ctx, SyncVenvOptions{Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(svWorkspaceManifest(t), "needs-newer") {
		t.Fatal("fitting plugin did not rejoin the workspace")
	}
}

// A plugin requiring a newer icebath through a dependency or a chosen extra sits out until icebath fits
func TestSurvivalPluginRequiringANewerIcebathSitsOutUntilIcebathFits(t *testing.T) {
	cases := []struct {
		id, dependencies, chosen string
		extras                   []string
	}{
		{"runtime-dependency", `"member-dep==1.0", "icebath-agent>=2"`, `"member-dep==1.0"`, nil},
		{"selected-extra", `"member-dep==1.0"`, `"icebath-agent>=2"`, []string{"chosen"}},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			tmp, home := svAdmissionEnv(t)
			ctx := context.Background()
			core := filepath.Join(tmp, "core")
			wsEdit(t, filepath.Join(core, "pyproject.toml"), func(s string) string {
				return strings.Replace(s, `name = "fake-core"`, `name = "icebath-agent"`, 1) + "[project.optional-dependencies]\nchosen = []\n"
			})
			env := peIsolatedEnv(t, tmp)
			env["UV_CACHE_DIR"] = filepath.Join(tmp, "uv-cache")
			peRun(t, []string{peUV(t), "lock", "--python", peHostPython(t)}, core, env)
			wsHost(t, "1.0.0")
			for _, name := range []string{"fits", "needs-newer"} {
				svInstallingMember(t, filepath.Join(home, "plugins", name), name, filepath.Join(tmp, "wheels"), "")
			}
			wsEdit(t, filepath.Join(home, "plugins", "needs-newer", "pyproject.toml"), func(s string) string {
				return strings.Replace(s, "dependencies=[\"member-dep==1.0\"]\n",
					"dependencies=["+tc.dependencies+"]\n[project.optional-dependencies]\nchosen=["+tc.chosen+"]\n", 1)
			})
			svWriteEnabled(t, home, []string{"fits", "needs-newer"}, "")
			config := filepath.Join(home, "config.yaml")
			before := peRead(t, config)

			if err := syncVenv(ctx, SyncVenvOptions{Extras: tc.extras, Explicit: true}); err != nil {
				t.Fatal(err)
			}
			if built := svWorkspaceManifest(t); !strings.Contains(built, "fits") || strings.Contains(built, "needs-newer") {
				t.Fatalf("plain sync built %s", built)
			}
			if err := syncVenv(ctx, SyncVenvOptions{Extras: tc.extras, Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
				t.Fatal(err)
			}
			if peRead(t, config) != before || strings.Contains(svWorkspaceManifest(t), "needs-newer") {
				t.Fatal("update sync disabled or built the misfit")
			}
			svEngineCurrent(t, core, nil, true)

			wsHost(t, "2.0.0")
			svEngineCurrent(t, core, nil, false)
			if err := syncVenv(ctx, SyncVenvOptions{Extras: tc.extras, Explicit: true, EvictIncompatiblePlugins: true}); err != nil {
				t.Fatal(err)
			}
			if peRead(t, config) != before || !strings.Contains(svWorkspaceManifest(t), "needs-newer") {
				t.Fatal("fitting plugin did not rejoin with config untouched")
			}
		})
	}
}

// Fails unless the engine currency probe of the core answers want
func svEngineCurrent(t *testing.T, core string, extras []string, want bool) {
	t.Helper()
	current, err := venvIsCurrent(context.Background(), extras, nil, core)
	if err != nil || current != want {
		t.Fatalf("venv current %v want %v (%v)", current, want, err)
	}
}
