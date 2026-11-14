package provider

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func runCollectMasked(chars string) (string, string, error) {
	var output []string
	runes := []rune(chars)
	readChar := func() string {
		if len(runes) == 0 {
			return ""
		}
		ch := runes[0]
		runes = runes[1:]
		return string(ch)
	}
	value, err := collectMaskedInput(readChar, func(text string) { output = append(output, text) }, "API key: ", "*")
	return value, strings.Join(output, ""), err
}

func TestCollectMaskedInputShowsFeedbackWithoutEchoingSecret(t *testing.T) {
	value, output, err := runCollectMasked("secret\n")
	if err != nil || value != "secret" || output != "API key: ******\r\n" || strings.Contains(output, "secret") {
		t.Fatalf("value=%q output=%q err=%v", value, output, err)
	}
}

func TestCollectMaskedInputRaisesKeyboardInterrupt(t *testing.T) {
	var output []string
	_, err := collectMaskedInput(func() string { return "\x03" }, func(text string) { output = append(output, text) }, "API key: ", "*")
	if !errors.Is(err, ErrSecretPromptInterrupted) || strings.Join(output, "") != "API key: \r\n" {
		t.Fatalf("err=%v output=%q", err, output)
	}
}

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

func TestCollectMaskedInputEditsAndSkipsEscapes(t *testing.T) {
	value, output, err := runCollectMasked("ab\x7fc\x1bd\r")
	if err != nil || value != "acd" || output != "API key: **\b \b**\r\n" {
		t.Fatalf("value=%q output=%q err=%v", value, output, err)
	}
	if _, output, err := runCollectMasked("x"); err == nil || output != "API key: *\r\n" {
		t.Fatalf("closed stream output=%q err=%v", output, err)
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
