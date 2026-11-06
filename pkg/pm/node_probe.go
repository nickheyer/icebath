package pm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Installed pm npm and its Node dependency composed over env without provisioning anything
func WithIcebathNodePath(env map[string]string) map[string]string {
	return EnvFor([]string{"npm"}, env)
}

// Hidden ten second version probe, its exit code and false when it could not run or timed out
var runVersionProbe = func(argv []string, env map[string]string) (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cmd.Env = make([]string, 0, len(keys))
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	hideWindow(cmd, true)
	err := cmd.Run()
	if ctx.Err() != nil {
		return 0, false
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), true
	}
	if err != nil {
		return 0, false
	}
	return 0, true
}

// Reports whether path --version exits zero under the managed Node PATH
func versionProbeOK(path string) bool {
	code, ok := runVersionProbe([]string{path, "--version"}, WithIcebathNodePath(nil))
	return ok && code == 0
}

// Reports whether path is a Node, npm or npx binary that answers a version probe
func NodeToolRunnable(path string) bool {
	if path == "" {
		return false
	}
	var present bool
	if runtime.GOOS == "windows" {
		st, err := os.Stat(path)
		present = err == nil && st.Mode().IsRegular()
	} else {
		present = home.IsExecutableFile(path)
	}
	return present && versionProbeOK(path)
}
