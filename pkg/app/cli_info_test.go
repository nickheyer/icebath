package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreSessionCwdMovesIntoTheRecordedDirectory(t *testing.T) {
	t.Setenv("COLUMNS", "500")
	rec := recordPrints(t)
	c := bareCLI()
	c.console = console()
	start := t.TempDir()
	recorded := t.TempDir()
	t.Chdir(start)
	t.Setenv("TERMINAL_CWD", start)
	t.Cleanup(func() { moveCLIFolder("") })
	c.restoreSessionCwd(map[string]any{"cwd": recorded}, false)
	now, _ := os.Getwd()
	want, _ := filepath.EvalSymlinks(recorded)
	got, _ := filepath.EvalSymlinks(now)
	if got != want || movedOrHandedFolder() != recorded {
		t.Fatalf("cwd %q session folder %q", now, movedOrHandedFolder())
	}
	rec.reset()
	c.restoreSessionCwd(map[string]any{"cwd": recorded}, false)
	c.restoreSessionCwd(map[string]any{}, false)
	if len(rec.raw()) != 0 {
		t.Fatalf("an unchanged or unrecorded cwd printed %q", rec.raw())
	}
	gone := filepath.Join(recorded, "gone")
	c.restoreSessionCwd(map[string]any{"cwd": gone}, false)
	if out := strings.Join(rec.plain(), "\n"); !strings.Contains(out, gone) {
		t.Fatalf("a missing directory must be named: %q", out)
	}
	if after, _ := os.Getwd(); after != now {
		t.Fatal("a missing directory must leave the process where it is")
	}
}
