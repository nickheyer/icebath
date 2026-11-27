package terminal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	dockerconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/docker/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/terminal/sandbox"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What docker says of a container removed behind icebath's back
var goneMarkers = []string{"No such container", "is not running", "no such container"}

// A Docker container and the bash its commands run in, the runner under the environment's shell
type container struct {
	terminal.HostProcs
	r      *registrant
	spec   *terminal.Spec
	shell  *terminal.Shell
	docker string
	o      *options
	// Flags of docker run that hold whatever the image: user, writable folders, resources, proxy host, mounts, variables, extra flags
	args []string
	// The container runs as the host user
	asUser bool
	// Writable folders and mounts, what reuse tells containers apart by beside the image
	fixed []string
	// Variables set at docker run, those of the settings merged with the proxy's
	runEnv map[string]string
	// Variables and unset names of the commands of a login shell, fixed when the container was made
	initEnv   map[string]string
	initUnset []string
	// Sandbox folders of the host bound at /root and /workspace when sandbox files are kept
	binds []bind
	// Host folder mounted for the session and where the container sees it
	hostCwd, mount string
	// Labels for an egress setup, kept to find the container again
	egress string

	mu    sync.Mutex
	id    string
	image string
	// The image's entry point is the s6 overlay's init
	s6     bool
	labels map[string]string
	// Teardown in flight
	cleanup chan struct{}
	// Held while the container is made again
	recovering sync.Mutex
}

// The environment's shell over its container
type environment struct {
	*terminal.Shell
	box *container
}

// Makes the container of the environment, or finds the one an earlier run left, and takes the snapshot of its login shell
func (r *registrant) create(ctx context.Context, spec *terminal.Spec) (terminal.Environment, error) {
	cfg := spec.Config
	o, err := readOptions(cfg, terminal.Task(cfg, name, spec.Key))
	if err != nil {
		return nil, err
	}
	docker, err := available(ctx, cfg)
	if err != nil {
		return nil, err
	}
	r.reap(ctx, cfg, docker, o.shared)
	cwd := spec.Cwd
	if cwd == "~" || cwd == "" {
		cwd = "/root"
	}
	slog.Info("DockerEnvironment volumes", "volumes", listOf(o.volumes))
	c := &container{r: r, spec: spec, docker: docker, o: o, image: o.image}
	resources, err := r.resourceArgs(ctx, docker, o)
	if err != nil {
		return nil, err
	}
	volumes, writable, err := c.mounts()
	if err != nil {
		return nil, err
	}
	if c.mount != "" && followsMount(cwd, c.mount) {
		slog.Info("Container cwd follows configured host workspace", "mount", c.mount, "requested", cwd)
		cwd = c.mount
	}
	needs, _ := spec.Skills(r.skills)
	files, folders := terminal.Handed(cfg, terminal.SandboxProfile, needs)
	volumes = append(volumes, readonly(files, folders)...)
	proxy, err := egressOf(spec.Terminal.Proxy(cfg))
	if err != nil {
		return nil, err
	}
	if len(proxy.overrides) > 0 {
		if err := proxy.checkForward(o.forward); err != nil {
			return nil, err
		}
		if err := proxy.checkEnv(o.env); err != nil {
			return nil, err
		}
		if err := proxy.checkExtra(o.extra); err != nil {
			return nil, err
		}
	}
	c.runEnv, c.egress = proxy.merge(o.env), proxy.label()
	volumes = append(volumes, proxy.volumes...)
	user := hostUserArgs(o.asUser)
	slog.Info("Docker volume_args", "args", listOf(volumes))
	c.args = slices.Concat(user, writable, resources, proxy.hosts, volumes, nameOnly(c.runEnv), o.extra)
	c.asUser, c.fixed = len(user) > 0, slices.Concat(writable, volumes)
	if err := c.start(ctx, cwd); err != nil {
		return nil, err
	}
	values, unset := c.forwarded()
	c.initEnv = maps.Clone(o.env)
	maps.Copy(c.initEnv, values)
	for _, name := range unset {
		delete(c.initEnv, name)
	}
	c.initUnset = unset
	c.shell = terminal.NewShell(spec, c, terminal.ShellOptions{
		Backend:       name,
		Cwd:           cwd,
		Timeout:       spec.Timeout,
		Persist:       true,
		SudoProbe:     true,
		Persistent:    o.persistent,
		SessionScoped: o.sessionScoped,
		HostCwd:       c.hostCwd,
		Mount:         c.mount,
		Forwarded:     c.names,
	})
	c.shell.Init(ctx)
	return &environment{Shell: c.shell, box: c}, nil
}

