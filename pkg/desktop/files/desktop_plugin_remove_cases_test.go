package files

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Plain map of an ordered answer, as the renderer reads it
func plain(o *OrderedObject) map[string]any {
	v, _ := jsonParse(jsonStringify(o, "", ""))
	m, _ := v.(map[string]any)
	return m
}

func makeRoot(t *testing.T) (string, string) {
	home := t.TempDir()
	root := filepath.Join(home, "desktop-plugins")
	os.MkdirAll(root, 0o755)
	return home, root
}

func dirNames(t *testing.T, dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestRemoveDesktopPluginDeletesExactlyTheNamedStandaloneFolderUnderTheRootAndLeavesItsNeighbours(t *testing.T) {
	_, root := makeRoot(t)
	writeFile(t, filepath.Join(root, "hello", "plugin.js"), "export default {}")
	writeFile(t, filepath.Join(root, "hello", "assets", "icon.svg"), "<svg/>")
	writeFile(t, filepath.Join(root, "other", "plugin.js"), "export default {}")
	got := plain(RemoveDesktopPlugin(root, "hello"))
	if !reflect.DeepEqual(got, map[string]any{"ok": true, "path": filepath.Join(root, "hello")}) || exists(filepath.Join(root, "hello")) || !exists(filepath.Join(root, "other", "plugin.js")) {
		t.Fatal(got)
	}
}

func TestRemoveDesktopPluginRefusesAnythingThatIsNotASingleFolderNameInsideTheRootDeletingNothing(t *testing.T) {
	home, root := makeRoot(t)
	writeFile(t, filepath.Join(home, "config.yaml"), "model: x")
	writeFile(t, filepath.Join(root, "hello", "plugin.js"), "export default {}")
	for _, name := range []string{"../config.yaml", "..", ".", "", "hello/plugin.js", ".." + string(filepath.Separator) + "config.yaml", filepath.Join(root, "hello")} {
		if plain(RemoveDesktopPlugin(root, name))["ok"] != false {
			t.Fatal(name)
		}
	}
	if !exists(filepath.Join(home, "config.yaml")) || !exists(filepath.Join(root, "hello", "plugin.js")) {
		t.Fatal("deleted")
	}
}

func TestRemoveDesktopPluginRemovesAsTheMatchingFolder(t *testing.T) {
	for _, tc := range [][4]string{
		{"a BOM-prefixed name", "\ufeffgizmo", "\ufeffgizmo", "gizmo"},
		{"an existing folder ending in NEL", "gizmo\u0085", "gizmo\u0085", "gizmo"},
		{"an existing folder with outer spaces", " gizmo ", " gizmo ", "gizmo"},
		{"a name wrapped in NEL and U+001C with no such folder", "\u0085gizmo\x1c", "gizmo", "\ufeffgizmo"},
	} {
		t.Run(tc[0], func(t *testing.T) {
			_, root := makeRoot(t)
			writeFile(t, filepath.Join(root, tc[2], "plugin.js"), "export default {}")
			writeFile(t, filepath.Join(root, tc[3], "plugin.js"), "export default {}")
			got := plain(RemoveDesktopPlugin(root, tc[1]))
			if !reflect.DeepEqual(got, map[string]any{"ok": true, "path": filepath.Join(root, tc[2])}) || !reflect.DeepEqual(dirNames(t, root), []string{tc[3]}) {
				t.Fatal(got, dirNames(t, root))
			}
		})
	}
}

func TestRemoveDesktopPluginRefusesAUnifiedPackageHalfMarkerPresentAndAMissingFolder(t *testing.T) {
	_, root := makeRoot(t)
	writeFile(t, filepath.Join(root, "uni", "plugin.js"), "export default {}")
	writeFile(t, filepath.Join(root, "uni", PackageMarker), `{"package":"uni","source":"/x"}`)
	if plain(RemoveDesktopPlugin(root, "uni"))["ok"] != false || !exists(filepath.Join(root, "uni", "plugin.js")) {
		t.Fatal("half removed")
	}
	if plain(RemoveDesktopPlugin(root, "ghost"))["ok"] != false {
		t.Fatal("ghost removed")
	}
}

func TestRemoveDesktopPluginRemovesASymlinkedPluginFolderAsTheLinkNeverFollowingIt(t *testing.T) {
	home, root := makeRoot(t)
	real := filepath.Join(home, "elsewhere")
	writeFile(t, filepath.Join(real, "plugin.js"), "export default {}")
	mustSymlink(t, real, filepath.Join(root, "linked"))
	if plain(RemoveDesktopPlugin(root, "linked"))["ok"] != true || exists(filepath.Join(root, "linked")) || !exists(filepath.Join(real, "plugin.js")) {
		t.Fatal("link")
	}
}
