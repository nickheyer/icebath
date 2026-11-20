package mattermost

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

// Records of a scripted wizard run, the env saves and removals its fakes saw
type setupIO struct {
	saved    map[string]string
	writes   [][2]string
	removed  []string
	existing map[string]string
	infos    []string
	errors   []string
}

// Replaces the cli_output and config names the wizard calls with fakes over existing values and scripted prompts
func patchSetupIO(t *testing.T, prompts []string, yesNo []bool, existing map[string]string) *setupIO {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var out bytes.Buffer
	t.Cleanup(provider.WithSetupOutput(&out))
	savedCLI, savedConfig := gateway.WizardCLI, gateway.WizardConfig
	t.Cleanup(func() { gateway.WizardCLI, gateway.WizardConfig = savedCLI, savedConfig })
	io := &setupIO{saved: map[string]string{}, existing: existing}
	gateway.WizardConfig.GetEnvValue = func(_ context.Context, key string) (string, error) { return io.existing[key], nil }
	gateway.WizardConfig.SaveEnvValue = func(_ context.Context, key, value string) error {
		io.writes = append(io.writes, [2]string{key, value})
		io.saved[key] = value
		return nil
	}
	gateway.WizardConfig.RemoveEnvValue = func(_ context.Context, key string) (bool, error) {
		io.removed = append(io.removed, key)
		_, ok := io.existing[key]
		delete(io.existing, key)
		return ok, nil
	}
	gateway.WizardCLI.Prompt = func(string, string, bool) string {
		if len(prompts) == 0 {
			t.Fatal("unexpected prompt")
		}
		v := prompts[0]
		prompts = prompts[1:]
		return v
	}
	gateway.WizardCLI.PromptYesNo = func(context.Context, string, bool) bool {
		if len(yesNo) == 0 {
			return false
		}
		v := yesNo[0]
		yesNo = yesNo[1:]
		return v
	}
	gateway.WizardCLI.PrintHeader, gateway.WizardCLI.PrintSuccess, gateway.WizardCLI.PrintWarning = func(string) {}, func(string) {}, func(string) {}
	gateway.WizardCLI.PrintInfo = func(line string) { io.infos = append(io.infos, line) }
	gateway.WizardCLI.PrintError = func(line string) { io.errors = append(io.errors, line) }
	return io
}

func TestMattermostHomeChannelClearBlankRemovesExistingHomeChannel(t *testing.T) {
	io := patchSetupIO(t, []string{"https://mm.example.com", "mm-token", "", ""}, nil, map[string]string{"MATTERMOST_HOME_CHANNEL": "old-channel-id"})
	if err := InteractiveSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !contains(io.removed, "MATTERMOST_HOME_CHANNEL") {
		t.Fatalf("removed %v", io.removed)
	}
	if _, ok := io.saved["MATTERMOST_HOME_CHANNEL"]; ok {
		t.Fatalf("saved %v", io.saved)
	}
}

func TestSecurityHintNamesADocumentedUserIdLookup(t *testing.T) {
	io := patchSetupIO(t, []string{"https://mm.example.com", "mm-token", "", ""}, nil, map[string]string{})
	if err := InteractiveSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := strings.Join(io.infos, "\n")
	if !strings.Contains(out, "User Management") || !strings.Contains(out, "/api/v4/users/username/") || strings.Contains(out, "click your avatar") {
		t.Fatalf("hint %q", out)
	}
}

func contains(items []string, s string) bool {
	for _, item := range items {
		if item == s {
			return true
		}
	}
	return false
}
