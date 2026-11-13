package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/pm"
)

func sbWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sbRun(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out)
	}
	return string(out)
}

func sbEvents(t *testing.T, root string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return []map[string]any{}
	}
	if err != nil {
		t.Fatal(err)
	}
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

func sbSteps(t *testing.T, root string) []string {
	out := []string{}
	for _, e := range sbEvents(t, root) {
		out = append(out, e["step"].(string))
	}
	return out
}

// Captures the desktop and product build output for one test
func sbCapture(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	prev := desktopOut
	desktopOut = &buf
	t.Cleanup(func() { desktopOut = prev })
	return &buf
}

// Replaces the migrations, feature deps and stage publication of an update with no ops
func sbQuietUpdateSteps(t *testing.T) {
	prev := productSteps
	productSteps.featureDeps = func(context.Context, string) error { return nil }
	productSteps.memoryMigrate = func(context.Context, func(string)) ([]string, error) { return nil, nil }
	productSteps.leftCoreMigrate = func(context.Context, func(string)) ([]string, error) { return nil, nil }
	productSteps.publishStage = func(string) {}
	t.Cleanup(func() { productSteps = prev })
}

// Failure the fixture dashboard compiler reports when the checkout asks it to fail
var sbWebFailure = errors.New("fixture web failure")

func sbNeedsLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
}

func sbNeedsPosix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
}

func TestAutomaticBuildPreservesPmAdmissionIntent(t *testing.T) {
	var intent []bool
	t.Cleanup(SwapSourceToolsForTests(func(_ context.Context, name string, opts pm.EnsureOptions) (*pm.Runner, error) {
		intent = append(intent, opts.Explicit)
		return &pm.Runner{Name: name, Env: opts.BaseEnv}, nil
	}, sourceNodeBinary))
	if _, err := SourceBuildEnv(context.Background(), nil, false, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(intent, []bool{false}) {
		t.Fatalf("intent %v", intent)
	}
}

func TestPackagedDesktopIsReusedOnlyWhileItNamesHead(t *testing.T) {
	sbNeedsPosix(t)
	root := filepath.Join(t.TempDir(), "checkout")
	_ = os.MkdirAll(root, 0o755)
	git := []string{"git", "-C", root, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}
	sbRun(t, root, "git", "-C", root, "init", "-q")
	sbRun(t, root, append(git, "commit", "-q", "--allow-empty", "-m", "one")...)
	desktopDir := filepath.Join(root, "apps/desktop")
	exe := filepath.Join(desktopDir, "release/app/Icebath")
	prevExe := desktopPackagedExecutable
	desktopPackagedExecutable = func(string) (string, bool) { return exe, true }
	t.Cleanup(func() { desktopPackagedExecutable = prevExe })
	resources := packagedResourcesDir(desktopDir)
	_ = os.MkdirAll(filepath.Join(resources, "app.asar.unpacked/dist"), 0o755)
	receiptCurrent := true
	prevNeeded := desktopBuildNeeded
	desktopBuildNeeded = func(context.Context, string, string, bool) bool { return !receiptCurrent }
	t.Cleanup(func() { desktopBuildNeeded = prevNeeded })
	head := func() string { return strings.TrimSpace(sbRun(t, root, "git", "-C", root, "rev-parse", "HEAD")) }
	reused := func() bool { return packagedDesktopCurrentForHead(context.Background(), desktopDir, root) }
	if reused() {
		t.Fatal("reused without a baked stamp")
	}
	raw, _ := json.Marshal(map[string]any{"commit": head()})
	sbWrite(t, filepath.Join(resources, "install-stamp.json"), string(raw))
	if !reused() {
		t.Fatal("not reused while naming HEAD")
	}
	receiptCurrent = false
	if reused() {
		t.Fatal("reused with a stale receipt")
	}
	receiptCurrent = true
	sbRun(t, root, append(git, "commit", "-q", "--allow-empty", "-m", "two")...)
	if reused() {
		t.Fatal("reused after HEAD moved")
	}
}

func TestUpdateRebuildsTheWebappBundleAHomeServes(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			tmp := t.TempDir()
			root := filepath.Join(tmp, "checkout")
			sbWrite(t, filepath.Join(root, "apps/desktop/vite.config.ts"), "")
			homes := []string{filepath.Join(tmp, "default"), filepath.Join(tmp, "profiles", "work")}
			for _, h := range homes {
				_ = os.MkdirAll(h, 0o755)
			}
			sbWrite(t, filepath.Join(homes[1], "config.yaml"), "webapp:\n  enabled: "+map[bool]string{false: "false", true: "true"}[enabled]+"\n")
			sbQuietUpdateSteps(t)
			sbCapture(t)
			productSteps.dependencyHomes = func(context.Context) ([]string, error) { return homes, nil }
			type build struct {
				root                       string
				force, skipBuild, explicit bool
			}
			var builds []build
			prev := PrepareWebappRenderer
			PrepareWebappRenderer = func(_ context.Context, projectRoot string, force, skipBuild, explicit bool) (string, error) {
				builds = append(builds, build{projectRoot, force, skipBuild, explicit})
				return "", nil
			}
			t.Cleanup(func() { PrepareWebappRenderer = prev })
			if err := BuildUpdateProducts(context.Background(), root, false); err != nil {
				t.Fatal(err)
			}
			var want []build
			if enabled {
				want = []build{{root, false, false, true}}
			}
			if !reflect.DeepEqual(builds, want) {
				t.Fatalf("builds %v", builds)
			}
		})
	}
}

