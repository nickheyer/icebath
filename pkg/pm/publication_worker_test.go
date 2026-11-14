package pm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Selection, config and receipt checks of a publication whose dependencies are already current
func TestPublicationWorkerPublishesSelectionEvenWhenDependenciesAreCurrent(t *testing.T) {
	tmp, homeDir := pubClient(t)
	config := filepath.Join(homeDir, "config.yaml")
	peWrite(t, config, "# keep my comment\nmodel: \"untouched\"\nplugins:\n  enabled: [old]\n")
	repo := pubCurrentEnvironment(t, tmp, nil)
	facts := peRead(t, pubState(t, repo, "facts.json"))
	ctx := WithReceiptScope(context.Background())
	id := "selection-publication"
	restore := WorkerContext(ctx, &id)
	err := SyncVenv(ctx, SyncVenvOptions{Explicit: true, Plugins: &Selection{Data: map[string]any{
		"home": homeDir, "enabled": pubList("plain"), "disabled": pubList("old"), "extra_dirs": pubList(),
	}}})
	result := LastForUpdate(ctx, id, true)
	restore()
	if err != nil {
		t.Fatal(err)
	}
	text := peRead(t, config)
	if plugins := svConfig(t, config)["plugins"]; !reflect.DeepEqual(plugins, map[string]any{"enabled": []any{"plain"}, "disabled": []any{"old"}}) {
		t.Fatalf("plugins %v", plugins)
	}
	if !strings.Contains(text, "# keep my comment") || !strings.Contains(text, "model: \"untouched\"") {
		t.Fatalf("config %s", text)
	}
	if peRead(t, pubState(t, repo, "facts.json")) != facts || pathExists(pubState(t, repo, "publication.json")) {
		t.Fatal("a current publication rewrote facts or left its journal")
	}
	rebuild, _ := result["venv_rebuild"].(map[string]any)
	if result == nil || result["outcome"] != "ok" || rebuild == nil || rebuild["ok"] != false {
		t.Fatalf("receipt %v", result)
	}
	var latest map[string]any
	if err := json.Unmarshal([]byte(peRead(t, filepath.Join(homeDir, "logs", "update_receipts", "latest.json"))), &latest); err != nil || latest["outcome"] != "ok" {
		t.Fatalf("latest receipt %v %v", latest, err)
	}
}

