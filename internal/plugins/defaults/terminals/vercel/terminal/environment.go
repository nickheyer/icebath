package terminal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	vercelconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/vercel/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	createAttempts = 3
	writeAttempts  = 3
	// Waits for a session to run, and to stop
	runningWait = 30 * time.Second
	runningPoll = 250 * time.Millisecond
	stopWait    = 15 * time.Second
	stopPoll    = 500 * time.Millisecond
	// Least a sandbox lives, which is also Vercel's longest on every plan
	lifetime = 45 * time.Minute
)

// Commands run in a Vercel cloud microVM through the sandbox REST API, its filesystem snapshotted between sessions when sandbox files are
// kept
type environment struct {
	*terminal.Shell
	runner *runner
}

// Pushes changed host files into the sandbox now
func (e *environment) SyncFiles() {
	if _, _, _, files := e.runner.current(); files != nil {
		files.Sync(true)
	}
}

type runner struct {
	spec   *terminal.Spec
	skills *skills.Registry
	shell  *terminal.Shell
	client *client
	params createParams
	// Snapshots are taken and restored, for the environment's key
	persistent bool
	task       string
	snapshots  terminal.Snapshots
	// Folder the environment was asked to start in
	requested string
	// Serializes the check that the session still runs, its making again and its removal
	ready sync.Mutex

	mu      sync.Mutex
	session *session
	// Folder the sandbox starts commands in, and the home of its user
	root, home string
	sync       *terminal.FileSync
}

// What a session is made with under the settings, living at least as long as a command may run
func paramsOf(cfg *config.Config, timeout time.Duration) createParams {
	p := createParams{timeout: max(timeout, lifetime), runtime: setting(cfg, vercelconfig.Runtime), image: cmp.Or(setting(cfg, vercelconfig.Image), vercelconfig.DefaultImage)}
	if p.runtime != "" {
		p.image = ""
	}
	if resources, err := terminal.SandboxResources(cfg); err == nil {
		if resources.CPU > 0 {
			p.vcpus = int(math.Floor(resources.CPU))
		}
		if resources.Memory > 0 {
			p.memory = resources.Memory
		}
	}
	return p
}

func newHTTP() *http.Client { return &http.Client{} }

// Polls until the session runs, failing on a final status or once the wait is over
func waitRunning(ctx context.Context, c *client, s *session) error {
	if s.Status == "" || s.Status == "running" {
		return nil
	}
	if ended(s.Status) {
		return fmt.Errorf("The Vercel sandbox stopped with the status %s before it was running.", s.Status)
	}
	for deadline := time.Now().Add(runningWait); time.Now().Before(deadline); {
		time.Sleep(runningPoll)
		current, err := c.get(ctx, s.ID)
		if err != nil {
			return err
		}
		s.Status = current.Status
		if s.Status == "running" {
			return nil
		}
		if ended(s.Status) {
			return fmt.Errorf("The Vercel sandbox stopped with the status %s before it was running.", s.Status)
		}
	}
	return fmt.Errorf("The Vercel sandbox was not running after %s. Its last status was %s.", strs.Span(runningWait), s.Status)
}

// Stops the session and waits for it to say it stopped
func stop(c *client, s session) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := c.stop(ctx, s.ID); err != nil {
		return
	}
	for deadline := time.Now().Add(stopWait); time.Now().Before(deadline); time.Sleep(stopPoll) {
		if current, err := c.get(ctx, s.ID); err != nil || ended(current.Status) {
			return
		}
	}
}

