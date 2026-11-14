//go:build !windows

package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Routes the default logger into a buffer at WARNING and above for the test duration
func captureCredentialWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// Writes an empty JSON object to a file under a fresh home with the given permission bits
func credentialFixture(t *testing.T, name string, mode os.FileMode) string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	f := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f, mode); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestWarnIfCredentialFileBroadlyReadableWarnsOnWorldReadable(t *testing.T) {
	f := credentialFixture(t, "slack_tokens.json", 0o644)
	logs := captureCredentialWarnings(t)
	warned := WarnIfCredentialFileBroadlyReadable(f, "[Slack]", nil)
	if !warned {
		t.Fatal("world readable file did not warn")
	}
	for _, want := range []string{"group/world-readable", "chmod 600", "[Slack]"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log %q lacks %q", logs.String(), want)
		}
	}
}

func TestWarnIfCredentialFileBroadlyReadableWarnsOnGroupReadable(t *testing.T) {
	f := credentialFixture(t, "tokens.json", 0o640)
	captureCredentialWarnings(t)
	if !WarnIfCredentialFileBroadlyReadable(f, "", nil) {
		t.Fatal("group readable file did not warn")
	}
}

func TestWarnIfCredentialFileBroadlyReadableSilentOn0600(t *testing.T) {
	f := credentialFixture(t, "tokens.json", 0o600)
	logs := captureCredentialWarnings(t)
	if WarnIfCredentialFileBroadlyReadable(f, "", nil) {
		t.Fatal("owner only file warned")
	}
	if strings.Contains(logs.String(), "group/world-readable") {
		t.Fatalf("owner only file logged %q", logs.String())
	}
}

func TestWarnIfCredentialFileBroadlyReadableSilentOnMissingFile(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	logs := captureCredentialWarnings(t)
	if WarnIfCredentialFileBroadlyReadable(filepath.Join(t.TempDir(), "nope.json"), "", nil) {
		t.Fatal("missing file warned")
	}
	if logs.String() != "" {
		t.Fatalf("missing file logged %q", logs.String())
	}
}
