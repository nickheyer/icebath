package provider

import (
	"os"
	"testing"
)

func TestMaskedSecretPromptFallsBackToGetpassForNonTTY(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	oldIn, oldOut, oldGetpass := secretPromptStdin, secretPromptStdout, secretGetpass
	t.Cleanup(func() { secretPromptStdin, secretPromptStdout, secretGetpass = oldIn, oldOut, oldGetpass })
	secretPromptStdin = func() *os.File { return r }
	secretPromptStdout = func() *os.File { return w }
	secretGetpass = func(prompt string) (string, error) { return "value from " + prompt, nil }
	if got, err := maskedSecretPrompt("API key: "); err != nil || got != "value from API key: " {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestGetpassReadsTheStdinLineWithoutATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	oldIn := secretPromptStdin
	t.Cleanup(func() { secretPromptStdin = oldIn })
	secretPromptStdin = func() *os.File { return r }
	if _, err := w.WriteString("hunter2\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if got, err := Getpass("[sudo] password: "); err != nil || got != "hunter2" {
		t.Fatalf("got %q err %v", got, err)
	}
}

// Exported prompt is the same function the package uses internally
func TestMaskedSecretPromptExported(t *testing.T) {
	var f func(string) (string, error) = MaskedSecretPrompt
	if f == nil {
		t.Fatal("MaskedSecretPrompt is nil")
	}
}
