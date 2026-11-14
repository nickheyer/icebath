package webbundle

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestDistPrefersTheCheckoutBuildThenUnpacksTheEmbeddedBundleOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	saved := Embedded
	t.Cleanup(func() { Embedded = saved })
	Embedded = fstest.MapFS{
		"index.html":        {Data: []byte("<html></html>")},
		"assets/app.js":     {Data: []byte("console.log(1)")},
		"hermes-build.json": {Data: []byte(`{"outputHash":"0123456789abcdef0123"}`)},
	}
	root := t.TempDir()
	dir := Dist(root)
	if dir != filepath.Join(home, "cache", "web_dist", "0123456789abcdef") {
		t.Fatalf("dist = %s", dir)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "assets", "app.js")); err != nil || string(raw) != "console.log(1)" {
		t.Fatalf("asset = %q %v", raw, err)
	}
	if again := Dist(root); again != dir {
		t.Fatalf("second dist = %s", again)
	}
	checkout := filepath.Join(root, "cmd", "icebath", "web_dist")
	os.MkdirAll(checkout, 0o755)
	os.WriteFile(filepath.Join(checkout, "index.html"), []byte("<html>checkout</html>"), 0o644)
	if got := Dist(root); got != checkout {
		t.Fatalf("checkout build not preferred: %s", got)
	}
}

func TestDistWithoutAnEmbeddedBundleIsTheCheckoutPath(t *testing.T) {
	saved := Embedded
	t.Cleanup(func() { Embedded = saved })
	Embedded = nil
	root := t.TempDir()
	if got := Dist(root); got != filepath.Join(root, "cmd", "icebath", "web_dist") {
		t.Fatalf("dist = %s", got)
	}
}
