// The computer use driver, cua-driver, shipped by default wherever upstream builds it
package packages

import (
	_ "embed"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
)

// Name of the driver's package
const CuaDriver = "cua-driver"

// Variables that keep the driver from sending usage data, and from checking for its own updates
var (
	TelemetryOff = map[string]string{"CUA_DRIVER_RS_TELEMETRY_ENABLED": "false", "CUA_TELEMETRY_ENABLED": "false", "CUA_TELEMETRY": "0", "DO_NOT_TRACK": "1"}
	UpdateCheck  = "CUA_DRIVER_RS_UPDATE_CHECK"
)

//go:embed lock.json
var lock []byte

func Register(registry *packages.Registry) {
	registry.Register(packages.Declared{Tools: []packages.Package{cuaDriver(packages.Pins(lock)[CuaDriver])}})
}

// trycua's release builds: macOS takes the folder archive carrying the signed app, the other targets the bare binary
func cuaDriver(pin packages.Pin) packages.Package {
	gaps := map[string]string{packages.Bionic: "cua-driver has no build for Android"}
	for target := range packages.Musl {
		gaps[target] = "cua-driver has no build for Linux systems built on musl, such as Alpine"
	}
	return packages.Package{
		Name: CuaDriver, Optional: true, Default: true, Gaps: gaps, Pin: pin,
		Binary: map[string]string{
			"darwin-arm64": "CuaDriver.app/Contents/MacOS/cua-driver",
			"darwin-x64":   "CuaDriver.app/Contents/MacOS/cua-driver",
			"win32":        "cua-driver.exe",
			"posix":        "cua-driver",
		},
		URLs: func(version, target string) ([]string, error) {
			arch, ok := map[string]string{"darwin-x64": "darwin-universal", "darwin-arm64": "darwin-universal", "linux-x64": "linux-x86_64",
				"linux-arm64": "linux-arm64", "win32-x64": "windows-x86_64", "win32-arm64": "windows-arm64"}[target]
			if !ok {
				return nil, fmt.Errorf("cua-driver has no download for %s", target)
			}
			ext, variant := "tar.gz", "-binary"
			if strings.HasPrefix(target, "win32") {
				ext = "zip"
			}
			if strings.HasPrefix(target, "darwin") {
				variant = ""
			}
			return []string{fmt.Sprintf("https://github.com/trycua/cua/releases/download/cua-driver-rs-v%s/cua-driver-rs-%s-%s%s.%s", version, version, arch,
				variant, ext)}, nil
		},
		Latest: func(cfg *config.Config, target, locked string) ([]string, error) {
			return packages.GitHubTags(cfg, "trycua/cua", "cua-driver-rs-v")
		},
		// The release folder is hoisted and its bundled SDK dropped
		Stage: func(b *packages.Build) error {
			if err := packages.Flatten(b.Dir); err != nil {
				return err
			}
			return os.RemoveAll(filepath.Join(b.Dir, "sdk"))
		},
		// The smoke run is a first run, so it never mints telemetry state
		ProbeEnv: func() map[string]string {
			env := map[string]string{}
			for _, entry := range os.Environ() {
				if name, value, ok := strings.Cut(entry, "="); ok && name != "" {
					env[name] = value
				}
			}
			maps.Copy(env, TelemetryOff)
			env[UpdateCheck] = "false"
			return env
		},
	}
}
