package selfupdate

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

const dbrHermesRepository = "../../references/projects/hermes-agent"

var dbrCopied = []string{
	"scripts/build/freshness.mjs", "scripts/build/frontend-common.mjs", "scripts/build/desktop-reuse.mjs",
	"apps/desktop/scripts/write-build-stamp.mjs", "apps/desktop/scripts/bundle-env.mjs",
	"apps/desktop/scripts/utils.mjs", "apps/desktop/product-identity.cjs",
	"scripts/msix-shared.mjs", "scripts/release-content-types.json",
}

func dbrGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func dbrNode(t *testing.T, root, script string) {
	t.Helper()
	node, _ := exec.LookPath("node")
	cmd := exec.Command(node, "--input-type=module", "-e", script)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// Checkout whose packaged app was built with a baked stamp and a recorded receipt at its first commit
func dbrPackagedCheckout(t *testing.T) string {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("the desktop reuse probe runs under Node")
	}
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	t.Setenv("ICEBATH_RUNTIME_DIR", filepath.Join(tmp, "tools"))
	t.Cleanup(SwapSourceToolsForTests(sourceEnsure, func(context.Context) (string, bool) { return node, true }))
	root := filepath.Join(tmp, "checkout")
	for _, name := range dbrCopied {
		raw, err := os.ReadFile(filepath.Join(dbrHermesRepository, name))
		if err != nil {
			t.Fatal(err)
		}
		sbWrite(t, filepath.Join(root, name), string(raw))
	}
	sbWrite(t, filepath.Join(root, "apps/desktop/src/main.tsx"), "export {}\n")
	sbWrite(t, filepath.Join(root, "cmd/icebath/banner.go"), "package main\n")
	sbWrite(t, filepath.Join(root, ".gitignore"), "apps/desktop/build/\napps/desktop/release/\n")
	dbrGit(t, root, "init", "-q", "-b", "main")
	dbrGit(t, root, "add", "-A")
	dbrGit(t, root, "commit", "-q", "-m", "one")
	desktop := filepath.Join(root, "apps/desktop")
	stamp := filepath.Join(desktop, "build/install-stamp.json")
	dist := filepath.Join(desktop, "release/linux-unpacked/resources/app.asar.unpacked/dist")
	sbWrite(t, filepath.Join(desktop, "release/linux-unpacked/icebath"), "")
	sbWrite(t, filepath.Join(dist, "electron-main.mjs"), "// baked\n")
	dbrNode(t, root, "import {resolveStamp, buildStampPayload, writeDesktopStamp} from './apps/desktop/scripts/write-build-stamp.mjs';"+
		"writeDesktopStamp('apps/desktop/build', buildStampPayload(resolveStamp({repoRoot: process.cwd()}), process.env, process.platform, null))")
	raw, err := os.ReadFile(stamp)
	if err != nil {
		t.Fatal(err)
	}
	sbWrite(t, filepath.Join(filepath.Dir(filepath.Dir(dist)), "install-stamp.json"), string(raw))
	distJSON, _ := json.Marshal(dist)
	stampJSON, _ := json.Marshal(stamp)
	dbrNode(t, root, "import {recordProduct, buildInputs} from './scripts/build/freshness.mjs';"+
		"recordProduct({source: '.', product: 'desktop', out: "+string(distJSON)+", inputs: buildInputs('.', 'desktop', {stamp: "+string(stampJSON)+"})})")
	return root
}

// Runs the update's desktop product step with the build, refresh and lock recorded, true when it built
func dbrUpdateBuildsDesktop(t *testing.T, root string) bool {
	var builds int
	prev := productSteps
	productSteps.buildDesktop = func(context.Context, string, bool, string, map[string]string, string) (string, error) {
		builds++
		return "", nil
	}
	productSteps.refreshApps = func(context.Context, string) {}
	productSteps.desktopLock = func(context.Context, string, bool) (*DesktopBuildLock, error) { return &DesktopBuildLock{}, nil }
	productSteps.publishStage = func(string) {}
	defer func() { productSteps = prev }()
	sbCapture(t)
	env := sourceEnviron()
	env["PATH"] = "/usr/bin:/bin"
	if err := buildDesktopProduct(context.Background(), root, env); err != nil {
		t.Fatal(err)
	}
	return builds > 0
}

