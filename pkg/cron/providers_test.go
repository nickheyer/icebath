package cron

import (
	"os"
	"path/filepath"
	"testing"
)

func writeProviderTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIsCronProviderDirReadsTheGuestManifest(t *testing.T) {
	root := t.TempDir()
	guest := "runtime: program\nentry: [scheduler]\n"
	writeProviderTree(t, root, map[string]string{
		"declared/plugin.yaml":  guest + "provides: [cron_scheduler]\n",
		"exclusive/plugin.yaml": guest + "kind: exclusive\n",
		"other/plugin.yaml":     guest + "kind: exclusive\nprovides: [memory_provider]\n",
		"plain/plugin.yaml":     guest,
		"bare/plugin.yaml":      "name: bare\n",
		"source/__init__.py":    "def register(ctx):\n    ctx.register_cron_scheduler(None)\n",
	})
	for name, want := range map[string]bool{"declared": true, "exclusive": true, "other": false, "plain": false, "bare": false, "source": false, "missing": false} {
		if got := isCronProviderDir(filepath.Join(root, name)); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}
