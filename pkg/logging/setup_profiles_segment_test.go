package logging

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetupLoggingUnderUnrelatedProfilesDirSucceeds(t *testing.T) {
	resetLogging(t)
	custom := filepath.Join(t.TempDir(), "srv", "profiles", "buildcache")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := mustSetup(t, Options{Home: custom, Force: true})
	if dir != filepath.Join(custom, "logs") {
		t.Fatalf("log dir %q", dir)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("log dir missing %v", err)
	}
}
