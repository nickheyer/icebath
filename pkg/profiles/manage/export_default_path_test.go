package manage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Default home at dir and the account home at accountHome for the export path helpers
func exportHomes(t *testing.T, dir, accountHome string) {
	t.Helper()
	t.Setenv(home.EnvHome, dir)
	t.Cleanup(home.PatchUserHome(accountHome))
	profiles.ResetFileCache()
	home.ResetKeyCache()
}

// Fails when the resolved parent of path sits under root
func assertOutside(t *testing.T, path, root string) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, inside := pypath.PathRelativeTo(filepath.Join(parent, filepath.Base(path)), resolvedRoot); inside {
		t.Fatalf("%s lies inside %s", path, root)
	}
}

func gitFileCheckout(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, ".git"), "gitdir: ../git\n")
}

func TestDefaultExportPathIsManagedAndOutsideNamedProfiles(t *testing.T) {
	tmp := t.TempDir()
	def := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	exportHomes(t, def, tmp)
	result, err := GetProfileExportPath("Research-Bot", "20260823-120000")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(def, "profile-exports", "research-bot-20260823-120000.tar.gz"); result != want {
		t.Fatalf("got %s want %s", result, want)
	}
	if st, err := os.Stat(filepath.Dir(result)); err != nil || !st.IsDir() {
		t.Fatalf("export dir %v", err)
	}
	if _, err := os.Stat(filepath.Join(def, "profiles", "research-bot", filepath.Base(result))); !os.IsNotExist(err) {
		t.Fatalf("archive inside the named profile: %v", err)
	}
}

func TestCustomHomeInsideACheckoutUsesASiblingStore(t *testing.T) {
	tmp := t.TempDir()
	checkout := filepath.Join(tmp, "checkout")
	gitFileCheckout(t, checkout)
	t.Chdir(checkout)
	exportHomes(t, checkout, filepath.Join(tmp, "home"))
	result, err := GetProfileExportPath("default", "20260823-120000")
	if err != nil {
		t.Fatal(err)
	}
	assertOutside(t, result, checkout)
}

func TestCheckoutDetectionDoesNotDependOnCwd(t *testing.T) {
	tmp := t.TempDir()
	checkout := filepath.Join(tmp, "checkout")
	gitFileCheckout(t, checkout)
	dataHome := filepath.Join(checkout, "data_home")
	elsewhere := filepath.Join(tmp, "elsewhere")
	for _, d := range []string{dataHome, elsewhere} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(elsewhere)
	exportHomes(t, dataHome, filepath.Join(tmp, "home"))
	result, err := GetProfileExportPath("default", "20260823-120000")
	if err != nil {
		t.Fatal(err)
	}
	assertOutside(t, result, checkout)
}

func TestCwdInUnrelatedCheckoutDoesNotProveSafety(t *testing.T) {
	tmp := t.TempDir()
	checkoutB := filepath.Join(tmp, "checkout-b")
	if err := os.MkdirAll(filepath.Join(checkoutB, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	checkoutA := filepath.Join(tmp, "checkout-a")
	gitFileCheckout(t, checkoutA)
	t.Chdir(checkoutA)
	exportHomes(t, checkoutB, filepath.Join(tmp, "home"))
	result, err := GetProfileExportPath("default", "20260823-120000")
	if err != nil {
		t.Fatal(err)
	}
	assertOutside(t, result, checkoutB)
	assertOutside(t, result, checkoutA)
}

func TestEveryCandidateInsideACheckoutFailsClosed(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	exportHomes(t, checkout, filepath.Join(checkout, "home"))
	saved := tempDir
	t.Cleanup(func() { tempDir = saved })
	tempDir = func() string { return filepath.Join(checkout, "tmp") }
	_, err := GetProfileExportPath("default", "")
	var ve *profiles.ValueError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "No safe automatic export destination") {
		t.Fatalf("got %v", err)
	}
}
