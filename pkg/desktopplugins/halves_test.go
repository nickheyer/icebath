package desktopplugins

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func posixOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func isSymlink(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func reconcileTouched(t *testing.T, root, appRoot string) []string {
	t.Helper()
	touched, err := ReconcileDesktopHalves(root, appRoot)
	if err != nil {
		t.Fatal(err)
	}
	return touched
}

// Package with a desktop half holding a link to outside
func makePackage(t *testing.T, folder, outside, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(folder, "desktop", "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(folder, "plugin.yaml"), "name: media\n")
	writeFile(t, filepath.Join(folder, "desktop", "plugin.js"), "export default {}")
	symlink(t, outside, filepath.Join(folder, "desktop", link))
}

func TestAnInstalledPackageWhoseDesktopFolderHoldsALinkIsNotPublished(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	posixOnly(t)
	for _, link := range []string{"leak.txt", "__pycache__/leak.txt"} {
		tmp := t.TempDir()
		root, appRoot := filepath.Join(tmp, "hermes"), filepath.Join(tmp, "hermes", "desktop-plugins")
		secret := filepath.Join(tmp, "secret.txt")
		writeFile(t, secret, "host secret")
		makePackage(t, filepath.Join(root, "plugins", "media"), secret, link)
		report, err := ReconcileDesktopHalvesReport(root, appRoot)
		if err != nil {
			t.Fatal(err)
		}
		if touched := report.Vals["touched"].([]any); len(touched) != 0 {
			t.Fatalf("%s: touched %v", link, touched)
		}
		refused := report.Vals["refused"].([]any)
		if len(refused) != 1 || refused[0].(*pyjson.Object).Vals["name"] != "media" {
			t.Fatalf("%s: refused %v", link, refused)
		}
		if exists(filepath.Join(appRoot, "media")) {
			t.Fatalf("%s: published", link)
		}
	}
}

func TestALinkedDevCheckoutStillPublishesItsDesktopFolder(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	posixOnly(t)
	tmp := t.TempDir()
	root, appRoot := filepath.Join(tmp, "hermes"), filepath.Join(tmp, "hermes", "desktop-plugins")
	asset := filepath.Join(tmp, "asset.txt")
	writeFile(t, asset, "checkout file")
	checkout := filepath.Join(tmp, "src", "media")
	makePackage(t, checkout, asset, "leak.txt")
	symlink(t, checkout, filepath.Join(root, "plugins", "media"))
	if got := reconcileTouched(t, root, appRoot); !slices.Equal(got, []string{filepath.Join(appRoot, "media")}) {
		t.Fatalf("touched %v", got)
	}
	if readText(t, filepath.Join(appRoot, "media", "plugin.js")) != "export default {}" {
		t.Fatal("plugin.js not published")
	}
}

// Root layout checkout linked into plugins
func linkedCheckout(t *testing.T) (root, appRoot, checkout, link string) {
	t.Helper()
	tmp := t.TempDir()
	root = filepath.Join(tmp, "hermes")
	checkout = filepath.Join(tmp, "src", "radar")
	writeFile(t, filepath.Join(checkout, "plugin.yaml"), "name: radar\n")
	writeFile(t, filepath.Join(checkout, "plugin.js"), "root entry")
	link = filepath.Join(root, "plugins", "radar")
	symlink(t, checkout, link)
	return root, filepath.Join(root, "desktop-plugins"), checkout, link
}

func TestALinkedRootLayoutCheckoutPublishesACopyAndLeavesWithItsLink(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	posixOnly(t)
	root, appRoot, checkout, link := linkedCheckout(t)
	target := filepath.Join(appRoot, "radar")
	if got := reconcileTouched(t, root, appRoot); !slices.Equal(got, []string{target}) {
		t.Fatalf("touched %v", got)
	}
	if isSymlink(target) || readText(t, filepath.Join(target, "plugin.js")) != "root entry" {
		t.Fatal("no real copy")
	}
	marker, _ := pyjson.Loads(readText(t, filepath.Join(target, PackageMarker)))
	m := marker.(*pyjson.Object)
	if m.Vals["package"] != "radar" || m.Vals["source"] != link || m.Vals["linked"] != true {
		t.Fatalf("marker %v", m)
	}
	if exists(filepath.Join(checkout, PackageMarker)) {
		t.Fatal("marker written into the checkout")
	}
	if got := reconcileTouched(t, root, appRoot); len(got) != 0 {
		t.Fatalf("second pass touched %v", got)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if got := reconcileTouched(t, root, appRoot); !slices.Equal(got, []string{target}) {
		t.Fatalf("prune touched %v", got)
	}
	if exists(target) {
		t.Fatal("copy not pruned")
	}
	entries, _ := os.ReadDir(checkout)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if !slices.Equal(names, []string{"plugin.js", "plugin.yaml"}) {
		t.Fatalf("checkout %v", names)
	}
}

func TestACopyThatKeptTheCheckoutLinkIsReplacedThenPrunedWithoutTouchingTheCheckout(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	posixOnly(t)
	root, appRoot, checkout, link := linkedCheckout(t)
	target := filepath.Join(appRoot, "radar")
	ghost, _ := pyjson.Dumps(pyjson.ObjectOf("linked", true, "package", "radar", "source", link, "sourceMtimeMs", 9e15))
	writeFile(t, filepath.Join(checkout, PackageMarker), ghost)
	symlink(t, checkout, target)
	if got := reconcileTouched(t, root, appRoot); !slices.Equal(got, []string{target}) {
		t.Fatalf("touched %v", got)
	}
	if isSymlink(target) || readText(t, filepath.Join(target, "plugin.js")) != "root entry" {
		t.Fatal("link not replaced by a copy")
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	symlink(t, checkout, target)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if got := reconcileTouched(t, root, appRoot); !slices.Equal(got, []string{target}) {
		t.Fatalf("prune touched %v", got)
	}
	if isSymlink(target) || exists(target) {
		t.Fatal("leftover link not pruned")
	}
	if readText(t, filepath.Join(checkout, "plugin.js")) != "root entry" {
		t.Fatal("checkout touched")
	}
}

func TestAPluginTheUserLinkedIntoTheAppRootIsNeverStamped(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	posixOnly(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "hermes")
	own := filepath.Join(tmp, "src", "pkg")
	writeFile(t, filepath.Join(own, "plugin.js"), "same")
	writeFile(t, filepath.Join(root, "plugins", "pkg", "desktop", "plugin.js"), "same")
	appRoot := filepath.Join(root, "desktop-plugins")
	symlink(t, own, filepath.Join(appRoot, "pkg"))
	if got := reconcileTouched(t, root, appRoot); len(got) != 0 {
		t.Fatalf("touched %v", got)
	}
	if exists(filepath.Join(own, PackageMarker)) || !isSymlink(filepath.Join(appRoot, "pkg")) {
		t.Fatal("user link stamped or replaced")
	}
}
