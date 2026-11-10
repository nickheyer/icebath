package pm

import (
	"errors"
	"io"
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

// Hidden version probe with captured output that must exit and close its pipes within ten seconds, false when it could not run
var runVersionProbe = func(argv []string, env map[string]string) (int, bool) {
	cmd := exec.Command(argv[0], argv[1:]...)
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
	outR, outW, err := os.Pipe()
	if err != nil {
		return 0, false
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return 0, false
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	started := cmd.Start()
	outW.Close()
	errW.Close()
	defer outR.Close()
	defer errR.Close()
	if started != nil {
		return 0, false
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	drained := make(chan struct{}, 2)
	for _, r := range []*os.File{outR, errR} {
		go func(r *os.File) {
			_, _ = io.Copy(io.Discard, r)
			drained <- struct{}{}
		}(r)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	var waitErr error
	gotExit := false
	for pending := 3; pending > 0; pending-- {
		select {
		case waitErr = <-exited:
			gotExit = true
		case <-drained:
		case <-deadline.C:
			_ = cmd.Process.Kill()
			if !gotExit {
				<-exited
			}
			return 0, false
		}
	}
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		return exit.ExitCode(), true
	}
	if waitErr != nil {
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
