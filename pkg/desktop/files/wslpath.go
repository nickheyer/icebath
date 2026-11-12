package files

import (
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Host OS the WSL bridge and path rules branch on, runtime.GOOS in production
var hostOS = runtime.GOOS

var (
	wslMountRE    = regexp.MustCompile(`(?i)^/mnt/([a-z])(?:/(.*))?$`)
	shellDriveRE  = regexp.MustCompile(`(?i)^/(?:cygdrive/|mnt/)?([a-z])(?:/|$)`)
	winDriveSepRE = regexp.MustCompile(`^([A-Za-z]):[\\/]`)
)

const defaultWslBridgeProfile = "default"

// WSL bridge state per backend profile, set by the connection lifecycle
var wslBridge = struct {
	sync.Mutex
	profiles map[string]bool
	active   string
	distro   string
	uncBase  string
}{profiles: map[string]bool{}, active: defaultWslBridgeProfile}

func normalizeWslBridgeProfile(profile *string) string {
	if profile == nil {
		return defaultWslBridgeProfile
	}
	if p := strings.TrimSpace(*profile); p != "" {
		return p
	}
	return defaultWslBridgeProfile
}

// Selects the profile used by callers that cannot name one
func SetActiveGatewayProfile(profile *string) {
	wslBridge.Lock()
	defer wslBridge.Unlock()
	wslBridge.active = normalizeWslBridgeProfile(profile)
}

// Records whether paths returned by one profile belong to this host's WSL
func SetWslBridgeProfileState(profile *string, active bool) {
	wslBridge.Lock()
	defer wslBridge.Unlock()
	wslBridge.profiles[normalizeWslBridgeProfile(profile)] = active
}

// Updates only the current fallback profile
func SetWslBridgeActive(active bool) {
	wslBridge.Lock()
	key := wslBridge.active
	wslBridge.Unlock()
	SetWslBridgeProfileState(&key, active)
}

// True when the profile's paths are this host's WSL paths, unknown profiles default to true
func IsWslBridgeActive(profile *string) bool {
	wslBridge.Lock()
	defer wslBridge.Unlock()
	key := wslBridge.active
	if profile != nil {
		key = normalizeWslBridgeProfile(profile)
	}
	if v, ok := wslBridge.profiles[key]; ok {
		return v
	}
	return true
}

// Default distro from wsl.exe -l -q output, NUL bytes of UTF-16 output stripped
func ParseDefaultDistro(raw string) string {
	for _, line := range regexp.MustCompile(`\r?\n`).Split(strings.ReplaceAll(raw, "\x00", ""), -1) {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimPrefix(line, "*"), " \t\r\n\f\v"))
		if line != "" {
			return line
		}
	}
	return ""
}

// Runs wsl.exe -l -q with UTF-8 output and discarded stderr
var listWslDistros = func() (string, error) {
	cmd := exec.Command("wsl.exe", "-l", "-q")
	cmd.Env = append(os.Environ(), "WSL_UTF8=1")
	hideConsole(cmd)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
		return string(out), err
	case <-time.After(2 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-done
		return "", os.ErrDeadlineExceeded
	}
}

// Default WSL distro name, cached, Ubuntu when none answers
func ResolveDefaultWslDistro() string {
	wslBridge.Lock()
	cached := wslBridge.distro
	wslBridge.Unlock()
	if cached != "" {
		return cached
	}
	distro := "Ubuntu"
	if hostOS == "windows" {
		if out, err := listWslDistros(); err == nil {
			if d := ParseDefaultDistro(out); d != "" {
				distro = d
			}
		}
	}
	wslBridge.Lock()
	wslBridge.distro = distro
	wslBridge.Unlock()
	return distro
}

func wslUncBase(distro string) string {
	wslBridge.Lock()
	cached := wslBridge.uncBase
	wslBridge.Unlock()
	if cached != "" {
		return cached
	}
	base := `\\wsl.localhost\` + distro
	legacy := `\\wsl$\` + distro
	if !pathExists(base) && pathExists(legacy) {
		base = legacy
	}
	wslBridge.Lock()
	wslBridge.uncBase = base
	wslBridge.Unlock()
	return base
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// A WSL POSIX path as the Windows host opens it, drvfs mounts to drives and the rest to the UNC share
func WslPosixToWindowsAccessible(posixPath, distro string, resolveDistro func() string) string {
	value := strings.TrimSpace(posixPath)
	normalized := strings.ReplaceAll(value, `\`, "/")
	if !strings.HasPrefix(normalized, "/") {
		return value
	}
	if m := wslMountRE.FindStringSubmatch(normalized); m != nil {
		tail := strings.ReplaceAll(m[2], "/", `\`)
		if tail != "" {
			return strings.ToUpper(m[1]) + `:\` + tail
		}
		return strings.ToUpper(m[1]) + `:\`
	}
	relative := strings.ReplaceAll(strings.TrimLeft(normalized, "/"), "/", `\`)
	if distro == "" {
		if resolveDistro == nil {
			resolveDistro = ResolveDefaultWslDistro
		}
		distro = resolveDistro()
	}
	return wslUncBase(distro) + `\` + relative
}

// Native folder dialog default path, a WSL cwd opened in the Windows picker
func ResolvePickerDefaultPath(defaultPath, distro string, profile *string) string {
	if defaultPath == "" {
		return ""
	}
	if !IsWslBridgeActive(profile) {
		return defaultPath
	}
	value := strings.TrimSpace(defaultPath)
	if strings.HasPrefix(value, "/") && !winDriveSepRE.MatchString(value) {
		return WslPosixToWindowsAccessible(value, distro, nil)
	}
	return defaultPath
}

// Git Bash, Cygwin or drvfs drive spelling as the native drive path, only when that file exists
func ShellDriveToNativePath(value string, exists func(string) bool) string {
	m := shellDriveRE.FindStringSubmatchIndex(value)
	if m == nil {
		return ""
	}
	native := strings.ToUpper(value[m[2]:m[3]]) + ":/" + value[m[1]:]
	if exists == nil {
		exists = pathExists
	}
	if exists(native) {
		return native
	}
	return ""
}

// fs read path, a WSL cwd made readable on Windows through its UNC or drive form
func ResolveLocalReadPath(dirPath, distro string, profile *string) string {
	value := strings.TrimSpace(dirPath)
	if hostOS == "windows" {
		if native := ShellDriveToNativePath(value, nil); native != "" {
			return native
		}
	}
	if !IsWslBridgeActive(profile) {
		return value
	}
	if hostOS == "windows" && strings.HasPrefix(value, "/") && !winDriveSepRE.MatchString(value) {
		return WslPosixToWindowsAccessible(value, distro, nil)
	}
	return value
}
