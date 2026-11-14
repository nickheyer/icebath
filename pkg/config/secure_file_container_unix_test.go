//go:build !windows

package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSecureFileSkipsChmodOnCanonicalContainerSignal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	for _, name := range []string{"ICEBATH_MANAGED", "ICEBATH_CONTAINER", "ICEBATH_SKIP_CHMOD"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	f := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f, 0o640); err != nil {
		t.Fatal(err)
	}
	SecureFile(context.Background(), filepath.Join(tmp, "home"), f)
	st, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}
