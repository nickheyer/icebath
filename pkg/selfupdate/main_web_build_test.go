package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Points the dashboard rebuild at this package's source build steps the way the command wiring does
func wireWebUISourceBuild(t *testing.T) {
	t.Helper()
	prev := gateway.WebUISourceBuild
	gateway.WebUISourceBuild.ProductCurrent = SourceProductCurrent
	gateway.WebUISourceBuild.Env = func(ctx context.Context) (map[string]string, error) { return SourceBuildEnv(ctx, nil, false, true) }
	gateway.WebUISourceBuild.PrepareLaunch = PrepareLaunchDependencies
	gateway.WebUISourceBuild.BuildWeb = func(ctx context.Context, root string, env map[string]string) error {
		return BuildSourceWeb(ctx, root, env, "")
	}
	gateway.WebUISourceBuild.ExceptionName = ExceptionName
	t.Cleanup(func() { gateway.WebUISourceBuild = prev })
}

// Runs the dashboard rebuild and fails on an uncaught error
func buildWebUIForTest(t *testing.T, webDir string, fatal bool) bool {
	t.Helper()
	var out bytes.Buffer
	ok, err := gateway.BuildWebUI(context.Background(), &out, webDir, fatal)
	if err != nil {
		t.Fatalf("BuildWebUI error %v, output %s", err, out.String())
	}
	return ok
}

// Whether the dashboard bundle of a frontend dir is stale
func webUIBuildNeededForTest(webDir string) bool {
	root := filepath.Dir(webDir)
	return !SourceProductCurrent(context.Background(), root, "web", filepath.Join(root, "cmd", "icebath", "web_dist"))
}

func TestWebBuildPreparesOnceAndSkipsACurrentProduct(t *testing.T) {
	sbNeedsPosix(t)
	root, acquired := sbSourceProducts(t)
	wireWebUISourceBuild(t)
	if !buildWebUIForTest(t, filepath.Join(root, "web"), true) {
		t.Fatal("build failed")
	}
	assertStrings(t, sbSteps(t, root), []string{"deps", "web"})
	assertStrings(t, *acquired, []string{"npm"})
	if webUIBuildNeededForTest(filepath.Join(root, "web")) {
		t.Fatal("product still stale")
	}
	if !buildWebUIForTest(t, filepath.Join(root, "web"), true) {
		t.Fatal("second build failed")
	}
	assertStrings(t, *acquired, []string{"npm"})
	if len(sbEvents(t, root)) != 2 {
		t.Fatalf("events %v", sbEvents(t, root))
	}
}

func TestWebFailureIsNotSuccessEvenWithAnOldDist(t *testing.T) {
	sbNeedsPosix(t)
	for _, fatal := range []bool{false, true} {
		root, acquired := sbSourceProducts(t)
		wireWebUISourceBuild(t)
		dist := filepath.Join(root, "cmd/icebath/web_dist/index.html")
		sbWrite(t, dist, "old product")
		sbWrite(t, filepath.Join(root, "fail-web"), "")
		if buildWebUIForTest(t, filepath.Join(root, "web"), fatal) {
			t.Fatalf("fatal %v failed build reported success", fatal)
		}
		assertStrings(t, *acquired, []string{"npm"})
		assertStrings(t, sbSteps(t, root), []string{"deps", "web"})
		if raw, _ := os.ReadFile(dist); string(raw) != "old product" {
			t.Fatalf("dist %q", raw)
		}
		if _, err := os.Stat(filepath.Join(root, "cmd/icebath/web_dist/hermes-build.json")); err == nil {
			t.Fatal("receipt written for a failed build")
		}
	}
}

func TestFailedPreparationNeverRunsWebCompilation(t *testing.T) {
	sbNeedsPosix(t)
	root, acquired := sbSourceProducts(t)
	wireWebUISourceBuild(t)
	sbWrite(t, filepath.Join(root, "package-lock.json"), "not json")
	if buildWebUIForTest(t, filepath.Join(root, "web"), true) {
		t.Fatal("failed preparation reported success")
	}
	assertStrings(t, *acquired, []string{"npm"})
	if len(sbEvents(t, root)) != 0 {
		t.Fatalf("events %v", sbEvents(t, root))
	}
	if _, err := os.Stat(filepath.Join(root, "cmd/icebath/web_dist/hermes-build.json")); err == nil {
		t.Fatal("receipt written")
	}
}

