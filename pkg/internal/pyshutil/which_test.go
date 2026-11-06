//go:build !windows

package pyshutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWhichSearchesTheGivenPathInOrder(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(first, "tool"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(second, "tool"), []byte("x"), 0o755)
	os.Mkdir(filepath.Join(first, "dir"), 0o755)
	path := first + string(os.PathListSeparator) + second
	if got, ok := Which("tool", &path); !ok || got != filepath.Join(second, "tool") {
		t.Fatalf("Which = %q %v", got, ok)
	}
	if _, ok := Which("dir", &path); ok {
		t.Fatal("a directory is never an executable")
	}
	empty := ""
	if _, ok := Which("tool", &empty); ok {
		t.Fatal("an empty path finds nothing")
	}
	if got, ok := Which(filepath.Join(second, "tool"), &empty); !ok || got != filepath.Join(second, "tool") {
		t.Fatalf("a command with a directory ignores the path, got %q %v", got, ok)
	}
	t.Setenv("PATH", second)
	if got, ok := Which("tool", nil); !ok || got != filepath.Join(second, "tool") {
		t.Fatalf("nil path reads PATH, got %q %v", got, ok)
	}
}
