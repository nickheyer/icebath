package slack

import (
	"bytes"
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

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

// Replaces the manifest builder like the hermes cases stub _build_full_manifest
func stubManifest(t *testing.T) {
	t.Helper()
	saved := buildFullManifestFn
	t.Cleanup(func() { buildFullManifestFn = saved })
	buildFullManifestFn = func(context.Context, string, string, bool, *string, *string) (*pyjson.Object, error) {
		return pyjson.ObjectOf("display_information", pyjson.NewObject()), nil
	}
}

func TestInteractiveSetupSavesHomeChannel(t *testing.T) {
	io := patchSetupIO(t, []string{"xoxb-test-token", "xapp-test-token", "", "C01ABC2DE3F"}, nil, map[string]string{})
	stubManifest(t)
	if err := InteractiveSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if io.saved["SLACK_HOME_CHANNEL"] != "C01ABC2DE3F" {
		t.Fatalf("saved %v", io.saved)
	}
	for _, k := range io.removed {
		if k == "SLACK_HOME_CHANNEL" {
			t.Fatalf("removed %v", io.removed)
		}
	}
}

func TestSlackHomeChannelClearBlankRemovesExistingHomeChannel(t *testing.T) {
	io := patchSetupIO(t, []string{"xoxb-test-token", "xapp-test-token", "", ""}, nil, map[string]string{"SLACK_HOME_CHANNEL": "C01OLDHOMEXYZ"})
	stubManifest(t)
	if err := InteractiveSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range io.removed {
		found = found || k == "SLACK_HOME_CHANNEL"
	}
	if _, ok := io.saved["SLACK_HOME_CHANNEL"]; !found || ok {
		t.Fatalf("removed %v saved %v", io.removed, io.saved)
	}
}
