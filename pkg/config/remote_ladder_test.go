package config

import (
	"context"
	"os"
	"testing"
)

const ladderLegacySoul = "# Beta identity\n\n## Messaging other agents\nKEEP THIS PROFILE SECTION\n"

// A config read on the bootstrapping call itself returns at once instead of waiting on the bootstrap
func TestSameThreadReentryDuringTheBootstrapDoesNotDeadlock(t *testing.T) {
	t.Setenv(BackendEnv, "")
	os.Unsetenv(BackendEnv)
	bootstrapped.Store(false)
	var seen []string
	RegisterConfigBootstrapEnv(func(ctx context.Context, _ func(string) []string, _, _ string) error {
		backend, err := GetConfigBackend(ctx)
		if err != nil {
			return err
		}
		seen = append(seen, backend.Name())
		return nil
	})
	t.Cleanup(func() { RegisterConfigBootstrapEnv(nil) })
	backend, err := GetConfigBackend(context.Background())
	if err != nil || backend.Name() != "file" || len(seen) != 1 || seen[0] != "file" || !bootstrapped.Load() {
		t.Fatalf("backend %v %v seen %v", backend, err, seen)
	}
}
