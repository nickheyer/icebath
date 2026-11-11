package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hidden entry of the install binary that respawns a gateway argv once the old pid exits
const GatewayRestartWatcherEntry = "gateway-restart-watcher"

// Seconds the detached restart watcher waits for the old pid to exit before giving up
const GatewayRestartWatcherTimeoutS = 120.0

// Package seams of the restart watcher spawn the tests swap as hermes monkeypatches subprocess
var (
	restartWatcherBinary = func() string {
		if binary := pm.ResolveInstallBinary(serviceDefProjectRoot()); binary != "" {
			return binary
		}
		binary, _ := os.Executable()
		return binary
	}
	restartWatcherStart = func(cmd *exec.Cmd) error {
		if err := cmd.Start(); err != nil {
			return err
		}
		go cmd.Wait()
		return nil
	}
	updateRestartCaptureArgv = captureGatewayArgv
	updateRestartByProfile   = LaunchDetachedProfileGatewayRestart
	updateRestartByCmdline   = LaunchDetachedGatewayRestartByCmdline
)

// Who relaunches a profile gateway after an update, its external supervisor, the profile watcher or the argv replay
func PrepareProfileGatewayUpdateRestart(ctx context.Context, profile string, pid int) (string, bool, error) {
	argv := updateRestartCaptureArgv(pid)
	if len(argv) > 0 && slices.Contains(argv, "--external-supervisor") {
		return "external-supervisor", true, nil
	}
	if launched, err := updateRestartByProfile(ctx, profile, pid); err != nil || launched {
		return "detached", launched, err
	}
	if len(argv) > 0 {
		if launched, err := updateRestartByCmdline(ctx, pid, slices.Clone(argv), nil); err != nil || launched {
			return "detached-cmdline", launched, err
		}
	}
	return "", false, nil
}

// Relaunches a gateway with no profile pid file by replaying its captured argv on the home it ran on
func LaunchDetachedGatewayRestartByCmdline(ctx context.Context, oldPid int, runArgv []string, profileHome *string) (bool, error) {
	if oldPid <= 0 || len(runArgv) == 0 {
		return false, nil
	}
	served := ""
	if profileHome != nil {
		served = *profileHome
	}
	return SpawnGatewayRestartWatcher(ctx, oldPid, slices.Clone(runArgv), nil, served)
}

// Relaunches a manually run profile gateway after its current pid exits
func LaunchDetachedProfileGatewayRestart(ctx context.Context, profile string, oldPid int) (bool, error) {
	if oldPid <= 0 {
		return false, nil
	}
	host := profile == "default"
	return SpawnGatewayRestartWatcher(ctx, oldPid, GatewayRunArgsForProfile(profile), &host, "")
}

// Index of the watcher entry token after an icebath binary in lowered tokens, minus one when absent
func restartWatcherEntryIndex(tokens []string) int {
	for i := 0; i+1 < len(tokens); i++ {
		base := tokens[i][strings.LastIndex(tokens[i], "/")+1:]
		if (base == "icebath" || base == "icebath.exe") && tokens[i+1] == GatewayRestartWatcherEntry {
			return i + 1
		}
	}
	return -1
}

// Installation bound gateway run argv of a profile, the default profile carrying no selector
func GatewayRunArgsForProfile(profile string) []string {
	var args []string
	if profile != "default" {
		args = append(args, "--profile", profile)
	}
	args = append(args, "gateway", "run", "--replace")
	return pm.InstallRuntimeCommand(serviceDefProjectRoot(), args, pm.LaunchCommandOptions{})
}

// True when argv relaunches the host multiplexer, decided by settled identity before the default root comparison
func RestartArgvIsHostGateway(ctx context.Context, argv []string) bool {
	if len(argv) == 0 || !slices.Contains(argv, "gateway") {
		return false
	}
	for _, flag := range []string{"--profile", "-p"} {
		if idx := slices.Index(argv, flag); idx >= 0 {
			name := ""
			if idx+1 < len(argv) {
				name = argv[idx+1]
			}
			return name == "default"
		}
	}
	if slices.Contains(argv, "--profile=default") {
		return true
	}
	for _, part := range argv {
		if strings.HasPrefix(part, "--profile=") && !strings.HasSuffix(part, "=default") {
			return false
		}
	}
	if provider.IsMultiplexActive(ctx) {
		return true
	}
	if record := ReadHostRecord(RoleGateway, false); record != nil && RecordNamesLiveOwner(record) && len(record.Profiles) > 1 {
		return true
	}
	mine, err1 := pypath.Resolve(home.Dir(ctx))
	root, err2 := pypath.Resolve(home.RootOf(home.Dir(ctx)))
	return err1 == nil && err2 == nil && mine == root
}

// Scrubbed default profile env of a detached host gateway respawn watcher
func hostGatewayWatcherEnv(ctx context.Context) (map[string]string, error) {
	env, err := tools.HostGatewayChildEnv(ctx, nil, nil)
	if err != nil {
		return nil, err
	}
	delete(env, "_ICEBATH_GATEWAY")
	return env, nil
}

