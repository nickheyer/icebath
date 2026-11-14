package selfupdate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/pm"
)

func wrWorkspaceTree(t *testing.T, root string) {
	for _, dir := range []string{"apps/desktop", "apps/shared", "web", "ui-tui"} {
		sbWrite(t, filepath.Join(root, dir, "package.json"), "{}")
	}
	sbWrite(t, filepath.Join(root, "package.json"), `{"private":true}`)
	sbWrite(t, filepath.Join(root, "package-lock.json"), "locked\n")
}

func wrTree(root string) []string {
	var all []string
	_ = filepath.WalkDir(root, func(p string, _ fs.DirEntry, _ error) error { all = append(all, p); return nil })
	slices.Sort(all)
	return all
}

// Replaces the renderer receipt, env and compiler for one test
func wrSteps(t *testing.T, current func() bool, build func(explicit bool) error) {
	prev, prevCurrent := webappSteps, sourceProductCurrent
	t.Cleanup(func() { webappSteps, sourceProductCurrent = prev, prevCurrent })
	sourceProductCurrent = func(context.Context, string, string, string) bool { return current() }
	webappSteps.buildEnv = func(context.Context, bool) (map[string]string, error) { return map[string]string{}, nil }
	webappSteps.build = func(_ context.Context, _ string, _ map[string]string, explicit bool) error { return build(explicit) }
}

func wrPrepare(t *testing.T, root string, force, skipBuild, explicit bool) (string, error) {
	sbCapture(t)
	return PrepareWebappRenderer(context.Background(), root, force, skipBuild, explicit)
}

func wrBuildError(t *testing.T, err error, fragment string) {
	t.Helper()
	var buildErr *WebappBuildError
	if !errors.As(err, &buildErr) || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("err %v, want a WebappBuildError naming %q", err, fragment)
	}
}

func TestSkipBuildRequiresTheSeparateWebappBundle(t *testing.T) {
	root := t.TempDir()
	wrWorkspaceTree(t, root)
	sbWrite(t, filepath.Join(root, "apps/desktop/dist/index.html"), "native")
	_, err := wrPrepare(t, root, false, true, false)
	wrBuildError(t, err, "dist-webapp")
}

func TestSkipBuildWritesNothingIntoTheInstallTree(t *testing.T) {
	root := t.TempDir()
	wrWorkspaceTree(t, root)
	dist := WebappDistDir(root)
	sbWrite(t, filepath.Join(dist, "index.html"), "<!doctype html>")
	tree := wrTree(root)
	if got, err := wrPrepare(t, root, false, true, false); err != nil || got != dist {
		t.Fatalf("got %q err %v", got, err)
	}
	if !reflect.DeepEqual(wrTree(root), tree) {
		t.Fatal("--skip-build wrote the install tree")
	}
	os.Remove(filepath.Join(dist, "index.html"))
	tree = wrTree(root)
	_, err := wrPrepare(t, root, false, true, false)
	wrBuildError(t, err, "no Webapp renderer exists")
	if !reflect.DeepEqual(wrTree(root), tree) {
		t.Fatal("a refused --skip-build wrote the install tree")
	}
}

func TestACurrentBundleIsReusedWithoutWritingTheInstallTree(t *testing.T) {
	root := t.TempDir()
	wrWorkspaceTree(t, root)
	dist := WebappDistDir(root)
	sbWrite(t, filepath.Join(dist, "index.html"), "<!doctype html>")
	wrSteps(t, func() bool { return true }, func(bool) error { t.Fatal("no build"); return nil })
	tree := wrTree(root)
	if got, err := wrPrepare(t, root, false, false, false); err != nil || got != dist {
		t.Fatalf("got %q err %v", got, err)
	}
	if !reflect.DeepEqual(wrTree(root), tree) {
		t.Fatal("reusing a current bundle wrote the install tree")
	}
}

func TestAPackagedBundleIsServedAsShipped(t *testing.T) {
	root := t.TempDir()
	wrWorkspaceTree(t, root)
	packaged := filepath.Join(root, "store", "webapp")
	sbWrite(t, filepath.Join(packaged, "index.html"), "<!doctype html>")
	t.Setenv(WebappDistEnv, packaged)
	wrSteps(t, func() bool { t.Fatal("no freshness check"); return false }, func(bool) error { t.Fatal("no build"); return nil })
	if WebappDistDir(root) != packaged {
		t.Fatalf("dist %q", WebappDistDir(root))
	}
	tree := wrTree(root)
	if got, err := wrPrepare(t, root, false, false, false); err != nil || got != packaged {
		t.Fatalf("got %q err %v", got, err)
	}
	if marked, err := MarkWebappStale(root); marked || err != nil {
		t.Fatalf("marked %v err %v", marked, err)
	}
	if !reflect.DeepEqual(wrTree(root), tree) {
		t.Fatal("a packaged bundle was written")
	}
	_, err := wrPrepare(t, root, true, false, false)
	wrBuildError(t, err, "cannot be rebuilt")
	os.Remove(filepath.Join(packaged, "index.html"))
	_, err = wrPrepare(t, root, false, false, false)
	wrBuildError(t, err, "ICEBATH_WEBAPP_DIST is set")
}

