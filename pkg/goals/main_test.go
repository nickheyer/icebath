package goals

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

var testSecretEnvSubstrings = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PASSWD", "AUTH", "DSN",
	"WEBHOOK", "CREDS", "BEARER", "APIKEY", "COOKIE"}

// Installs the wiring hook doubles the real auxiliary client path reaches, then runs the suite
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	state.SecretEnvSubstrings = testSecretEnvSubstrings
	state.SecretEnvName = func(name string) bool {
		upper := strings.ToUpper(name)
		for _, s := range testSecretEnvSubstrings {
			if strings.Contains(upper, s) {
				return true
			}
		}
		return false
	}
	provider.GatewayHooks.LegacyJSON = func(string) map[string]any { return nil }
	provider.GatewayHooks.BridgedAllowAllUsers = func() (string, bool) { return "", false }
	provider.GatewayHooks.MergePlatformSections = func(map[string]any, any, map[string]any, []string) map[string]any { return map[string]any{} }
	provider.GatewayHooks.PlatformEnabled = func(map[string]any) (bool, string) { return false, "" }
	provider.GatewayHooks.ServesHome = func(string) bool { return false }
	provider.InstallPluginManagerHooks(provider.PluginManagerHooks{
		CatalogEntry:    func(string) (string, bool) { return "", false },
		Install:         func(context.Context, string, bool) (map[string]any, error) { return nil, nil },
		LoadError:       func(string) string { return "" },
		DependencyHomes: func() ([]string, error) { return nil, nil },
		AuxiliaryTasks:  func() []map[string]any { return nil },
	})
	os.Exit(m.Run())
}
