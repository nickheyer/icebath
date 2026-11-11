package selfupdate

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	peMachineI386  = 0x014C
	peMachineAMD64 = 0x8664
	peMachineARM64 = 0xAA64
	// Flag GetMachineTypeAttributes sets for a machine type user mode code can run
	machineAttributeUserEnabled = 0x00000001
	// Linux desktop entry of the app id and the entry name it replaced
	desktopEntryName       = "com.nousresearch.icebath.desktop"
	legacyDesktopEntryName = "icebath.desktop"
)

// True when a local desktop renderer build is present
func desktopDistExists(desktopDir string) bool {
	_, err := os.Stat(filepath.Join(desktopDir, "dist", "index.html"))
	return err == nil
}

// Unpacked Electron executable of this platform under the release dir, false when none exists
var desktopPackagedExecutable = func(desktopDir string) (string, bool) {
	return desktopPackagedExecutableIn(filepath.Join(desktopDir, "release"))
}

// Unpacked Electron executable under a release or staging dir, the newest of the loadable candidates
func desktopPackagedExecutableIn(releaseDir string) (string, bool) {
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates, _ = filepath.Glob(filepath.Join(releaseDir, "mac*", "Icebath.app", "Contents", "MacOS", "Icebath"))
	case "windows":
		for _, d := range []string{"win-unpacked", "win-ia32-unpacked", "win-arm64-unpacked"} {
			candidates = append(candidates, filepath.Join(releaseDir, d, "Icebath.exe"))
		}
	default:
		for _, d := range []string{"linux-unpacked", "linux-arm64-unpacked"} {
			for _, n := range []string{"icebath", "Icebath"} {
				candidates = append(candidates, filepath.Join(releaseDir, d, n))
			}
		}
	}
	var existing []string
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			existing = append(existing, p)
		}
	}
	if len(existing) == 0 {
		return "", false
	}
	if runtime.GOOS == "windows" && len(existing) > 1 {
		expected := expectedWindowsPEMachines()
		var matching []string
		for _, p := range existing {
			if machine, ok := peMachineOrNone(p); ok && expected[machine] {
				matching = append(matching, p)
			}
		}
		if len(matching) > 0 {
			existing = matching
		}
	}
	best, bestTime := existing[0], int64(-1)
	for _, p := range existing {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if t := info.ModTime().UnixNano(); t > bestTime {
			best, bestTime = p, t
		}
	}
	return best, true
}

// Native Windows machine name in upper case PE vocabulary
func windowsNativeMachine() string {
	if runtime.GOOS == "windows" {
		switch hostinfo.NativeArch() {
		case "arm64":
			return "ARM64"
		case "amd64":
			return "AMD64"
		case "x86":
			return "X86"
		}
	}
	return strings.ToUpper(pyplatform.Machine())
}

// PE machines this Windows host loads, by the runnable machine query else by native name
func expectedWindowsPEMachines() map[uint16]bool {
	if runtime.GOOS == "windows" {
		if runnable, err := windowsUserRunnablePEMachines(); err == nil && len(runnable) > 0 {
			return runnable
		}
	}
	switch windowsNativeMachine() {
	case "AMD64", "X86_64", "X64":
		return map[uint16]bool{peMachineAMD64: true, peMachineI386: true}
	case "ARM64", "AARCH64":
		return map[uint16]bool{peMachineARM64: true, peMachineAMD64: true}
	case "X86", "I386", "I486", "I586", "I686":
		return map[uint16]bool{peMachineI386: true}
	}
	return map[uint16]bool{peMachineAMD64: true, peMachineARM64: true, peMachineI386: true}
}

