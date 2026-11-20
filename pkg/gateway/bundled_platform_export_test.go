package gateway

import "testing"

// Clears the bundled override and the scan cache for a test outside the package
func UseCompiledTreeForTest(t *testing.T) { useCompiledTree(t) }

// Bundled platform directory names and manifest aliases for a test outside the package
func ScanBundledPluginPlatformsForTest() (map[string]bool, map[string]string) {
	return scanBundledPluginPlatforms()
}

// Env overrides and enable credentials a bundled platform declares, for a test outside the package
func BundledEnvRowsForTest(name string) ([]EnvStep, []string, bool) { return bundledEnvRows(name) }
