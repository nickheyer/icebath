package process

import (
	"strings"

	"github.com/nickheyer/icebath/internal/shell"
)

const wslChainNote = "Spawned via a wsl[.exe] launcher: the recorded host PID is the short-lived " +
	"launcher, not the Linux-side workers. Inspect them with `wsl -e ps` / " +
	"`wsl --list --running` from the host."

func wslLauncherCommand(command string) bool {
	if strings.TrimSpace(command) == "" {
		return false
	}
	var candidates []string
	if words, err := shell.Split(command); err == nil {
		if len(words) > 0 {
			candidates = append(candidates, words[0])
		} else {
			candidates = append(candidates, "")
		}
	}
	if fields := strings.Fields(command); len(fields) > 0 {
		candidates = append(candidates, fields[0])
	}
	for _, first := range candidates {
		first = strings.ReplaceAll(first, "\\", "/")
		base := first[strings.LastIndex(first, "/")+1:]
		base = strings.ToLower(strings.Trim(base, "'\""))
		if base == "wsl" || base == "wsl.exe" {
			return true
		}
	}
	return false
}
