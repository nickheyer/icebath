package matrix

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	IsGatewayKnownCommand = func(ctx context.Context, name string) bool {
		return commands.IsGatewayKnownCommand(ctx, commands.Default, name)
	}
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

func TestMatrixHomeChannelClearBlankRemovesExistingHomeRoom(t *testing.T) {
	io := patchSetupIO(t, []string{"https://matrix.example.org", "syt_test_token_value", "@bot:matrix.example.org", "", ""}, []bool{false},
		map[string]string{"MATRIX_HOME_ROOM": "!oldRoomId:matrix.example.org"})
	if err := InteractiveSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range io.removed {
		found = found || k == "MATRIX_HOME_ROOM"
	}
	if _, ok := io.saved["MATRIX_HOME_ROOM"]; !found || ok {
		t.Fatalf("removed %v saved %v", io.removed, io.saved)
	}
}

func TestInteractiveSetupPreparesNoDependencyEnvironment(t *testing.T) {
	io := patchSetupIO(t, []string{"https://matrix.example.test", "test-token", "@bot:example.test", "@owner:example.test", "!home:example.test"}, nil, map[string]string{})
	gateway.WizardConfig.SaveEnvValue = func(context.Context, string, string) error { return nil }
	if err := InteractiveSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, line := range append(io.infos, io.errors...) {
		if strings.Contains(line, "dependencies") {
			t.Fatalf("wizard line %q", line)
		}
	}
}