func TestUpdateReusesThePackagedAppUntilADesktopInputChanges(t *testing.T) {
	sbNeedsLinux(t)
	root := dbrPackagedCheckout(t)
	builtAt := dbrGit(t, root, "rev-parse", "HEAD")
	sbWrite(t, filepath.Join(root, "cmd/icebath/banner.go"), "package main\n\nvar banner = 2\n")
	dbrGit(t, root, "commit", "-q", "-am", "go only")
	if dbrUpdateBuildsDesktop(t, root) {
		t.Fatal("a non desktop commit rebuilt the desktop app")
	}
	raw, _ := os.ReadFile(filepath.Join(root, "apps/desktop/release/linux-unpacked/resources/install-stamp.json"))
	var baked map[string]any
	_ = json.Unmarshal(raw, &baked)
	if baked["commit"] != builtAt {
		t.Fatalf("the reused app must keep its real commit, got %v", baked["commit"])
	}
	sbWrite(t, filepath.Join(root, "apps/desktop/src/main.tsx"), "export const changed = 1\n")
	dbrGit(t, root, "commit", "-q", "-am", "desktop change")
	if !dbrUpdateBuildsDesktop(t, root) {
		t.Fatal("a desktop source change must rebuild")
	}
}

func TestAStampABuildWouldBakeDifferentlyStillRebuilds(t *testing.T) {
	sbNeedsLinux(t)
	root := dbrPackagedCheckout(t)
	dbrGit(t, root, "checkout", "-q", "-b", "other")
	sbWrite(t, filepath.Join(root, "cmd/icebath/banner.go"), "package main\n\nvar banner = 3\n")
	dbrGit(t, root, "commit", "-q", "-am", "go only, on another branch")
	if !dbrUpdateBuildsDesktop(t, root) {
		t.Fatal("a stamp naming another branch was reused")
	}
}

func TestUpdatePathWaitsForAHeldLock(t *testing.T) {
	root := dblCheckout(t)
	t.Setenv("ICEBATH_RUNTIME_DIR", filepath.Join(t.TempDir(), "tools"))
	ctx := context.Background()
	holder := NewDesktopBuildLock(root)
	if !dblAcquire(t, holder, false) {
		t.Fatal("holder refused")
	}
	var releaseCalls []bool
	var built []bool
	prev := productSteps
	t.Cleanup(func() { productSteps = prev })
	productSteps.featureDeps = func(context.Context, string) error { return nil }
	productSteps.memoryMigrate = func(context.Context, func(string)) ([]string, error) { return nil, nil }
	productSteps.leftCoreMigrate = func(context.Context, func(string)) ([]string, error) { return nil, nil }
	productSteps.frontends = func(string) []string { return []string{"ui-tui", "web"} }
	productSteps.buildEnv = func(context.Context, map[string]string, bool, bool) (map[string]string, error) {
		return map[string]string{"PATH": "/usr/bin"}, nil
	}
	productSteps.prepareDeps = func(context.Context, string, []string, map[string]string, bool) error { return nil }
	productSteps.buildTUI = func(context.Context, string, map[string]string) error { return nil }
	productSteps.buildWeb = func(context.Context, string, map[string]string, string) error { return nil }
	productSteps.refreshApps = func(context.Context, string) {}
	productSteps.publishStage = func(string) {}
	productSteps.desktopLock = func(ctx context.Context, projectRoot string, wait bool) (*DesktopBuildLock, error) {
		releaseCalls = append(releaseCalls, wait)
		holder.Release(ctx)
		lock := NewDesktopBuildLock(projectRoot)
		lock.Out = &strings.Builder{}
		_, err := lock.Acquire(ctx, wait)
		return lock, err
	}
	productSteps.buildDesktop = func(context.Context, string, bool, string, map[string]string, string) (string, error) {
		built = append(built, true)
		return "", nil
	}
	t.Cleanup(SwapSourceToolsForTests(func(_ context.Context, name string, opts pm.EnsureOptions) (*pm.Runner, error) {
		return &pm.Runner{Name: name, Env: opts.BaseEnv}, nil
	}, func(context.Context) (string, bool) { return "", false }))
	sbCapture(t)
	if err := BuildUpdateProducts(ctx, root, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(releaseCalls, []bool{true}) {
		t.Fatalf("update path must acquire the lock in wait mode, calls %v", releaseCalls)
	}
	if !reflect.DeepEqual(built, []bool{true}) {
		t.Fatalf("built %v", built)
	}
}
