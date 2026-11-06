package gatewaycli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkdirs(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWindowsTaskNamesAndStartupEntries(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", mkdirs(t, filepath.Join(tmp, "root", "profiles", "coder")))
	old := serviceGOOS
	t.Cleanup(func() { serviceGOOS = old })
	serviceGOOS = "linux"
	if _, err := GetTaskName(ctx); !errors.Is(err, errWindowsOnly) {
		t.Fatalf("task name off windows %v", err)
	}
	if _, _, _, err := ExecSchtasks([]string{"/Query"}); !errors.Is(err, errWindowsOnly) {
		t.Fatalf("schtasks off windows %v", err)
	}
	serviceGOOS = "windows"
	if name, err := GetTaskName(ctx); err != nil || name != "Icebath_Gateway_coder" {
		t.Fatalf("task name %q %v", name, err)
	}
	t.Setenv("APPDATA", " "+filepath.Join(tmp, "appdata")+" ")
	startup := filepath.Join(tmp, "appdata", "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
	if p, err := GetStartupEntryPath(ctx); err != nil || p != filepath.Join(startup, "Icebath_Gateway_coder.vbs") {
		t.Fatalf("startup %q %v", p, err)
	}
	t.Setenv("APPDATA", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOME", filepath.Join(tmp, "u"))
	if p, _ := LegacyStartupEntryPath(ctx); p != filepath.Join(tmp, "u", "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "Icebath_Gateway_coder.cmd") {
		t.Fatalf("legacy %q", p)
	}
	t.Setenv("HOME", "")
	if _, err := GetStartupEntryPath(ctx); err == nil || !strings.Contains(err.Error(), "neither APPDATA nor USERPROFILE") {
		t.Fatalf("no startup dir %v", err)
	}
	if got := sanitizeFilename("a<b>c:d\"e/f\\g|h?i*j\x01k"); got != "a_b_c_d_e_f_g_h_i_j_k" {
		t.Fatalf("sanitize %q", got)
	}
}
