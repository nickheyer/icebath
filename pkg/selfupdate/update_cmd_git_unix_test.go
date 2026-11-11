//go:build unix

package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinIsWithheldWhenTheChurnCannotBeCleared(t *testing.T) {
	repo := ugManagedRepo(t, map[string]string{"a.py": "x = 1\n"})
	shim := filepath.Join(t.TempDir(), "git-no-checkout")
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = checkout ] && exit 1; done\nexec git \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ugNormalize(repo, shim)
	if got := ugAutocrlf(repo); got != "true" {
		t.Fatalf("autocrlf = %q", got)
	}
	if !strings.Contains(ugRead(t, filepath.Join(repo, "a.py")), "\r\n") {
		t.Fatal("the churn was cleared by a git that cannot check out")
	}
}
