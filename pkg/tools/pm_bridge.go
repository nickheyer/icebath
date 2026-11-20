package tools

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Composed environment of installed managed packages over base, never installing
func pmEnvFor(names []string, base map[string]string) map[string]string {
	return pm.EnvFor(names, base)
}

// Ensures a managed package under the lazy install policy and returns its runner's environment over base
func pmEnsureEnv(name string, base map[string]string) (map[string]string, error) {
	runner, err := pm.Ensure(context.Background(), name, pm.EnsureOptions{BaseEnv: base})
	if err != nil {
		return nil, err
	}
	return runner.Env, nil
}

// Binary of an installed managed package, false when it is not installed or PM cannot answer
func pmInstalledBinary(name string) (string, bool) {
	selected, err := pm.Installed(context.Background(), name, false)
	if err != nil || selected == nil {
		return "", false
	}
	return selected.Binary, true
}

// Validated executable of an installed isolated Python tool, false when none is selected
func pmPythonTool(name, bin string) (string, bool, error) {
	return pm.PythonTool(name, bin, "")
}

// Installs an isolated Python tool from requirements and returns its validated executable
func pmEnsurePythonTool(name string, requirements []string, bin string, timeout time.Duration) (string, error) {
	return pm.EnsurePythonTool(context.Background(), pm.EnsurePythonToolOptions{
		Name: name, Requirements: requirements, Executable: bin, Timeout: timeout,
	})
}
