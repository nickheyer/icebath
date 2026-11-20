package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Temp home for a config case that runs the registered provider credential lifecycle
func configModuleHome(t *testing.T, restore ...string) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(config.EnvManagedDir, filepath.Join(t.TempDir(), "no-managed"))
	config.InvalidateManagedCache()
	config.ResetCache()
	config.InvalidateEnvCache()
	t.Cleanup(func() { config.ResetCache(); config.InvalidateEnvCache() })
	for _, n := range restore {
		prev, had := os.LookupEnv(n)
		os.Unsetenv(n)
		t.Cleanup(func() {
			if had {
				os.Setenv(n, prev)
			} else {
				os.Unsetenv(n)
			}
		})
	}
	return h
}

func TestSaveEnvValueSecureSecureSaveReturnsMetadataOnly(t *testing.T) {
	h := configModuleHome(t, "GITHUB_TOKEN")
	result, err := config.SaveEnvValueSecure(t.Context(), h, "GITHUB_TOKEN", "ghp_test_secret")
	if err != nil {
		t.Fatal(err)
	}
	if result != (config.SecureSaveResult{Success: true, StoredAs: "GITHUB_TOKEN", Validated: false}) {
		t.Fatalf("result %+v", result)
	}
	if strings.Contains(strings.ToLower(fmt.Sprintf("%+v", result)), "secret") {
		t.Fatalf("result leaks the value: %+v", result)
	}
}