func TestWebRebuildReusesTheExistingDesktopUnion(t *testing.T) {
	sbNeedsLinux(t)
	root, acquired := sbSourceProducts(t)
	wireWebUISourceBuild(t)
	sbQuietUpdateSteps(t)
	if err := BuildUpdateProducts(context.Background(), root, true); err != nil {
		t.Fatal(err)
	}
	before := sbEvents(t, root)
	sbWrite(t, filepath.Join(root, "web/changed.ts"), "changed web source")
	if !buildWebUIForTest(t, filepath.Join(root, "web"), true) {
		t.Fatal("rebuild failed")
	}
	after := sbEvents(t, root)
	if len(after) != len(before)+1 || after[len(after)-1]["step"] != "web" || len(after[len(after)-1]) != 1 {
		t.Fatalf("events before %v after %v", before, after)
	}
	assertStrings(t, *acquired, []string{"npm", "npm"})
	if _, err := os.Stat(filepath.Join(root, "node_modules/apps-desktop")); err != nil {
		t.Fatal(err)
	}
}

func TestContendedBuildWaitsAndRechecksWinner(t *testing.T) {
	sbNeedsLinux(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("requires node")
	}
	for _, existing := range []bool{false, true} {
		tmp := t.TempDir()
		t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "_icebath_home"))
		t.Cleanup(SwapSourceToolsForTests(func(context.Context, string, pm.EnsureOptions) (*pm.Runner, error) {
			t.Error("duplicate preparation")
			return nil, os.ErrInvalid
		}, func(context.Context) (string, bool) { return node, true }))
		wireWebUISourceBuild(t)
		gateway.WebUISourceBuild.Env = func(context.Context) (map[string]string, error) {
			t.Error("duplicate preparation")
			return nil, os.ErrInvalid
		}
		for _, name := range []string{"freshness.mjs", "frontend-common.mjs"} {
			raw, err := os.ReadFile(filepath.Join(sbHermesScripts, name))
			if err != nil {
				t.Fatal(err)
			}
			sbWrite(t, filepath.Join(tmp, "scripts", "build", name), string(raw))
		}
		web := filepath.Join(tmp, "web")
		sbWrite(t, filepath.Join(web, "package.json"), "")
		dist := filepath.Join(tmp, "cmd", "icebath", "web_dist")
		if existing {
			sbWrite(t, filepath.Join(dist, "index.html"), "")
		}
		holder, err := os.OpenFile(filepath.Join(tmp, ".web_ui_build.lock"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o666)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := pm.LockFD(holder, false, 0); err != nil || !ok {
			t.Fatalf("holder lock %v %v", ok, err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer holder.Close()
			time.Sleep(500 * time.Millisecond)
			_ = os.MkdirAll(dist, 0o755)
			_ = os.WriteFile(filepath.Join(dist, "index.html"), []byte("winner"), 0o644)
			script, _ := json.Marshal("file://" + filepath.Join(tmp, "scripts/build/freshness.mjs"))
			_ = exec.Command(node, "--input-type=module", "-e",
				"import {recordProduct, buildInputs} from "+string(script)+";"+
					"const [source, product, out] = process.argv.slice(1);"+
					"recordProduct({source, product, out, inputs: buildInputs(source, product)});",
				tmp, "web", dist).Run()
		}()
		if !buildWebUIForTest(t, web, true) {
			t.Fatal("waiter build failed")
		}
		<-done
		if raw, _ := os.ReadFile(filepath.Join(dist, "index.html")); string(raw) != "winner" {
			t.Fatalf("existing %v index %q", existing, raw)
		}
	}
}

func TestLockOpenFailureDoesNotStartAnUnprotectedBuild(t *testing.T) {
	sbNeedsPosix(t)
	root, acquired := sbSourceProducts(t)
	wireWebUISourceBuild(t)
	if err := os.Mkdir(filepath.Join(root, ".web_ui_build.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	if buildWebUIForTest(t, filepath.Join(root, "web"), true) {
		t.Fatal("unprotected build ran")
	}
	if len(*acquired) != 0 || len(sbEvents(t, root)) != 0 {
		t.Fatalf("acquired %v events %v", *acquired, sbEvents(t, root))
	}
}

// Fails unless two string slices are equal
func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %q want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}
