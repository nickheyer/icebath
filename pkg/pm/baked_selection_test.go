package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const svBakedPyproject = "[project]\nname=\"baked\"\nversion=\"1\"\n[project.optional-dependencies]\nall=[]\nmessaging=[]\notlp=[]\nfal=[]\nazure-identity=[]\n"

// Baked install checkout and store, returning both
func svBaked(t *testing.T) (string, string) {
	t.Helper()
	tmp := isolateHome(t)
	root := sttMkdir(t, filepath.Join(tmp, "icebath"))
	writeFile(t, filepath.Join(root, "pyproject.toml"), []byte(svBakedPyproject), 0o666)
	store := sttMkdir(t, filepath.Join(tmp, "tools"))
	t.Setenv(EnvRuntimeDir, store)
	peSwap(t, &repoRoot, func() string { return root })
	svGates(t, nil)
	return root, store
}

// Venv of root whose stamp names its sorted extras
func svBakedVenv(root string) *pubVenv {
	v := &pubVenv{Venv: NewVenv(root).(*Venv), stamp: func(_ context.Context, extras []string, _ []MemberDir) (string, error) {
		sorted := append([]string{}, extras...)
		sort.Strings(sorted)
		return "stamp:" + strings.Join(sorted, ","), nil
	}}
	v.Venv.BasePackage.bind(v)
	return v
}

// Records a venv state into a facts file
func svRecordState(t *testing.T, path, stamp string, extras []string) {
	t.Helper()
	facts, err := NewFacts(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.RecordState("venv", stamp, extras, "", ""); err != nil {
		t.Fatal(err)
	}
}

// Venv fact of a facts file
func svFactOf(t *testing.T, path string) map[string]any {
	t.Helper()
	facts, err := NewFacts(path, false)
	if err != nil {
		t.Fatal(err)
	}
	fact, _ := facts.Get("venv")
	return fact
}

// Records every client sync's options, failing it with err
func svSyncCalls(t *testing.T, err error) *[]SyncVenvOptions {
	t.Helper()
	calls := &[]SyncVenvOptions{}
	peSwap(t, &clientSyncVenv, func(_ context.Context, opts SyncVenvOptions) error {
		*calls = append(*calls, opts)
		return err
	})
	return calls
}

// Answers every engine currency probe with current
func svAlwaysCurrent(t *testing.T) {
	t.Helper()
	peSwap(t, &engineVenvIsCurrent, func(context.Context, []string, PluginInput, string) (bool, error) { return true, nil })
}

func TestSVBuildRecordsTheExtraListAsTheStoreSelection(t *testing.T) {
	root, store := svBaked(t)
	ctx := context.Background()
	peSwap(t, &clientBuildEnvironment, func(context.Context, BuildEnvironmentOptions) (string, error) {
		return filepath.Join(root, "venv", "bin", "python"), nil
	})
	peSwap(t, &runFeatureProbe, func(context.Context, []string, string) (uvResult, error) {
		t.Fatal("no inventory expected")
		return uvResult{}, nil
	})
	writeFile(t, filepath.Join(root, "uv.lock"), []byte("version = 1\n"), 0o666)
	var stdout, stderr bytes.Buffer
	argv := []string{"--source", root, "--out", filepath.Join(root, "venv"), "--record-selection",
		"--extra", "all", "--extra", "messaging", "--extra", "otlp", "--extra", "all"}
	if code := BuildEnvMain(ctx, argv, "build_env", &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	stamp, err := NewVenv(resolvePath(root)).(*Venv).Stamp(ctx, []string{"all", "messaging", "otlp"}, []MemberDir{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"stamp": stamp, "extras": []any{"all", "messaging", "otlp"}}
	if got := svFactOf(t, filepath.Join(store, "facts.json")); !reflect.DeepEqual(got, want) {
		t.Fatalf("fact %v, want %v", got, want)
	}
}

func TestSVRecordSelectionRefusesBuildsWithoutAnExtraList(t *testing.T) {
	root, _ := svBaked(t)
	peSwap(t, &clientBuildEnvironment, func(context.Context, BuildEnvironmentOptions) (string, error) {
		t.Fatal("a refused selection record built an environment")
		return "", nil
	})
	var stdout, stderr bytes.Buffer
	argv := []string{"--source", root, "--out", filepath.Join(root, "venv"), "--record-selection", "--all-extras"}
	if code := BuildEnvMain(context.Background(), argv, "build_env", &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--record-selection") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestSVFirstLazyExtraExtendsTheRecordedImageSelection(t *testing.T) {
	for _, row := range []struct {
		store    []string
		expected []string
	}{{nil, []string{"fal"}}, {[]string{"all", "messaging", "otlp"}, []string{"all", "fal", "messaging", "otlp"}}} {
		t.Run(strings.Join(row.store, ","), func(t *testing.T) {
			root, store := svBaked(t)
			ctx := context.Background()
			if row.store != nil {
				svRecordState(t, filepath.Join(store, "facts.json"), "image", row.store)
			}
			facts, err := storeFacts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			fact, _ := facts.Get("venv")
			if fact == nil {
				fact = map[string]any{}
			}
			enabled, _, _, err := targetSelection(ctx, svBakedVenv(root), fact, []string{"fal"}, VenvApplyOptions{PluginDirs: []MemberDir{}}, false, nil, nil)
			if err != nil || !reflect.DeepEqual(enabled, row.expected) {
				t.Fatalf("enabled %v %v", enabled, err)
			}
		})
	}
}

func TestSVRefreshRestoresBakedExtrasAVolumeLost(t *testing.T) {
	for _, recorded := range [][]string{{"fal"}, {}} {
		t.Run(strings.Join(recorded, ","), func(t *testing.T) {
			root, store := svBaked(t)
			ctx := context.Background()
			svRecordState(t, filepath.Join(store, "facts.json"), "image", []string{"all", "messaging", "otlp"})
			svRecordState(t, EnvironmentRuntimeFactsPath(ctx, root), "lazy", recorded)
			calls := svSyncCalls(t, nil)
			svAlwaysCurrent(t)
			if got, err := RefreshDependencies(ctx, root); err != nil || got != "restored all, messaging, otlp" {
				t.Fatalf("refresh %q %v", got, err)
			}
			baseline := []string{"all", "messaging", "otlp"}
			if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0].Extras, baseline) || !(*calls)[0].Explicit || !reflect.DeepEqual((*calls)[0].Automatic, baseline) {
				t.Fatalf("calls %+v", *calls)
			}
		})
	}
}

func TestSVRefreshLeavesACompleteSelectionAlone(t *testing.T) {
	root, store := svBaked(t)
	ctx := context.Background()
	svRecordState(t, filepath.Join(store, "facts.json"), "image", []string{"all", "messaging"})
	svRecordState(t, EnvironmentRuntimeFactsPath(ctx, root), "lazy", []string{"all", "fal", "messaging"})
	calls := svSyncCalls(t, nil)
	svAlwaysCurrent(t)
	if got, err := RefreshDependencies(ctx, root); err != nil || got != "current" || len(*calls) != 0 {
		t.Fatalf("refresh %q %v, calls %v", got, err, *calls)
	}
}

func TestSVOfflineRefreshKeepsBakedExtrasForTheNextLazyInstall(t *testing.T) {
	root, store := svBaked(t)
	ctx := context.Background()
	svRecordState(t, filepath.Join(store, "facts.json"), "image", []string{"all", "messaging", "otlp"})
	svRecordState(t, EnvironmentRuntimeFactsPath(ctx, root), "lazy", []string{"fal"})
	svSyncCalls(t, &PyError{Class: "RuntimeError", Message: "uv sync exited 1: no network"})
	svAlwaysCurrent(t)
	if got, err := RefreshDependencies(ctx, root); err != nil || got != "fallback" {
		t.Fatalf("refresh %q %v", got, err)
	}
	fact := svFactOf(t, EnvironmentRuntimeFactsPath(ctx, root))
	if !reflect.DeepEqual(fact["extras"], []any{"fal", "all", "messaging", "otlp"}) {
		t.Fatalf("fact %v", fact)
	}
	if _, has := fact["environment"]; has {
		t.Fatalf("the image environment must boot, fact %v", fact)
	}
	enabled, _, _, err := targetSelection(ctx, svBakedVenv(root), fact, []string{"fal"}, VenvApplyOptions{PluginDirs: []MemberDir{}}, false, nil, nil)
	if err != nil || !reflect.DeepEqual(enabled, []string{"all", "fal", "messaging", "otlp"}) {
		t.Fatalf("enabled %v %v", enabled, err)
	}
}

func TestSVRefreshRefusesAnInvalidRecordedSelection(t *testing.T) {
	for i, venv := range []map[string]any{{"stamp": "s", "extras": "fal"}, {"stamp": "s", "extras": nil},
		{"stamp": "s", "extras": []any{1}}, {"stamp": "s"}, {"extras": []any{"fal"}}, {"stamp": "", "extras": []any{"fal"}},
		{"stamp": 7, "extras": []any{"fal"}}} {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			root, store := svBaked(t)
			ctx := context.Background()
			svRecordState(t, filepath.Join(store, "facts.json"), "image", []string{"all", "messaging"})
			path := EnvironmentRuntimeFactsPath(ctx, root)
			data, _ := json.Marshal(map[string]any{"schema": 1, "packages": map[string]any{"venv": venv}})
			if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
				t.Fatal(err)
			}
			writeFile(t, path, data, 0o666)
			peSwap(t, &clientSyncVenv, func(context.Context, SyncVenvOptions) error {
				t.Fatal("must not sync over a corrupt fact")
				return nil
			})
			_, err := RefreshDependencies(ctx, root)
			var pe *PyError
			if !errors.As(err, &pe) || pe.Class != "ValueError" || !strings.Contains(err.Error(), "invalid recorded dependency state") {
				t.Fatalf("corrupt fact %v gave %v", venv, err)
			}
		})
	}
}

