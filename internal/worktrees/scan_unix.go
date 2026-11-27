//go:build !linux && !windows

package worktrees

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Hands each running process's folder, as lsof lists them, to see until it says it found what it looked for
func scan(see func(pid int, place string) bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-n", "-P", "-w", "-d", "cwd", "-F", "pn").Output()
	if len(out) == 0 && err != nil {
		return
	}
	pid, found := 0, false
	for line := range strings.Lines(string(out)) {
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
			found = false
		case strings.HasPrefix(line, "n") && pid > 0 && !found:
			found = see(pid, line[1:])
		}
	}
}
