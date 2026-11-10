package provider

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestEnvWriteDenylistWindowsWriterRejectsMixedCaseProtectedName(t *testing.T) {
	for _, key := range []string{"Icebath_Yolo_Mode", "Icebath_Optional_Mcps", "Icebath_Copilot_Acp_Command", "Icebath_Copilot_Acp_Args"} {
		t.Run(key, func(t *testing.T) {
			h := configModuleHome(t, key)
			if err := config.EnsureIcebathHome(t.Context(), h); err != nil {
				t.Fatal(err)
			}
			err := config.SaveEnvValueContext(t.Context(), h, key, "1")
			if err == nil || !strings.Contains(err.Error(), "denylist") {
				t.Fatalf("%s written: %v", key, err)
			}
		})
	}
}
