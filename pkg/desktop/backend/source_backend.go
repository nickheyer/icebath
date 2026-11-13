package backend

import (
	"context"
	"os"
	"runtime"
	"strings"
)

// Runs a probe hidden in cwd with env and stdio ignored, nil only for a clean exit within the timeout
var runInstallationProbe = func(command string, args []string, cwd string, env map[string]string, shell bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), ResolveProbeTimeout(os.Getenv))
	defer cancel()
	cmd := spawnCommand(command, args, shell)
	cmd.Dir = cwd
	cmd.Env = envList(env)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		terminateProcess(cmd.Process)
		<-done
		return ctx.Err()
	}
}

// Backend of an installation root whose published launcher answers --version, nil when it has none or it fails
func ResolveSourceInstallationBackend(root string, args []string, launcher func(root string) string, home HomeInputs) *ResolvedBackend {
	command := launcher(root)
	if command == "" {
		return nil
	}
	isWindows := runtime.GOOS == "windows"
	lower := strings.ToLower(command)
	shell := isWindows && (strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat"))
	if shell {
		command = `"` + command + `"`
	}
	env := BuildDesktopBackendEnv(home.Env, home.Platform, home)
	probeEnv := ProcessEnv()
	for k, v := range home.Env {
		probeEnv[k] = v
	}
	for k, v := range env {
		probeEnv[k] = v
	}
	if err := runInstallationProbe(command, []string{"--version"}, root, probeEnv, shell); err != nil {
		return nil
	}
	return &ResolvedBackend{BackendCommand: BackendCommand{Command: command, Args: append([]string(nil), args...), Env: env, Shell: shell}, Kind: "command", Label: "Icebath at " + root}
}
