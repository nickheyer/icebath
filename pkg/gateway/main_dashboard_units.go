package gateway

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Commands restarting a systemd unit in its scope, both forms for an unknown scope, an error for an unhashable one
func SystemdRestartCommands(unit string, scope any) ([]string, error) {
	userCmd, systemCmd := "systemctl --user restart "+unit, "sudo systemctl restart "+unit
	if pyrepr.Truthy(scope) {
		switch key := scope.(type) {
		case string:
			if key == "user" {
				return []string{userCmd}, nil
			}
			if key == "system" {
				return []string{systemCmd}, nil
			}
		case map[string]any, []any:
			return nil, fmt.Errorf("TypeError: unhashable type: '%s'", pyrepr.TypeName(scope))
		}
	}
	return []string{userCmd, systemCmd}, nil
}

// Restart commands as a hint, each in backticks and joined by or
func FormatRestartCommands(commands []string) string {
	quoted := make([]string, len(commands))
	for i, c := range commands {
		quoted[i] = "`" + c + "`"
	}
	return strings.Join(quoted, " or ")
}
