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
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// KanbanWorkerClientEntry is the argv word that runs this binary as the kanban worker client
const KanbanWorkerClientEntry = "__icebath_kanban_worker_client__"

// Running binary invocation, the Go form of the interpreter bound module argv
func moduleIcebathArgv() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe}, nil
}

// Absolute filesystem path of a resolved icebath executable
func absoluteIcebathPath(path string) string {
	expanded := pypath.Expanduser(path)
	if pypath.IsAbs(expanded) {
		return expanded
	}
	abs, err := pypath.Abspath(expanded)
	if err != nil {
		return expanded
	}
	return abs
}

var driveLetterRE = regexp.MustCompile(`^[A-Za-z]:`)

// Whether a command override is an explicit path rather than a bare name
func looksLikePath(value string) bool {
	expanded := pypath.Expanduser(value)
	return strings.HasPrefix(expanded, "~") || pypath.IsAbs(expanded) || pypath.Dirname(expanded) != "" ||
		strings.Contains(expanded, `\`) || driveLetterRE.MatchString(expanded)
}

// Whether a path is a Windows batch shim unsafe as argv zero
func isWindowsBatchShim(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat")
}

// Executable names to try for an unqualified command, PATHEXT expanded on Windows
func pathSearchNames(command string) []string {
	if !isWindows || filepath.Ext(command) != "" {
		return []string{command}
	}
	raw := os.Getenv("PATHEXT")
	if raw == "" {
		raw = ".COM;.EXE;.BAT;.CMD"
	}
	var names []string
	for _, ext := range strings.Split(raw, ";") {
		if ext != "" {
			names = append(names, command+ext)
		}
	}
	return names
}

// Bare command resolved from explicit PATH entries only, never the current directory
func safeWhichNoCwd(command string) string {
	for _, rawDir := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if rawDir == "" || rawDir == "." {
			continue
		}
		directory := pypath.Expanduser(rawDir)
		for _, name := range pathSearchNames(command) {
			candidate := filepath.Join(directory, name)
			st, err := os.Stat(candidate)
			if err != nil || !st.Mode().IsRegular() {
				continue
			}
			if isWindows || executableAccess(candidate) {
				return candidate
			}
		}
	}
	return ""
}

// Argv for a resolved icebath executable path, batch shims falling back to the running binary
func icebathPathArgv(path string) ([]string, error) {
	if isWindows && isWindowsBatchShim(path) {
		return moduleIcebathArgv()
	}
	return []string{absoluteIcebathPath(path)}, nil
}

// Icebath invocation from ICEBATH_BIN, then the running binary, then a PATH search
func resolveIcebathArgv() ([]string, error) {
	if envBin := strings.TrimSpace(os.Getenv("ICEBATH_BIN")); envBin != "" {
		if looksLikePath(envBin) {
			return icebathPathArgv(envBin)
		}
		if resolved := safeWhichNoCwd(envBin); resolved != "" {
			return icebathPathArgv(resolved)
		}
		return moduleIcebathArgv()
	}
	if argv, err := moduleIcebathArgv(); err == nil {
		return argv, nil
	}
	bin := ""
	if isWindows {
		bin = safeWhichNoCwd("icebath")
	} else if found, err := exec.LookPath("icebath"); err == nil {
		bin = found
	}
	if bin != "" {
		return icebathPathArgv(bin)
	}
	return moduleIcebathArgv()
}

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

// Sorted effective CLI toolsets of the assigned profile, nil when unresolvable or empty
func resolveWorkerCLIToolsets(ctx context.Context, icebathHome string) (toolsets []string) {
	if icebathHome == "" {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			log.Debug(fmt.Sprintf("kanban worker: could not resolve CLI toolsets for ICEBATH_HOME=%s (%v)", pyrepr.Repr(icebathHome), r))
			toolsets = nil
		}
	}()
	scoped := workerProfileScope(ctx, icebathHome, true)
	cfg, err := config.LoadContext(scoped, icebathHome, os.LookupEnv)
	if err != nil {
		log.Debug(fmt.Sprintf("kanban worker: could not resolve CLI toolsets for ICEBATH_HOME=%s (%s)", pyrepr.Repr(icebathHome), pyrepr.ExceptionMessage(err)))
		return nil
	}
	for name := range tools.Default.PlatformTools(scoped, "", cfg, "cli", tools.PlatformToolsOptions{}) {
		toolsets = append(toolsets, name)
	}
	sort.Strings(toolsets)
	if len(toolsets) == 0 {
		return nil
	}
	return toolsets
}

var (
	retaggedWorkspaceRootsMu sync.Mutex
	retaggedWorkspaceRoots   = map[string]bool{}
)

// Retags pre tag worker sessions of a workspaces root once per process, never failing the tick
func retagLegacyWorkerSessions(ctx context.Context, workspacesRootPath string) {
	retaggedWorkspaceRootsMu.Lock()
	done := retaggedWorkspaceRoots[workspacesRootPath]
	retaggedWorkspaceRootsMu.Unlock()
	if done {
		return
	}
	db, err := state.Acquire(ctx, "")
	if err != nil {
		log.Debug(fmt.Sprintf("kanban worker: legacy session retag skipped (%s)", pyrepr.ExceptionMessage(err)))
		return
	}
	_, err = db.RetagKanbanWorkerSessions(ctx, workspacesRootPath)
	state.ReleaseOrClose(db)
	if err != nil {
		log.Debug(fmt.Sprintf("kanban worker: legacy session retag skipped (%s)", pyrepr.ExceptionMessage(err)))
		return
	}
	retaggedWorkspaceRootsMu.Lock()
	retaggedWorkspaceRoots[workspacesRootPath] = true
	retaggedWorkspaceRootsMu.Unlock()
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
	maxBytes, backupCount, err := WorkerLogRotationConfig(ctx, nil)
	if err != nil {
		return nil, err
	}
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
	for _, name := range []string{"TERMINAL_TIMEOUT", "TERMINAL_MAX_FOREGROUND_TIMEOUT"} {
		var current *string
		if v, ok := env[name]; ok {
			current = &v
		}
		if override := workerTerminalTimeoutEnv(task.MaxRuntimeSeconds, current); override != nil {
			env[name] = *override
		}
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
	delete(env, "ICEBATH_TUI")
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

// One daemon tick that prints any failure as a traceback instead of ending the loop
func daemonTick(ctx context.Context, o DaemonOptions) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(o.Stderr, "Traceback (most recent call last):\n%s%v\n", debug.Stack(), r)
		}
	}()
	maxInProgress := ResolveMaxInProgress(ConfiguredMaxInProgress(ctx))
	var res *DispatchResult
	err := ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error {
		opts := DefaultDispatchOptions()
		opts.MaxSpawn, opts.MaxInProgress, opts.FailureLimit = o.MaxSpawn, maxInProgress, o.FailureLimit
		var err error
		res, err = DispatchOnce(ctx, conn, opts)
		return err
	})
	if err != nil {
		fmt.Fprintf(o.Stderr, "Traceback (most recent call last):\n%s: %s\n", pyrepr.ExceptionName(err), pyrepr.ExceptionMessage(err))
		return
	}
	if o.OnTick != nil {
		func() {
			defer func() { _ = recover() }()
			o.OnTick(res)
		}()
	}
}
