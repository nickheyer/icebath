package plugins

import (
	"strings"
	"testing"
)

// A desktop plugin may import the SDK under the icebath name or the hermes name, any other dynamic import fails
func TestDesktopPluginDynamicImportAcceptsBothSdkNames(t *testing.T) {
	detail := vtFailed(vtDesktopReport(vtDesktopPlugin(t, "const a = await import('@icebath/plugin-sdk')\nconst b = await import(\"@hermes/plugin-sdk\")\nconst c = await import('./other.js')\n")))["desktop surface"]
	if strings.Contains(detail, "plugin.js:1)") || strings.Contains(detail, "plugin.js:2)") || !strings.Contains(detail, "dynamic import outside the SDK (desktop/plugin.js:3)") {
		t.Fatalf("%q", detail)
	}
}