// Builds the image when it is icebath's own, then reuses the container an earlier run left or runs a new one, and gives back to root what
// was handed to the host user
func (c *container) start(ctx context.Context, cwd string) error {
	cfg := c.spec.Config
	c.mu.Lock()
	image := c.image
	c.mu.Unlock()
	if err := sandbox.Ensure(ctx, c.docker, image, buildLog); err != nil {
		return err
	}
	s6 := initEntrypoint(ctx, c.docker, image)
	if s6 {
		slog.Info("Docker: image uses /init (s6-overlay) as entrypoint — skipping --init and mounting /run with exec.", "image", image)
	}
	if c.o.snap {
		slog.Warn(dockerconfig.SnapCompat + ": running without --init and no-new-privileges (snap Docker under AppArmor)")
	}
	labels := map[string]string{agentLabel: "1", taskLabel: label(c.o.task), profileLabel: identity(cfg, c.o.shared), egressLabel: c.egress}
	if m := workspaceTask.FindStringSubmatch(c.o.task); m != nil {
		labels[workspaceLabel] = m[1]
	}
	if c.o.shared == "" {
		labels[environmentLabel] = fingerprint(cfg, image, c.fixed)
	}
	c.mu.Lock()
	c.s6, c.labels = s6, labels
	c.mu.Unlock()
	slog.Info("Docker run_args", "args", listOf(c.runArgs()))
	if c.o.reuse && c.attach(ctx) {
		c.r.stripPrivileges(ctx, cfg, c.docker, c.current(), nil)
	} else {
		id, err := c.run(ctx, cwd)
		if err != nil {
			return err
		}
		c.set(id)
	}
	reclaim(ctx, c.docker, c.current(), c.binds)
	return nil
}

// Progress of an image build: its steps logged as they start, every other line at debug
func buildLog(line string, headline bool) {
	if headline {
		slog.Info(line)
		return
	}
	slog.Debug(line)
}

// Flags of docker run: the security ones the image and user decide, then the rest
func (c *container) runArgs() []string {
	c.mu.Lock()
	s6 := c.s6
	c.mu.Unlock()
	return append(securityArgs(c.asUser, s6, c.o.snap), c.args...)
}

// Mounts of the settings' volumes and the session's folder, and the writable sandbox folders: folders of the host kept between runs when
// sandbox files are kept, small in-memory ones otherwise
func (c *container) mounts() (volumes, writable []string, err error) {
	volumes = volumeArgs(c.o.volumes)
	explicit := workspaceTaken(volumes)
	target, existing := "", false
	if host := c.spec.HostCwd; host != "" {
		if st, statErr := os.Stat(host); statErr != nil || !st.IsDir() {
			slog.Debug("Skipping docker cwd mount: host_cwd is not a valid directory", "host_cwd", host)
		} else {
			target, existing = bindTarget(volumes, host)
			c.hostCwd, c.mount = host, target
		}
	}
	bound := target != "" && !existing
	workspace := !(bound && target == "/workspace") && !explicit
	if c.o.persistent {
		dir, err := terminal.SandboxDir(c.spec.Config)
		if err != nil {
			return nil, nil, err
		}
		folder := filepath.Join(dir, "docker", terminal.SafeName(c.o.task))
		home := filepath.Join(folder, "home")
		if err := os.MkdirAll(home, 0o755); err != nil {
			return nil, nil, err
		}
		c.binds = []bind{{home, "/root"}}
		if workspace {
			path := filepath.Join(folder, "workspace")
			if err := os.MkdirAll(path, 0o755); err != nil {
				return nil, nil, err
			}
			c.binds = append(c.binds, bind{path, "/workspace"})
		}
		for _, b := range c.binds {
			writable = append(writable, "-v", b.host+":"+b.sandbox)
		}
	} else {
		if workspace {
			writable = append(writable, "--tmpfs", "/workspace:rw,exec,size=10g")
		}
		writable = append(writable, "--tmpfs", "/home:rw,exec,size=1g", "--tmpfs", "/root:rw,exec,size=1g")
	}
	switch {
	case bound:
		slog.Info("Mounting configured host cwd", "target", target, "host", c.hostCwd)
		volumes = append([]string{"-v", c.hostCwd + ":" + target}, volumes...)
	case explicit && !existing:
		slog.Debug("Skipping docker cwd mount: /workspace already mounted by user config")
	}
	return volumes, writable, nil
}