func wrAssertBuildLockExcludesSecondOpen(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "webapp.lock")
	open := func() *os.File {
		f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	_, err := withWebappBuildLock(lockPath, func() (string, error) {
		contender := open()
		defer contender.Close()
		if held, err := pm.LockFD(contender, false, 0); err != nil || held {
			t.Fatalf("a second open took the held lock (%v, %v)", held, err)
		}
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	contender := open()
	defer contender.Close()
	if held, err := pm.LockFD(contender, false, 0); err != nil || !held {
		t.Fatalf("the released lock stayed held (%v, %v)", held, err)
	}
	pm.UnlockFD(contender)
	successor := open()
	defer successor.Close()
	if held, err := pm.LockFD(successor, false, 0); err != nil || !held {
		t.Fatalf("the unlocked lock refused the next open (%v, %v)", held, err)
	}
	pm.UnlockFD(successor)
}

func TestWebappBuildLockExcludesASecondOpen(t *testing.T) { wrAssertBuildLockExcludesSecondOpen(t) }

func TestWebappBuildLockExcludesASecondOpenOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("platforms windows")
	}
	wrAssertBuildLockExcludesSecondOpen(t)
}

func TestLaunchRebuildsOnlyStaleOrForcedRenderers(t *testing.T) {
	root := t.TempDir()
	wrWorkspaceTree(t, root)
	current := true
	var builds []bool
	wrSteps(t, func() bool { return current }, func(explicit bool) error { builds = append(builds, explicit); return nil })
	dist, err := wrPrepare(t, root, false, false, false)
	if err != nil || dist != filepath.Join(root, "apps", "desktop", "dist-webapp") || len(builds) != 0 {
		t.Fatalf("dist %q err %v builds %v", dist, err, builds)
	}
	if _, err := wrPrepare(t, root, true, false, true); err != nil {
		t.Fatal(err)
	}
	current = false
	if _, err := wrPrepare(t, root, false, false, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(builds, []bool{true, false}) {
		t.Fatalf("builds %v", builds)
	}
}

func TestRendererBuildFailuresAreReportedNotRaisedRaw(t *testing.T) {
	for name, failure := range map[string]error{
		"permission": &fs.PathError{Op: "open", Path: "node_modules", Err: fs.ErrPermission},
		"called":     &pm.CalledProcessError{Argv: []string{"node", "build-webapp.mjs"}, Code: 1},
		"runtime":    desktopPyError{"RuntimeError", "npm is unavailable and lazy installs are disabled"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			wrWorkspaceTree(t, root)
			wrSteps(t, func() bool { return false }, func(bool) error { return failure })
			_, err := wrPrepare(t, root, false, false, false)
			wrBuildError(t, err, "build failed")
		})
	}
}

func TestABuildOrACurrentBundleClearsTheOutOfDateMark(t *testing.T) {
	root := t.TempDir()
	wrWorkspaceTree(t, root)
	dist := WebappDistDir(root)
	sbWrite(t, filepath.Join(dist, "index.html"), "<!doctype html>")
	current := false
	wrSteps(t, func() bool { return current }, func(bool) error { return nil })
	if marked, err := MarkWebappStale(root); !marked || err != nil {
		t.Fatalf("marked %v err %v", marked, err)
	}
	if gateway.BundleMissingReason(dist) == "" {
		t.Fatal("a marked bundle has no reason")
	}
	if _, err := wrPrepare(t, root, false, true, false); err != nil {
		t.Fatal(err)
	}
	if gateway.BundleMissingReason(dist) == "" {
		t.Fatal("--skip-build cleared the mark")
	}
	if _, err := wrPrepare(t, root, false, false, false); err != nil {
		t.Fatal(err)
	}
	if gateway.BundleMissingReason(dist) != "" {
		t.Fatal("a build left the mark")
	}
	if marked, err := MarkWebappStale(root); !marked || err != nil {
		t.Fatalf("marked %v err %v", marked, err)
	}
	current = true
	if _, err := wrPrepare(t, root, false, false, false); err != nil {
		t.Fatal(err)
	}
	if gateway.BundleMissingReason(dist) != "" {
		t.Fatal("a current bundle left the mark")
	}
}

func TestNoBundleIsMarkedOutOfDateBeforeOneIsBuilt(t *testing.T) {
	root := t.TempDir()
	wrWorkspaceTree(t, root)
	tree := wrTree(root)
	if marked, err := MarkWebappStale(root); marked || err != nil {
		t.Fatalf("marked %v err %v", marked, err)
	}
	if !reflect.DeepEqual(wrTree(root), tree) {
		t.Fatal("an unbuilt bundle was marked")
	}
}
