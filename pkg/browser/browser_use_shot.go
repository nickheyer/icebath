package browser

import (
	"os"
	"slices"

	"github.com/nickheyer/icebath/pkg/browser/harnesshost"
)

// Which browser the harness helpers may activate tabs on, chosen by browser_exec from its route
const (
	activateNever   = harnesshost.ActivateNever
	activateAlways  = harnesshost.ActivateAlways
	activateIfCloud = harnesshost.ActivateIfCloud
)

var harnessModuleArgs = []string{"-m", "browser_harness.run"}

// The harness module command as this binary hosting that interpreter with the shot helpers installed, any other command unchanged
func harnessCommand(cmd []string, activate string) ([]string, error) {
	if len(cmd) == 0 || !slices.Equal(cmd[1:], harnessModuleArgs) {
		return cmd, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, harnesshost.Entry, "--python", cmd[0], "--activate", activate}, nil
}