// Makes the session, from the key's snapshot when sandbox files are kept, puts the profile's files in it and takes the session's snapshot
func create(ctx context.Context, spec *terminal.Spec, skillset *skills.Registry) (terminal.Environment, error) {
	cfg := spec.Config
	resources, err := terminal.SandboxResources(cfg)
	if err != nil {
		return nil, err
	}
	if resources.Disk != 0 && resources.Disk != defaultDisk {
		return nil, errors.New("Vercel Sandbox does not support configurable container_disk. Use the default shared setting.")
	}
	creds, err := credentialsOf(cfg)
	if err != nil {
		return nil, err
	}
	params := paramsOf(cfg, spec.Timeout)
	if params.runtime != "" {
		slog.Warn(fmt.Sprintf("Vercel: %s is deprecated by Vercel; set %s (default %s) instead", vercelconfig.Runtime, vercelconfig.Image, vercelconfig.DefaultImage))
	}
	r := &runner{
		spec:       spec,
		skills:     skillset,
		client:     &client{credentials: creds, http: newHTTP()},
		params:     params,
		persistent: resources.Persistent && !spec.Probe,
		task:       cmp.Or(terminal.Task(cfg, "vercel_sandbox", spec.Key), "default"),
		snapshots:  terminal.NewSnapshots(cfg, "vercel_sandbox_snapshots.json"),
		requested:  cmp.Or(spec.Cwd, workspace),
		root:       workspace,
		home:       workspace,
	}
	r.shell = terminal.NewShell(spec, r, terminal.ShellOptions{
		Backend:    "vercel_sandbox",
		Cwd:        r.requested,
		Timeout:    spec.Timeout,
		Persist:    true,
		Persistent: r.persistent,
		Forwarded:  r.forwarded,
	})
	if err := r.attach(ctx, r.requested); err != nil {
		return nil, err
	}
	if !spec.Probe {
		r.shell.Init(ctx)
	}
	return &environment{Shell: r.shell, runner: r}, nil
}

// Session, the folders of its sandbox and its file sync, a nil session once cleaned up
func (r *runner) current() (*session, string, string, *terminal.FileSync) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session, r.root, r.home, r.sync
}

// Error of a command after the environment was cleaned up
var cleaned = errors.New("The Vercel sandbox was removed. Run the command again.")

// Makes a session, from the stored snapshot first when there is one and afresh when that fails
func (r *runner) make(ctx context.Context) (session, error) {
	snapshot := ""
	if r.persistent {
		snapshot = r.snapshots.Get(r.task)
	}
	if snapshot != "" {
		s, err := retry("sandbox restore", createAttempts, func() (session, error) { return r.client.create(ctx, r.params, snapshot) })
		if err == nil {
			return s, nil
		}
		slog.Warn("Vercel: failed to restore snapshot; falling back to a fresh sandbox", "snapshot", snapshot, "task", r.task, "error", err)
		r.snapshots.Delete(r.task, snapshot)
	}
	return retry("sandbox create", createAttempts, func() (session, error) { return r.client.create(ctx, r.params, "") })
}

// Makes a session, waits for it to run, then finds its folders, moves to the one asked for and syncs the profile's files
func (r *runner) attach(ctx context.Context, requested string) error {
	s, err := r.make(ctx)
	if err != nil {
		return err
	}
	if err := waitRunning(ctx, r.client, &s); err != nil {
		return err
	}
	root := workspace
	if strings.HasPrefix(s.Cwd, "/") {
		root = s.Cwd
	}
	home := root
	if output, _, err := r.client.run(ctx, s.ID, "sh", []string{"-lc", `printf %s "$HOME"`}, root, nil); err != nil {
		slog.Debug("Vercel: home detection failed", "task", r.task, "error", err)
	} else if found := strings.TrimSpace(output); strings.HasPrefix(found, "/") {
		home = found
	}
	r.shell.StampHome(home)
	switch requested {
	case "~":
		r.shell.SetCwd(home)
	case "", workspace:
		r.shell.SetCwd(root)
	default:
		r.shell.SetCwd(requested)
	}
	r.mu.Lock()
	r.session, r.root, r.home = &s, root, home
	r.mu.Unlock()
	if r.spec.Probe {
		return nil
	}
	cfg, base := r.spec.Config, strings.TrimRight(home, "/")+"/.icebath"
	files := terminal.NewFileSync(cfg, func() []terminal.SyncFile {
		needs, _ := r.spec.Skills(r.skills)
		return terminal.SyncFiles(cfg, base, needs)
	}, terminal.Transport{Delete: r.remove, BulkUpload: r.upload, BulkDownload: r.download})
	r.mu.Lock()
	r.sync = files
	r.mu.Unlock()
	files.Sync(true)
	return nil
}

