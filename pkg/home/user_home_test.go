package home

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

func TestUserHomeServesPathlibHomeUntilPatched(t *testing.T) {
	t.Setenv(EnvSuffix, "")
	real, realErr := pypath.PathHome()
	if got, err := UserHome(); got != real || (err == nil) != (realErr == nil) {
		t.Fatalf("UserHome() = %q, %v want %q, %v", got, err, real, realErr)
	}
	envHome := os.Getenv("HOME")
	outer, inner := t.TempDir(), t.TempDir()
	restoreOuter := PatchUserHome(outer)
	restoreInner := PatchUserHome(inner)
	if got, err := UserHome(); err != nil || got != inner {
		t.Fatalf("patched UserHome() = %q, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		if got := PlatformDefault(); got != filepath.Join(inner, ".icebath") {
			t.Fatalf("PlatformDefault() = %q", got)
		}
	}
	if got := Display(filepath.Join(inner, "AppData", "Local", "icebath")); got != "~/AppData/Local/icebath" {
		t.Fatalf("Display() = %q", got)
	}
	if os.Getenv("HOME") != envHome {
		t.Fatal("patching the user home changed HOME")
	}
	restoreInner()
	if got, _ := UserHome(); got != outer {
		t.Fatalf("inner restore left %q", got)
	}
	restoreOuter()
	if got, _ := UserHome(); got != real {
		t.Fatalf("outer restore left %q", got)
	}
}

func TestAccountDefaultComparesTheResolvedUserHome(t *testing.T) {
	t.Setenv(EnvSuffix, "")
	t.Setenv("LOCALAPPDATA", "")
	tmp := t.TempDir()
	fallback := filepath.Join(tmp, "account")
	if err := os.MkdirAll(fallback, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_REAL_HOME", fallback)
	account := fallback
	if runtime.GOOS != "windows" {
		if u, err := user.LookupId(strconv.Itoa(os.Getuid())); err == nil {
			account = pypath.PathStr(u.HomeDir)
		}
	}
	accountDefault, err := platformDefault(os.Getenv, func() (string, error) { return account, nil })
	if err != nil {
		t.Fatal(err)
	}
	restore := PatchUserHome(filepath.Join(tmp, "elsewhere"))
	if got := AccountDefault(); got != accountDefault {
		t.Fatalf("foreign user home AccountDefault() = %q want %q", got, accountDefault)
	}
	restore()
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(account, link); err != nil {
		t.Fatal(err)
	}
	defer PatchUserHome(link)()
	if got := AccountDefault(); got != PlatformDefault() || got != platformBase(link) {
		t.Fatalf("same resolved user home AccountDefault() = %q want %q", got, PlatformDefault())
	}
}

// Platform default directory hermes derives from a user home with no suffix and no LOCALAPPDATA
func platformBase(h string) string {
	if runtime.GOOS == "windows" {
		return pypath.PathJoin(h, "AppData", "Local", "icebath")
	}
	return pypath.PathJoin(h, ".icebath")
}
