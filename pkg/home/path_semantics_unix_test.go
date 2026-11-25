//go:build !windows

package home

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unicode/utf8"
)

// Fails unless the captured log holds the warning text exactly once
func warnedOnce(t *testing.T, warnings *bytes.Buffer, text string) {
	t.Helper()
	if n := strings.Count(warnings.String(), "level=WARN msg="+strconv.Quote(text)); n != 1 {
		t.Fatalf("warning %q logged %d times, want once:\n%s", text, n, warnings.String())
	}
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

func TestHomeModeReadsOctalDigitsAndFallsBackTo700(t *testing.T) {
	swap(t, &ignoredEnvWarned, &sync.Map{})
	warnings := captureWarnings(t)
	cases := map[string]uint32{"_755": 0o700, "0o7_55": 0o700, "\u0667\u0665\u0665": 0o700, "\t751\t": 0o751, "0o_750": 0o700, "78": 0o700}
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
	for range 2 {
		dir := mkdirs(t, filepath.Join(tmp, "d"), 0o755)
		ApplySecureDirPolicy(context.Background(), dir, tmp)
		if got := sMode(t, dir); got != 0o700 {
			t.Fatalf("ICEBATH_HOME_MODE beyond 32 bits gave %o, want 700", got)
		}
	}
	warnedOnce(t, warnings, `ICEBATH_HOME_MODE="0o77777777777" is beyond the largest file mode, using 700`)
	warnedOnce(t, warnings, `ICEBATH_HOME_MODE="78" is not an octal file mode, using 700`)
	warnedOnce(t, warnings, `ICEBATH_HOME_MODE="0o7_55" is not an octal file mode, using 700`)
}

func TestIcebathUIDGIDBadValuesWarnOnceAndLeaveTheIdUnchanged(t *testing.T) {
	swap(t, &ignoredEnvWarned, &sync.Map{})
	warnings := captureWarnings(t)
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
	if got := run("1_000", "\u0663"); len(got) != 0 {
		t.Fatalf("underscore and Unicode digits = %v", got)
	}
	if got := run("-1", ""); len(got) != 1 || got[0] != (call{-1, -1}) {
		t.Fatalf("an explicit -1 still chowns = %v", got)
	}
	if got := run("x", " "); len(got) != 0 {
		t.Fatalf("invalid values chown = %v", got)
	}
	for range 2 {
		if got := run("-2", ""); len(got) != 0 {
			t.Fatalf("a uid below -1 chowns = %v", got)
		}
	}
	if got := run("4294967296", ""); len(got) != 0 {
		t.Fatalf("a uid beyond 32 bits chowns = %v", got)
	}
	if got := run("1", "99999999999999999999"); len(got) != 1 || got[0] != (call{1, -1}) {
		t.Fatalf("a gid beyond int64 is not left unchanged = %v", got)
	}
	if got := run("", "-99999999999999999999"); len(got) != 0 {
		t.Fatalf("a gid below int64 chowns = %v", got)
	}
	warnedOnce(t, warnings, `ICEBATH_UID="1_000" is not a whole number, leaving that id unchanged`)
	warnedOnce(t, warnings, "ICEBATH_GID=\"\u0663\" is not a whole number, leaving that id unchanged")
	warnedOnce(t, warnings, `ICEBATH_UID="x" is not a whole number, leaving that id unchanged`)
	warnedOnce(t, warnings, `ICEBATH_UID="-2" is outside the ids from -1 to 4294967295, leaving that id unchanged`)
	warnedOnce(t, warnings, `ICEBATH_UID="4294967296" is outside the ids from -1 to 4294967295, leaving that id unchanged`)
	warnedOnce(t, warnings, `ICEBATH_GID="99999999999999999999" is outside the ids from -1 to 4294967295, leaving that id unchanged`)
	warnedOnce(t, warnings, `ICEBATH_GID="-99999999999999999999" is outside the ids from -1 to 4294967295, leaving that id unchanged`)
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
	dir := filepath.Join(base, strings.Repeat("\u00e9", 50-utf8.RuneCountInString(base)-1))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", dir)
	if got, err := SocketSafeTmpdir(); err != nil || got != dir {
		t.Fatalf("SocketSafeTmpdir = %q, %v, want the %d code point root %q", got, err, utf8.RuneCountInString(dir), dir)
	}
}
