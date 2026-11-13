package native

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/install"
)

var windowsDrivePath = regexp.MustCompile(`^[A-Za-z]:[\\/]|^/mnt/[a-z]/.*\.exe$`)

// Command path of a name on PATH or an existing explicit path, empty when neither resolves
func findOnPath(command string) string {
	if command == "" {
		return ""
	}
	if filepath.IsAbs(command) || strings.ContainsRune(command, os.PathSeparator) || (runtime.GOOS == "windows" && strings.Contains(command, "/")) {
		if _, err := os.Stat(command); err != nil {
			return ""
		}
		return command
	}
	found, err := exec.LookPath(command)
	if err != nil {
		return ""
	}
	return found
}

func isCommandScript(command string) bool {
	lower := strings.ToLower(command)
	return runtime.GOOS == "windows" && (strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat"))
}

// Whether a candidate CLI path is this desktop executable itself, which Windows PATH lookups can return
func looksLikeDesktopAppBinary(candidate string) bool {
	if runtime.GOOS != "windows" || candidate == "" {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	norm := func(p string) string { return strings.ToLower(filepath.Clean(p)) }
	return norm(candidate) == norm(self)
}

// DesktopResolveOptions are what resolving the desktop backend reads besides the backend args
type DesktopResolveOptions struct {
	Payload         *backend.PayloadInfo
	Home            backend.HomeInputs
	IcebathHome     string
	Env             func(string) string
	IsWSL           bool
	Log             func(string)
	Gate            *backend.InstalledRuntimeGate[backend.ResolvedBackend]
	RepairRequested bool
}

// Backend the desktop launches, the bundled payload, the CLI override, the active install, a user bin install, else a bootstrap
func ResolveDesktopBackend(ctx context.Context, args []string, o DesktopResolveOptions) backend.ResolvedBackend {
	if o.Payload != nil {
		return backend.BundledPayloadBackend(o.Payload, args, o.Home, o.Log)
	}
	if resolved, ok := resolveCLIOverride(args, o.Env, o.IsWSL, o.Log); ok {
		return resolved
	}
	launcher := func(root string) string { return install.ResolveInstalledCommand(ctx, root) }
	activeRoot := install.InstallRootOf(o.IcebathHome)
	active, _ := o.Gate.Resolve(activeRoot, func() (*backend.ResolvedBackend, error) {
		return backend.ResolveSourceInstallationBackend(activeRoot, args, launcher, o.Home), nil
	})
	state := backend.ActiveRuntimeStateOf(activeRoot, active != nil)
	if active != nil && !o.RepairRequested {
		if !state.HasValidMarker {
			o.Log(fmt.Sprintf("[bootstrap] Active Icebath runtime at %s is usable but the bootstrap marker is missing or stale; skipping first-run bootstrap.", activeRoot))
		}
		return *active
	}
	if o.RepairRequested {
		o.Log("[bootstrap] repair requested; bypassing the usable active runtime to re-run the installer")
	} else if userLauncher, userRoot := backend.UserLauncherInstallRoot(runtime.GOOS == "windows", o.IcebathHome, publishesInstallBinary); userRoot != "" {
		userBackend, _ := o.Gate.Resolve(userRoot, func() (*backend.ResolvedBackend, error) {
			return backend.ResolveSourceInstallationBackend(userRoot, args, launcher, o.Home), nil
		})
		if userBackend != nil {
			o.Log(fmt.Sprintf("[boot] Using Icebath install at %s (published launcher %s)", userRoot, userLauncher))
			return *userBackend
		}
		o.Log(fmt.Sprintf("[bootstrap] Icebath install at %s (from %s) is not usable", userRoot, userLauncher))
	} else {
		o.Log(fmt.Sprintf("[bootstrap] no usable Icebath install at %s and no published user-bin launcher", activeRoot))
	}
	return backend.ResolvedBackend{BackendCommand: backend.BackendCommand{Args: args, Env: map[string]string{}}, Kind: "bootstrap-needed", Label: "Icebath Agent not installed yet; bootstrap required", Bootstrap: true}
}

// Deployment override of the Icebath CLI, refused under WSL for a Windows path and for this desktop executable
func resolveCLIOverride(args []string, env func(string) string, isWSL bool, log func(string)) (backend.ResolvedBackend, bool) {
	override := env("ICEBATH_DESKTOP_ICEBATH")
	if override == "" {
		return backend.ResolvedBackend{}, false
	}
	command := findOnPath(override)
	if command == "" {
		if isWSL && windowsDrivePath.MatchString(override) {
			log("Ignoring Windows Icebath override under WSL: " + override)
		} else {
			command = override
		}
	}
	if command == "" {
		return backend.ResolvedBackend{}, false
	}
	if looksLikeDesktopAppBinary(command) {
		log("Ignoring desktop app executable on PATH while resolving Icebath CLI: " + command)
		return backend.ResolvedBackend{}, false
	}
	shell := isCommandScript(command)
	if backend.ShouldTrustHermesOverride(override) || backend.VerifyHermesCli(command, backend.ResolveProbeTimeout(env)) {
		return backend.ResolvedBackend{BackendCommand: backend.BackendCommand{Command: command, Args: args, Env: map[string]string{}, Shell: shell},
			Kind: "command", Label: "existing Icebath CLI at " + command}, true
	}
	log("Ignoring existing Icebath CLI at " + command + ": --version probe failed; falling through to bootstrap.")
	return backend.ResolvedBackend{}, false
}

// Whether a root holds the launcher or binary an Icebath install publishes
func publishesInstallBinary(root string) bool {
	names := []string{"icebath"}
	if runtime.GOOS == "windows" {
		names = []string{"icebath.exe", "icebath.cmd"}
	}
	for _, name := range names {
		if directoryEntryIsFile(filepath.Join(root, ".icebath", "bin", name)) {
			return true
		}
	}
	return directoryEntryIsFile(install.InstallBinaryPath(root))
}

func directoryEntryIsFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func directoryExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// Electron platform name of this build
func electronPlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// Launcher an installation root publishes for itself, empty when it has none
func ResolveInstallationLauncher(root string) string {
	return install.ResolveInstalledCommand(context.Background(), root)
}
