package pm

import (
	"context"
	"testing"
)

func TestOverrideCoreRepoRestoresThePreviousRoot(t *testing.T) {
	before := repoRoot()
	restore := OverrideCoreRepo("/elsewhere/core")
	if got := repoRoot(); got != "/elsewhere/core" {
		t.Fatalf("repoRoot during override = %q", got)
	}
	restore()
	if got := repoRoot(); got != before {
		t.Fatalf("repoRoot after restore = %q, want %q", got, before)
	}
}

func TestOverrideUVToolchainRestoresThePreviousLookup(t *testing.T) {
	restore := OverrideUVToolchain("/bin/uv", "/bin/python")
	uv, python, found, err := uvToolchain(context.Background(), true, true)
	if err != nil || !found || uv != "/bin/uv" || python != "/bin/python" {
		t.Fatalf("overridden toolchain = %q %q %v %v", uv, python, found, err)
	}
	restore()
	if uvToolchain == nil {
		t.Fatal("restore left no toolchain lookup")
	}
}
