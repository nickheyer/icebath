package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	apiclient "github.com/daytona/clients/api-client-go"
	"github.com/daytona/clients/sdk-go/pkg/daytona"
	sdkerrors "github.com/daytona/clients/sdk-go/pkg/errors"
	"github.com/daytona/clients/sdk-go/pkg/options"
	"github.com/daytona/clients/sdk-go/pkg/types"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// daytonaEnv runs commands in a Daytona cloud sandbox, persistent mode stopping it on cleanup and resuming it next time
type daytonaEnv struct {
	baseEnv
	client      *daytona.Client
	sandbox     *daytona.Sandbox
	persistent  bool
	taskID      string
	remoteHome  string
	lock        sync.Mutex
	syncManager *FileSyncManager
}

// DaytonaOptions are the sandbox settings, memory and disk in MB
type DaytonaOptions struct {
	Image      string
	Cwd        string
	Timeout    int
	CPU        int
	Memory     int
	Disk       int
	Persistent bool
	TaskID     string
}

// Daytona client from DAYTONA_API_KEY in the secret scope, falling back to the process env
func newDaytonaClient(env *Env) (*daytona.Client, error) {
	if key := secretValue(env, "DAYTONA_API_KEY"); key != "" {
		return daytona.NewClientWithConfig(&types.DaytonaConfig{APIKey: key, APIUrl: os.Getenv("DAYTONA_API_URL"), Target: os.Getenv("DAYTONA_TARGET")})
	}
	return daytona.NewClient()
}

// NewDaytonaEnvironment resumes the task's sandbox or creates one, resolves the home, seeds the sync and starts the session
func NewDaytonaEnvironment(ctx context.Context, env *Env, o DaytonaOptions) (*daytonaEnv, error) {
	if o.Cwd == "" {
		o.Cwd = "/home/daytona"
	}
	if o.Timeout == 0 {
		o.Timeout = 60
	}
	if o.TaskID == "" {
		o.TaskID = "default"
	}
	if o.CPU == 0 {
		o.CPU = 1
	}
	if o.Memory == 0 {
		o.Memory = 5120
	}
	if o.Disk == 0 {
		o.Disk = 10240
	}
	d := &daytonaEnv{persistent: o.Persistent, taskID: o.TaskID}
	d.stdinMode = "payload"
	initBaseEnv(&d.baseEnv, d, env, o.Cwd, o.Timeout, nil, true)
	client, err := newDaytonaClient(env)
	if err != nil {
		return nil, err
	}
	d.client = client
	log := envLog(env)
	memoryGiB := max(1, int(math.Ceil(float64(o.Memory)/1024)))
	diskGiB := max(1, int(math.Ceil(float64(o.Disk)/1024)))
	if diskGiB > 10 {
		log.Warn(fmt.Sprintf("Daytona: requested disk (%dGB) exceeds platform limit (10GB). Capping to 10GB.", diskGiB))
		diskGiB = 10
	}
	labels := map[string]string{"icebath_task_id": o.TaskID}
	name := "icebath-" + o.TaskID
	if o.Persistent {
		sb, err := client.Get(ctx, name)
		if err == nil {
			if err := sb.Start(ctx); err == nil {
				d.sandbox = sb
				log.Info("Daytona: resumed sandbox", "sandbox", sb.ID, "task", o.TaskID)
			} else {
				log.Warn("Daytona: failed to resume sandbox", "task", o.TaskID, "error", err)
			}
		} else if !errors.Is(err, sdkerrors.ErrNotFound) {
			log.Warn("Daytona: failed to resume sandbox", "task", o.TaskID, "error", err)
		}
		if d.sandbox == nil {
			limit := 1
			it := client.List(ctx, &daytona.ListSandboxesQuery{Labels: labels, Limit: &limit})
			if it.Next() {
				sb := it.Value()
				if err := sb.Start(ctx); err == nil {
					d.sandbox = sb
					log.Info("Daytona: resumed legacy sandbox", "sandbox", sb.ID, "task", o.TaskID)
				} else {
					log.Debug("Daytona: no legacy sandbox found", "task", o.TaskID, "error", err)
				}
			} else if err := it.Err(); err != nil {
				log.Debug("Daytona: no legacy sandbox found", "task", o.TaskID, "error", err)
			}
		}
	}
	if d.sandbox == nil {
		zero := 0
		sb, err := client.Create(ctx, types.ImageParams{SandboxBaseParams: types.SandboxBaseParams{Name: name, Labels: labels, AutoStopInterval: &zero},
			Image: o.Image, Resources: &types.Resources{CPU: o.CPU, Memory: memoryGiB, Disk: diskGiB}})
		if err != nil {
			return nil, err
		}
		d.sandbox = sb
		log.Info("Daytona: created sandbox", "sandbox", sb.ID, "task", o.TaskID)
	}
	d.remoteHome = "/root"
	if resp, err := d.sandbox.Process.ExecuteCommand(ctx, "echo $HOME"); err == nil {
		if home := strings.TrimSpace(resp.Result); home != "" {
			d.remoteHome = home
			if o.Cwd == "~" || o.Cwd == "/home/daytona" {
				d.SetCwd(home)
			}
		}
	}
	log.Info("Daytona: resolved home and cwd", "home", d.remoteHome, "cwd", d.Cwd())
	base := d.remoteHome + "/.icebath"
	d.syncManager = NewFileSyncManager(env, func() []syncFile { return iterSyncFiles(env, base) },
		func(h, r string) error { return d.daytonaUpload(ctx, h, r) }, func(paths []string) error { return d.daytonaDelete(ctx, paths) },
		func(files []syncFile) error { return d.daytonaBulkUpload(ctx, files) }, func(dest string) error { return d.daytonaBulkDownload(ctx, dest) })
	d.syncManager.Sync(true)
	d.initSession(ctx)
	return d, nil
}