// Read only mounts of the files and folders the sandbox is handed, those not there as they should be left out
func readonly(files, folders []terminal.Mount) []string {
	var args []string
	mount := func(m terminal.Mount, kind, problem string) {
		if problem != "" {
			slog.Warn("Docker: skipping mount", "kind", kind, "problem", problem, "source", m.Host)
			return
		}
		args = append(args, "-v", m.Host+":"+m.Sandbox+":ro")
		slog.Info("Docker: mounting", "kind", kind, "host", m.Host, "container", m.Sandbox)
	}
	for _, m := range files {
		problem := ""
		if st, err := os.Stat(m.Host); err == nil && st.IsDir() {
			problem = "source is a directory (likely Docker-in-Docker auto-creation)"
		} else if err != nil || !st.Mode().IsRegular() {
			problem = "source not found"
		}
		mount(m, "credential", problem)
	}
	for _, m := range folders {
		problem := ""
		if st, err := os.Stat(m.Host); err != nil || !st.IsDir() {
			problem = "source is not a directory"
		}
		mount(m, "folder", problem)
	}
	return args
}

func (c *container) current() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.id
}

func (c *container) set(id string) {
	c.mu.Lock()
	c.id = id
	c.mu.Unlock()
}

// The container's id, forgotten as it goes
func (c *container) take() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.id
	c.id = ""
	return id
}

// Labels of the container as filters of docker ps
func (c *container) filters() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	filters := []string{"--filter", "label=" + agentLabel + "=1"}
	for _, key := range []string{taskLabel, profileLabel, egressLabel, environmentLabel} {
		if value := c.labels[key]; value != "" {
			filters = append(filters, "--filter", "label="+key+"="+value)
		}
	}
	return filters
}

// Container an earlier run left with the same labels, a running one first, with its state
func (c *container) find(ctx context.Context) (string, string, bool) {
	argv := append(append([]string{c.docker, "ps", "-a"}, c.filters()...), "--format", "{{.ID}}\t{{.State}}")
	captured := query(ctx, argv, 10*time.Second, "docker ps probe failed — will start a fresh container", "docker ps probe returned nonzero — will start a fresh container")
	if captured == nil {
		return "", "", false
	}
	var first, firstState string
	for _, line := range strings.Split(captured.Stdout, "\n") {
		id, state, ok := strings.Cut(line, "\t")
		if !ok || strings.TrimSpace(id) == "" {
			continue
		}
		state = strings.ToLower(strings.TrimSpace(state))
		if state == "running" {
			return id, state, true
		}
		if first == "" {
			first, firstState = id, state
		}
	}
	return first, firstState, first != ""
}

