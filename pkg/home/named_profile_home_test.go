package home

import (
	"os"
	"path/filepath"
	"testing"
)

// Isolates the root home from the layouts under test
func isolatedRoot(t *testing.T) string {
	t.Helper()
	t.Setenv(EnvHome, filepath.Join(t.TempDir(), "root-home"))
	return t.TempDir()
}

func TestLogsUnderNamedProfileResolveToProfileHome(t *testing.T) {
	tmp := isolatedRoot(t)
	if err := os.WriteFile(filepath.Join(tmp, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(tmp, "profiles", "worker")
	if got := NamedProfileHome(filepath.Join(worker, "logs")); got != worker {
		t.Fatalf("logs resolve to %q", got)
	}
	if got := NamedProfileHome(worker); got != worker {
		t.Fatalf("worker resolves to %q", got)
	}
}

func TestDotIcebathLayoutResolvesWithoutMarkers(t *testing.T) {
	worker := filepath.Join(isolatedRoot(t), ".icebath", "profiles", "worker")
	if got := NamedProfileHome(filepath.Join(worker, "logs")); got != worker {
		t.Fatalf("logs resolve to %q", got)
	}
	if got := NamedProfileHome(worker); got != worker {
		t.Fatalf("worker resolves to %q", got)
	}
}

func TestDefaultHomeWithProfilesInPathIsNotNamed(t *testing.T) {
	def := filepath.Join(isolatedRoot(t), "foo", "profiles", "notahome", ".icebath")
	if got := NamedProfileHome(def); got != "" {
		t.Fatalf("default home resolves to %q", got)
	}
	if got := NamedProfileHome(filepath.Join(def, "logs")); got != "" {
		t.Fatalf("default logs resolve to %q", got)
	}
}

func TestUnrelatedProfilesDirIsNotNamed(t *testing.T) {
	custom := filepath.Join(isolatedRoot(t), "srv", "profiles", "buildcache")
	if got := NamedProfileHome(custom); got != "" {
		t.Fatalf("custom home resolves to %q", got)
	}
	if got := NamedProfileHome(filepath.Join(custom, "logs")); got != "" {
		t.Fatalf("custom logs resolve to %q", got)
	}
}

func TestUnrelatedProfilesDirStillMkdirs(t *testing.T) {
	logDir := filepath.Join(isolatedRoot(t), "srv", "profiles", "buildcache", "logs")
	if _, err := Mkdir(logDir); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(logDir); err != nil || !st.IsDir() {
		t.Fatalf("log dir missing: %v", err)
	}
}

func TestTombstoneDirMarksProfilesRoot(t *testing.T) {
	profilesDir := filepath.Join(isolatedRoot(t), "opt", "profiles")
	if err := os.MkdirAll(filepath.Join(profilesDir, ".deleted"), 0o755); err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(profilesDir, "worker")
	if got := NamedProfileHome(filepath.Join(worker, "logs")); got != worker {
		t.Fatalf("logs resolve to %q", got)
	}
}
