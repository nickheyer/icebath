package azure

import (
	"os/exec"

	"github.com/nickheyer/icebath/internal/hostinfo"
)

func arcTokensDir() string { return "/var/opt/azcmagent/tokens" }

func browserCommand(target string, host *hostinfo.Host) *exec.Cmd {
	if host != nil && host.WSL() {
		return exec.Command("powershell.exe", "-NoProfile", "-Command", "Start-Process '"+target+"'")
	}
	return exec.Command("xdg-open", target)
}
