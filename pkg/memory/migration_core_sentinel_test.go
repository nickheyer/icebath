package memory

import (
	"context"
	"testing"
)

func TestMigrationNeverInstallsACoreSentinel(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeMigrationHome(t, h, "memory:\n  provider: builtin\n")
	withMigrationSteps(t, func(name string) (string, bool) {
		t.Errorf("catalog must not be consulted, asked for %s", name)
		return name, true
	})
	var said []string
	install := func(name string) (map[string]any, error) {
		t.Errorf("must not install %s", name)
		return nil, nil
	}
	got, err := MigrateMemoryHome(context.Background(), h, install, func(s string) { said = append(said, s) })
	if err != nil || got != "" {
		t.Fatalf("migrated %q %v", got, err)
	}
	if len(said) != 0 {
		t.Fatalf("said = %q", said)
	}
}
