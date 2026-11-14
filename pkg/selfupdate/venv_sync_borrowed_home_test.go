package selfupdate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Facts a committed dependency sync records for a checkout
const vsCompleteFacts = `{"packages": {"venv": {"stamp": "complete", "extras": ["all"]}}}`

// Records the completion children a launch spawns instead of running them, exiting with the chosen status
type vsSpawned struct {
	commands [][]string
	kwargs   ChildOptions
	exitCode int
}

func vsCompletionTail(t *testing.T) *vsSpawned {
	t.Helper()
	spawned := &vsSpawned{}
	vsSwap(t, &completionCustodyRun, func(_ context.Context, argv []string, _ bool, opts ChildOptions) (*CompletedProcess, error) {
		spawned.commands = append(spawned.commands, argv)
		spawned.kwargs = opts
		return &CompletedProcess{Args: argv, ReturnCode: spawned.exitCode}, nil
	})
	return spawned
}

// Keeps a launch sync from downloading the lockfile tools it publishes first
func vsNoToolDownloads(t *testing.T) {
	t.Helper()
	vsSwap(t, &ensureToolsForSync, func(context.Context) error { return nil })
}

// Test directory with symlinks resolved, as pytest hands out tmp_path
func vsResolvedTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Makes tmp the user's home directory for the platform and home lookups
func vsUserHome(t *testing.T, tmp string) {
	t.Helper()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Cleanup(home.PatchUserHome(tmp))
}

// The store binary a launch resolves is this process's own executable
func vsStoreIsThisProcess(t *testing.T) {
	t.Helper()
	vsSwap(t, &resolveStorePython, func(string) string {
		exe, _ := runningExecutable()
		return exe
	})
}

// Checkout in the install.sh layout under the platform default root, with that root
func vsBorrowedCheckout(t *testing.T) (string, string, string) {
	t.Helper()
	tmp := vsResolvedTemp(t)
	vsUserHome(t, tmp)
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", tmp)
	}
	vsDataHome(t, filepath.Join(tmp, "icebath_test"))
	def := home.PlatformDefault()
	t.Setenv(pm.EnvDisableLazyInstalls, "")
	os.Unsetenv(pm.EnvDisableLazyInstalls)
	t.Setenv(pm.EnvRuntimeDir, "")
	os.Unsetenv(pm.EnvRuntimeDir)
	pm.SetCommandArgv(func(argv []string) []string { return argv })
	root := filepath.Join(def, "icebath-agent")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	vsWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname='example'\n")
	vsWrite(t, filepath.Join(root, "install-stamp.json"), `{"updateMechanism": "self", "commit": "abc"}`)
	return tmp, def, root
}

// Records the committed dependency state a data root keeps for the checkout
func vsDataState(t *testing.T, dataRoot, checkout string) string {
	t.Helper()
	facts := filepath.Join(dataRoot, "installs", pm.InstallKey(checkout), "facts.json")
	vsWrite(t, facts, vsCompleteFacts)
	return facts
}

// Makes path the process data root
func vsDataHome(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, path)
}

func TestABorrowingLaunchSyncsItsOwnDependenciesAndNothingOfTheCheckout(t *testing.T) {
	tmp, def, root := vsBorrowedCheckout(t)
	vsNoToolDownloads(t)
	tail := vsCompletionTail(t)
	ctx := context.Background()
	vsDataState(t, def, root)
	vsDataHome(t, filepath.Join(tmp, "tmp", "icebath-flash-compat-x", "a"))
	if owner := pm.OwningHomeRoot(ctx, root); owner != def {
		t.Fatalf("owner %q, want %q", owner, def)
	}
	if store, err := pm.EnvironmentStoreRoot(ctx, root, true); err != nil || store != filepath.Join(def, "tools") {
		t.Fatalf("store %q %v", store, err)
	}
	stamp, err := os.ReadFile(filepath.Join(root, "install-stamp.json"))
	if err != nil {
		t.Fatal(err)
	}
	ownerMarker := filepath.Join(root, ".update-incomplete")
	vsWrite(t, ownerMarker, "owner's update\n")
	borrowedFacts := filepath.Join(pm.InstallStateDir(ctx, root), "facts.json")
	var syncs [][]string
	sync := func(_ context.Context, opts pm.SyncVenvOptions) error {
		syncs = append(syncs, opts.Extras)
		vsWrite(t, borrowedFacts, "{}")
		return nil
	}
	python := filepath.Join(def, "tools", "python", "bin", "python3")
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return isRegular(borrowedFacts), nil })
	vsSwap(t, &syncVenv, sync)
	vsSwap(t, &resolveStorePython, func(string) string { return python })
	vsSwap(t, &publishLaunchers, func(context.Context, string) error {
		t.Fatal("published the owner's launchers")
		return nil
	})

	if target, err := PrepareLaunch(ctx, root, []string{"kanban", "init"}); err != nil || target != python {
		t.Fatalf("relaunch into the synced env %q %v", target, err)
	}
	if len(syncs) != 1 {
		t.Fatalf("syncs %v", syncs)
	}
	if len(tail.commands) != 0 {
		t.Fatal("a borrowing launch ran the checkout's update tail")
	}
	if existsFollow(CompletionPendingPath(ctx, root)) {
		t.Fatal("a tail was armed for a borrowing root")
	}
	if !isRegular(ownerMarker) {
		t.Fatal("the owner's update marker was removed")
	}
	if after, err := os.ReadFile(filepath.Join(root, "install-stamp.json")); err != nil || !bytes.Equal(after, stamp) {
		t.Fatalf("install stamp changed %q %v", after, err)
	}

	vsSwap(t, &runningExecutable, func() (string, error) { return python, nil })
	if target, err := PrepareLaunch(ctx, root, []string{"kanban", "init"}); err != nil || target != "" {
		t.Fatalf("current relaunch %q %v", target, err)
	}
	if len(syncs) != 1 || len(tail.commands) != 0 {
		t.Fatalf("syncs %v tail %v", syncs, tail.commands)
	}
}

func TestTheOwnerStillOwesAndRunsItsTail(t *testing.T) {
	_, def, root := vsBorrowedCheckout(t)
	vsNoToolDownloads(t)
	tail := vsCompletionTail(t)
	facts := vsDataState(t, def, root)
	t.Setenv(home.EnvHome, "")
	os.Unsetenv(home.EnvHome)
	if err := os.Remove(facts); err != nil {
		t.Fatal(err)
	}
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return isRegular(facts), nil })
	vsSwap(t, &syncVenv, func(context.Context, pm.SyncVenvOptions) error { return os.WriteFile(facts, []byte("{}"), 0o666) })
	vsStoreIsThisProcess(t)
	var published []string
	vsSwap(t, &publishLaunchers, func(_ context.Context, r string) error { published = append(published, r); return nil })

	exe, _ := runningExecutable()
	if target, err := PrepareLaunch(context.Background(), root, []string{}); err != nil || target != exe {
		t.Fatalf("owner launch %q %v", target, err)
	}
	if len(tail.commands) != 1 {
		t.Fatalf("tail ran %d times", len(tail.commands))
	}
	if !reflect.DeepEqual(published, []string{root}) {
		t.Fatalf("published %v", published)
	}
}
