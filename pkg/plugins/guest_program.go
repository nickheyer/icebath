package plugins

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Environment variables telling an external program plugin which plugin it runs as
const (
	GuestPluginNameEnv = "ICEBATH_PLUGIN_NAME"
	GuestPluginDirEnv  = "ICEBATH_PLUGIN_DIR"
)

// Program a manifest entry names, a file of the plugin directory first and a program on PATH second
func guestProgramPath(spec *guestSpec) (string, error) {
	name := spec.entry[0]
	if !filepath.IsAbs(name) {
		if local := filepath.Join(spec.dir, filepath.FromSlash(name)); isFile(local) {
			return local, nil
		}
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("plugin '%s' entry %q is neither a file of the plugin directory nor a program on PATH", spec.name, name)
	}
	return resolved, nil
}

// Starts the entry program in its own process group with the profile environment and joins its standard streams to a channel
func (h *PluginHost) launchProgram(s *hostStart, spec *guestSpec) (*Channel, *hostProcess, error) {
	program, err := guestProgramPath(spec)
	if err != nil {
		return nil, nil, err
	}
	env, err := tools.ServedProfileChildEnv(h.baseCtx, nil, nil, h.home, true)
	if err != nil {
		return nil, nil, err
	}
	ambient, err := hostedAmbientCredentials(h.baseCtx, h.home)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range ambient {
		env[k] = v
	}
	for k, v := range h.envOverride {
		env[k] = v
	}
	env[GuestPluginNameEnv], env[GuestPluginDirEnv] = spec.name, spec.dir
	if _, set := env[HostLogLevelEnv]; !set {
		env[HostLogLevelEnv] = "WARNING"
	}
	env[HostProcessEnv] = "1"
	launcher, err := HostLauncher(h.baseCtx, nil)
	if err != nil {
		return nil, nil, err
	}
	argv := append(append(append([]string{}, launcher...), program), spec.entry[1:]...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = envList(env), spec.dir
	proc.Group(cmd)
	var parentEnds, childEnds []*os.File
	closeAll := func(files []*os.File) {
		for _, f := range files {
			_ = f.Close()
		}
	}
	for i := 0; i < 3; i++ {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll(parentEnds)
			closeAll(childEnds)
			return nil, nil, err
		}
		if i == 0 {
			parentEnds, childEnds = append(parentEnds, w), append(childEnds, r)
		} else {
			parentEnds, childEnds = append(parentEnds, r), append(childEnds, w)
		}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = childEnds[0], childEnds[1], childEnds[2]
	err = cmd.Start()
	closeAll(childEnds)
	if err != nil {
		closeAll(parentEnds)
		return nil, nil, err
	}
	stdin, stdout, stderr := parentEnds[0], parentEnds[1], parentEnds[2]
	child := &hostProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		pumpHostStderr(stderr, spec.name)
		_ = stderr.Close()
	}()
	go func() {
		_ = cmd.Wait()
		child.code = exitCode(cmd.ProcessState)
		close(child.done)
	}()
	ch, err := h.installGuest(s, child, stdout, stdin, fmt.Sprintf("plugin %s[%d]", spec.name, cmd.Process.Pid))
	return ch, child, err
}
