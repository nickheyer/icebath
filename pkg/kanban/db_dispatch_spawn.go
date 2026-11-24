package kanban

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// KanbanWorkerClientEntry is the argv word that runs this binary as the kanban worker client
const KanbanWorkerClientEntry = "__icebath_kanban_worker_client__"

// Context bound to a profile's secrets and, with bindHome, its home and terminal policy
func workerProfileScope(ctx context.Context, profileHome string, bindHome bool) context.Context {
	resolved, _ := pypath.Resolve(profileHome)
	launch, _ := pypath.Resolve(home.Process())
	isLaunchHome := resolved == launch
	if bindHome {
		ctx = home.WithOverride(ctx, profileHome)
	}
	if isLaunchHome {
		ctx = provider.WithSecretScope(ctx, provider.LaunchSecretScopeCtx(ctx, profileHome), "")
	} else {
		ctx = provider.WithSecretScope(ctx, provider.BuildProfileSecretScope(profileHome), profileHome)
	}
	if bindHome {
		var overlay map[string]string
		if isLaunchHome {
			overlay = provider.LaunchTerminalEnv()
		}
		ctx = tools.WithProfileTerminalScope(ctx, profileHome, overlay)
	}
	return ctx
}

// Worker argv running this binary as the owner submitting client
func workerArgv() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, KanbanWorkerClientEntry}, nil
}

// Append mode per task worker log under the board root, rotated first
func openWorkerLog(ctx context.Context, task *Task, board *string) (*os.File, error) {
	logDir, err := WorkerLogsDir(ctx, board)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(logDir, 0o777); err != nil {
		return nil, err
	}
	logPath := filepath.Join(logDir, task.ID+".log")
	maxBytes, backupCount := workerLogRotation(ctx)
	RotateWorkerLog(logPath, maxBytes, backupCount)
	return os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
}

// Worker argv wrapped in a restart safe systemd scope when the dispatcher runs under one
func restartSafeWorkerArgv(task *Task, command []string) ([]string, error) {
	if task.CurrentRunID == nil {
		dispatch, err := tools.RestartSafeGatewayChildArgv(command, fmt.Sprintf("kanban-%s-run-missing", task.ID), true, true)
		if err != nil {
			return nil, err
		}
		if dispatch.Mode != "in_process" {
			return nil, &RuntimeError{"cannot create restart-safe systemd scope for Kanban worker: the claimed task has no current run id"}
		}
		return command, nil
	}
	dispatch, err := tools.RestartSafeGatewayChildArgv(command, fmt.Sprintf("kanban-%s-run-%d", task.ID, *task.CurrentRunID), true, true)
	if err != nil {
		return nil, err
	}
	return dispatch.Argv, nil
}

