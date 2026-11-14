package pyplatform

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestSystemAndMachineMatchUname(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Setenv("PROCESSOR_ARCHITEW6432", "")
		t.Setenv("PROCESSOR_ARCHITECTURE", "AMD64")
		if System() != "Windows" || Machine() != "AMD64" {
			t.Fatalf("got %q %q", System(), Machine())
		}
		return
	}
	for flag, got := range map[string]string{"-s": System(), "-m": Machine()} {
		out, err := exec.Command("uname", flag).Output()
		if err != nil {
			t.Fatalf("uname %s: %v", flag, err)
		}
		if want := strings.TrimSpace(string(out)); got != want {
			t.Fatalf("uname %s = %q, got %q", flag, want, got)
		}
	}
}

func TestReleaseNamesTheKernelOrProductRelease(t *testing.T) {
	if Release() == "" {
		t.Fatal("empty release")
	}
}
