package gateway

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Checkout with the workspace manifests a source build reads
func webappWorkspaceTree(t *testing.T, root string) {
	t.Helper()
	for _, dir := range []string{"apps/desktop", "apps/shared", "web", "ui-tui"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{"package.json": `{"private":true}`, "package-lock.json": "locked\n",
		"apps/desktop/package.json": "{}", "apps/shared/package.json": "{}", "web/package.json": "{}", "ui-tui/package.json": "{}"}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDashboardAndWebappBuildsShareTheWorkspaceLock(t *testing.T) {
	tmp := t.TempDir()
	webappWorkspaceTree(t, tmp)
	var webappLockFree []bool
	swapSeam(t, &doBuildWebUI, func(_ context.Context, _ io.Writer, _ string, _ bool) (bool, error) {
		contender, err := os.OpenFile(filepath.Join(tmp, ".web_ui_build.lock"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o666)
		if err != nil {
			return false, err
		}
		defer contender.Close()
		free, err := pm.LockFD(contender, false, 0)
		if err != nil {
			return false, err
		}
		webappLockFree = append(webappLockFree, free)
		return true, nil
	})
	ok, err := BuildWebUI(context.Background(), &bytes.Buffer{}, filepath.Join(tmp, "web"), false)
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if len(webappLockFree) != 1 || webappLockFree[0] {
		t.Fatalf("webapp lock free %v", webappLockFree)
	}
}
