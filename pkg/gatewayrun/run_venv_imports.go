package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Points a detached Windows gateway at the first install venv built for the pinned Python, skipping PM installs
func ensureWindowsGatewayVenvImports(ctx context.Context) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	projectRoot := pm.RepoRoot()
	committed, err := pm.CommittedVenv(ctx, projectRoot)
	if err != nil {
		return err
	}
	if committed != "" {
		return nil
	}
	var candidates []string
	if v := os.Getenv("VIRTUAL_ENV"); v != "" {
		candidates = append(candidates, v)
	}
	candidates = append(candidates, filepath.Join(projectRoot, "venv"))
	pinMajor, pinMinor, err := pm.PinnedPythonVersion()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, venvDir := range candidates {
		resolved := venvDir
		if abs, err := filepath.Abs(venvDir); err == nil {
			resolved = abs
			if real, err := filepath.EvalSymlinks(abs); err == nil {
				resolved = real
			}
		}
		key := strings.ToLower(resolved)
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, err := os.Stat(filepath.Join(resolved, "Lib", "site-packages")); err != nil {
			continue
		}
		major, minor, ok, err := pm.VenvPythonVersion(resolved)
		if err != nil {
			return err
		}
		if !ok || major != pinMajor || minor != pinMinor {
			found := "unknown"
			if ok {
				found = fmt.Sprintf("(%s, %s)", major, minor)
			}
			runLog().Debug(fmt.Sprintf("Skipping venv overlay %s: Python %s, gateway (%s, %s)", resolved, found, pinMajor, pinMinor))
			continue
		}
		return os.Setenv("VIRTUAL_ENV", resolved)
	}
	return nil
}
