package home

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

func TestDisplayHermesHomePosix(t *testing.T) {
	user := pypath.Expanduser("~")
	t.Run("nested_home_renders_forward_slashes", func(t *testing.T) {
		out := Display(filepath.Join(user, "AppData", "Local", "icebath"))
		if out != "~/AppData/Local/icebath" || strings.Contains(out, `\`) {
			t.Fatal(out)
		}
	})
	t.Run("default_home_unchanged", func(t *testing.T) {
		if out := Display(filepath.Join(user, ".icebath")); out != "~/.icebath" {
			t.Fatal(out)
		}
	})
	t.Run("outside_home_falls_back_to_absolute", func(t *testing.T) {
		outside := "/opt/icebath-custom"
		if runtime.GOOS == "windows" {
			outside = `C:\opt\icebath-custom`
		}
		if out := Display(outside); out != outside {
			t.Fatal(out)
		}
	})
}

func TestNormalizeScopeKeepsTheGlobalLayer(t *testing.T) {
	dir := t.TempDir()
	if NormalizeScope("") != "" || NormalizeScope(dir) != Key(dir) || NormalizeScope(" ") == "" {
		t.Fatal("scope keys")
	}
}