// Reuses the container an earlier run left: one on another image only when the image was chosen and can be had, one of an air-gapped
// sandbox only when it has no network, started again when it stopped
func (c *container) attach(ctx context.Context) bool {
	id, state, ok := c.find(ctx)
	if !ok {
		return false
	}
	c.mu.Lock()
	image, task, profile := c.image, c.labels[taskLabel], c.labels[profileLabel]
	c.mu.Unlock()
	remove := func(text string, attrs ...any) bool {
		slog.Warn(text, attrs...)
		if _, err := terminal.Capture(ctx, []string{c.docker, "rm", "-f", id}, 30*time.Second, nil); err != nil {
			slog.Warn("Failed to remove mismatched container", "container", short(id), "error", err)
		}
		return false
	}
	if actual := inspectValue(ctx, c.docker, id, "{{.Config.Image}}", "docker inspect Image"); actual != "" && actual != image {
		attrs := []any{"container", short(id), "running", actual, "configured", image, "task", task, "profile", profile}
		switch {
		case !c.o.pinned:
			slog.Warn("Existing container runs another image; the default Docker image changed. Keeping the existing sandbox — approve the switch by "+
				"setting Container image in /settings under Terminal → Docker (files in /root and /workspace carry over), or set it to the image the "+
				"sandbox runs to keep it and stop this notice.", attrs...)
		case !imageReady(ctx, c.docker, image):
			slog.Warn("Existing container runs another image; the Docker image could not be pulled — keeping the current sandbox until it can.", attrs...)
		default:
			return remove("Existing container runs another image than the Docker image setting — removing it and starting fresh.", attrs...)
		}
	}
	if !c.o.network {
		if mode := inspectValue(ctx, c.docker, id, "{{.HostConfig.NetworkMode}}", "docker inspect NetworkMode"); mode != "none" {
			return remove("Existing container NetworkMode mismatches the air-gapped Docker network setting — removing it and starting fresh.", "container",
				short(id), "mode", cmp.Or(mode, "unknown"), "task", task, "profile", profile)
		}
	}
	if state != "running" {
		if err := c.resume(ctx, id); err != nil {
			slog.Warn("Failed to start existing container — falling back to a fresh container.", "container", short(id), "state", state, "error", err)
			return false
		}
	}
	c.set(id)
	slog.Info("Reusing container", "container", short(id), "task", task, "profile", profile, "prior_state", state)
	return true
}

// One value docker inspect formats of the container, empty when it can't say
func inspectValue(ctx context.Context, docker, id, format, what string) string {
	captured := query(ctx, []string{docker, "inspect", "--format", format, id}, 10*time.Second, what+" failed", what+" returned nonzero")
	if captured == nil {
		return ""
	}
	return strings.TrimSpace(captured.Stdout)
}

// Whether the image is there to run: icebath's own built, another found locally or pulled
func imageReady(ctx context.Context, docker, image string) bool {
	if sandbox.Default(image) {
		if err := sandbox.Ensure(ctx, docker, image, buildLog); err != nil {
			slog.Warn("Docker: could not build the sandbox image", "image", image, "error", err)
			return false
		}
		return true
	}
	if captured, err := terminal.Capture(ctx, []string{docker, "image", "inspect", image, "--format", "{{.Id}}"}, 30*time.Second, nil); err == nil && captured.Code == 0 {
		return true
	}
	captured, err := terminal.Capture(ctx, []string{docker, "pull", image}, 900*time.Second, nil)
	if err != nil {
		slog.Warn("Docker: could not pull image", "image", image, "error", err)
		return false
	}
	return captured.Code == 0
}

// Starts a stopped container again
func (c *container) resume(ctx context.Context, id string) error {
	argv := []string{c.docker, "start", id}
	captured, err := terminal.Capture(ctx, argv, 30*time.Second, nil)
	if err != nil {
		return err
	}
	return captured.Err(argv)
}

// docker run of the container: detached, with tini as its init unless the image or snap Docker keeps it out, its labels, the folder it
// starts in, every flag, and sleep for ever
func (c *container) runCommand(name, workdir string) []string {
	c.mu.Lock()
	s6, image := c.s6, c.image
	labels := maps.Clone(c.labels)
	c.mu.Unlock()
	argv := []string{c.docker, "run", "-d"}
	if !s6 && !c.o.snap {
		argv = append(argv, "--init")
	}
	argv = append(argv, "--name", name)
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		argv = append(argv, "--label", key+"="+labels[key])
	}
	argv = append(argv, "-w", workdir)
	argv = append(argv, c.runArgs()...)
	return append(argv, image, "sleep", "infinity")
}

// Runs a new container, removing what a failed run left
func (c *container) run(ctx context.Context, cwd string) (string, error) {
	name := "icebath-" + strs.Random(4)
	argv := c.runCommand(name, cwd)
	slog.Debug("Starting container", "argv", terminal.Join(argv))
	captured, err := terminal.Capture(ctx, argv, 120*time.Second, client(c.runEnv))
	if err == nil {
		err = captured.Err(argv)
	}
	if err != nil {
		slog.Warn("docker run failed, cleaning up orphaned container", "name", name, "error", err)
		terminal.Capture(context.Background(), []string{c.docker, "rm", "-f", name}, 10*time.Second, nil)
		return "", err
	}
	id := strings.TrimSpace(captured.Stdout)
	slog.Info("Started container", "name", name, "id", short(id))
	return id, nil
}

