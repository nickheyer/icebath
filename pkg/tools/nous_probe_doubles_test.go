package tools_test

import (
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Absent local stack probes for the Nous features, pkg/browser and pkg/media install the real ones in the binary
func init() {
	if provider.BrowserHarnessCLI == nil {
		provider.BrowserHarnessCLI = func(*tools.Env) []string { return nil }
	}
	if provider.ChromiumExecutable == nil {
		provider.ChromiumExecutable = func(*tools.Env, bool) string { return "" }
	}
	if provider.FasterWhisperImportable == nil {
		provider.FasterWhisperImportable = func() bool { return false }
	}
	if provider.ManagedImageRoute == nil {
		provider.ManagedImageRoute = func(any, any) string { return "" }
	}
}
