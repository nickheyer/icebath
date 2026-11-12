package plugins

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Writes plugin.yaml and the named desktop entry under a fresh tree
func desktopSurfaceTree(t *testing.T, tree, entry string) {
	t.Helper()
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "plugin.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if entry == "" {
		return
	}
	path := filepath.Join(tree, filepath.FromSlash(entry))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("export default {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func desktopSurfaceDelta(t *testing.T, old, newTree string) map[string][]string {
	t.Helper()
	manifest := map[string]any{"name": "demo"}
	before, err := pluginSurface(manifest, old)
	if err != nil {
		t.Fatal(err)
	}
	after, err := pluginSurface(manifest, newTree)
	if err != nil {
		t.Fatal(err)
	}
	return surfaceDelta(before, after)
}

func TestNewDesktopHalfWidensTheSurfaceInEitherLayout(t *testing.T) {
	for _, entry := range []string{"plugin.js", "desktop/plugin.js"} {
		t.Run(entry, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			txOAuthSurface(t)
			tmp := t.TempDir()
			old, newTree := filepath.Join(tmp, "old"), filepath.Join(tmp, "new")
			desktopSurfaceTree(t, old, "")
			desktopSurfaceTree(t, newTree, entry)
			delta := desktopSurfaceDelta(t, old, newTree)
			if !reflect.DeepEqual(delta["desktop"], []string{entry}) {
				t.Fatalf("delta[desktop] = %v, want [%s]", delta["desktop"], entry)
			}
		})
	}
}

func TestMovingAnExistingDesktopHalfBetweenLayoutsWidensNothing(t *testing.T) {
	for _, pair := range [][2]string{{"plugin.js", "desktop/plugin.js"}, {"desktop/plugin.js", "plugin.js"}} {
		t.Run(pair[0]+"->"+pair[1], func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			txOAuthSurface(t)
			tmp := t.TempDir()
			old, newTree := filepath.Join(tmp, "old"), filepath.Join(tmp, "new")
			desktopSurfaceTree(t, old, pair[0])
			desktopSurfaceTree(t, newTree, pair[1])
			if delta := desktopSurfaceDelta(t, old, newTree); len(delta) != 0 {
				t.Fatalf("delta = %v, want {}", delta)
			}
		})
	}
}
