package connections

import (
	"regexp"
	"strings"
)

var (
	gpuOverrideOn  = map[string]bool{"1": true, "true": true, "yes": true, "on": true}
	gpuOverrideOff = map[string]bool{"0": true, "false": true, "no": true, "off": true}
	rdpSessionName = regexp.MustCompile(`(?i)^rdp-`)
)

// DetectRemoteDisplay names the remote or forwarded display that needs software rendering, empty for a local one
func DetectRemoteDisplay(env func(string) string, platform string) string {
	override := strings.ToLower(strings.TrimSpace(env("ICEBATH_DESKTOP_DISABLE_GPU")))
	if gpuOverrideOn[override] {
		return "override (ICEBATH_DESKTOP_DISABLE_GPU)"
	}
	if gpuOverrideOff[override] {
		return ""
	}
	if env("SSH_CONNECTION") != "" || env("SSH_CLIENT") != "" || env("SSH_TTY") != "" {
		return "ssh-session"
	}
	if platform == "linux" {
		display := env("DISPLAY")
		if host, _, found := strings.Cut(display, ":"); found && host != "" {
			return "x11-forwarding (DISPLAY=" + display + ")"
		}
	}
	if platform == "win32" {
		if session := env("SESSIONNAME"); rdpSessionName.MatchString(session) {
			return "rdp (SESSIONNAME=" + session + ")"
		}
	}
	return ""
}
