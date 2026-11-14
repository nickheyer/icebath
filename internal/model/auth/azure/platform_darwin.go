package azure

import (
	"os/exec"

	"github.com/nickheyer/icebath/internal/hostinfo"
)

func arcTokensDir() string { return "" }

func browserCommand(target string, _ *hostinfo.Host) *exec.Cmd { return exec.Command("open", target) }
