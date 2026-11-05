package pm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const foreignHelperEnv = "PM_FOREIGN_CHECKOUT_HELPER"

// Serves the worker protocol over the toolchain the parent test passed in
func TestForeignCheckoutHelperProcess(t *testing.T) {
	encoded := os.Getenv(foreignHelperEnv)
	if encoded == "" {
		return
	}
	var args map[string]string
	json.Unmarshal([]byte(encoded), &args)
	uvToolchain = func(context.Context, bool, bool) (string, string, bool, error) {
		return args["uv"], args["python"], true, nil
	}
	wsHost(t, "")
	os.Exit(WorkerMain())
}

// A sync of another checkout runs in its own worker and selects a generation under that checkout's state
func TestWorkerForeignCheckoutSyncUsesItsOwnPMGeneration(t *testing.T) {
	tmp := ieClientEnv(t)
	ctx := context.Background()
	uv, python := peUV(t), peHostPython(t)
	peToolchain(t, uv, python)
	wsHost(t, "")
	original := sttMkdir(t, filepath.Join(tmp, "checkout"))
	peSwap(t, &repoRoot, func() string { return original })
	encoded, _ := json.Marshal(map[string]string{"uv": uv, "python": python})
	t.Setenv(foreignHelperEnv, string(encoded))
	peSwap(t, &workerArgv, func() ([]string, error) {
		return []string{os.Args[0], "-test.run=^TestForeignCheckoutHelperProcess$", "-test.count=1"}, nil
	})
	foreign := filepath.Join(tmp, "other checkout")
	peWrite(t, filepath.Join(foreign, "pyproject.toml"),
		"[project]\nname=\"foreign-proof\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n[tool.uv]\npackage=false\n")
	if err := LockProject(ctx, LockProjectOptions{Source: foreign, Offline: true, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	if current, err := VenvIsCurrent(ctx, VenvIsCurrentOptions{ProjectRoot: foreign}); err != nil || current {
		t.Fatalf("unsynced foreign checkout current %v %v", current, err)
	}

	if err := SyncVenv(ctx, SyncVenvOptions{Extras: []string{}, ProjectRoot: foreign, Plugins: &Members{Dirs: []MemberDir{}}, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	selected, err := SelectedVenv(ctx, foreign)
	if err != nil {
		t.Fatal(err)
	}
	facts := EnvironmentRuntimeFactsPath(ctx, foreign)
	if selected == filepath.Join(foreign, "venv") || !pathIsRelativeTo(selected, filepath.Dir(facts)) || !isRegularFile(facts) {
		t.Fatalf("foreign selection %s under facts %s", selected, facts)
	}
	if current, err := VenvIsCurrent(ctx, VenvIsCurrentOptions{ProjectRoot: foreign}); err != nil || !current {
		t.Fatalf("synced foreign checkout current %v %v", current, err)
	}
	if RepoRoot() != original {
		t.Fatalf("repo root moved to %s", RepoRoot())
	}
	if pathExists(EnvironmentRuntimeFactsPath(ctx, original)) {
		t.Fatal("the foreign sync recorded facts for the running checkout")
	}
}
