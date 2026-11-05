//go:build windows

package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"golang.org/x/sys/windows"
)

// Replaces the Git Bash resolver for the test duration
func withFindBash(t *testing.T, fn func() (string, error)) {
	t.Helper()
	prev := findBash
	findBash = fn
	t.Cleanup(func() { findBash = prev })
}

func TestInlineShellKeepsOutputContract(t *testing.T) {
	s := NewScope(t.TempDir(), config.Defaults())
	for _, tc := range [][2]string{
		{`printf "hello"`, "hello"},
		{`:`, ""},
		{`printf "diagnostic" >&2; exit 3`, "diagnostic"},
		{`printf "preferred"; printf "ignored" >&2`, "preferred"},
	} {
		if got := s.RunInlineShell(context.Background(), tc[0], t.TempDir(), 5); got != tc[1] {
			t.Fatalf("%s: got %q want %q", tc[0], got, tc[1])
		}
	}
}

func TestInlineShellResolverFailureIsAMarker(t *testing.T) {
	withFindBash(t, func() (string, error) { return "", errors.New("Git Bash unavailable") })
	s := NewScope(t.TempDir(), config.Defaults())
	if got := s.RunInlineShell(context.Background(), ":", t.TempDir(), 5); got != "[inline-shell error: Git Bash unavailable]" {
		t.Fatalf("got %q", got)
	}
}

func TestDisabledInlineShellDoesNotResolveBash(t *testing.T) {
	withFindBash(t, func() (string, error) {
		t.Fatal("disabled inline shell must not resolve an interpreter")
		return "", nil
	})
	s := NewScope(t.TempDir(), config.Defaults())
	if got := s.PreprocessSkillContent(context.Background(), "!`printf hello`", t.TempDir(), "", map[string]any{}); got != "!`printf hello`" {
		t.Fatalf("got %q", got)
	}
}

func TestInlineShellExecutesInNativeSkillDirectory(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("native-skill"), 0o644)
	s := NewScope(t.TempDir(), config.Defaults())
	got := s.PreprocessSkillContent(context.Background(), "value=!`read -r value < marker.txt; printf \"%s\" \"$value\"`", dir, "",
		map[string]any{"inline_shell": true, "inline_shell_timeout": 5})
	if got != "value=native-skill" {
		t.Fatalf("got %q", got)
	}
}

func TestInlineShellInvalidWorkingDirectoryIsTheCreateProcessError(t *testing.T) {
	s := NewScope(t.TempDir(), config.Defaults())
	file := filepath.Join(t.TempDir(), "file.txt")
	os.WriteFile(file, []byte("x"), 0o644)
	text, _ := pypath.ErrnoText(windows.ERROR_DIRECTORY)
	if got := s.RunInlineShell(context.Background(), ":", file, 5); got != "[inline-shell error: "+text+"]" || !strings.HasPrefix(text, "[WinError 267] ") {
		t.Fatalf("got %q want the FormatMessage text %q", got, text)
	}
}
