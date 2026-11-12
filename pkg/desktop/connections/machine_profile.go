package connections

import (
	"os"
	"os/user"
	"runtime"
	"strings"
	"time"
)

// MachineProfile is the machine facts the onboarding guide reads to tailor its first questions
type MachineProfile struct {
	AgeDays  *int   `json:"ageDays"`
	Arch     string `json:"arch"`
	Locale   string `json:"locale"`
	Platform string `json:"platform"`
	Release  string `json:"release"`
	Username string `json:"username"`
}

// appLocale is the application UI locale the shell reports, the operating system preference resolved against Chromium locales
var appLocale = func() string { return ApplicationLocale(preferredUILanguages()) }

// ReadMachineProfile reports home directory age, architecture, locale, platform, kernel release and login name
func ReadMachineProfile() MachineProfile {
	profile := MachineProfile{
		Arch:     machineArch(runtime.GOARCH),
		Locale:   appLocale(),
		Platform: machinePlatform(runtime.GOOS),
		Release:  osRelease(),
	}
	if home, err := os.UserHomeDir(); err == nil {
		if born, ok := fileBirthtime(home); ok && born.UnixNano() > 0 {
			elapsed := time.Now().UnixMilli()*int64(time.Millisecond) - born.UnixNano()
			days := int(max(0, elapsed/int64(24*time.Hour)))
			profile.AgeDays = &days
		}
	}
	if u, err := user.Current(); err == nil {
		profile.Username = u.Username
		if runtime.GOOS == "windows" {
			if i := strings.LastIndex(u.Username, `\`); i >= 0 {
				profile.Username = u.Username[i+1:]
			}
		}
	}
	return profile
}

// machinePlatform is the Node process.platform name of a GOOS
func machinePlatform(goos string) string {
	switch goos {
	case "windows":
		return "win32"
	case "solaris", "illumos":
		return "sunos"
	}
	return goos
}

// machineArch is the Node process.arch name of a GOARCH
func machineArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	case "mipsle":
		return "mipsel"
	case "mips64le":
		return "mips64el"
	case "ppc64le":
		return "ppc64"
	}
	return goarch
}
