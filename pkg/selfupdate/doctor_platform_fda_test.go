package selfupdate

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Installs a host on the platform with a temp home and TCC directory, the probe failing with denied when set
func fakeTCCHost(t *testing.T, platform string, makeDir bool, denied error) string {
	t.Helper()
	userHome := t.TempDir()
	tcc := filepath.Join(userHome, "Library", "Application Support", "com.apple.TCC")
	if makeDir {
		if err := os.MkdirAll(tcc, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prevPlatform, prevHome, prevList := doctorSysPlatform, doctorUserHomeDir, doctorListDir
	doctorSysPlatform = platform
	doctorUserHomeDir = func() (string, error) { return userHome, nil }
	if denied != nil {
		doctorListDir = func(path string) error {
			if path == tcc {
				return denied
			}
			return prevList(path)
		}
	}
	t.Cleanup(func() { doctorSysPlatform, doctorUserHomeDir, doctorListDir = prevPlatform, prevHome, prevList })
	return tcc
}

// Doctor output of the FDA check
func fdaDoctorOutput() string {
	var out bytes.Buffer
	if err := CheckMacosFullDiskAccess(&out); err != nil {
		panic(err)
	}
	return out.String()
}

var permissionDenied = &fs.PathError{Op: "open", Path: "com.apple.TCC", Err: fs.ErrPermission}

func TestDoctorReportsGrantedWithoutGuidance(t *testing.T) {
	fakeTCCHost(t, "darwin", true, nil)
	out := fdaDoctorOutput()
	if !strings.Contains(out, "Full Disk Access granted") || strings.Contains(out, "Privacy_AllFiles") {
		t.Fatalf("screen %q", out)
	}
	if out != "  ✓ macOS Full Disk Access granted (no per-folder permission prompts will occur)\n" {
		t.Fatalf("screen %q", out)
	}
}

func TestDoctorPrintsOneSwitchGuidanceWhenDenied(t *testing.T) {
	fakeTCCHost(t, "darwin", true, permissionDenied)
	out := fdaDoctorOutput()
	if !strings.Contains(out, "Privacy_AllFiles") || strings.Contains(out, "granted") {
		t.Fatalf("screen %q", out)
	}
	want := "    → One switch silences all macOS folder prompts: grant your terminal app Full Disk Access and Icebath will never trip " +
		"per-folder dialogs (Desktop/Downloads/Documents/...) again. Open: System Settings → Privacy & Security → Full Disk Access — or run:\n" +
		"      open \"x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles\"\n" +
		"    then enable your terminal (and Icebath.app if you use Desktop), and restart them once. " +
		"With Icebath's stable signing identities the grant survives every update.\n"
	if out != want {
		t.Fatalf("screen\n%q\nwant\n%q", out, want)
	}
}

func TestDoctorIsSilentWhenProbeIsIndeterminate(t *testing.T) {
	fakeTCCHost(t, "darwin", false, nil)
	if out := fdaDoctorOutput(); out != "" {
		t.Fatalf("screen %q", out)
	}
}

func TestDoctorIsSilentOffMacos(t *testing.T) {
	fakeTCCHost(t, "linux", true, permissionDenied)
	if out := fdaDoctorOutput(); out != "" {
		t.Fatalf("screen %q", out)
	}
}
