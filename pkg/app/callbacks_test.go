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

func TestSecretCaptureCallbackCanBeCompletedFromCliStateMachine(t *testing.T) {
	c, _ := newModalTestCLI(t, nil)
	captureStreams(t)
	type outcome struct {
		result map[string]any
		err    error
	}
	results := make(chan outcome, 1)
	go func() {
		r, err := c.secretCaptureCallback("TENOR_API_KEY", "Tenor API key", nil)
		results <- outcome{r, err}
	}()
	waitFor(t, "secret panel", func() bool { return c.secretModal() != nil })
	c.submitSecretResponse("super-secret-value")
	got := <-results
	result := secretResultOf(t, got.result, got.err)
	if result["success"] != true || result["stored_as"] != "TENOR_API_KEY" || result["skipped"] != false {
		t.Fatalf("result %v", result)
	}
}

func TestCancelSecretCaptureMarksSetupSkipped(t *testing.T) {
	c, _ := newModalTestCLI(t, nil)
	c.app = nil
	c.secretState = &secretModalState{VarName: "TENOR_API_KEY", Prompt: "Tenor API key", Metadata: map[string]any{}, response: make(chan string, 1)}
	c.secretDeadline = 123
	c.cancelSecretCapture()
	if c.secretModal() != nil || c.secretDeadline != 0 {
		t.Fatalf("state %v deadline %v", c.secretModal(), c.secretDeadline)
	}
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
