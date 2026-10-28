package harness

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestStatePath(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, cfg)
	a, err := StatePath("/home/x/code/app")
	if err != nil {
		t.Fatal(err)
	}
	b, err := StatePath("/home/y/code/app")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two projects named app share %s", a)
	}
	if filepath.Dir(a) != filepath.Join(cfg, ProjectsDir) {
		t.Fatalf("state dir %s not under %s", a, filepath.Join(cfg, ProjectsDir))
	}
	if !strings.HasPrefix(filepath.Base(a), "app-") {
		t.Fatalf("state dir %s not named after the project", a)
	}
	again, _ := StatePath("/home/x/code/app")
	if again != a {
		t.Fatalf("state path not stable: %s then %s", a, again)
	}
	odd, err := StatePath("/tmp/my project (v2)")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(filepath.Base(odd), " ()") {
		t.Fatalf("state dir %s keeps characters unfit for a file name", odd)
	}
}
