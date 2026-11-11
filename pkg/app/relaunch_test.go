package app

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Installs the argv a relaunch reads for the test
func setRelaunchArgv(t *testing.T, argv ...string) {
	t.Helper()
	saved := relaunchArgv
	relaunchArgv = func() []string { return argv }
	t.Cleanup(func() { relaunchArgv = saved })
}

// Writes an executable file and returns its path
func writeExecutable(t *testing.T, path, text string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Native looking icebath executable standing in for the resolved entry point, argv set to it alone
func resolvedRelaunchBin(t *testing.T) string {
	t.Helper()
	bin := writeExecutable(t, filepath.Join(t.TempDir(), "icebath"+relaunchExeSuffix), "\x7fELF native")
	setRelaunchArgv(t, bin)
	return bin
}

func TestResolveHermesBinPrefersAbsoluteArgv0WhenExecutable(t *testing.T) {
	fake := writeExecutable(t, filepath.Join(t.TempDir(), "nix", "store", "abc", "bin", "icebath"+relaunchExeSuffix), "\x7fELF native")
	setRelaunchArgv(t, fake)
	if got, ok := resolveHermesBin(); !ok || got != fake {
		t.Fatalf("resolved %q %v", got, ok)
	}
}

func TestResolveHermesBinFallsBackToPathWhich(t *testing.T) {
	dir := t.TempDir()
	onPath := writeExecutable(t, filepath.Join(dir, "icebath"+relaunchExeSuffix), "\x7fELF native")
	t.Setenv("PATH", dir)
	setRelaunchArgv(t, "-c")
	if got, ok := resolveHermesBin(); !ok || got != onPath {
		t.Fatalf("resolved %q %v, want %q", got, ok, onPath)
	}
}

func TestExtractInheritedFlagsExtractsTuiAndDev(t *testing.T) {
	if got := extractInheritedFlags([]string{"--tui", "--dev", "chat"}); !slices.Equal(got, []string{"--tui", "--dev"}) {
		t.Fatalf("flags %v", got)
	}
}

func TestExtractInheritedFlagsPreservesMultipleSkills(t *testing.T) {
	argv := []string{"-s", "foo", "-s", "bar", "--tui"}
	if got := extractInheritedFlags(argv); !slices.Equal(got, argv) {
		t.Fatalf("flags %v", got)
	}
}

func TestInheritedFlagTableExcludedFlagsAreNotInherited(t *testing.T) {
	table := map[string]bool{}
	for _, entry := range inheritedFlagsTable() {
		table[entry.flag] = true
	}
	for _, flag := range []string{"-w", "--worktree", "-Q", "--quiet", "-v", "--verbose", "--source"} {
		if table[flag] {
			t.Fatalf("%s should not be inherited", flag)
		}
	}
	for _, flag := range []string{"--tui", "--dev", "--profile", "-p", "-s"} {
		if !table[flag] {
			t.Fatalf("%s is missing from the table %v", flag, table)
		}
	}
}

func TestBuildRelaunchArgvPreservesInheritedFlags(t *testing.T) {
	bin := resolvedRelaunchBin(t)
	original := []string{"--tui", "--dev", "--profile", "work", "sessions", "browse"}
	argv, err := buildRelaunchArgv([]string{"--resume", "abc"}, true, original)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--tui", "--dev", "--profile", "work", "--resume", "abc"} {
		if !slices.Contains(argv, want) {
			t.Fatalf("argv %v lacks %s", argv, want)
		}
	}
	if slices.Contains(argv, "sessions") || slices.Contains(argv, "browse") || argv[0] != bin {
		t.Fatalf("argv %v", argv)
	}
}

func TestBuildRelaunchArgvCanDisablePreserve(t *testing.T) {
	bin := resolvedRelaunchBin(t)
	argv, err := buildRelaunchArgv([]string{"--resume", "abc"}, false, []string{"--tui", "chat"})
	if err != nil || !slices.Equal(argv, []string{bin, "--resume", "abc"}) {
		t.Fatalf("argv %v %v", argv, err)
	}
}