func (d *daytonaEnv) EnvType() string { return "daytona" }

// Uploads a single file, the remote parent created with a POSIX path
func (d *daytonaEnv) daytonaUpload(ctx context.Context, hostPath, remotePath string) error {
	if _, err := d.sandbox.Process.ExecuteCommand(ctx, quotedMkdirCommand([]string{path.Dir(remotePath)})); err != nil {
		return err
	}
	return d.sandbox.FileSystem.UploadFile(ctx, hostPath, remotePath)
}

// Uploads many files after one batched mkdir
func (d *daytonaEnv) daytonaBulkUpload(ctx context.Context, files []syncFile) error {
	if len(files) == 0 {
		return nil
	}
	if parents := uniqueParentDirs(files); len(parents) > 0 {
		if _, err := d.sandbox.Process.ExecuteCommand(ctx, quotedMkdirCommand(parents)); err != nil {
			return err
		}
	}
	for _, f := range files {
		if err := d.sandbox.FileSystem.UploadFile(ctx, f.host, f.remote); err != nil {
			return err
		}
	}
	return nil
}

// Downloads the remote icebath tree as a tar archive through a pid suffixed remote temp path
func (d *daytonaEnv) daytonaBulkDownload(ctx context.Context, dest string) error {
	relBase := strings.TrimLeft(d.remoteHome+"/.icebath", "/")
	remoteTar := fmt.Sprintf("/tmp/.icebath_sync.%d.tar", os.Getpid())
	if _, err := d.sandbox.Process.ExecuteCommand(ctx, "tar cf "+shQuote(remoteTar)+" --exclude='*.sock' -C / "+shQuote(relBase)); err != nil {
		return err
	}
	if _, err := d.sandbox.FileSystem.DownloadFile(ctx, remoteTar, &dest); err != nil {
		return err
	}
	d.sandbox.Process.ExecuteCommand(ctx, "rm -f "+shQuote(remoteTar))
	return nil
}