// COFF machine field of a structurally complete PE file, a readable reason when it is not one
func parsePEMachine(path string) (uint16, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("unreadable: %s", pypath.OSErrorText(err))
	}
	size := info.Size()
	if size < 512 {
		return 0, fmt.Errorf("file is only %d bytes — far too small to be a Windows executable", size)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	head := make([]byte, 64)
	n, _ := io.ReadFull(f, head)
	if n < 64 || string(head[:2]) != "MZ" {
		return 0, errors.New("missing MZ header — not a Windows executable (a truncated or non-binary file saved as .exe?)")
	}
	lfanew := int64(binary.LittleEndian.Uint32(head[0x3C:]))
	if lfanew <= 0 || lfanew+24 > size {
		return 0, errors.New("corrupt DOS header: PE header offset points past end of file")
	}
	peHead := make([]byte, 24)
	if _, err := f.Seek(lfanew, io.SeekStart); err != nil {
		return 0, err
	}
	n, _ = io.ReadFull(f, peHead)
	if n < 24 || string(peHead[:4]) != "PE\x00\x00" {
		return 0, errors.New("missing PE signature — corrupt executable header")
	}
	machine := binary.LittleEndian.Uint16(peHead[4:])
	sections := int(binary.LittleEndian.Uint16(peHead[6:]))
	optional := int64(binary.LittleEndian.Uint16(peHead[20:]))
	if _, err := f.Seek(lfanew+24+optional, io.SeekStart); err != nil {
		return 0, err
	}
	var maxEnd int64
	section := make([]byte, 40)
	for range sections {
		if n, _ := io.ReadFull(f, section); n < 40 {
			return 0, errors.New("truncated PE section table")
		}
		raw, pointer := int64(binary.LittleEndian.Uint32(section[16:])), int64(binary.LittleEndian.Uint32(section[20:]))
		maxEnd = max(maxEnd, pointer+raw)
	}
	if size < maxEnd {
		return 0, fmt.Errorf("truncated executable: file is %d bytes but its PE sections extend to %d bytes", size, maxEnd)
	}
	return machine, nil
}

// PE machine of a file, false when it is not a complete PE
func peMachineOrNone(path string) (uint16, bool) {
	machine, err := parsePEMachine(path)
	return machine, err == nil
}

// Path of an environment variable when set, else the fallback
func envDir(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// Standard install locations of the packaged desktop app on this OS, existing or not
func packagedGUIAppPaths() []string {
	userHome, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return []string{"/Applications/Icebath.app", filepath.Join(userHome, "Applications", "Icebath.app")}
	case "windows":
		local := envDir("LOCALAPPDATA", filepath.Join(userHome, "AppData", "Local"))
		out := []string{filepath.Join(local, "Programs", "Icebath"), filepath.Join(local, "icebath-desktop")}
		if programFiles := os.Getenv("ProgramFiles"); programFiles != "" {
			out = append(out, filepath.Join(programFiles, "Icebath"))
		}
		return out
	}
	dataBase := envDir("XDG_DATA_HOME", filepath.Join(userHome, ".local", "share"))
	xdgData := filepath.Join(userHome, ".local", "share")
	if raw := os.Getenv("XDG_DATA_HOME"); pystr.Strip(raw) != "" {
		xdgData = pypath.Expanduser(raw)
	}
	icons := filepath.Join(dataBase, "icons", "hicolor")
	out := []string{filepath.Join(xdgData, "applications", desktopEntryName),
		filepath.Join(dataBase, "applications", legacyDesktopEntryName), filepath.Join(dataBase, "applications", "Icebath.desktop")}
	for _, size := range []string{"scalable", "24x24", "32x32", "48x48", "256x256", "512x512", "1024x1024"} {
		out = append(out, filepath.Join(icons, size, "apps", "icebath.png"))
	}
	return out
}

// Record of the installed app copies an update keeps current, under the default root
func desktopInstallRecord() string { return filepath.Join(home.Root(), "desktop-installed-apps.json") }

// Existing bundles whose install stamp says only the update keeps them current
func updateOwnedMacosBundles(candidates []string) []string {
	var owned []string
	for _, app := range candidates {
		raw, err := os.ReadFile(filepath.Join(app, "Contents", "Resources", "install-stamp.json"))
		if err != nil {
			continue
		}
		text, err := pystr.DecodeUTF8Sig(raw)
		if err != nil {
			continue
		}
		stamp, err := pyjson.Loads(text)
		if err != nil {
			continue
		}
		obj, ok := stamp.(*pyjson.Object)
		if ok && obj.GetOr("updateMechanism", "self") == "self" {
			owned = append(owned, app)
		}
	}
	return owned
}

// True on macOS when this checkout is the one the packaged app runs under the default home
func ownsInstalledDesktopApps() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	project, err1 := pypath.Resolve(fleetProjectRoot())
	expected, err2 := pypath.Resolve(filepath.Join(home.Root(), "icebath-agent"))
	return err1 == nil && err2 == nil && project == expected
}

// Installed macOS app bundles this checkout's update owns, else recorded copies that went missing
func installedDesktopApps() []string {
	if !ownsInstalledDesktopApps() {
		return []string{}
	}
	candidates := packagedGUIAppPaths()
	if owned := updateOwnedMacosBundles(candidates); len(owned) > 0 {
		return owned
	}
	recorded, ok := pyjson.ReadObjectOrEmpty(desktopInstallRecord()).GetOr("apps", nil).([]any)
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, app := range candidates {
		if slices.Contains(recorded, any(app)) {
			if _, err := os.Stat(app); err != nil {
				out = append(out, app)
			}
		}
	}
	return out
}
