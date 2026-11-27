package terminal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/strs"
)

// A named Apptainer or Singularity instance and the bash its commands run in, the runner under the environment's shell
type instance struct {
	terminal.HostProcs
	r     *registrant
	spec  *terminal.Spec
	shell *terminal.Shell
	exe   string
	image string
	id    string
	// Writes land in an overlay folder kept until icebath stops, else in a tmpfs gone with the instance
	persistent bool
	overlay    string
	cpu        float64
	memory     int
	// Read only binds of the files and folders it is handed
	binds   []string
	started atomic.Bool
}

// The environment's shell over its instance
type environment struct {
	*terminal.Shell
	box *instance
}

// Starts a hardened instance of the image with its overlay, binds and limits, and takes the snapshot of its login shell
func (r *registrant) create(ctx context.Context, spec *terminal.Spec) (terminal.Environment, error) {
	cfg := spec.Config
	r.checkUsage(cfg)
	resources, err := terminal.SandboxResources(cfg)
	if err != nil {
		return nil, err
	}
	exe, err := available(ctx)
	if err != nil {
		return nil, err
	}
	image, err := r.sif(ctx, cfg, reference(configured(cfg)), exe, func(string) {})
	if err != nil {
		return nil, err
	}
	i := &instance{r: r, spec: spec, exe: exe, image: image, id: "icebath_" + strs.Random(6), persistent: resources.Persistent, cpu: resources.CPU,
		memory: resources.Memory}
	scratch, scratchErr := scratchDir(cfg)
	if scratchErr == nil {
		r.remember(scratch)
	} else {
		slog.Debug("Singularity scratch directory unavailable for the exit sweep", "error", scratchErr)
	}
	if i.persistent {
		if scratchErr != nil {
			return nil, scratchErr
		}
		i.overlay = filepath.Join(scratch, "icebath-overlays", "overlay-"+terminal.SafeName(terminal.Task(cfg, name, spec.Key)))
		if err := os.MkdirAll(i.overlay, 0o755); err != nil {
			return nil, err
		}
	}
	needs, _ := spec.Skills(r.skills)
	files, folders := terminal.Handed(cfg, home, needs)
	for _, m := range slices.Concat(files, folders) {
		i.binds = append(i.binds, "--bind", m.Host+":"+m.Sandbox+":ro")
	}
	if err := i.start(ctx); err != nil {
		return nil, err
	}
	i.shell = terminal.NewShell(spec, i, terminal.ShellOptions{
		Backend:    name,
		Cwd:        spec.Cwd,
		Timeout:    spec.Timeout,
		Persist:    true,
		SudoProbe:  true,
		Persistent: i.persistent,
		Forwarded:  i.names,
	})
	i.shell.Init(ctx)
	return &environment{Shell: i.shell, box: i}, nil
}

// Starts the instance contained, without the host's home, writing to its overlay or a writable tmpfs, with its binds and limits
func (i *instance) start(ctx context.Context) error {
	argv := []string{i.exe, "instance", "start", "--containall", "--no-home"}
	if i.persistent && i.overlay != "" {
		argv = append(argv, "--overlay", i.overlay)
	} else {
		argv = append(argv, "--writable-tmpfs")
	}
	argv = append(argv, i.binds...)
	if i.memory > 0 {
		argv = append(argv, "--memory", strconv.Itoa(i.memory)+"M")
	}
	if i.cpu > 0 {
		argv = append(argv, "--cpus", strconv.FormatFloat(i.cpu, 'f', -1, 64))
	}
	argv = append(argv, i.image, i.id)
	captured, err := terminal.Capture(ctx, argv, 120*time.Second, nil)
	switch {
	case errors.Is(err, terminal.ErrTimedOut):
		return errors.New("Instance start timed out")
	case err != nil:
		return err
	case captured.Code != 0:
		return fmt.Errorf("Failed to start instance: %s", captured.Stderr)
	}
	i.started.Store(true)
	slog.Info(fmt.Sprintf("Singularity instance %s started (persistent=%t)", i.id, i.persistent))
	return nil
}

// Variables handed each command: those the profile passes through and those the skills of the sessions read, with the names to unset
func (i *instance) forwarded() (map[string]string, []string) {
	_, values := i.spec.Skills(i.r.skills)
	return i.spec.Terminal.Forward(i.spec.Config, values)
}

// Names of the variables handed each command, kept out of the shell's snapshot
func (i *instance) names() []string {
	values, unset := i.forwarded()
	return append(slices.Collect(maps.Keys(values)), unset...)
}

// Prefix of the client's variables the program sets inside an instance that keeps the host's environment out
func (i *instance) envPrefix() string {
	if strings.Contains(strings.ToLower(filepath.Base(i.exe)), "singularity") {
		return "SINGULARITYENV_"
	}
	return "APPTAINERENV_"
}

// The program's exec into the instance from its root folder, never the host folder it lacks, with the host's environment kept out
func (i *instance) prefix() []string {
	return []string{i.exe, "exec", "--cleanenv", "--pwd", "/", "instance://" + i.id}
}

// Starts bash in the instance, the variables handed to it set through the client's environment
func (i *instance) Run(_ context.Context, script string, login bool, _ time.Duration, stdin *string) (terminal.Proc, error) {
	if !i.started.Load() {
		return nil, errors.New("Singularity instance not started")
	}
	values, unset := i.forwarded()
	argv := append(i.prefix(), "bash")
	if login {
		argv = append(argv, "-l")
	}
	argv = append(argv, "-c", terminal.Unset(script, unset))
	cmd := exec.Command(argv[0], argv[1:]...)
	env := terminal.ProcessEnv()
	for name, value := range values {
		env[i.envPrefix()+name] = value
	}
	cmd.Env = terminal.Environ(env)
	terminal.NewSession(cmd)
	p, err := terminal.Start(cmd, stdin)
	if err != nil {
		return nil, err
	}
	p.Own(i.spec.Terminal.Supervisor)
	return p, nil
}

// Stops the instance; a persistent overlay stays until icebath stops
func (i *instance) Cleanup(bool) error {
	if !i.started.Load() {
		return nil
	}
	argv := []string{i.exe, "instance", "stop", i.id}
	_, err := terminal.Capture(context.Background(), argv, 30*time.Second, nil)
	switch {
	case errors.Is(err, terminal.ErrTimedOut):
		slog.Warn(fmt.Sprintf("Failed to stop Singularity instance %s: Command '%s' timed out after 30 seconds", i.id, terminal.Join(argv)))
	case err != nil:
		slog.Warn(fmt.Sprintf("Failed to stop Singularity instance %s: %s", i.id, err))
	default:
		slog.Info(fmt.Sprintf("Singularity instance %s stopped", i.id))
	}
	i.started.Store(false)
	return nil
}

// The program's exec into the instance, nil before it started; instances run as the user who started them, whoever is asked for
func (e *environment) ExecPrefix(string, bool) ([]string, error) {
	if !e.box.started.Load() {
		return nil, nil
	}
	return e.box.prefix(), nil
}
