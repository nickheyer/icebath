package gateway

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestCmdlineCaptureReadsProcCmdlineWhenAvailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX /proc cmdline path")
	}
	procDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(procDir, "777"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "777", "cmdline"), []byte("/usr/bin/python3\x00-m\x00icebath_cli.main\x00serve\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	swapSeam(t, &scanProcDir, procDir)
	argv, ok := dashboardCmdlineForPID(context.Background(), 777)
	if !ok || !slices.Equal(argv, []string{"/usr/bin/python3", "-m", "icebath_cli.main", "serve"}) {
		t.Fatalf("argv = %v, %v", argv, ok)
	}
}

func TestCmdlineCaptureFallsBackToPsWithoutProc(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ps cmdline fallback")
	}
	swapSeam(t, &scanProcDir, t.TempDir())
	swapSeam(t, &RunServiceCommand, func(_ context.Context, _ time.Duration, argv ...string) (CommandResult, error) {
		if !slices.Equal(argv, []string{"ps", "-p", "888", "-o", "command="}) {
			t.Errorf("argv = %v", argv)
		}
		return CommandResult{Stdout: "icebath serve --port 8300\n"}, nil
	})
	argv, ok := dashboardCmdlineForPID(context.Background(), 888)
	if !ok || !slices.Equal(argv, []string{"icebath", "serve", "--port", "8300"}) {
		t.Fatalf("argv = %v, %v", argv, ok)
	}
}

func TestCmdlineCaptureReturnsNoneOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	if argv, ok := dashboardCmdlineForPID(context.Background(), 123); ok || argv != nil {
		t.Fatalf("argv = %v, %v", argv, ok)
	}
}