func TestSVRefreshMatchesRecordedExtrasByPep685Name(t *testing.T) {
	root, store := svBaked(t)
	ctx := context.Background()
	svRecordState(t, filepath.Join(store, "facts.json"), "image", []string{"all", "azure-identity"})
	svRecordState(t, EnvironmentRuntimeFactsPath(ctx, root), "lazy", []string{"all", "Azure_Identity", "fal"})
	calls := svSyncCalls(t, nil)
	svAlwaysCurrent(t)
	if got, err := RefreshDependencies(ctx, root); err != nil || got != "current" || len(*calls) != 0 {
		t.Fatalf("refresh %q %v, calls %v", got, err, *calls)
	}
}

func TestSVRefreshIgnoresBakedExtrasTheSourceNoLongerDeclares(t *testing.T) {
	root, store := svBaked(t)
	ctx := context.Background()
	svRecordState(t, filepath.Join(store, "facts.json"), "image", []string{"all", "hindsight"})
	svRecordState(t, EnvironmentRuntimeFactsPath(ctx, root), "lazy", []string{"all"})
	calls := svSyncCalls(t, nil)
	svAlwaysCurrent(t)
	if got, err := RefreshDependencies(ctx, root); err != nil || got != "current" || len(*calls) != 0 {
		t.Fatalf("refresh %q %v, calls %v", got, err, *calls)
	}
}
