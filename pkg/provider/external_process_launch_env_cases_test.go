package provider

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestEnvWriterRefusesAProviderExecutableOverride(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	registerPluginForTest(t, Profile{Name: "vendor-acp", AuthType: AuthTypeExternalProcess, ProcessCommand: "vendor",
		ProcessCommandEnvVars: []string{"VENDOR_ACP_COMMAND", "VENDOR_CLI_PATH"}, ProcessArgsEnvVar: "VENDOR_ACP_ARGS"})
	registerPluginForTest(t, Profile{Name: "vendor-api", AuthType: AuthTypeAPIKey, KeyEnv: []string{"VENDOR_API_KEY"}})
	for _, name := range []string{"VENDOR_ACP_COMMAND", "VENDOR_CLI_PATH", "VENDOR_ACP_ARGS"} {
		t.Run(name, func(t *testing.T) {
			err := config.ValidateEnvNameForWrite(name)
			if err == nil || !strings.Contains(err.Error(), "writer denylist") {
				t.Fatalf("%s: %v", name, err)
			}
			if err := config.ValidateEnvNameForWrite("VENDOR_API_KEY"); err != nil {
				t.Fatalf("VENDOR_API_KEY: %v", err)
			}
		})
	}
}