// Real core project locked offline through the client with the real toolchain
func pubLockedProject(t *testing.T, tmp, name string) string {
	t.Helper()
	project := filepath.Join(tmp, "project")
	peWrite(t, filepath.Join(project, "pyproject.toml"), "[project]\nname=\""+name+"\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n[tool.uv]\npackage=false\n")
	peSwap(t, &repoRoot, func() string { return project })
	peToolchain(t, peUV(t), peHostPython(t))
	wsHost(t, "")
	if err := LockProject(context.Background(), LockProjectOptions{Source: project, Offline: true, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	return project
}

func TestPublicationStagedPluginUsesInstalledIdentityAndLocalDependencies(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("active=%v/missing=%v", active, missing), func(t *testing.T) {
				tmp, homeDir := pubClient(t)
				ctx := context.Background()
				project := pubLockedProject(t, tmp, "publication-core")
				target := filepath.Join(homeDir, "plugins", "example")
				enabled := "[]"
				if active {
					enabled = "[example]"
				}
				peWrite(t, filepath.Join(homeDir, "config.yaml"), "plugins:\n  enabled: "+enabled+"\n")
				peWrite(t, filepath.Join(target, "plugin.yaml"), "name: example\n")
				peWrite(t, filepath.Join(target, "code.py"), "old code")
				metadata := filepath.Join(homeDir, "plugins", ".install-metadata.json")
				peWrite(t, metadata, "{\"example\":{\"revision\":\"old\"}}\n")
				if err := SyncVenv(ctx, SyncVenvOptions{Explicit: true}); err != nil {
					t.Fatal(err)
				}
				previous := svSelected(t, project)
				if missing {
					os.RemoveAll(target)
				}
				staged := filepath.Join(tmp, "staged")
				wheel := peWheel(t, tmp, "publication_dep", "", nil, nil)
				peWrite(t, filepath.Join(staged, "plugin.yaml"), "name: example\npython_dependencies: [\"publication-dep==1.0\"]\n")
				links, _ := json.Marshal(filepath.ToSlash(filepath.Dir(wheel)))
				peWrite(t, filepath.Join(project, "pyproject.toml"), peRead(t, filepath.Join(project, "pyproject.toml"))+"no-index=true\nfind-links=["+string(links)+"]\n")
				peWrite(t, filepath.Join(staged, "code.py"), "new code")
				digest, err := optionalTreeDigest(target)
				if err != nil {
					t.Fatal(err)
				}
				err = SyncVenv(ctx, SyncVenvOptions{Explicit: true, Plugins: &StagedUpdate{Data: map[string]any{
					"staged": staged, "target": target, "target_digest": digest,
					"old_metadata": map[string]any{"example": map[string]any{"revision": "old"}},
					"new_metadata": map[string]any{"example": map[string]any{"revision": "new"}},
				}}})
				if err != nil {
					t.Fatal(err)
				}
				if peRead(t, filepath.Join(target, "code.py")) != "new code" || pathExists(staged) {
					t.Fatal("staged tree not published")
				}
				var records map[string]map[string]any
				json.Unmarshal([]byte(peRead(t, metadata)), &records)
				if records["example"]["revision"] != "new" {
					t.Fatalf("metadata %v", records)
				}
				selected := svSelected(t, project)
				if (selected != previous) != active {
					t.Fatalf("selected %s previous %s", selected, previous)
				}
				if active {
					out, err := exec.Command(VenvPython(selected), "-I", "-c", "import publication_dep; print(publication_dep.__version__)").CombinedOutput()
					if err != nil || strings.TrimSpace(string(out)) != "1.0" {
						t.Fatalf("probe %s %v", out, err)
					}
					facts := peRead(t, pubState(t, project, "facts.json"))
					if err := SyncVenv(ctx, SyncVenvOptions{Explicit: true}); err != nil {
						t.Fatal(err)
					}
					if peRead(t, pubState(t, project, "facts.json")) != facts {
						t.Fatal("a current sync rewrote facts")
					}
				}
				if pathExists(pubState(t, project, "publication.json")) {
					t.Fatal("journal left behind")
				}
				if left, _ := filepath.Glob(filepath.Join(homeDir, "plugins", ".previous-*")); len(left) != 0 {
					t.Fatalf("backups left %v", left)
				}
			})
		}
	}
}

func TestPublicationSelectionRefusesConfigEditsDuringPreparation(t *testing.T) {
	for _, mutation := range []string{"sibling", "active"} {
		t.Run(mutation, func(t *testing.T) {
			tmp, homeDir := pubClient(t)
			config := filepath.Join(homeDir, "config.yaml")
			peWrite(t, config, "plugins:\n  enabled: [old]\n")
			repo := pubCurrentEnvironment(t, tmp, nil)
			peWrite(t, filepath.Join(repo, "uv.lock"), "version = 2\n")
			edited := config
			if mutation == "sibling" {
				edited = filepath.Join(homeDir, "profiles", "sibling", "config.yaml")
			}
			expected := "plugins:\n  enabled: [concurrent]\n"
			facts := peRead(t, pubState(t, repo, "facts.json"))
			pubSwapVenv(t, &pubVenv{prepare: func(context.Context, []string, VenvApplyOptions) (*VenvGeneration, error) {
				peWrite(t, edited, expected)
				return nil, nil
			}})
			err := SyncVenv(context.Background(), SyncVenvOptions{Explicit: true, Plugins: &Selection{Data: map[string]any{
				"home": homeDir, "enabled": pubList("new"), "disabled": pubList(),
			}}})
			pubWantValueError(t, err, "changed")
			if peRead(t, edited) != expected || peRead(t, pubState(t, repo, "facts.json")) != facts || pathExists(pubState(t, repo, "publication.json")) {
				t.Fatal("refused selection touched state")
			}
		})
	}
}

