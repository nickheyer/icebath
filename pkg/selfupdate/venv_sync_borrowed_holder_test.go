package selfupdate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Records an interpreter under a store entry the way a committed python install leaves it
func vsRecordStorePython(t *testing.T, store, entry string) string {
	t.Helper()
	python := filepath.Join(store, entry, "bin", "python3")
	if runtime.GOOS == "windows" {
		python = filepath.Join(store, entry, "python.exe")
	}
	vsWrite(t, python, "")
	vsWrite(t, filepath.Join(store, "facts.json"), `{"schema": 1, "packages": {"python": {"version": "fixture", "entry": "`+entry+`"}}}`)
	return python
}

// The install.sh --dir layout, the default root's checkout held under another directory, with its own install binary
func vsHeldCheckout(t *testing.T) (string, string, string, string) {
	t.Helper()
	tmp := vsResolvedTemp(t)
	vsUserHome(t, tmp)
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", tmp)
	}
	vsDataHome(t, filepath.Join(tmp, "icebath_test"))
	t.Setenv(pm.EnvDisableLazyInstalls, "")
	os.Unsetenv(pm.EnvDisableLazyInstalls)
	t.Setenv(pm.EnvRuntimeDir, "")
	os.Unsetenv(pm.EnvRuntimeDir)
	pm.SetCommandArgv(func(argv []string) []string { return argv })
	holder := filepath.Join(tmp, "data", "dir")
	root := filepath.Join(holder, "icebath-agent")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	vsWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname='example'\n")
	vsWrite(t, filepath.Join(root, "install-stamp.json"), `{"updateMechanism": "self", "commit": "abc"}`)
	binary := filepath.Join(root, "icebath")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	vsWrite(t, binary, "fake install binary\n")
	return tmp, home.PlatformDefault(), holder, root
}

// Bytes of each published launcher by path
func vsLauncherBytes(t *testing.T, paths []string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out[path] = data
	}
	return out
}

func vsLaunchersUnchanged(t *testing.T, published map[string][]byte, when string) {
	t.Helper()
	for path, data := range published {
		if now, err := os.ReadFile(path); err != nil || !bytes.Equal(now, data) {
			t.Fatalf("%s rewrote %s to %q %v", when, path, now, err)
		}
	}
}

func TestABorrowerHoldingTheCheckoutNeverBecomesItsOwner(t *testing.T) {
	_, def, holder, root := vsHeldCheckout(t)
	vsNoToolDownloads(t)
	ctx := context.Background()
	vsRecordStorePython(t, filepath.Join(def, "tools"), "python-owner")
	vsDataState(t, def, root)
	vsDataHome(t, def)
	written, err := pm.EnsureInstallLaunchers(ctx, root, filepath.Join(root, ".icebath", "bin"))
	if err != nil {
		t.Fatal(err)
	}
	published := vsLauncherBytes(t, written)
	if len(published) == 0 {
		t.Fatal("the owner published no launchers")
	}

	vsDataHome(t, holder)
	vsDataState(t, holder, root)
	if owner := pm.OwningHomeRoot(ctx, root); owner != def {
		t.Fatalf("owner %q, want %q", owner, def)
	}
	if store, err := pm.EnvironmentStoreRoot(ctx, root, true); err != nil || store != filepath.Join(def, "tools") {
		t.Fatalf("store %q %v", store, err)
	}
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return true, nil })
	if target, err := PrepareLaunch(ctx, root, []string{}); err != nil || target != pm.ResolveInstallBinary(root) {
		t.Fatalf("relaunch target %q %v, want %q", target, err, pm.ResolveInstallBinary(root))
	}
	vsLaunchersUnchanged(t, published, "a borrowing launch")

	vsRecordStorePython(t, filepath.Join(holder, "tools"), "python-holder")
	t.Setenv(pm.EnvRuntimeDir, filepath.Join(holder, "tools"))
	restore := pm.SwapExposeCLI(func(context.Context, string, bool) (pm.ExposeResult, error) {
		return pm.ExposeResult{OK: true}, nil
	})
	defer pm.SwapExposeCLI(restore)
	if result, err := SyncTree(ctx, root, false); err != nil || !result.OK {
		t.Fatalf("explicit sync %+v %v", result, err)
	}
	vsLaunchersUnchanged(t, published, "an explicit borrowing sync")
	os.Unsetenv(pm.EnvRuntimeDir)
	if owner := pm.OwningHomeRoot(ctx, root); owner != def {
		t.Fatalf("owner after the sync %q, want %q", owner, def)
	}
}
