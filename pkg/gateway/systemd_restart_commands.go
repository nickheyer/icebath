package gateway

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/errchain"
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
			return nil, fmt.Errorf("restart scope must be user or system, got %s", errchain.Kind(scope))
		}
	}
	return []string{userCmd, systemCmd}, nil
}
