package teams

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	os.Exit(m.Run())
}

func TestTeamsInteractiveSetupInteractiveSetupPersistsCredentials(t *testing.T) {
	icebathHome := filepath.Join(t.TempDir(), "icebath")
	t.Setenv("ICEBATH_HOME", icebathHome)
	for _, key := range []string{"TEAMS_CLIENT_ID", "TEAMS_CLIENT_SECRET", "TEAMS_TENANT_ID", "TEAMS_ALLOWED_USERS", "TEAMS_ALLOW_ALL_USERS"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	var out bytes.Buffer
	t.Cleanup(provider.WithSetupOutput(&out))
	saved := gateway.WizardCLI
	t.Cleanup(func() { gateway.WizardCLI = saved })
	answers := []string{"client-id", "client-secret", "tenant-id", "aad-1, aad-2"}
	gateway.WizardCLI.Prompt = func(string, string, bool) string {
		v := answers[0]
		answers = answers[1:]
		return v
	}
	gateway.WizardCLI.PromptYesNo = func(context.Context, string, bool) bool { return true }
	gateway.WizardCLI.PrintInfo, gateway.WizardCLI.PrintSuccess, gateway.WizardCLI.PrintWarning = func(string) {}, func(string) {}, func(string) {}
	if err := InteractiveSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(icebathHome, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "TEAMS_CLIENT_ID=client-id") || !strings.Contains(text, "TEAMS_TENANT_ID=tenant-id") {
		t.Fatalf("env file %q", text)
	}
}
