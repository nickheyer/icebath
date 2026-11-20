//go:build unix

package provider

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Replaces each path with a FIFO so any read blocks, releasing blocked readers at cleanup
func anthReadTraps(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		_ = os.Remove(p)
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
		path := p
		t.Cleanup(func() {
			if f, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
				_ = f.Close()
			}
		})
	}
}

func TestAnthropicAPIKeyPathReadsNoDiscoveredCredentialsOrPool(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant...ykey")
	anthReadTraps(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json"), filepath.Join(h, "auth.json"))
	done := make(chan string, 1)
	go func() { done <- ResolveAnthropicToken(env, "") }()
	select {
	case got := <-done:
		if got != "sk-ant...ykey" {
			t.Fatalf("api key path resolved %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("api key path read the Claude Code credentials or the credential pool")
	}
}
