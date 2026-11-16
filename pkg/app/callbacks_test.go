package app

import (
	"bytes"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// Routes secret saves into the temp home so the stored result can be asserted
func secretResultOf(t *testing.T, result map[string]any, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSecretCaptureUsesMaskedPromptWithoutTui(t *testing.T) {
	c, _ := newModalTestCLI(t, nil)
	c.app = nil
	var out, errOut bytes.Buffer
	prompts := 0
	restore := SetStreams(&cliui.Console{Out: &out, MaskedSecretPrompt: func(prompt string) (string, error) {
		prompts++
		return "secret-value", nil
	}}, &errOut)
	t.Cleanup(restore)
	r, err := promptForSecret(c, "TENOR_API_KEY", "Tenor API key", nil)
	result := secretResultOf(t, r, err)
	if prompts != 1 || result["success"] != true || result["stored_as"] != "TENOR_API_KEY" || result["skipped"] != false {
		t.Fatalf("prompts %d result %v", prompts, result)
	}
}

func TestSecretCaptureTimeoutClearsHiddenInputBuffer(t *testing.T) {
	c, app := newModalTestCLI(t, nil)
	captureStreams(t)
	fakeMonotonic(t, 0, 121)
	r, err := promptForSecret(c, "TENOR_API_KEY", "Tenor API key", nil)
	result := secretResultOf(t, r, err)
	if result["success"] != true || result["skipped"] != true || result["reason"] != "timeout" {
		t.Fatalf("result %v", result)
	}
	if app.bufferResets() == 0 {
		t.Fatal("the hidden input buffer was never cleared")
	}
}