func (d *daytonaEnv) daytonaDelete(ctx context.Context, remotePaths []string) error {
	_, err := d.sandbox.Process.ExecuteCommand(ctx, quotedRmCommand(remotePaths))
	return err
}

// Restarts the sandbox when a previous interrupt stopped it
func (d *daytonaEnv) ensureSandboxReady(ctx context.Context) error {
	if err := d.sandbox.RefreshData(ctx); err != nil {
		return err
	}
	if d.sandbox.State == apiclient.SANDBOXSTATE_STOPPED || d.sandbox.State == apiclient.SANDBOXSTATE_ARCHIVED {
		if err := d.sandbox.Start(ctx); err != nil {
			return err
		}
		envLog(d.env).Info("Daytona: restarted sandbox", "sandbox", d.sandbox.ID)
	}
	return nil
}

func (d *daytonaEnv) beforeExecute(ctx context.Context) error {
	d.lock.Lock()
	err := d.ensureSandboxReady(ctx)
	d.lock.Unlock()
	if err != nil {
		return err
	}
	d.syncManager.Sync(false)
	return nil
}

// Spawn per call through a threaded handle, stdin staged as a sandbox file the shell unlinks before running
func (d *daytonaEnv) runBash(ctx context.Context, cmd string, login bool, timeout int, stdinData *string) (ProcessHandle, error) {
	sandbox := d.sandbox
	var stateMu sync.Mutex
	cancelled, dispatched := false, false
	staged := ""
	scrubStaged := func() {
		if staged != "" && !dispatched {
			sandbox.FileSystem.DeleteFile(context.Background(), staged, false)
			staged = ""
		}
	}
	cancel := func() {
		stateMu.Lock()
		cancelled = true
		scrubStaged()
		stateMu.Unlock()
		stopCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		sandbox.Stop(stopCtx)
	}
	execFn := func() (string, int) {
		command := cmd
		if stdinData != nil && *stdinData != "" {
			stateMu.Lock()
			if cancelled {
				stateMu.Unlock()
				return "", 130
			}
			stateMu.Unlock()
			remoteStdin := d.stagedStdinPath()
			payload, err := pystr.SurrogateEscapeEncode(*stdinData)
			if err != nil {
				return "", 1
			}
			if err := sandbox.FileSystem.UploadFile(context.Background(), payload, remoteStdin); err != nil {
				return "Daytona stdin upload failed: " + err.Error(), 1
			}
			stateMu.Lock()
			staged = remoteStdin
			stateMu.Unlock()
			if err := sandbox.FileSystem.SetFilePermissions(context.Background(), remoteStdin, options.WithPermissionMode("600")); err != nil {
				return "Daytona stdin permissions failed: " + err.Error(), 1
			}
			command = redirectStdinFromFile(cmd, remoteStdin)
		}
		shellCmd := "bash -c " + shQuote(command)
		if login {
			shellCmd = "bash -l -c " + shQuote(command)
		}
		stateMu.Lock()
		if cancelled {
			scrubStaged()
			stateMu.Unlock()
			return "", 130
		}
		dispatched = true
		stateMu.Unlock()
		resp, err := sandbox.Process.ExecuteCommand(context.Background(), shellCmd, options.WithExecuteTimeout(time.Duration(timeout)*time.Second))
		if err != nil {
			return "Daytona exec failed: " + err.Error(), 1
		}
		return resp.Result, resp.ExitCode
	}
	return NewThreadedProcessHandle(execFn, cancel)
}

