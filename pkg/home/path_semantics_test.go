package home

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Serves a user home lookup that fails as it does without HOME or an account entry
func noUserHome(t *testing.T) {
	t.Helper()
	lookup := func() (string, error) { return "", pathutil.ErrNoHome }
	prev := userHomeLookup.Swap(&lookup)
	t.Cleanup(func() { userHomeLookup.Store(prev) })
}

// Runs fn and fails unless it panicked with the missing data home refusal naming ICEBATH_HOME
func panicsWithNoDataHome(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if err, ok := recover().(error); !ok || !errors.Is(err, pathutil.ErrNoHome) || !strings.Contains(err.Error(), EnvHome) {
			t.Fatalf("%s did not refuse for the missing data home", what)
		}
	}()
	fn()
}

func TestNamedProfileHomeKeepsDotDot(t *testing.T) {
	root := t.TempDir()
	writeText(t, filepath.Join(root, "config.yaml"), "")
	profile := filepath.Join(root, "profiles", "coder")
	if got := NamedProfileHome(profile + string(filepath.Separator) + ".."); got != profile {
		t.Fatalf("NamedProfileHome(%s/..) = %q, want %q", profile, got, profile)
	}
}

func TestProfilesRootMatchesTheDefaultRootThroughALink(t *testing.T) {
	tmp, _ := hcFresh(t)
	t.Setenv(EnvSuffix, "-x")
	native := filepath.Join(tmp, ".icebath-x")
	if err := os.MkdirAll(filepath.Join(native, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(native, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	profile := filepath.Join(link, "profiles", "coder")
	if got := NamedProfileHome(profile); got != profile {
		t.Fatalf("NamedProfileHome through a link to the default root = %q", got)
	}
}

func TestProfileNameForExpandsOnlyTheUserHome(t *testing.T) {
	tmp, _ := hcFresh(t)
	root := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_TEST_ROOT", root)
	t.Setenv("HOME", tmp)
	if got := ProfileNameFor("$ICEBATH_TEST_ROOT"); got != "" {
		t.Fatalf("ProfileNameFor($ICEBATH_TEST_ROOT) = %q, pathlib never expands variables", got)
	}
	if got := ProfileNameFor("~/.icebath"); runtime.GOOS != "windows" && got != "default" {
		t.Fatalf("ProfileNameFor(~/.icebath) = %q", got)
	}
}

func TestTombstonePathOfATrailingSeparator(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profiles", "coder")
	want := filepath.Join(root, "profiles", ".deleted", "coder")
	if got := TombstonePath(profile + string(filepath.Separator)); got != want {
		t.Fatalf("TombstonePath = %q, want %q", got, want)
	}
}

func TestClearProfileDeletedRefusesADirectory(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profiles", "coder")
	tomb := TombstonePath(profile)
	if err := os.MkdirAll(tomb, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ClearProfileDeleted(profile); err == nil {
		t.Fatal("unlink of a tombstone directory succeeded")
	}
	if st, err := os.Stat(tomb); err != nil || !st.IsDir() {
		t.Fatalf("tombstone directory removed: %v", err)
	}
	if err := os.Remove(tomb); err != nil {
		t.Fatal(err)
	}
	if err := ClearProfileDeleted(profile); err != nil {
		t.Fatalf("missing tombstone = %v", err)
	}
}

func TestMarkProfileDeletedWritesTextModeLines(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profiles", "coder")
	if err := MarkProfileDeleted(profile); err != nil {
		t.Fatal(err)
	}
	want := "deleted\n"
	if runtime.GOOS == "windows" {
		want = "deleted\r\n"
	}
	if data, err := os.ReadFile(TombstonePath(profile)); err != nil || string(data) != want {
		t.Fatalf("tombstone = %q, %v", data, err)
	}
	if !ProfileDeleted(profile) {
		t.Fatal("ProfileDeleted after marking")
	}
}

func TestMissingUserHomeRefusesTheDataHome(t *testing.T) {
	hcFresh(t)
	noUserHome(t)
	panicsWithNoDataHome(t, "PlatformDefault", func() { PlatformDefault() })
	panicsWithNoDataHome(t, "Process", func() { Process() })
	panicsWithNoDataHome(t, "Root", func() { Root() })
	if got, want := Display("/x"), pathutil.CleanKeepDotDot("/x"); got != want {
		t.Fatalf("Display without a user home = %q, want the path itself %q", got, want)
	}
	if got := ProfileNameFor("/x/profiles/coder"); got != "" {
		t.Fatalf("ProfileNameFor swallowed nothing: %q", got)
	}
	if ApplyScratchTmpEnv(context.Background(), map[string]string{}) {
		t.Fatal("ApplyScratchTmpEnv pointed the temp vars at a home it could not resolve")
	}
	h := t.TempDir()
	t.Setenv(EnvHome, h)
	if Process() != h || Dir(context.Background()) != h || Root() != h {
		t.Fatal("an explicit home must not need the user home")
	}
}

func TestKeyOfAnUnknownUserKeepsTheTilde(t *testing.T) {
	hcFresh(t)
	if runtime.GOOS == "windows" {
		t.Skip("Windows derives a ~user home from the profile directory, so no user home is unknown")
	}
	path := "~icebath-no-such-user-zz/x"
	want, err := resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := Key(path); got != pathutil.FoldCase(want) {
		t.Fatalf("Key(%q) = %q, want %q", path, got, pathutil.FoldCase(want))
	}
}

func TestManagedSystemReadsTheMarkerLikeReadText(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_MANAGED", "")
	cases := map[string]string{"NIX\t": "nix", "guix\r\n": "guix", "nix\rdev": "nix\ndev", "\t\r\n": "nixos"}
	for body, want := range cases {
		writeText(t, filepath.Join(h, ".managed"), body)
		if got := ManagedSystem(context.Background(), h); got != want {
			t.Errorf("ManagedSystem(%q) = %q, want %q", body, got, want)
		}
	}
	t.Setenv("ICEBATH_MANAGED", "\tHomebrew")
	if got := ManagedSystem(context.Background(), h); got != "" {
		t.Errorf("ignored manager = %q", got)
	}
}

func TestManagedSystemOfTheActiveHomeWarnsLikeGetIcebathHome(t *testing.T) {
	for _, env := range []string{"", "nixos"} {
		tmp, buf := hcFresh(t)
		writeText(t, filepath.Join(tmp, ".icebath", "active_profile"), "coder\n")
		t.Setenv("ICEBATH_MANAGED", env)
		ManagedSystem(context.Background(), "")
		if !strings.Contains(buf.String(), "[ICEBATH_HOME fallback]") {
			t.Fatalf("ICEBATH_MANAGED=%q: no fallback warning, got %q", env, buf.String())
		}
	}
}

func TestChmodSkipConsultsTheUncachedContainerProbe(t *testing.T) {
	if reflect.ValueOf(detectContainer).Pointer() != reflect.ValueOf(hostinfo.DetectContainer).Pointer() {
		t.Fatal("chmod skip must read _detect_container, not the cached is_container")
	}
	if reflect.ValueOf(isContainer).Pointer() != reflect.ValueOf(hostinfo.IsContainer).Pointer() {
		t.Fatal("subprocess home must read the cached is_container")
	}
}

func TestApplyScratchTmpEnvKeepsAScratchDirItCouldNotCreate(t *testing.T) {
	scratchPruneSpawns(t)
	file := filepath.Join(t.TempDir(), "file")
	writeText(t, file, "")
	h := filepath.Join(file, "home")
	env := map[string]string{EnvHome: h}
	if !ApplyScratchTmpEnv(context.Background(), env) {
		t.Fatal("get_scratch_dir swallows the mkdir OSError, the vars are still rewritten")
	}
	if env["TMPDIR"] != filepath.Join(h, "cache", "scratch") {
		t.Fatalf("TMPDIR = %q", env["TMPDIR"])
	}
}

func TestEnsureScratchDirOfTheActiveHomeWarnsLikeGetIcebathHome(t *testing.T) {
	scratchPruneSpawns(t)
	tmp, buf := hcFresh(t)
	writeText(t, filepath.Join(tmp, ".icebath", "active_profile"), "coder\n")
	if got := EnsureScratchDir(context.Background(), ""); got != filepath.Join(tmp, ".icebath", "cache", "scratch") {
		t.Fatalf("scratch = %q", got)
	}
	if !strings.Contains(buf.String(), "[ICEBATH_HOME fallback]") {
		t.Fatalf("no fallback warning: %q", buf.String())
	}
}

func TestPruneScratchDirLogsBesideTheHomeOfATrailingSeparator(t *testing.T) {
	h := t.TempDir()
	scratch := testScratch(t, h)
	idleEntry(t, scratch, "old")
	if n, err := PruneScratchDir(context.Background(), scratch+string(filepath.Separator), ScratchMaxIdleHours); err != nil || n != 1 {
		t.Fatalf("PruneScratchDir = %d, %v", n, err)
	}
	if !strings.Contains(pruneLog(t, h), "old") {
		t.Fatal("removal not logged under <home>/logs")
	}
}

func TestScratchDirUsageBytesWalksALinkedRoot(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	writeText(t, filepath.Join(real, "a", "file"), "12345")
	writeText(t, filepath.Join(real, "top"), "12")
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(real, "a"), filepath.Join(real, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if got := ScratchDirUsageBytes(context.Background(), link); got != 7 {
		t.Fatalf("usage through a linked root = %d, want 7", got)
	}
}

func TestPackagedDirDefaultIsAPathlibPath(t *testing.T) {
	t.Setenv("ICEBATH_TEST_PACKAGED", "")
	got := PackagedDir(context.Background(), "ICEBATH_TEST_PACKAGED", "a//b/./", "s")
	if want := filepath.Join("a", "b"); got != want {
		t.Fatalf("PackagedDir default = %q, want %q", got, want)
	}
}

func TestWSLTranslatorsTrimWhitespace(t *testing.T) {
	if got, ok := WindowsPathToWSL("\tC:\\Users\\x\t"); !ok || got != "/mnt/c/Users/x" {
		t.Fatalf("WindowsPathToWSL = %q %v", got, ok)
	}
	if got, ok := WSLUNCPathToPosix("\t\\\\wsl$\\Ubuntu\\home\\x"); !ok || got != "/home/x" {
		t.Fatalf("WSLUNCPathToPosix = %q %v", got, ok)
	}
}

func TestLayoutPathsKeepDotDot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX separators")
	}
	h := "/a/b/../c"
	for got, want := range map[string]string{
		ConfigPath(h): "/a/b/../c/config.yaml", EnvPath(h): "/a/b/../c/.env",
		SkillsDir(h): "/a/b/../c/skills", ScratchDir(h): "/a/b/../c/cache/scratch",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}
