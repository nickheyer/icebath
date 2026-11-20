package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestGatewayPersistsToTheProfileConfigItWasGiven(t *testing.T) {
	root := t.TempDir()
	processHome, profileHome := filepath.Join(root, "default"), filepath.Join(root, "profiles", "named")
	for _, h := range []string{processHome, profileHome} {
		if err := os.MkdirAll(h, 0o700); err != nil {
			t.Fatal(err)
		}
		writeConfigYAML(t, h, "model:\n  default: old\n  provider: openai-codex\n")
	}
	t.Setenv("ICEBATH_HOME", processHome)
	ctx := home.WithOverride(t.Context(), processHome)
	if err := PersistModelSelection(ctx, persistGlobalResult, filepath.Join(profileHome, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	if got := persistedModelBlock(t, filepath.Join(profileHome, "config.yaml"))["default"]; got != "claude-haiku" {
		t.Fatalf("profile default %v", got)
	}
	if got := persistedModelBlock(t, filepath.Join(processHome, "config.yaml"))["default"]; got != "old" {
		t.Fatalf("process default %v", got)
	}
}

func TestOpenaiCodexUnknownButPlausibleModelIsAcceptedWithWarning(t *testing.T) {
	ctx := switchValidateHome(t, "")
	providerModelIDsFn = catalogIDs("gpt-5.5", "gpt-5.4", "gpt-5.3-codex")
	verdict := ValidateRequestedModel(ctx, "gpt-5.3-codex-spark", "openai-codex", ValidateModelOptions{})
	if !verdict.Accepted || !verdict.Persist || verdict.Recognized {
		t.Fatalf("verdict %+v", verdict)
	}
	if !strings.Contains(verdict.Message, "gpt-5.3-codex-spark") || !strings.Contains(verdict.Message, "gpt-5.3-codex") {
		t.Fatalf("message %q", verdict.Message)
	}
}
