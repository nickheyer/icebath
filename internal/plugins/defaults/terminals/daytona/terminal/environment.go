package terminal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	apiclient "github.com/daytona/clients/api-client-go"
	"github.com/daytona/clients/sdk-go/pkg/daytona"
	sdkerrors "github.com/daytona/clients/sdk-go/pkg/errors"
	"github.com/daytona/clients/sdk-go/pkg/options"
	"github.com/daytona/clients/sdk-go/pkg/types"

	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
)

const (
	// Largest disk Daytona gives a sandbox, in GB
	diskLimit = 10
	// Longest stopping a sandbox, or removing it, may take
	stopWait    = 30 * time.Second
	cleanupWait = 120 * time.Second
)

// Commands run in a Daytona cloud sandbox: kept stopped between sessions when sandbox files are kept, deleted otherwise
type environment struct {
	*terminal.Shell
	runner *runner
}

// Pushes changed host files into the sandbox now
func (e *environment) SyncFiles() {
	if _, _, files := e.runner.current(); files != nil {
		files.Sync(true)
	}
}

type runner struct {
	spec   *terminal.Spec
	skills *skills.Registry
	shell  *terminal.Shell
	client *daytona.Client
	// What a sandbox is made with, found by and named
	params types.ImageParams
	// Sandbox files are kept between sessions
	persistent bool
	// Serializes the readiness check of the sandbox, its making again and its removal
	ready sync.Mutex

	mu      sync.Mutex
	sandbox *daytona.Sandbox
	home    string
	sync    *terminal.FileSync
}

// Starts the stopped sandbox of the key when sandbox files are kept, else makes one, finds its home, puts the profile's files there and
// takes the session's snapshot
func create(ctx context.Context, spec *terminal.Spec, skillset *skills.Registry) (terminal.Environment, error) {
	cfg := spec.Config
	if apiKey(cfg) == "" {
		return nil, errors.New(missing)
	}
	resources, err := terminal.SandboxResources(cfg)
	if err != nil {
		return nil, err
	}
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	cpu := max(int(math.Floor(resources.CPU)), 1)
	memory := cmp.Or(resources.Memory, 5120)
	disk := cmp.Or(resources.Disk, 10240)
	memoryGB := max(1, int(math.Ceil(float64(memory)/1024)))
	diskGB := max(1, int(math.Ceil(float64(disk)/1024)))
	if diskGB > diskLimit {
		slog.Warn(fmt.Sprintf("Daytona: requested disk (%dGB) exceeds platform limit (%dGB). Capping to %dGB.", diskGB, diskLimit, diskLimit))
		diskGB = diskLimit
	}
	task := terminal.SafeName(cmp.Or(terminal.Task(cfg, "daytona", spec.Key), "default"))
	never := 0
	r := &runner{
		spec:       spec,
		skills:     skillset,
		client:     client,
		persistent: resources.Persistent && !spec.Probe,
		params: types.ImageParams{
			SandboxBaseParams: types.SandboxBaseParams{Name: "icebath-" + task, Labels: map[string]string{"icebath_task_id": task}, AutoStopInterval: &never},
			Image:             image(cfg),
			Resources:         &types.Resources{CPU: cpu, Memory: memoryGB, Disk: diskGB},
		},
	}
	r.shell = terminal.NewShell(spec, r, terminal.ShellOptions{
		Backend:    "daytona",
		Cwd:        cmp.Or(spec.Cwd, "/home/daytona"),
		Timeout:    spec.Timeout,
		Persist:    true,
		Persistent: r.persistent,
		Forwarded:  r.forwarded,
	})
	if err := r.start(ctx); err != nil {
		client.Close(context.Background())
		return nil, err
	}
	if !spec.Probe {
		r.shell.Init(ctx)
	}
	return &environment{Shell: r.shell, runner: r}, nil
}

// Sandbox, its home and its file sync, nil once cleaned up
func (r *runner) current() (*daytona.Sandbox, string, *terminal.FileSync) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sandbox, r.home, r.sync
}

// Error of a command after the environment was cleaned up
var cleaned = errors.New("The Daytona sandbox was removed. Run the command again.")

