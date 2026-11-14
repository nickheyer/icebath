package selfupdate

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
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