// Variables handed each command, the forwarded ones and those the skills of the sessions read, with the names to unset
func (c *container) forwarded() (map[string]string, []string) {
	_, values := c.spec.Skills(c.r.skills)
	return c.spec.Terminal.Forward(c.spec.Config, values, c.o.forward...)
}

// Names of the variables handed each command, kept out of the shell's snapshot
func (c *container) names() []string {
	values, unset := c.forwarded()
	return append(slices.Collect(maps.Keys(values)), unset...)
}

// Starts bash in the container through docker exec, the variables handed to it named on the command line and valued through the client's
// environment
func (c *container) Run(_ context.Context, script string, login bool, _ time.Duration, stdin *string) (terminal.Proc, error) {
	id := c.current()
	if id == "" {
		return nil, errors.New("Container not started")
	}
	argv := []string{c.docker, "exec"}
	if stdin != nil {
		argv = append(argv, "-i")
	}
	values, unset := c.initEnv, c.initUnset
	if !login {
		values, unset = c.forwarded()
	}
	argv = append(argv, nameOnly(values)...)
	argv = append(argv, id, "bash")
	if login {
		argv = append(argv, "-l")
	}
	argv = append(argv, "-c", terminal.Unset(script, unset))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = client(values)
	terminal.NewSession(cmd)
	p, err := terminal.Start(cmd, stdin)
	if err != nil {
		return nil, err
	}
	p.Own(c.spec.Terminal.Supervisor)
	return p, nil
}

// Runs a teardown in the background, waited on by WaitCleanup and when icebath stops
func (c *container) background(work func()) {
	done := make(chan struct{})
	c.mu.Lock()
	c.cleanup = done
	c.mu.Unlock()
	c.spec.Terminal.Background(func() {
		defer close(done)
		work()
	})
}

// Leaves a container later runs reuse running, stripped of setuid bits, else stops it, hands its sandbox files back to the host user and
// removes it; force removes either
func (c *container) Cleanup(force bool) error {
	id := c.take()
	if id == "" {
		return nil
	}
	cfg, docker, binds := c.spec.Config, c.docker, slices.Clone(c.binds)
	if !force && c.o.reuse {
		c.background(func() { c.r.stripPrivileges(context.Background(), cfg, docker, id, nil) })
		return nil
	}
	c.background(func() { c.r.remove(context.Background(), cfg, docker, id, binds) })
	return nil
}

// Stops the container, hands its sandbox files back to the host user and removes it
func (r *registrant) remove(ctx context.Context, cfg *config.Config, docker, id string, binds []bind) {
	if _, err := terminal.Capture(ctx, []string{docker, "stop", "-t", "10", id}, 30*time.Second, nil); err != nil {
		slog.Warn("docker stop timed out / failed", "container", short(id), "error", err)
	}
	if len(binds) > 0 {
		r.handBack(ctx, cfg, docker, id, binds)
	}
	if _, err := terminal.Capture(ctx, []string{docker, "rm", "-f", id}, 30*time.Second, nil); err != nil {
		slog.Warn("docker rm -f failed", "container", short(id), "error", err)
	}
}

// Runs the command, once more when its container was removed behind icebath's back and made again, after moving the container to the
// image the settings chose since it was made
func (e *environment) Execute(ctx context.Context, command string, opts terminal.Exec) (*terminal.Result, error) {
	if err := e.box.follow(ctx); err != nil {
		return nil, err
	}
	result, err := e.Shell.Execute(ctx, command, opts)
	if err != nil {
		return result, err
	}
	if result.Code != 0 && e.RecoverGone(ctx, result.Output) {
		return e.Shell.Execute(ctx, command, opts)
	}
	return result, nil
}

// Makes a container that was removed behind icebath's back again, when its sandbox is one later runs reuse
func (e *environment) RecoverGone(ctx context.Context, output string) bool {
	gone := slices.ContainsFunc(goneMarkers, func(marker string) bool { return strings.Contains(output, marker) })
	return gone && e.box.o.reuse && e.box.recreate(ctx)
}