// Fails unless err is a Python ValueError whose message contains fragment
func pubWantValueError(t *testing.T, err error, fragment string) {
	t.Helper()
	var py *PyError
	if !errors.As(err, &py) || py.Class != "ValueError" || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("want ValueError with %q, got %T %v", fragment, err, err)
	}
}

func TestPublicationWorkerRejectsUnloadableStagedPluginWithoutAppDependencies(t *testing.T) {
	for _, invalid := range []string{"name: [", "manifest_version: 999", "requires_icebath: '>=999'", "name: other"} {
		t.Run(invalid, func(t *testing.T) {
			tmp, homeDir := pubClient(t)
			pubCurrentEnvironment(t, tmp, nil)
			wsHost(t, "1.0.0")
			target := filepath.Join(homeDir, "plugins", "example")
			peWrite(t, filepath.Join(target, "plugin.yaml"), "name: example\n")
			staged := filepath.Join(tmp, "staged")
			peWrite(t, filepath.Join(staged, "plugin.yaml"), invalid)
			previous, _ := TreeDigest(target)
			err := SyncVenv(context.Background(), SyncVenvOptions{Explicit: true, Plugins: &StagedUpdate{Data: map[string]any{
				"target": target, "staged": staged, "target_digest": previous,
				"old_metadata": map[string]any{}, "new_metadata": map[string]any{"example": map[string]any{"revision": "new"}},
			}}})
			pubWantValueError(t, err, "")
			if now, _ := TreeDigest(target); now != previous || !pathExists(staged) {
				t.Fatal("refused staged plugin changed the install")
			}
		})
	}
}

