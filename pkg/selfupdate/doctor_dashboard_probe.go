package selfupdate

import (
	"context"
	"os"
)

// Argv of a child of this binary that runs the dashboard entry through its status report and exits
func dashboardImportProbeArgv(context.Context, string) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, "dashboard", "--status"}, nil
}