// Cleanup syncs back then stops a persistent sandbox or deletes an ephemeral one
func (d *daytonaEnv) Cleanup(forceRemove bool) error {
	d.lock.Lock()
	defer d.lock.Unlock()
	if d.sandbox == nil {
		return nil
	}
	log := envLog(d.env)
	if d.syncManager != nil {
		log.Info("Daytona: syncing files from sandbox...")
		d.syncManager.SyncBack(d.env.Home)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if d.persistent {
		if err := d.sandbox.Stop(ctx); err != nil {
			log.Warn("Daytona: cleanup failed", "error", err)
		} else {
			log.Info("Daytona: stopped sandbox (filesystem preserved)", "sandbox", d.sandbox.ID)
		}
	} else if err := d.sandbox.Delete(ctx); err != nil {
		log.Warn("Daytona: cleanup failed", "error", err)
	} else {
		log.Info("Daytona: deleted sandbox", "sandbox", d.sandbox.ID)
	}
	d.sandbox = nil
	return nil
}

// ---- provider -----------------------------------------------------------------------------------

type daytonaBackend struct{}

func (daytonaBackend) Name() string                           { return "daytona" }
func (daytonaBackend) DisplayName() string                    { return "Daytona" }
func (daytonaBackend) Description() string                    { return "Persistent cloud development environment" }
func (daytonaBackend) EnvDescription() string                 { return "a Daytona workspace (Linux)" }
func (daytonaBackend) IsRemote() bool                         { return true }
func (daytonaBackend) IsContainer() bool                      { return true }
func (daytonaBackend) SkipContainerGuards() bool              { return true }
func (daytonaBackend) CachePathBase() string                  { return "~/.icebath" }
func (daytonaBackend) DefaultCwd() string                     { return "" }
func (daytonaBackend) StripEnvKeys() []string                 { return nil }
func (daytonaBackend) SessionIsolatedWhenNonpersistent() bool { return false }
func (daytonaBackend) SupportsStreams() bool                  { return false }

// IsAvailable reports whether an API key is in the process environment
func (daytonaBackend) IsAvailable() bool { return os.Getenv("DAYTONA_API_KEY") != "" }

// CheckRequirements needs the API key in the secret scope
func (daytonaBackend) CheckRequirements(env *Env, cfg map[string]any) error {
	if secretValue(env, "DAYTONA_API_KEY") != "" {
		return nil
	}
	return errors.New("the 'daytona' backend is missing its API key or SDK")
}

// Probe reports whether the API key is configured
func (daytonaBackend) Probe() (string, string) {
	if os.Getenv("DAYTONA_API_KEY") == "" {
		return "needs_setup", "DAYTONA_API_KEY is not configured."
	}
	return "ready", "Daytona API key configured"
}

// SetupInstructions explains the Daytona account and key
func (daytonaBackend) SetupInstructions(env *Env) []string {
	return []string{
		"Persistent cloud development environments.",
		"Each session gets a dedicated sandbox with filesystem persistence.",
		"Sign up at: https://daytona.io and set DAYTONA_API_KEY.",
	}
}

// DoctorChecks reports the API key row
func (daytonaBackend) DoctorChecks() []DoctorCheck {
	if os.Getenv("DAYTONA_API_KEY") == "" {
		return []DoctorCheck{{OK: false, Label: "Daytona API key missing", Detail: "(needed for the 'daytona' terminal backend)"}}
	}
	return []DoctorCheck{{OK: true, Label: "Daytona API key", Detail: "(configured)"}}
}

// CreateEnvironment builds the sandbox from the shaped resources, cpu truncated to a whole core count
func (daytonaBackend) CreateEnvironment(ctx context.Context, env *Env, opts CreateOptions) (Environment, error) {
	res := sandboxResources(opts.ContainerConfig)
	cpu, _ := pyIntValue(res["cpu"])
	memory, _ := pyIntValue(res["memory"])
	disk, _ := pyIntValue(res["disk"])
	return NewDaytonaEnvironment(ctx, env, DaytonaOptions{Image: opts.Image, Cwd: opts.Cwd, Timeout: opts.Timeout, CPU: cpu, Memory: memory, Disk: disk,
		Persistent: truthy(res["persistent_filesystem"]), TaskID: opts.TaskID})
}
