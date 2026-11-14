package auth

import (
	"os/exec"

	"github.com/nickheyer/icebath/internal/proc"
)

func browserSession(cmd *exec.Cmd) { proc.NewSession(cmd) }

func platformBrowsers(func(string) string) []browserCandidate {
	return []browserCandidate{
		{key: "MacOSX", name: "default", argv: []string{"default"}, mode: launchOSAScript},
		{key: "chrome", name: "google chrome", argv: []string{"google chrome"}, mode: launchOSAScript},
		{key: "firefox", name: "firefox", argv: []string{"firefox"}, mode: launchOSAScript},
		{key: "safari", name: "safari", argv: []string{"safari"}, mode: launchOSAScript},
	}
}

func openNative(string) bool { return false }
