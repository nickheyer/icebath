//go:build unix

package mcp

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOAuthTokenWriterCreatesItsFileAt0600(t *testing.T) {
	dir := oauthHome(t)
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	target := filepath.Join(dir, "mcp", "probe.tokens.json")
	if err := writeOAuthJSON(target, map[string]any{"access_token": "x"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", info, err)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temp file left behind %v %v", entries, err)
	}
}