// Resumes the sandbox of the key or makes one, then finds its home, moves there when it starts in a home and syncs the profile's files
func (r *runner) start(ctx context.Context) error {
	name, task := r.params.Name, r.params.Labels["icebath_task_id"]
	var sandbox *daytona.Sandbox
	existing, err := r.client.Get(ctx, name)
	switch {
	case err == nil && r.persistent:
		if err := existing.Start(ctx); err != nil {
			slog.Warn("Daytona: failed to resume sandbox", "task", task, "error", err)
			return terminal.NewConnectionError(fmt.Sprintf("Daytona: the sandbox %s kept for this session could not be started: %v", name, err), "")
		}
		sandbox = existing
		slog.Info("Daytona: resumed sandbox", "sandbox", existing.ID, "task", task)
	case err == nil:
		if err := existing.DeleteAndWait(ctx, stopWait); err != nil {
			slog.Warn("Daytona: could not delete a sandbox an earlier run left", "sandbox", existing.ID, "error", err)
		}
	case !errors.Is(err, sdkerrors.ErrNotFound):
		slog.Warn("Daytona: failed to resume sandbox", "task", task, "error", err)
	}
	if sandbox == nil {
		if sandbox, err = r.client.Create(ctx, r.params); err != nil {
			return err
		}
		slog.Info("Daytona: created sandbox", "sandbox", sandbox.ID, "task", task)
	}
	home := "/root"
	if response, err := sandbox.Process.ExecuteCommand(ctx, "echo $HOME"); err == nil {
		if found := strings.TrimSpace(response.Result); found != "" {
			home = found
			if cwd := r.shell.Cwd(); cwd == "~" || cwd == "/home/daytona" {
				r.shell.SetCwd(home)
			}
		}
	}
	r.shell.StampHome(home)
	slog.Info("Daytona: resolved home and cwd", "home", home, "cwd", r.shell.Cwd())
	r.mu.Lock()
	r.sandbox, r.home = sandbox, home
	r.mu.Unlock()
	if r.spec.Probe {
		return nil
	}
	cfg, base := r.spec.Config, home+"/.icebath"
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

// Uploads the files one by one after one mkdir of their folders
func (r *runner) upload(files []terminal.SyncFile) error {
	sandbox, _, _ := r.current()
	if sandbox == nil {
		return cleaned
	}
	ctx := context.Background()
	if parents := terminal.Parents(files); len(parents) > 0 {
		if _, err := sandbox.Process.ExecuteCommand(ctx, terminal.MkdirCommand(parents)); err != nil {
			return err
		}
	}
	for _, f := range files {
		if err := sandbox.FileSystem.UploadFile(ctx, f.Host, f.Remote); err != nil {
			return err
		}
	}
	return nil
}

// Downloads the sandbox's icebath folder as a tar made at a temporary path of the sandbox
func (r *runner) download(dest string) error {
	sandbox, home, _ := r.current()
	if sandbox == nil {
		return cleaned
	}
	ctx := context.Background()
	remote := fmt.Sprintf("/tmp/.icebath_sync.%d.tar", os.Getpid())
	command := "tar cf " + terminal.Quote(remote) + " --exclude='*.sock' -C / " + terminal.Quote(strings.TrimLeft(path.Join(home, ".icebath"), "/"))
	if _, err := sandbox.Process.ExecuteCommand(ctx, command); err != nil {
		return err
	}
	if _, err := sandbox.FileSystem.DownloadFile(ctx, remote, &dest); err != nil {
		return err
	}
	sandbox.Process.ExecuteCommand(ctx, "rm -f "+terminal.Quote(remote))
	return nil
}

func (r *runner) remove(remotes []string) error {
	sandbox, _, _ := r.current()
	if sandbox == nil {
		return cleaned
	}
	_, err := sandbox.Process.ExecuteCommand(context.Background(), terminal.RemoveCommand(remotes))
	return err
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

// Starts the sandbox again when an interrupt or Daytona stopped it, makes it anew when it is gone, then pushes changed host files
func (r *runner) Prepare(ctx context.Context) error {
	r.ready.Lock()
	defer r.ready.Unlock()
	sandbox, _, _ := r.current()
	if sandbox == nil {
		return cleaned
	}
	err := sandbox.RefreshData(ctx)
	switch {
	case errors.Is(err, sdkerrors.ErrNotFound) || err == nil && (sandbox.State == apiclient.SANDBOXSTATE_DESTROYED || sandbox.State == apiclient.SANDBOXSTATE_DESTROYING):
		slog.Warn("Daytona: sandbox is gone; recreating", "sandbox", sandbox.ID)
		r.shell.MarkRecreated()
		if err := r.start(ctx); err != nil {
			return err
		}
		r.shell.Init(ctx)
	case err != nil:
		return terminal.NewConnectionError("Daytona sandbox could not be reached: "+err.Error(), "")
	case sandbox.State == apiclient.SANDBOXSTATE_STOPPED || sandbox.State == apiclient.SANDBOXSTATE_ARCHIVED || sandbox.State == apiclient.SANDBOXSTATE_PAUSED:
		if err := sandbox.Start(ctx); err != nil {
			return terminal.NewConnectionError("Daytona sandbox could not be started again: "+err.Error(), "")
		}
		slog.Info("Daytona: restarted sandbox", "sandbox", sandbox.ID)
	}
	if _, _, files := r.current(); files != nil {
		files.Sync(false)
	}
	return nil
}

// Runs bash in the sandbox on a goroutine, stdin staged as a sandbox file the shell removes before running, the variables handed through
// the exec and the scoped ones unset first; stopping it stops the sandbox, which the next command starts again
func (r *runner) Run(_ context.Context, script string, login bool, timeout time.Duration, stdin *string) (terminal.Proc, error) {
	sandbox, _, _ := r.current()
	if sandbox == nil {
		return nil, cleaned
	}
	values, unset := r.forward()
	script = terminal.Unset(script, unset)
	var mu sync.Mutex
	cancelled, dispatched := false, false
	staged := ""
	scrub := func() {
		if staged != "" && !dispatched {
			sandbox.FileSystem.DeleteFile(context.Background(), staged, false)
			staged = ""
		}
	}
	stop := func() {
		mu.Lock()
		cancelled = true
		scrub()
		mu.Unlock()
		ctx, done := context.WithTimeout(context.Background(), stopWait)
		defer done()
		sandbox.Stop(ctx)
	}
	run := func() (string, int) {
		ctx := context.Background()
		command := script
		if stdin != nil && *stdin != "" {
			mu.Lock()
			if cancelled {
				mu.Unlock()
				return "", 130
			}
			mu.Unlock()
			remote := terminal.StdinPath(r.shell.TempDir())
			if err := sandbox.FileSystem.UploadFile(ctx, []byte(*stdin), remote); err != nil {
				return "Daytona stdin upload failed: " + err.Error(), 1
			}
			mu.Lock()
			staged = remote
			mu.Unlock()
			if err := sandbox.FileSystem.SetFilePermissions(ctx, remote, options.WithPermissionMode("600")); err != nil {
				return "Daytona stdin permissions failed: " + err.Error(), 1
			}
			command = terminal.FromFile(script, remote)
		}
		shell := "bash -c "
		if login {
			shell = "bash -l -c "
		}
		mu.Lock()
		if cancelled {
			scrub()
			mu.Unlock()
			return "", 130
		}
		dispatched = true
		mu.Unlock()
		opts := []func(*options.ExecuteCommand){options.WithExecuteTimeout(timeout)}
		if len(values) > 0 {
			opts = append(opts, options.WithCommandEnv(values))
		}
		response, err := sandbox.Process.ExecuteCommand(ctx, shell+terminal.Quote(command), opts...)
		if err != nil {
			return "Daytona exec failed: " + err.Error(), 1
		}
		return response.Result, response.ExitCode
	}
	return terminal.NewFuncProc(run, stop)
}

func (r *runner) Kill(p terminal.Proc)      { p.Kill() }
func (r *runner) ForceKill(p terminal.Proc) { p.Kill() }

// Copies the sandbox's changes back, then stops the sandbox when sandbox files are kept and the profile stays, deletes it otherwise
func (r *runner) Cleanup(force bool) error {
	r.ready.Lock()
	defer r.ready.Unlock()
	sandbox, _, files := r.current()
	if sandbox == nil {
		return nil
	}
	if files != nil {
		slog.Info("Daytona: syncing files from sandbox...")
		files.SyncBack()
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupWait)
	defer cancel()
	if r.persistent && !force {
		if err := sandbox.Stop(ctx); err != nil {
			slog.Warn("Daytona: cleanup failed", "error", err)
		} else {
			slog.Info("Daytona: stopped sandbox (filesystem preserved)", "sandbox", sandbox.ID)
		}
	} else if err := sandbox.Delete(ctx); err != nil {
		slog.Warn("Daytona: cleanup failed", "error", err)
	} else {
		slog.Info("Daytona: deleted sandbox", "sandbox", sandbox.ID)
	}
	r.client.Close(ctx)
	r.mu.Lock()
	r.sandbox, r.sync = nil, nil
	r.mu.Unlock()
	return nil
}