func TestAFailedWebappRebuildInUpdateMarksTheServedBundleOutOfDate(t *testing.T) {
	for _, built := range []bool{true, false} {
		t.Run(map[bool]string{false: "unbuilt", true: "built"}[built], func(t *testing.T) {
			tmp := t.TempDir()
			root := filepath.Join(tmp, "checkout")
			sbWrite(t, filepath.Join(root, "apps/desktop/vite.config.ts"), "")
			dist := WebappDistDir(root)
			_ = os.MkdirAll(dist, 0o755)
			if built {
				sbWrite(t, filepath.Join(dist, "index.html"), "<!doctype html>")
			}
			h := filepath.Join(tmp, "default")
			sbWrite(t, filepath.Join(h, "config.yaml"), "webapp:\n  enabled: true\n")
			sbQuietUpdateSteps(t)
			out := sbCapture(t)
			productSteps.dependencyHomes = func(context.Context) ([]string, error) { return []string{h}, nil }
			prev := PrepareWebappRenderer
			PrepareWebappRenderer = func(context.Context, string, bool, bool, bool) (string, error) {
				return "", &WebappBuildError{"Browser-hosted Desktop build failed: npm ci exited 1"}
			}
			t.Cleanup(func() { PrepareWebappRenderer = prev })
			listing := func() []string {
				var all []string
				_ = filepath.Walk(root, func(p string, _ os.FileInfo, _ error) error { all = append(all, p); return nil })
				slices.Sort(all)
				return all
			}
			before := listing()
			err := BuildUpdateProducts(context.Background(), root, false)
			var failure *ProductBuildError
			if !errors.As(err, &failure) || !strings.Contains(err.Error(), "Webapp build") {
				t.Fatalf("err %v", err)
			}
			reason := gateway.BundleMissingReason(dist)
			if !strings.HasPrefix(reason, gateway.WebappNotBuilt) || !strings.Contains(reason, "icebath webapp") {
				t.Fatalf("reason %q", reason)
			}
			if built {
				if raw, _ := os.ReadFile(filepath.Join(dist, "index.html")); string(raw) != "<!doctype html>" {
					t.Fatalf("index %q", raw)
				}
				if !strings.Contains(out.String(), "icebath webapp") {
					t.Fatalf("out %s", out.String())
				}
			} else if !reflect.DeepEqual(listing(), before) {
				t.Fatal("an unbuilt bundle was marked")
			}
		})
	}
}
