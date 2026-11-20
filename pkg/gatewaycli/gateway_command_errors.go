package gatewaycli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

const journalHint = `journalctl --user -u icebath-gateway --since "5 min ago"`

var systemctlFailedLines = []string{
	"Could not {verb} the gateway service; systemd reported an error.",
	"See why with `icebath gateway status --deep` or `{journal}`.",
	"To reinstall the service run `icebath gateway install --force`.",
}

var noSystemctlLines = []string{
	"This system has no systemd, so Icebath cannot install a background service here.",
	"Run the gateway directly with `icebath gateway run` (keep it alive with tmux or screen).",
}

// First start, stop or restart token of the failed command, start when none appears
func verbFor(cmd []string) string {
	for _, token := range cmd {
		if token == "start" || token == "stop" || token == "restart" {
			return token
		}
	}
	return "start"
}

// Guidance lines for a systemd failure escaping the gateway command, nil for a failure it cannot explain
func ExplainServiceFailure(err error) []string {
	var unavailable gateway.SystemctlUnavailableError
	var unavailablePtr *gateway.SystemctlUnavailableError
	if errors.As(err, &unavailable) || errors.As(err, &unavailablePtr) {
		return append([]string{}, noSystemctlLines...)
	}
	var cmd []string
	var gwErr *gateway.CalledProcessError
	var spErr *pysubprocess.CalledProcessError
	switch {
	case errors.As(err, &gwErr):
		cmd, err = gwErr.Cmd, gwErr
	case errors.As(err, &spErr):
		cmd, err = spErr.Cmd, spErr
	default:
		return nil
	}
	lines := make([]string, 0, len(systemctlFailedLines)+1)
	for _, line := range systemctlFailedLines {
		lines = append(lines, strings.NewReplacer("{verb}", verbFor(cmd), "{journal}", journalHint).Replace(line))
	}
	return append(lines, fmt.Sprintf("Details: %s", err))
}