// Finds the container again, or runs a new one, and takes the snapshot of its shell anew
func (c *container) recreate(ctx context.Context) bool {
	c.recovering.Lock()
	defer c.recovering.Unlock()
	old := c.take()
	slog.Warn("Container appears to be gone — attempting recovery", "container", short(old))
	if id, state, ok := c.find(ctx); ok {
		if state == "running" {
			c.set(id)
			slog.Info("Recovery: reusing running container", "container", short(id))
		} else if err := c.resume(ctx, id); err == nil {
			c.set(id)
			slog.Info("Recovery: restarted container", "container", short(id))
		} else {
			slog.Warn("Recovery: failed to start container", "container", short(id), "error", err)
		}
	}
	if c.current() == "" {
		name := "icebath-" + strs.Random(4)
		argv := c.runCommand(name, c.shell.Cwd())
		captured, err := terminal.Capture(ctx, argv, 120*time.Second, client(c.runEnv))
		if err == nil {
			err = captured.Err(argv)
		}
		if err != nil {
			slog.Error("Recovery: failed to create new container", "error", err)
			return false
		}
		c.set(strings.TrimSpace(captured.Stdout))
		slog.Info("Recovery: created fresh container", "name", name, "container", short(c.current()))
	}
	c.shell.Init(ctx)
	slog.Info("Recovery successful — new container", "container", short(c.current()))
	c.shell.MarkRecreated()
	return true
}

// Moves the container to the image the settings chose since it was made: the old one stopped, its files handed back and removed, a new
// one run on the image, whose /root and /workspace carry over when sandbox files are kept
func (c *container) follow(ctx context.Context) error {
	cfg := c.spec.Config
	want := cmp.Or(strings.TrimSpace(cfg.GetString(dockerconfig.Image)), sandbox.Image)
	c.mu.Lock()
	image := c.image
	c.mu.Unlock()
	if want == image || !pinned(cfg) {
		return nil
	}
	c.recovering.Lock()
	defer c.recovering.Unlock()
	if c.current() == "" {
		return nil
	}
	slog.Info("Docker image setting changed — moving the sandbox to it", "from", image, "to", want)
	c.r.remove(ctx, cfg, c.docker, c.take(), c.binds)
	c.mu.Lock()
	c.image = want
	c.mu.Unlock()
	if err := c.start(ctx, c.shell.Cwd()); err != nil {
		return fmt.Errorf("moving the Docker sandbox to %s: %w", want, err)
	}
	c.shell.Init(ctx)
	c.shell.MarkRecreated()
	return nil
}

// Stops a workspace's container when it goes idle rather than removing it, so the workspace's next session starts it again; any other
// is cleaned up
func (e *environment) StopIdle() error {
	c := e.box
	if !workspaceTask.MatchString(c.o.task) {
		return c.Cleanup(false)
	}
	id := c.take()
	if id == "" {
		return nil
	}
	cfg, docker := c.spec.Config, c.docker
	c.background(func() {
		ctx := context.Background()
		if _, err := terminal.Capture(ctx, []string{docker, "stop", "-t", "10", id}, 30*time.Second, nil); err != nil {
			slog.Warn("docker stop (idle workspace) failed", "container", short(id), "error", err)
		}
		c.r.stripPrivileges(ctx, cfg, docker, id, nil)
	})
	return nil
}

// Waits for the teardown in flight
func (e *environment) WaitCleanup(timeout time.Duration) {
	e.box.mu.Lock()
	done := e.box.cleanup
	e.box.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// Container of the environment and the program that runs it
func (e *environment) Box() (string, string) {
	return e.box.current(), e.box.docker
}

// docker exec into the container as the user, stdin kept open when interactive
func (e *environment) ExecPrefix(user string, interactive bool) ([]string, error) {
	id := e.box.current()
	if id == "" {
		return nil, errors.New("Container not started")
	}
	argv := []string{e.box.docker, "exec"}
	if interactive {
		argv = append(argv, "-i")
	}
	if user != "" {
		argv = append(argv, "-u", user)
	}
	return append(argv, id), nil
}