// Runs a login shell script in the sandbox's folder, failing with its output on a non-zero exit
func (r *runner) checked(script, label string) error {
	s, root, _, _ := r.current()
	if s == nil {
		return cleaned
	}
	output, code, err := r.client.run(context.Background(), s.ID, "bash", []string{"-lc", script}, root, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("Vercel %s failed: %s", label, strings.TrimSpace(output))
	}
	return nil
}

func (r *runner) upload(files []terminal.SyncFile) error {
	s, _, _, _ := r.current()
	if s == nil {
		return cleaned
	}
	payload := make([]file, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f.Host)
		if err != nil {
			return err
		}
		st, err := os.Stat(f.Host)
		if err != nil {
			return err
		}
		payload = append(payload, file{path: f.Remote, content: data, mode: int64(st.Mode().Perm())})
	}
	_, err := retry("write_files", writeAttempts, func() (struct{}, error) {
		return struct{}{}, r.client.write(context.Background(), s.ID, payload)
	})
	return err
}

func (r *runner) remove(remotes []string) error {
	if len(remotes) == 0 {
		return nil
	}
	return r.checked(terminal.RemoveCommand(remotes), "delete")
}

// Downloads the sandbox's icebath folder as a tar made at a temporary path of the sandbox
func (r *runner) download(dest string) error {
	s, root, home, _ := r.current()
	if s == nil {
		return cleaned
	}
	remote := fmt.Sprintf("/tmp/.icebath_sync.%d.tar", os.Getpid())
	defer r.client.run(context.Background(), s.ID, "bash", []string{"-lc", "rm -f " + terminal.Quote(remote)}, root, nil)
	member := strings.TrimLeft(strings.TrimRight(home, "/")+"/.icebath", "/")
	if err := r.checked("tar cf "+terminal.Quote(remote)+" --exclude='*.sock' -C / "+terminal.Quote(member), "bulk download"); err != nil {
		return err
	}
	data, err := r.client.read(context.Background(), s.ID, remote)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o600)
}

// Variables handed each command and the names unset for it: those the profile passes through and those its skills need
func (r *runner) forward() (map[string]string, []string) {
	_, extra := r.spec.Skills(r.skills)
	return r.spec.Terminal.Forward(r.spec.Config, extra)
}

// Names of the variables handed each command, kept out of the snapshot
func (r *runner) forwarded() []string {
	values, unset := r.forward()
	return append(slices.Collect(maps.Keys(values)), unset...)
}

// Reuses the running session, makes a new one when Vercel ended it or it is gone, then pushes changed host files
func (r *runner) Prepare(ctx context.Context) error {
	r.ready.Lock()
	defer r.ready.Unlock()
	s, _, _, _ := r.current()
	if s == nil {
		return cleaned
	}
	current, err := r.client.get(ctx, s.ID)
	switch {
	case err != nil && !notFound(err):
		return terminal.NewConnectionError("Vercel sandbox could not be reached: "+err.Error(), "")
	case err == nil && !ended(current.Status):
		if err := waitRunning(ctx, r.client, &current); err != nil {
			return err
		}
	default:
		if err != nil {
			slog.Warn("Vercel: sandbox refresh failed; recreating", "task", r.task, "error", err)
		} else {
			slog.Warn("Vercel: sandbox entered a terminal state; recreating", "status", current.Status, "task", r.task)
		}
		r.shell.MarkRecreated()
		if err := r.attach(ctx, cmp.Or(r.shell.Cwd(), r.requested, workspace)); err != nil {
			return err
		}
		r.shell.Init(ctx)
	}
	if _, _, _, files := r.current(); files != nil {
		files.Sync(false)
	}
	return nil
}

