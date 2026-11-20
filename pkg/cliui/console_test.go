package cliui

import (
	"strings"
	"testing"
)

func TestStdinIsTTYFollowsTheInstalledCheck(t *testing.T) {
	c := &Console{In: strings.NewReader(""), Out: &strings.Builder{}}
	if c.StdinIsTTY() {
		t.Fatal("a string reader is not a terminal")
	}
	c.StdinTTY = func() bool { return true }
	if !c.StdinIsTTY() {
		t.Fatal("installed check ignored")
	}
}

func TestStdoutIsTTYFollowsTheInstalledCheck(t *testing.T) {
	c := &Console{In: strings.NewReader(""), Out: &strings.Builder{}}
	if c.StdoutIsTTY() {
		t.Fatal("a string builder is not a terminal")
	}
	c.StdoutTTY = func() bool { return true }
	if !c.StdoutIsTTY() {
		t.Fatal("installed check ignored")
	}
}

func TestMaskedSecretUsesTheInstalledPrompt(t *testing.T) {
	c := &Console{MaskedSecretPrompt: func(prompt string) (string, error) { return "v:" + prompt, nil }}
	got, err := c.MaskedSecret("key")
	if err != nil || got != "v:key" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestSizeReportsTheInstalledTerminalSize(t *testing.T) {
	c := &Console{TermSize: func() (int, int, error) { return 24, 80, nil }}
	rows, cols, err := c.Size()
	if err != nil || rows != 24 || cols != 80 {
		t.Fatalf("got %d %d %v", rows, cols, err)
	}
}
