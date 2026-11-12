package native

import (
	"context"
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

// Icebath command the desktop runs, the deployment override first, then the published install, else a bootstrap
func ResolveIcebathBackend(ctx context.Context, args []string, icebathHome string, env func(string) string, isWSL bool, log func(string)) backend.ResolvedBackend {
	if override := env("ICEBATH_DESKTOP_ICEBATH"); override != "" {
		command := findOnPath(override)
		if command == "" {
			if isWSL && windowsDrivePath.MatchString(override) {
				log("Ignoring Windows Icebath override under WSL: " + override)
			} else {
				command = override
			}
		}
		if command != "" {
			if looksLikeDesktopAppBinary(command) {
				log("Ignoring desktop app executable on PATH while resolving Icebath CLI: " + command)
			} else if backend.ShouldTrustHermesOverride(override) {
				return backend.ResolvedBackend{BackendCommand: backend.BackendCommand{Command: command, Args: args, Env: map[string]string{}, Shell: isCommandScript(command)},
					Kind: "command", Label: "existing Icebath CLI at " + command}
			}
		}
	}
	root := install.InstallRootOf(icebathHome)
	if command := install.ResolveInstalledCommand(ctx, root); command != "" {
		log("[boot] Using Icebath install at " + root + " (published launcher " + command + ")")
		return backend.ResolvedBackend{BackendCommand: backend.BackendCommand{Command: command, Args: args, Env: map[string]string{}, Shell: isCommandScript(command)},
			Kind: "command", Label: "Icebath install at " + root}
	}
	return backend.ResolvedBackend{BackendCommand: backend.BackendCommand{Args: args, Env: map[string]string{}}, Kind: "bootstrap-needed", Label: "Icebath bootstrap", Bootstrap: true}
}