// Starts the detached worker client for a claimed task with the profile scoped environment, returning its pid
func defaultSpawn(ctx context.Context, task *Task, workspace string, board *string) (*int64, error) {
	if task.Assignee == nil || *task.Assignee == "" {
		return nil, &ValueError{fmt.Sprintf("task %s has no assignee", task.ID)}
	}
	profileArg, err := profiles.NormalizeProfileName(*task.Assignee)
	if err != nil {
		return nil, err
	}
	profileHome, err := profiles.ResolveProfileEnv(profileArg)
	if err != nil {
		var missing *profiles.NotFoundError
		if !errors.As(err, &missing) {
			return nil, err
		}
		profileHome = ""
	}
	routed := profileHome != "" && tools.IsRoutedHome(profileHome)
	scoped := ctx
	if profileHome != "" {
		scoped = workerProfileScope(ctx, profileHome, false)
	}
	env, err := tools.BuildSubprocessEnv(scoped, tools.ToolEnvFrom(scoped), tools.SubprocessEnvOptions{
		ScrubSecrets: provider.IsMultiplexActive(scoped) || routed, InheritProfileHome: true,
	})
	if err != nil {
		return nil, err
	}
	for _, key := range tools.SessionContextEnvVars {
		delete(env, key)
	}
	if profileHome != "" {
		env[home.EnvHome] = profileHome
		env = tools.StripLaunchProfileEnv(tools.ToolEnvFrom(ctx), env, profileHome)
	}
	if task.Tenant != nil && *task.Tenant != "" {
		env["ICEBATH_TENANT"] = *task.Tenant
	}
	env["ICEBATH_KANBAN_TASK"] = task.ID
	env["ICEBATH_KANBAN_WORKSPACE"] = workspace
	env["ICEBATH_SESSION_SOURCE"] = "kanban"
	if workspace != "" && filepath.IsAbs(workspace) {
		if st, err := os.Stat(workspace); err == nil && st.IsDir() {
			env["TERMINAL_CWD"] = workspace
		}
	}
	if task.BranchName != nil && *task.BranchName != "" {
		env["ICEBATH_KANBAN_BRANCH"] = *task.BranchName
	}
	if task.CurrentRunID != nil {
		env["ICEBATH_KANBAN_RUN_ID"] = strconv.FormatInt(*task.CurrentRunID, 10)
	}
	if task.ClaimLock != nil && *task.ClaimLock != "" {
		env["ICEBATH_KANBAN_CLAIM_LOCK"] = *task.ClaimLock
	}
	if task.GoalMode {
		env["ICEBATH_KANBAN_GOAL_MODE"] = "1"
		if task.GoalMaxTurns != nil {
			env["ICEBATH_KANBAN_GOAL_MAX_TURNS"] = strconv.FormatInt(*task.GoalMaxTurns, 10)
		}
	}
	for name, raised := range workerTerminalTimeouts(scoped, task.MaxRuntimeSeconds) {
		env[name] = raised
	}
	dbPath, err := KanbanDBPath(ctx, board)
	if err != nil {
		return nil, err
	}
	env["ICEBATH_KANBAN_DB"] = dbPath
	wsRoot, err := WorkspacesRoot(ctx, board)
	if err != nil {
		return nil, err
	}
	env["ICEBATH_KANBAN_WORKSPACES_ROOT"] = wsRoot
	slug, err := normalizeBoardSlug(board)
	if err != nil {
		return nil, err
	}
	if slug == "" {
		slug = GetCurrentBoard(ctx)
	}
	env["ICEBATH_KANBAN_BOARD"] = slug
	env["ICEBATH_PROFILE"] = profileArg
	delete(env, tools.DelegatedChildEnvMarker)
	cmd, err := workerArgv()
	if err != nil {
		return nil, err
	}
	if cmd, err = restartSafeWorkerArgv(task, cmd); err != nil {
		return nil, err
	}
	env = tools.SystemdUserBusEnv(env)
	logF, err := openWorkerLog(ctx, task, board)
	if err != nil {
		return nil, err
	}
	defer logF.Close()
	proc := exec.Command(cmd[0], cmd[1:]...)
	if st, err := os.Stat(workspace); err == nil && st.IsDir() {
		proc.Dir = workspace
	}
	proc.Stdout, proc.Stderr = logF, logF
	proc.Env = make([]string, 0, len(env))
	for k, v := range env {
		proc.Env = append(proc.Env, k+"="+v)
	}
	proc.SysProcAttr = workerSpawnAttr()
	if err := proc.Start(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &RuntimeError{"`icebath` executable not found on PATH. " +
				"Install Icebath Agent or activate its venv before running the kanban dispatcher."}
		}
		return nil, err
	}
	pid := int64(proc.Process.Pid)
	registerLiveWorker(proc.Process.Pid, proc)
	return &pid, nil
}

// DaemonOptions are the keyword arguments of the long lived dispatcher loop
type DaemonOptions struct {
	Interval     float64
	MaxSpawn     *int
	FailureLimit int64
	Stop         <-chan struct{}
	OnTick       func(*DispatchResult)
	Stderr       io.Writer
}

// Dispatcher loop ticking every interval until the stop channel closes or SIGINT or SIGTERM arrives
func RunDaemon(ctx context.Context, o DaemonOptions) {
	if o.Interval == 0 {
		o.Interval = defaultDispatchDaemonInterval
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	stop := make(chan struct{})
	var once sync.Once
	halt := func() { once.Do(func() { close(stop) }) }
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			halt()
		case <-o.Stop:
			halt()
		case <-stop:
		}
	}()
	defer halt()
	for {
		select {
		case <-stop:
			return
		default:
		}
		daemonTick(ctx, o)
		select {
		case <-stop:
			return
		case <-time.After(time.Duration(o.Interval * float64(time.Second))):
		}
	}
}

// One daemon tick that reports any failure on one line and logs its detail instead of ending the loop
func daemonTick(ctx context.Context, o DaemonOptions) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(o.Stderr, "kanban dispatcher tick failed: %v\n", r)
			pkgLog().Error(fmt.Sprintf("kanban dispatcher tick failed: %v", r), "stack", string(debug.Stack()))
		}
	}()
	var res *DispatchResult
	err := ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error {
		opts := ConfiguredDispatchOptions(ctx)
		if o.MaxSpawn != nil {
			opts.MaxSpawn = o.MaxSpawn
		}
		if o.FailureLimit > 0 {
			opts.FailureLimit = o.FailureLimit
		}
		var err error
		res, err = DispatchOnce(ctx, conn, opts)
		return err
	})
	if err != nil {
		fmt.Fprintf(o.Stderr, "kanban dispatcher tick failed: %s\n", err)
		pkgLog().Error("kanban dispatcher tick failed: "+err.Error(), "error_chain", errchain.Text(err))
		return
	}
	if o.OnTick != nil {
		func() {
			defer func() { _ = recover() }()
			o.OnTick(res)
		}()
	}
}
