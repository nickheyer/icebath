package desktopplugins

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Package with a plain desktop half
func plainPackage(t *testing.T, folder string) {
	t.Helper()
	writeFile(t, filepath.Join(folder, "plugin.yaml"), "name: media\n")
	writeFile(t, filepath.Join(folder, "desktop", "plugin.js"), "export default {}")
}

// Reports the paths as junctions for the case
func stubJunctions(t *testing.T, paths ...string) {
	saved := isJunction
	t.Cleanup(func() { isJunction = saved })
	isJunction = func(p string) bool { return slices.Contains(paths, p) }
}

func report(t *testing.T, root, appRoot string) *pyjson.Object {
	t.Helper()
	r, err := ReconcileDesktopHalvesReport(root, appRoot)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAFolderTheHostReportsAsAJunctionIsRefusedLikeALink(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, junction := range []string{"desktop", "desktop/assets"} {
		tmp := t.TempDir()
		root, appRoot := filepath.Join(tmp, "hermes"), filepath.Join(tmp, "hermes", "desktop-plugins")
		pkg := filepath.Join(root, "plugins", "media")
		plainPackage(t, pkg)
		if err := os.MkdirAll(filepath.Join(pkg, "desktop", "assets"), 0o755); err != nil {
			t.Fatal(err)
		}
		stubJunctions(t, filepath.Join(pkg, filepath.FromSlash(junction)))
		r := report(t, root, appRoot)
		refused := r.Vals["refused"].([]any)
		if len(refused) != 1 || refused[0].(*pyjson.Object).Vals["name"] != "media" ||
			!strings.Contains(refused[0].(*pyjson.Object).Vals["reason"].(string), "link (desktop") {
			t.Fatalf("%s: refused %v", junction, refused)
		}
		if exists(filepath.Join(appRoot, "media")) {
			t.Fatalf("%s: published", junction)
		}
	}
}

func TestAPackageFolderTheHostReportsAsAJunctionIsALinkedCheckout(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	root, appRoot := filepath.Join(tmp, "hermes"), filepath.Join(tmp, "hermes", "desktop-plugins")
	pkg := filepath.Join(root, "plugins", "media")
	plainPackage(t, pkg)
	stubJunctions(t, pkg)
	r := report(t, root, appRoot)
	if !pyjson.Equal(r.Vals["touched"], []any{filepath.Join(appRoot, "media")}) {
		t.Fatalf("touched %v", r.Vals["touched"])
	}
	marker, _ := pyjson.Loads(readText(t, filepath.Join(appRoot, "media", PackageMarker)))
	if marker.(*pyjson.Object).Vals["linked"] != true {
		t.Fatalf("marker %v", marker)
	}
}

func TestAPlainFolderTheHostReportsAsAJunctionAtTheTargetIsLeftAlone(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	root, appRoot := filepath.Join(tmp, "hermes"), filepath.Join(tmp, "hermes", "desktop-plugins")
	plainPackage(t, filepath.Join(root, "plugins", "media"))
	writeFile(t, filepath.Join(appRoot, "media", "mine.txt"), "mine")
	stubJunctions(t, filepath.Join(appRoot, "media"))
	r := report(t, root, appRoot)
	if !pyjson.Equal(r, pyjson.ObjectOf("touched", []any{}, "refused", []any{})) {
		t.Fatalf("report %v", r.Vals)
	}
	if readText(t, filepath.Join(appRoot, "media", "mine.txt")) != "mine" {
		t.Fatal("user folder changed")
	}
}

func TestADesktopFolderSpelledWithACapitalPublishesOnACaseInsensitiveVolume(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("platforms macos, windows")
	}
	tmp := t.TempDir()
	root, appRoot := filepath.Join(tmp, "hermes"), filepath.Join(tmp, "hermes", "desktop-plugins")
	pkg := filepath.Join(root, "plugins", "media")
	writeFile(t, filepath.Join(pkg, "Desktop", "plugin.js"), "export default {}")
	if st, err := os.Stat(filepath.Join(pkg, "desktop", "plugin.js")); err != nil || !st.Mode().IsRegular() {
		t.Skip("this temporary folder is on a case-sensitive volume")
	}
	r := report(t, root, appRoot)
	if !pyjson.Equal(r, pyjson.ObjectOf("touched", []any{filepath.Join(appRoot, "media")}, "refused", []any{})) {
		t.Fatalf("report %v", r.Vals)
	}
	if readText(t, filepath.Join(appRoot, "media", "plugin.js")) != "export default {}" {
		t.Fatal("not published")
	}
}
