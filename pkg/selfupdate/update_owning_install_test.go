package selfupdate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
)

func TestPmGenerationVenvIsOwnedByTheCheckoutItsInstallKeyNames(t *testing.T) {
	tmp := t.TempDir()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	checkout := filepath.Join(tmp, "icebath-agent")
	write(filepath.Join(checkout, owningInstallBinary()), "")
	state := filepath.Join(tmp, "installs", pm.InstallKey(checkout))
	generation := filepath.Join(state, "environments", "0123abcd")
	if err := os.MkdirAll(filepath.Join(generation, "venv"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(generation, "workspace")
	write(filepath.Join(workspace, owningInstallBinary()), "")
	record := filepath.Join(state, "inputs", ".project-root")
	write(record, checkout)
	saved := owningVenvPrefix
	owningVenvPrefix = func() (string, bool, error) { return filepath.Join(generation, "venv"), true, nil }
	t.Cleanup(func() { owningVenvPrefix = saved })
	t.Setenv("PYTHONPATH", "")
	os.Unsetenv("PYTHONPATH")

	resolved, err := pypath.Resolve(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if owner, err := owningInstallRoot(workspace); err != nil || owner != resolved {
		t.Fatalf("owner = %q, %v, want %q", owner, err, resolved)
	}

	write(record, filepath.Join(tmp, "elsewhere"))
	if owner, err := owningInstallRoot(workspace); err != nil || owner != "" {
		t.Fatalf("owner = %q, %v, want none", owner, err)
	}
}
