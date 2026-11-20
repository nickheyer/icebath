//go:build unix && !linux

package cron

import (
	"os/exec"
	"strconv"
	"strings"
)

// Running and neither zombie nor gone per the ps state column
func scriptProcessLive(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	state := strings.TrimSpace(string(out))
	return err == nil && state != "" && !strings.HasPrefix(state, "Z")
}