// Runs bash in the sandbox on a goroutine, stdin staged as a sandbox file so it never becomes an argument, the variables handed through
// the command and the scoped ones unset first; stopping it stops the session, which the next command makes anew
func (r *runner) Run(_ context.Context, script string, login bool, _ time.Duration, stdin *string) (terminal.Proc, error) {
	s, root, _, _ := r.current()
	if s == nil {
		return nil, errors.New("Vercel sandbox is not attached")
	}
	target := *s
	values, unset := r.forward()
	script = terminal.Unset(script, unset)
	var mu sync.Mutex
	cancelled, dispatched := false, false
	staged := ""
	scrub := func() {
		if staged != "" && !dispatched {
			r.client.write(context.Background(), target.ID, []file{{path: staged, mode: 0o600}})
			staged = ""
		}
	}
	cancel := func() {
		mu.Lock()
		cancelled = true
		scrub()
		mu.Unlock()
		stop(r.client, target)
	}
	run := func() (string, int) {
		command := script
		if stdin != nil && *stdin != "" {
			mu.Lock()
			if cancelled {
				mu.Unlock()
				return "", 130
			}
			mu.Unlock()
			remote := terminal.StdinPath(r.shell.TempDir())
			payload := []byte(*stdin)
			if _, err := retry("stdin upload", writeAttempts, func() (struct{}, error) {
				return struct{}{}, r.client.write(context.Background(), target.ID, []file{{path: remote, content: payload, mode: 0o600}})
			}); err != nil {
				return "Vercel stdin upload failed: " + err.Error(), 1
			}
			command = terminal.FromFile(script, remote)
			mu.Lock()
			staged = remote
			mu.Unlock()
		}
		mu.Lock()
		if cancelled {
			scrub()
			mu.Unlock()
			return "", 130
		}
		dispatched = true
		mu.Unlock()
		flag := "-c"
		if login {
			flag = "-lc"
		}
		output, code, err := r.client.run(context.Background(), target.ID, "bash", []string{flag, command}, root, values)
		if err != nil {
			return "Vercel exec failed: " + err.Error(), 1
		}
		return output, code
	}
	return terminal.NewFuncProc(run, cancel)
}

func (r *runner) Kill(p terminal.Proc)      { p.Kill() }
func (r *runner) ForceKill(p terminal.Proc) { p.Kill() }

// Copies the sandbox's changes back, snapshots its filesystem when sandbox files are kept and the profile stays, and stops it
func (r *runner) Cleanup(force bool) error {
	r.ready.Lock()
	defer r.ready.Unlock()
	s, _, _, files := r.current()
	if s != nil && files != nil {
		files.SyncBack()
	}
	r.mu.Lock()
	r.session, r.sync = nil, nil
	r.mu.Unlock()
	if s == nil {
		return nil
	}
	if r.persistent && !force {
		r.snapshot(*s)
	}
	stop(r.client, *s)
	return nil
}

// Snapshots the session's filesystem for the key
func (r *runner) snapshot(s session) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	id, err := r.client.snapshot(ctx, s.ID)
	switch {
	case err != nil:
		slog.Warn("Vercel: filesystem snapshot failed", "task", r.task, "error", err)
	case id == "":
		slog.Warn("Vercel: filesystem snapshot did not return a snapshot id", "task", r.task)
	default:
		if err := r.snapshots.Set(r.task, id); err != nil {
			slog.Warn("Vercel: filesystem snapshot failed", "task", r.task, "error", err)
			return
		}
		slog.Info("Vercel: saved filesystem snapshot", "snapshot", id, "task", r.task)
	}
}
