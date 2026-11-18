package pm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A dependency sync of a tree without a project provisions its runtime set and never builds a project environment
func TestSyncVenvOfAProjectlessTreeProvisionsItsRuntimeSet(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(PatchRepoRoot(root))
	savedRuntime, savedWorker := syncRuntimeEnvironment, clientWorkerCall
	t.Cleanup(func() { syncRuntimeEnvironment, clientWorkerCall = savedRuntime, savedWorker })
	var provisioned []RuntimeEnvironmentOptions
	syncRuntimeEnvironment = func(_ context.Context, opts RuntimeEnvironmentOptions) (string, error) {
		provisioned = append(provisioned, opts)
		return filepath.Join(root, "venv"), nil
	}
	worker := 0
	clientWorkerCall = func(context.Context, string, policyArgs, func(context.Context) error) error {
		worker++
		return errors.New("the project sync ran")
	}
	for _, opts := range []SyncVenvOptions{
		{Explicit: true, ProjectRoot: root, EvictIncompatiblePlugins: true},
		{Explicit: true, Extras: []string{"nemo_relay"}},
		{Repair: true},
	} {
		if err := SyncVenv(context.Background(), opts); err != nil {
			t.Fatalf("%+v: %v", opts, err)
		}
	}
	if worker != 0 || len(provisioned) != 3 || !provisioned[0].Explicit || !provisioned[2].Explicit || provisioned[0].Output == nil {
		t.Fatalf("worker %d provisioned %+v", worker, provisioned)
	}
	if err := SyncVenv(context.Background(), SyncVenvOptions{Explicit: true, Extras: []string{"matrix"}}); err == nil || worker != 1 {
		t.Fatalf("an extra beyond the runtime set must reach the project sync: worker %d err %v", worker, err)
	}
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[project]\nname = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SyncVenv(context.Background(), SyncVenvOptions{Explicit: true}); err == nil || worker != 2 || len(provisioned) != 3 {
		t.Fatalf("a tree with a project must reach the project sync: worker %d provisioned %d err %v", worker, len(provisioned), err)
	}
}
