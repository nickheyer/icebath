//go:build !windows

package home

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Runs fn and fails unless it panicked with the OverflowError text CPython raises
func panicsWithOverflow(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		e, ok := recover().(*pyint.Error)
		if !ok || e.Kind != "OverflowError" || e.Msg != want {
			t.Fatalf("want OverflowError %q, got %v", want, e)
		}
	}()
	fn()
}

func TestMarkProfileDeletedCreatesWithTheUmaskModes(t *testing.T) {
	prev := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(prev) })
	profile := filepath.Join(t.TempDir(), "profiles", "coder")
	if err := MarkProfileDeleted(profile); err != nil {
		t.Fatal(err)
	}
	if got := sMode(t, filepath.Dir(TombstonePath(profile))); got != 0o755 {
		t.Fatalf("tombstone dir mode %o, want 755", got)
	}
	if got := sMode(t, TombstonePath(profile)); got != 0o644 {
		t.Fatalf("tombstone mode %o, want 644", got)
	}
}

func TestHomeModeParsesLikePythonIntBaseEight(t *testing.T) {
	cases := map[string]uint32{"_755": 0o700, "0o7_55": 0o755, "\u0667\u0665\u0665": 0o755, "\x1c751\x1c": 0o751, "0o_750": 0o750, "78": 0o700}
	for text, want := range cases {
		tmp := t.TempDir()
		isolatePolicyEnv(t, tmp)
		t.Setenv("ICEBATH_HOME_MODE", text)
		dir := mkdirs(t, filepath.Join(tmp, "d"), 0o755)
		ApplySecureDirPolicy(context.Background(), dir, tmp)
		if got := sMode(t, dir); got != want {
			t.Errorf("ICEBATH_HOME_MODE=%q gave %o, want %o", text, got, want)
		}
	}
	tmp := t.TempDir()
	isolatePolicyEnv(t, tmp)
	t.Setenv("ICEBATH_HOME_MODE", "0o77777777777")
	panicsWithOverflow(t, "Python int too large to convert to C int", func() { ApplySecureDirPolicy(context.Background(), tmp, tmp) })
}

func TestIcebathUIDGIDConvertLikeOsChown(t *testing.T) {
	type call struct{ uid, gid int }
	run := func(uid, gid string) []call {
		tmp := t.TempDir()
		isolatePolicyEnv(t, tmp)
		var calls []call
		swap(t, &chownPath, func(p string, u, g int) error { calls = append(calls, call{u, g}); return nil })
		t.Setenv("ICEBATH_UID", uid)
		t.Setenv("ICEBATH_GID", gid)
		ApplySecureDirPolicy(context.Background(), mkdirs(t, filepath.Join(tmp, "d"), 0), tmp)
		return calls
	}
	if got := run("1_000", "\u0663"); len(got) != 1 || got[0] != (call{1000, 3}) {
		t.Fatalf("underscore and Unicode digits = %v", got)
	}
	if got := run("-1", ""); len(got) != 1 || got[0] != (call{-1, -1}) {
		t.Fatalf("an explicit -1 still chowns = %v", got)
	}
	if got := run("x", " "); len(got) != 0 {
		t.Fatalf("invalid values chown = %v", got)
	}
	panicsWithOverflow(t, "uid is less than minimum", func() { run("-2", "") })
	panicsWithOverflow(t, "uid is less than minimum", func() { run("4294967296", "") })
	panicsWithOverflow(t, "gid is greater than maximum", func() { run("1", "99999999999999999999") })
	panicsWithOverflow(t, "gid is less than minimum", func() { run("", "-99999999999999999999") })
}

func TestScratchUsageCountsEveryNonDirectoryLstat(t *testing.T) {
	root := t.TempDir()
	writeText(t, filepath.Join(root, "f"), "123")
	if err := os.Symlink("missing-target", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if got := ScratchDirUsageBytes(context.Background(), root); got != 3+int64(len("missing-target")) {
		t.Fatalf("usage = %d", got)
	}
}

func TestSocketSafeTmpdirCountsCodePoints(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin always answers /tmp")
	}
	base, err := os.MkdirTemp("/tmp", "sst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	dir := filepath.Join(base, strings.Repeat("\u00e9", 50-pystr.Len(base)-1))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", dir)
	if got, err := SocketSafeTmpdir(); err != nil || got != dir {
		t.Fatalf("SocketSafeTmpdir = %q, %v, want the %d code point root %q", got, err, pystr.Len(dir), dir)
	}
}