func TestPublicationAdditionalCandidatesAreDiscoveredBySyncAndPassiveProbe(t *testing.T) {
	tmp, _ := pubClient(t)
	ctx := context.Background()
	candidate := filepath.Join(tmp, "candidate")
	peWrite(t, filepath.Join(candidate, "plugin.yaml"), "name: candidate\npython_dependencies: [fixture-dep==1]\n")
	pubCurrentEnvironment(t, tmp, []string{candidate})
	wsHost(t, "")
	current := func(plugins PluginInput) bool {
		ok, err := VenvIsCurrent(ctx, VenvIsCurrentOptions{Plugins: plugins})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !current(&Candidates{Dirs: []string{candidate}}) || current(nil) {
		t.Fatal("candidate currency")
	}
	if err := SyncVenv(ctx, SyncVenvOptions{Explicit: true, Plugins: &Candidates{Dirs: []string{candidate}}}); err != nil {
		t.Fatal(err)
	}
	if !current(&Candidates{Dirs: []string{candidate}}) {
		t.Fatal("candidate sync not current")
	}
}

func TestPublicationStagedPublicationRefusesConcurrentInputEdits(t *testing.T) {
	for _, mutation := range []string{"metadata", "target", "staged", "sibling-manifest"} {
		t.Run(mutation, func(t *testing.T) {
			tmp, homeDir := pubClient(t)
			repo := pubCurrentEnvironment(t, tmp, nil)
			wsHost(t, "")
			target := filepath.Join(homeDir, "plugins", "example")
			peWrite(t, filepath.Join(target, "plugin.yaml"), "name: example\n")
			peWrite(t, filepath.Join(target, "code.py"), "old code")
			sibling := filepath.Join(homeDir, "plugins", "sibling")
			peWrite(t, filepath.Join(sibling, "plugin.yaml"), "name: sibling\npython_dependencies: [fixture-dep==1]\n")
			peWrite(t, filepath.Join(homeDir, "config.yaml"), "plugins:\n  enabled: [example, sibling]\n")
			metadata := filepath.Join(homeDir, "plugins", ".install-metadata.json")
			peWrite(t, metadata, "{}\n")
			staged := filepath.Join(tmp, "staged")
			peWrite(t, filepath.Join(staged, "plugin.yaml"), "name: example\npython_dependencies: [fixture-dep==1]\n")
			peWrite(t, filepath.Join(staged, "code.py"), "new code")
			edited := map[string]string{"metadata": metadata, "target": filepath.Join(target, "code.py"),
				"staged": filepath.Join(staged, "code.py"), "sibling-manifest": filepath.Join(sibling, "plugin.yaml")}[mutation]
			proposed := "concurrent edit"
			if mutation == "sibling-manifest" {
				proposed = "name: sibling\nversion: changed\npython_dependencies: [fixture-dep==2]\n"
			}
			pubSwapVenv(t, &pubVenv{prepare: func(context.Context, []string, VenvApplyOptions) (*VenvGeneration, error) {
				peWrite(t, edited, proposed)
				return nil, nil
			}})
			facts := peRead(t, pubState(t, repo, "facts.json"))
			digest, _ := TreeDigest(target)
			err := SyncVenv(context.Background(), SyncVenvOptions{Explicit: true, Plugins: &StagedUpdate{Data: map[string]any{
				"target": target, "staged": staged, "target_digest": digest,
				"old_metadata": map[string]any{}, "new_metadata": map[string]any{"example": map[string]any{"revision": "new"}},
			}}})
			pubWantValueError(t, err, "changed")
			want := "old code"
			if mutation == "target" {
				want = proposed
			}
			if peRead(t, edited) != proposed || peRead(t, filepath.Join(target, "code.py")) != want {
				t.Fatal("refused publication changed the inputs")
			}
			if peRead(t, pubState(t, repo, "facts.json")) != facts || pathExists(pubState(t, repo, "publication.json")) {
				t.Fatal("refused publication touched state")
			}
		})
	}
}

func TestPublicationStagedPublicationPreservesAConcurrentSiblingInstallRecord(t *testing.T) {
	tmp, homeDir := pubClient(t)
	pubCurrentEnvironment(t, tmp, nil)
	wsHost(t, "")
	target := filepath.Join(homeDir, "plugins", "example")
	peWrite(t, filepath.Join(target, "plugin.yaml"), "name: example\n")
	peWrite(t, filepath.Join(target, "code.py"), "old code")
	peWrite(t, filepath.Join(homeDir, "config.yaml"), "plugins:\n  enabled: [example]\n")
	metadata := filepath.Join(homeDir, "plugins", ".install-metadata.json")
	peWrite(t, metadata, "{\"example\":{\"revision\":\"old\"}}\n")
	staged := filepath.Join(tmp, "staged")
	peWrite(t, filepath.Join(staged, "plugin.yaml"), "name: example\npython_dependencies: [fixture-dep==1]\n")
	peWrite(t, filepath.Join(staged, "code.py"), "new code")
	pubSwapVenv(t, &pubVenv{prepare: func(context.Context, []string, VenvApplyOptions) (*VenvGeneration, error) {
		peWrite(t, metadata, "{\"example\": {\"revision\": \"old\"}, \"sibling\": {\"revision\": \"sibling-new\"}}\n")
		return nil, nil
	}})
	digest, _ := TreeDigest(target)
	err := SyncVenv(context.Background(), SyncVenvOptions{Explicit: true, Plugins: &StagedUpdate{Data: map[string]any{
		"target": target, "staged": staged, "target_digest": digest,
		"old_metadata": map[string]any{"example": map[string]any{"revision": "old"}},
		"new_metadata": map[string]any{"example": map[string]any{"revision": "new"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var records map[string]any
	json.Unmarshal([]byte(peRead(t, metadata)), &records)
	if !reflect.DeepEqual(records, map[string]any{"example": map[string]any{"revision": "new"}, "sibling": map[string]any{"revision": "sibling-new"}}) ||
		peRead(t, filepath.Join(target, "code.py")) != "new code" {
		t.Fatalf("metadata %v", records)
	}
}

func TestPublicationInactivePortablePublicationDoesNotInspectUnrelatedDependencyManifests(t *testing.T) {
	tmp, homeDir := pubClient(t)
	pubCurrentEnvironment(t, tmp, nil)
	wsHost(t, "")
	target := filepath.Join(homeDir, "plugins", "inactive")
	if err := os.MkdirAll(target, 0o777); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(homeDir, "plugins", "sibling")
	writeFile(t, filepath.Join(sibling, "plugin.yaml"), []byte{0xff}, 0o644)
	peWrite(t, filepath.Join(homeDir, "config.yaml"), "plugins:\n  enabled: [sibling]\n")
	staged := filepath.Join(tmp, "staged")
	manifest, _ := json.Marshal(map[string]any{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json", "name": "inactive", "version": "1.0.0"})
	writeFile(t, filepath.Join(staged, "plugin.json"), manifest, 0o644)
	digest, _ := TreeDigest(target)
	err := SyncVenv(context.Background(), SyncVenvOptions{Explicit: true, Plugins: &StagedUpdate{Data: map[string]any{
		"target": target, "staged": staged, "target_digest": digest,
		"old_metadata": map[string]any{}, "new_metadata": map[string]any{"inactive": map[string]any{"revision": "new"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var published map[string]any
	json.Unmarshal([]byte(peRead(t, filepath.Join(target, "plugin.json"))), &published)
	if published["name"] != "inactive" || peRead(t, filepath.Join(sibling, "plugin.yaml")) != "\xff" {
		t.Fatalf("published %v", published)
	}
}

func TestPublicationSelectionPreservesYAML11ValuesAndQuotesPluginNames(t *testing.T) {
	tmp, homeDir := pubClient(t)
	pubCurrentEnvironment(t, tmp, nil)
	config := filepath.Join(homeDir, "config.yaml")
	peWrite(t, config, "feature: yes\nother: no\nlabel: \"on\"\nplugins: {enabled: []}\n")
	load := func() map[string]any {
		doc, err := loadRoundtrip([]byte(peRead(t, config)))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]any{}
		root := rtRoot(doc)
		for i := 0; i+1 < len(root.Content); i += 2 {
			value := rtDeref(root.Content[i+1])
			if tag := rtTag(value); tag == "!!bool" {
				out[root.Content[i].Value] = rtTruthy(value)
			} else if tag == "!!str" {
				out[root.Content[i].Value] = value.Value
			} else {
				var decoded any
				value.Decode(&decoded)
				out[root.Content[i].Value] = decoded
			}
		}
		return out
	}
	before := load()
	err := SyncVenv(context.Background(), SyncVenvOptions{Explicit: true, Plugins: &Selection{Data: map[string]any{
		"home": homeDir, "enabled": pubList("on", "yes", "no"), "disabled": pubList(),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	after := load()
	for _, key := range []string{"feature", "other", "label"} {
		if !reflect.DeepEqual(after[key], before[key]) {
			t.Fatalf("%s %v became %v", key, before[key], after[key])
		}
	}
	enabled := map[string]bool{}
	for _, name := range svList(svAt(after, "plugins", "enabled")) {
		enabled[name] = true
	}
	if !reflect.DeepEqual(enabled, map[string]bool{"on": true, "yes": true, "no": true}) || !strings.Contains(peRead(t, config), "label: \"on\"") {
		t.Fatalf("config %s", peRead(t, config))
	}
}

func TestPublicationExplicitPublicationKeepsItsIntentThroughToolAcquisition(t *testing.T) {
	tmp, homeDir := pubClient(t)
	project := pubLockedProject(t, tmp, "explicit-proof")
	t.Setenv(EnvDisableLazyInstalls, "1")
	tools := uvToolchain
	peSwap(t, &uvToolchain, func(ctx context.Context, realize, explicit bool) (string, string, bool, error) {
		if !explicit {
			return "", "", false, NewInstallError("tools", "explicit intent was lost", "")
		}
		return tools(ctx, realize, explicit)
	})
	err := SyncVenv(context.Background(), SyncVenvOptions{Explicit: true, Plugins: &Selection{Data: map[string]any{
		"home": homeDir, "enabled": pubList(), "disabled": pubList(),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !isRegularFile(filepath.Join(svSelected(t, project), "pyvenv.cfg")) {
		t.Fatal("explicit publication built no environment")
	}
}
