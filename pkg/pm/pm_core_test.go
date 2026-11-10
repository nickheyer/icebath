package pm

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// Environment variable selecting what a re-executed test binary does
const sttHelperEnv = "STT_PM_HELPER"

// Re-executed test binary entry running one helper mode chosen by the parent test
func TestSttHelperProcess(t *testing.T) {
	mode := os.Getenv(sttHelperEnv)
	if mode == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(sttHelperEnv+"_ARGS")), &args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(6)
	}
	switch mode {
	case "lease":
		if _, err := LeaseDirectory(args[0]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	case "lockwriter":
		lock := NewLockfile(args[0])
		lock.SetPin(args[1], "2", map[string]any{})
		fmt.Println("ready")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "save" {
			fmt.Fprintf(os.Stderr, "expected save, got %q %v\n", line, err)
			os.Exit(3)
		}
		if err := lock.Save(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "unknown helper mode", mode)
	os.Exit(5)
}

// Command re-executing this test binary in one helper mode with its arguments
func sttHelperCommand(mode string, args ...string) *exec.Cmd {
	encoded, _ := json.Marshal(args)
	cmd := exec.Command(os.Args[0], "-test.run=^TestSttHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), sttHelperEnv+"="+mode, sttHelperEnv+"_ARGS="+string(encoded))
	return cmd
}

// Creates a directory tree failing the test on error
func sttMkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o777); err != nil {
		t.Fatal(err)
	}
	return path
}

// Creates an empty file or bumps its mtime
func sttTouch(t *testing.T, path string) string {
	t.Helper()
	if err := touchFile(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// Sorted child paths of a directory, empty when it is missing
func sttChildren(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// Reports whether path exists following symlinks
func sttExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Reports whether path is a directory following symlinks
func sttIsDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// Fails unless two path lists hold the same paths in the same order
func sttSamePaths(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Opens a file and takes its byte lock without waiting, failing unless the lock was free
func sttHold(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := LockFD(f, false, 0)
	if err != nil || !locked {
		f.Close()
		t.Fatalf("lock %s not taken: %v", path, err)
	}
	return f
}

// Points the binary's own tree at dir for the rest of the test
func sttRepoRoot(t *testing.T, dir string) {
	t.Helper()
	saved := repoRoot
	repoRoot = func() string { return dir }
	t.Cleanup(func() { repoRoot = saved })
}

// Host interpreter the Python suite runs on, failing the test when none is installed
func sttHostPython(t *testing.T) string {
	t.Helper()
	name := "python3"
	if runtime.GOOS == "windows" {
		name = "python"
	}
	python, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("a host %s interpreter is required: %v", name, err)
	}
	return python
}

// Output of a command with a deadline, failing the test on a nonzero exit
func sttOutput(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%q failed: %v\n%s", argv, err, stderr.String())
	}
	return string(out)
}

func TestComposeEnvDependentsWinNonPathToo(t *testing.T) {
	env := ComposeEnv([]EnvDiff{{"X": "dep"}, {"X": "dependent"}}, map[string]string{})
	if env["X"] != "dependent" {
		t.Fatalf("X = %q", env["X"])
	}
}

// PE image whose header carries the given machine
func sttPE(machine uint16) []byte {
	head := make([]byte, 64)
	copy(head, "MZ")
	binary.LittleEndian.PutUint32(head[60:64], 64)
	out := append(head, "PE\x00\x00"...)
	return binary.LittleEndian.AppendUint16(out, machine)
}

// Fails unless the binary check answers the expected match and knowledge
func sttMachine(t *testing.T, bin, target string, wantMatch, wantKnown bool) {
	t.Helper()
	match, known := MachineMatchesBinary(bin, target)
	if match != wantMatch || known != wantKnown {
		t.Fatalf("%s on %s = (%v, %v), want (%v, %v)", filepath.Base(bin), target, match, known, wantMatch, wantKnown)
	}
}

func TestMachineMatchesBinaryPEHeaders(t *testing.T) {
	isolateHome(t)
	tmp := t.TempDir()
	amd := filepath.Join(tmp, "amd.exe")
	writeFile(t, amd, sttPE(0x8664), 0o666)
	arm := filepath.Join(tmp, "arm.exe")
	writeFile(t, arm, sttPE(0xAA64), 0o666)
	script := filepath.Join(tmp, "tool")
	writeFile(t, script, []byte("#!/bin/sh\n"), 0o666)

	sttMachine(t, amd, "win32-x64", true, true)
	sttMachine(t, amd, "win32-arm64", false, true)
	sttMachine(t, arm, "win32-arm64", true, true)
	sttMachine(t, script, "win32-x64", false, false)
}

func TestMachineMatchesBinaryELF(t *testing.T) {
	isolateHome(t)
	elf := make([]byte, 64)
	copy(elf, "\x7fELF")
	binary.LittleEndian.PutUint16(elf[18:20], 0xB7)
	b := filepath.Join(t.TempDir(), "tool")
	writeFile(t, b, elf, 0o666)
	sttMachine(t, b, "linux-arm64", true, true)
	sttMachine(t, b, "linux-x64", false, true)
}

func TestArchGuardSeesEmulatedX64PEOnWin32Arm64(t *testing.T) {
	isolateHome(t)
	binDir := sttMkdir(t, filepath.Join(t.TempDir(), "store", "chromium-1234", "chrome-win64"))
	x64PE := []byte("MZ")
	x64PE = append(x64PE, make([]byte, 58)...)
	x64PE = binary.LittleEndian.AppendUint32(x64PE, 0x80)
	x64PE = append(x64PE, make([]byte, 64)...)
	x64PE = append(x64PE, "PE\x00\x00"...)
	x64PE = binary.LittleEndian.AppendUint16(x64PE, 0x8664)
	x64PE = append(x64PE, make([]byte, 54)...)
	bin := filepath.Join(binDir, "chrome.exe")
	writeFile(t, bin, x64PE, 0o666)
	sttMachine(t, bin, "win32-arm64", false, true)
}

func TestPatchRepoRootSwapsAndRestoresTheBinaryTree(t *testing.T) {
	before := RepoRoot()
	dir := t.TempDir()
	restore := PatchRepoRoot(dir)
	if RepoRoot() != dir {
		t.Fatalf("patched root %q", RepoRoot())
	}
	restore()
	if RepoRoot() != before {
		t.Fatalf("restored root %q want %q", RepoRoot(), before)
	}
}