// KEY=value list of an env map in sorted key order
func environList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// Spawns the detached watcher that respawns runArgv once oldPid exits, false when it could not be started
func SpawnGatewayRestartWatcher(ctx context.Context, oldPid int, runArgv []string, host *bool, profileHome string) (bool, error) {
	if oldPid <= 0 || len(runArgv) == 0 {
		return false, nil
	}
	respawnCwd, overlay := "", map[string]string{}
	if serviceGOOS == "windows" {
		runArgv, respawnCwd, overlay = windowlessGatewayRestartSpec(ctx, slices.Clone(runArgv), profileHome)
	}
	cwdLiteral, err := pyjson.Dumps(respawnCwd)
	if err != nil {
		return false, err
	}
	envLiteral, err := pyjson.Dumps(overlay, pyjson.SortKeys(true))
	if err != nil {
		return false, err
	}
	watcherArgv := append([]string{restartWatcherBinary(), GatewayRestartWatcherEntry, cwdLiteral, envLiteral, strconv.Itoa(oldPid)}, runArgv...)
	isHost := false
	if host != nil {
		isHost = *host
	} else {
		isHost = RestartArgvIsHostGateway(ctx, runArgv)
	}
	var watcherEnv []string
	if isHost {
		env, err := hostGatewayWatcherEnv(ctx)
		if err != nil {
			return false, err
		}
		watcherEnv = environList(env)
	}
	start := func(breakaway bool) error {
		cmd := exec.Command(watcherArgv[0], watcherArgv[1:]...)
		cmd.Env = watcherEnv
		cmd.SysProcAttr = home.DetachAttr(breakaway)
		return restartWatcherStart(cmd)
	}
	if err := start(true); err != nil {
		if err := start(false); err != nil {
			return false, nil
		}
	}
	return true, nil
}

// Runs the restart watcher entry and exits when this process was launched as one
func DispatchGatewayRestartWatcherEntry(ctx context.Context) {
	if len(os.Args) > 1 && os.Args[1] == GatewayRestartWatcherEntry {
		os.Exit(GatewayRestartWatcherMain(ctx, os.Args[2:], os.Stderr))
	}
}

// Gateway stdio sidecar log under the process home, nil when the home or the log cannot be opened
func openRestartWatcherStdio() (fh *os.File, logPath string) {
	defer func() {
		if recover() != nil {
			fh, logPath = nil, ""
		}
	}()
	logDir := filepath.Join(home.Process(), "logs")
	if err := os.MkdirAll(logDir, 0o777); err != nil {
		return nil, ""
	}
	logPath = filepath.Join(logDir, "gateway-stdio.log")
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return nil, ""
	}
	return f, logPath
}

// Respawn cwd, env overlay, old pid and command a watcher was spawned with
func parseRestartWatcherArgs(args []string) (string, map[string]string, int, []string, error) {
	if len(args) < 3 {
		return "", nil, 0, nil, errors.New("IndexError: list index out of range")
	}
	var respawnCwd string
	if err := json.Unmarshal([]byte(args[0]), &respawnCwd); err != nil {
		return "", nil, 0, nil, err
	}
	overlay := map[string]string{}
	if err := json.Unmarshal([]byte(args[1]), &overlay); err != nil {
		return "", nil, 0, nil, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(args[2]))
	if err != nil {
		return "", nil, 0, nil, fmt.Errorf("ValueError: invalid literal for int() with base 10: %s", pyrepr.Repr(args[2]))
	}
	return respawnCwd, overlay, pid, args[3:], nil
}

// Waits for the old pid to exit then respawns the command detached, its output in the gateway stdio log
func GatewayRestartWatcherMain(ctx context.Context, args []string, stderr io.Writer) int {
	respawnCwd, overlay, pid, command, err := parseRestartWatcherArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	deadline := time.Now().Add(time.Duration(GatewayRestartWatcherTimeoutS * float64(time.Second)))
	for time.Now().Before(deadline) {
		if !proc.Exists(pid) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	stdio, logPath := openRestartWatcherStdio()
	if stdio != nil {
		defer stdio.Close()
	}
	if len(command) == 0 {
		fmt.Fprintln(stderr, "IndexError: list index out of range")
		return 1
	}
	baseEnv := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			baseEnv[k] = v
		}
	}
	for k, v := range overlay {
		baseEnv[k] = v
	}
	popen := func(env []string, attr *syscall.SysProcAttr) error {
		cmd := exec.Command(command[0], command[1:]...)
		if stdio != nil {
			cmd.Stdout, cmd.Stderr = stdio, stdio
		}
		cmd.Dir, cmd.Env, cmd.SysProcAttr = respawnCwd, env, attr
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	switch {
	case runtime.GOOS == "windows":
		withStamp := func(stamp string) []string {
			env := maps.Clone(baseEnv)
			env[windowsGatewayBreakawayEnv] = stamp
			return environList(env)
		}
		if err := popen(withStamp("1"), home.DetachAttr(true)); err != nil {
			if err := popen(withStamp("0"), home.DetachAttr(false)); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
		return 0
	case runtime.GOOS == "darwin" && stdio != nil:
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		job, err := startGatewayJob(ctx, command, baseEnv, cwd, logPath, math.Inf(1))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if job != nil {
			return 0
		}
	}
	var env []string
	if len(overlay) > 0 {
		env = environList(baseEnv)
	}
	if err := popen(env, home.DetachAttr(true)); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
