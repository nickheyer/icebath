package install

import (
	"path/filepath"
	"strings"
)

type AppMode string

const (
	ModeInstall AppMode = "install"
	ModeUpdate  AppMode = "update"
)

// Update when any argument is --update, install otherwise
func ModeFromArgs(args []string) AppMode {
	for _, a := range args {
		if a == "--update" {
			return ModeUpdate
		}
	}
	return ModeInstall
}

// Reports --reinstall or --repair, which force the installer UI over the launcher fast path
func ForceSetupFromArgs(args []string) bool {
	for _, a := range args {
		if a == "--reinstall" || a == "--repair" {
			return true
		}
	}
	return false
}

// Reports a bare macOS install launch, which relaunches an installed desktop before any window exists
func HandoffBeforeAppKit(isMacOS bool, mode AppMode, forceSetup bool) bool {
	return isMacOS && mode == ModeInstall && !forceSetup
}

// Value of --name or --name=value, false when absent
func argValue(args []string, name string) (string, bool) {
	for i := 0; i < len(args); i++ {
		if args[i] == name {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
		if v, ok := strings.CutPrefix(args[i], name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// Update flags naming the channel the desktop checked, else the branch, the build pin branch, then main
func UpdateTargetFromArgs(args []string) []string {
	value := func(name string) string {
		v, _ := argValue(args, name)
		return strings.TrimSpace(v)
	}
	if channel := value("--channel"); channel != "" {
		return []string{"--channel", channel}
	}
	branch := value("--branch")
	if branch == "" {
		branch = BuildPinBranch
	}
	if branch == "" {
		branch = "main"
	}
	return []string{"--branch", branch}
}

// The --target-app value when it names a .app bundle
func TargetAppFromArgs(args []string) string {
	v, ok := argValue(args, "--target-app")
	if !ok || filepath.Ext(v) != ".app" {
		return ""
	}
	return v
}

// Release pins baked in at build time with -ldflags -X
var (
	BuildPinCommit string
	BuildPinBranch string
)
