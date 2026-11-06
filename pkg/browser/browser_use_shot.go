package browser

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

//go:embed browser_use_shot.py
var browserUseShotSource []byte

// Which browser the harness helpers may activate tabs on, chosen by browser_exec from its route
const (
	activateNever   = "never"
	activateAlways  = "always"
	activateIfCloud = "if-cloud"
)

const browserUseShotFile = "browser_use_shot.py"

var harnessModuleArgs = []string{"-m", "browser_harness.run"}

const harnessLauncher = "import importlib.util, runpy\n" +
	"spec = importlib.util.spec_from_file_location('icebath_browser_use_shot', %s)\n" +
	"shot = importlib.util.module_from_spec(spec); spec.loader.exec_module(shot)\n" +
	"import browser_harness.helpers\n" +
	"shot.install(browser_harness.helpers, activate=%s)\n" +
	"runpy.run_module('browser_harness.run', run_name='__main__', alter_sys=True)\n"

// Writes the embedded helper module into dir by rename when its content differs and returns its absolute path
func WriteBrowserUseShot(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path, err := pypath.Abspath(filepath.Join(dir, browserUseShotFile))
	if err != nil {
		return "", err
	}
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, browserUseShotSource) {
		return path, nil
	}
	tmp, err := os.CreateTemp(dir, ".browser_use_shot_*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(browserUseShotSource); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// The harness module command as a launcher that installs the shot helpers first, any other command unchanged
func harnessCommand(cmd []string, activate, shotPath string) ([]string, error) {
	if len(cmd) == 0 || !slices.Equal(cmd[1:], harnessModuleArgs) {
		return cmd, nil
	}
	abs, err := pypath.Abspath(shotPath)
	if err != nil {
		return nil, err
	}
	return []string{cmd[0], "-c", fmt.Sprintf(harnessLauncher, pyrepr.StrRepr(abs), pyrepr.StrRepr(activate))}, nil
}
