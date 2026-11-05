//go:build !windows

package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestInlineShellSubprocessSemantics(t *testing.T) {
	ctx := context.Background()
	s := NewScope(t.TempDir(), config.Defaults())
	file := filepath.Join(t.TempDir(), "file.txt")
	os.WriteFile(file, []byte("x"), 0o644)
	if got, want := s.RunInlineShell(ctx, ":", file, 5), "[inline-shell error: [Errno 20] Not a directory: "+pyrepr.StrRepr(file)+"]"; got != want {
		t.Fatalf("file cwd got %q want %q", got, want)
	}
	if got := s.RunInlineShell(ctx, ":", filepath.Join(t.TempDir(), "missing"), 5); got != "[inline-shell error: bash not found]" {
		t.Fatalf("missing cwd got %q", got)
	}
	if got := s.RunInlineShell(ctx, "kill -9 $$", t.TempDir(), 5); got != "[inline-shell exit -9 with no output: kill -9 $$]" {
		t.Fatalf("signal got %q", got)
	}
	if got := s.RunInlineShell(ctx, `printf 'a\r\nb\rc\n'`, t.TempDir(), 5); got != "a\nb\nc" {
		t.Fatalf("newlines got %q", got)
	}
	if got := s.RunInlineShell(ctx, `printf '\377\376ok'`, t.TempDir(), 5); got != "��ok" {
		t.Fatalf("replace got %q", got)
	}
}
