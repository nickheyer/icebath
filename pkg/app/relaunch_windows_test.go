//go:build windows

package app

import (
	"path/filepath"
	"slices"
	"testing"
)

const relaunchExeSuffix = ".exe"

// Records the commands a Windows relaunch spawns and the exit code it leaves with
func captureWindowsRelaunch(t *testing.T, returncode int) (*[][]string, *[]int, *int) {
	t.Helper()
	var spawned [][]string
	var exits []int
	execs := 0
	savedRun, savedExit, savedExec := relaunchRun, relaunchExit, relaunchExecvp
	relaunchRun = func(argv []string) (int, error) {
		spawned = append(spawned, slices.Clone(argv))
		return returncode, nil
	}
	relaunchExit = func(code int) { exits = append(exits, code) }
	relaunchExecvp = func(string, []string) error {
		execs++
		return nil
	}
	t.Cleanup(func() { relaunchRun, relaunchExit, relaunchExecvp = savedRun, savedExit, savedExec })
	return &spawned, &exits, &execs
}

func TestRelaunchWindowsUsesSubprocessNotExecvp(t *testing.T) {
	bin := resolvedRelaunchBin(t)
	spawned, exits, execs := captureWindowsRelaunch(t, 0)
	relaunch([]string{"chat"}, true)
	if !slices.Equal(*exits, []int{0}) || *execs != 0 || len(*spawned) != 1 || !slices.Equal((*spawned)[0], []string{bin, "chat"}) {
		t.Fatalf("spawned %v exits %v execs %d", *spawned, *exits, *execs)
	}
}

func TestRelaunchWindowsPropagatesChildExitCode(t *testing.T) {
	resolvedRelaunchBin(t)
	_, exits, _ := captureWindowsRelaunch(t, 42)
	relaunch([]string{"chat"}, true)
	if !slices.Equal(*exits, []int{42}) {
		t.Fatalf("exits %v", *exits)
	}
}

func TestResolveHermesBinWindowsPyGuardWindowsRejectsPyArgv0FallsThroughToPath(t *testing.T) {
	dir := t.TempDir()
	script := writeExecutable(t, filepath.Join(dir, "main.py"), "# source launcher")
	onPath := writeExecutable(t, filepath.Join(dir, "Scripts", "icebath.exe"), "MZ native")
	setRelaunchArgv(t, script, "chat")
	t.Setenv("PATH", filepath.Dir(onPath))
	if got, ok := resolveHermesBin(); !ok || got != onPath {
		t.Fatalf("resolved %q %v, want %q", got, ok, onPath)
	}
}

func TestResolveHermesBinWindowsPyGuardWindowsPyArgv0WithNoHermesOnPathReturnsNone(t *testing.T) {
	dir := t.TempDir()
	script := writeExecutable(t, filepath.Join(dir, "main.py"), "# source launcher")
	setRelaunchArgv(t, script, "chat")
	t.Setenv("PATH", t.TempDir())
	if got, ok := resolveHermesBin(); ok {
		t.Fatalf("resolved %q", got)
	}
}
