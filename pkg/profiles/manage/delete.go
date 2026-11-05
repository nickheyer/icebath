package manage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// Reader behind the delete confirmation prompt, swapped by tests
var stdin io.Reader = os.Stdin

var backendTokens = map[string]bool{"serve": true, "dashboard": true, "gateway": true}

var icebathArgvMarkers = []string{"icebath_cli.main", "icebath-gateway", "tui_gateway"}

var pythonInterpreterRe = regexp.MustCompile(`^python[\d.]*w?(\.exe)?$`)

var icebathConsoleScripts = map[string]bool{"icebath": true, "icebath-agent": true, "icebath-acp": true}

// Reports an icebath process by entrypoint marker, executable name or an interpreter running a console script
func isIcebathArgv(argv []string) bool {
	joined := strings.Join(argv, " ")
	exe := strings.ToLower(pathBase(argv[0]))
	for _, m := range icebathArgvMarkers {
		if strings.Contains(joined, m) {
			return true
		}
	}
	if strings.HasPrefix(exe, "icebath") {
		return true
	}
	if len(argv) >= 2 && pythonInterpreterRe.MatchString(exe) {
		script := strings.ToLower(pathBase(argv[1]))
		if i := strings.LastIndex(script, "."); i >= 0 {
			script = script[:i]
		}
		return icebathConsoleScripts[script]
	}
	return false
}

// Final component like os.path.basename, splitting on the host separators
func pathBase(p string) string {
	i := strings.LastIndexAny(p, string(filepath.Separator)+"/")
	return p[i+1:]
}

// Every profile name an argv selects through -p, --profile or --profile=
func argvProfileSelectors(argv []string) []string {
	var out []string
	for i, tok := range argv {
		if (tok == "--profile" || tok == "-p") && i+1 < len(argv) {
			out = append(out, argv[i+1])
		} else if v, ok := strings.CutPrefix(tok, "--profile="); ok {
			out = append(out, v)
		}
	}
	return out
}

// Process probes behind the backend scan, swapped by tests
var (
	listPIDs    = proc.PIDs
	procParents = proc.Parents
	procUser    = proc.Username
	procCmdline = proc.Cmdline
	procEnviron = proc.Environ
)

// Pids of this user's icebath backends bound to the profile, never this process or its ancestors
func profileBoundBackendPids(canon, profileDir string) []int {
	resolved := resolvePath(profileDir)
	skip := map[int]bool{os.Getpid(): true}
	if parents, err := procParents(os.Getpid()); err == nil {
		for _, p := range parents {
			skip[p] = true
		}
	}
	currentUser, userErr := procUser(os.Getpid())
	pids, err := listPIDs()
	if err != nil {
		return []int{}
	}
	out := []int{}
	for _, pid := range pids {
		if skip[pid] {
			continue
		}
		if userErr == nil {
			if u, err := procUser(pid); err != nil || u != currentUser {
				continue
			}
		}
		argv, err := procCmdline(pid)
		if err != nil || len(argv) == 0 || !isIcebathArgv(argv) {
			continue
		}
		backend := false
		for _, tok := range argv {
			backend = backend || backendTokens[strings.ToLower(tok)]
		}
		if !backend {
			continue
		}
		bound, skipProc := false, false
		for _, sel := range argvProfileSelectors(argv) {
			n, err := profiles.NormalizeProfileName(sel)
			if err != nil {
				skipProc = true
				break
			}
			bound = bound || n == canon
		}
		if skipProc {
			continue
		}
		if !bound {
			if env, err := procEnviron(pid); err == nil {
				envHome := env[home.EnvHome]
				bound = envHome != "" && resolvePath(envHome) == resolved
			}
		}
		if bound {
			out = append(out, pid)
		}
	}
	return out
}

// Sleeps between liveness polls of stopping processes, swapped by tests
var killPollSleep = time.Sleep

// Waits up to wait for pids to exit then force kills stragglers, true when all exited gracefully
func waitThenForceKill(pids []int, startTimes map[int]*int64, wait time.Duration) bool {
	for range int(wait / (500 * time.Millisecond)) {
		killPollSleep(500 * time.Millisecond)
		alive := false
		for _, pid := range pids {
			alive = alive || gatewayPidExists(pid)
		}
		if !alive {
			return true
		}
	}
	for _, pid := range pids {
		if gatewayPidExists(pid) {
			expected, ok := startTimes[pid]
			if !ok {
				if st, known := gatewayProcessStartTime(pid); known {
					expected = &st
				}
			}
			_ = gatewayTerminatePid(pid, true, expected)
		}
	}
	return false
}

// Terminates stray backends bound to the profile that the gateway pid file never names
func stopProfileBackends(canon, profileDir string) {
	pids := profileBoundBackendPids(canon, profileDir)
	if len(pids) == 0 {
		return
	}
	for _, pid := range pids {
		_ = gatewayTerminatePid(pid, false, nil)
	}
	waitThenForceKill(pids, map[int]*int64{}, 10*time.Second)
	fmt.Fprintf(stdout, "✓ Stopped %d profile backend process(es)\n", len(pids))
}

// Stops the profile's Bot Desktop under its own home before the directory moves or goes, logging failures
func stopBotDesktop(ctx context.Context, profileDir string) {
	if !botDesktopSupportedHost() {
		return
	}
	pctx := home.WithOverride(ctx, profileDir)
	stopped, err := botDesktopStop(pctx)
	if err == nil && stopped {
		fmt.Fprintln(stdout, "✓ Bot Desktop stopped")
		err = botDesktopLeaseRelease(pctx)
	}
	if err != nil {
		slog.Default().Warn(fmt.Sprintf("Could not stop the Bot Desktop of %s: %v", profileDir, err))
	}
}

// Shows what a delete is about to remove
func printDeleteSummary(ctx context.Context, canon, profileDir string, gwRunning bool, wrapperPath string) error {
	model, prov, err := profiles.ReadConfigModel(ctx, profileDir)
	if err != nil {
		return err
	}
	skillCount := countSkills(profileDir)
	distName, distVersion, distSource := profiles.ReadDistributionMeta(profileDir)
	fmt.Fprintf(stdout, "\nProfile: %s\n", canon)
	fmt.Fprintf(stdout, "Path:    %s\n", profileDir)
	if truthy(model) {
		line := "Model:   " + pyrepr.Str(model)
		if truthy(prov) {
			line += " (" + pyrepr.Str(prov) + ")"
		}
		fmt.Fprintln(stdout, line)
	}
	if skillCount > 0 {
		fmt.Fprintf(stdout, "Skills:  %d\n", skillCount)
	}
	if truthy(distName) {
		version := "?"
		if truthy(distVersion) {
			version = pyrepr.Str(distVersion)
		}
		fmt.Fprintf(stdout, "Distribution: %s@%s\n", pyrepr.Str(distName), version)
		if truthy(distSource) {
			fmt.Fprintf(stdout, "Installed from: %s\n", pyrepr.Str(distSource))
		}
	}
	fmt.Fprintln(stdout, "\nThis will permanently delete:")
	fmt.Fprintln(stdout, "  • All config, API keys, memories, sessions, skills, cron jobs")
	if wrapperPath != "" {
		fmt.Fprintf(stdout, "  • Command alias (%s)\n", wrapperPath)
	}
	if gwRunning {
		fmt.Fprintln(stdout, "  ⚠ Gateway is running — it will be stopped.")
	}
	return nil
}

// Error of a delete whose directory went but whose durable identity settlement is still pending
type ProfileIdentitySettlementPending struct {
	Profile      string
	Path         string
	RetryCommand string
}

func (e *ProfileIdentitySettlementPending) Error() string {
	return fmt.Sprintf("Profile '%s' was deleted, but its session/routing identity settlement is still pending — run: %s", e.Profile, e.RetryCommand)
}

func newSettlementPending(profile, path string) *ProfileIdentitySettlementPending {
	return &ProfileIdentitySettlementPending{Profile: profile, Path: path, RetryCommand: "icebath profile purge-identity " + profile}
}

// Error of a delete or rename step that failed after the confirmation, the RuntimeError of the Python flow
type RuntimeError struct {
	Msg   string
	Cause error
}

func (e *RuntimeError) Error() string { return e.Msg }
func (e *RuntimeError) Unwrap() error { return e.Cause }

// Deletes a profile with its wrapper, gateway service, backends and identity after a typed confirmation unless yes
func DeleteProfile(ctx context.Context, name string, yes bool) (string, error) {
	canon, err := profiles.NormalizeProfileName(name)
	if err != nil {
		return "", err
	}
	if canon == "default" {
		return "", &profiles.ValueError{Msg: "Cannot delete the default profile (~/.icebath).\nTo remove everything, use: icebath uninstall"}
	}
	canon, profileDir, err := profiles.ExistingProfileDir(canon)
	if err != nil {
		return "", err
	}
	gwRunning := checkGatewayRunning(profileDir)
	wrapperPath, err := profiles.FindOurWrapper(canon)
	if err != nil {
		return "", err
	}
	if err := printDeleteSummary(ctx, canon, profileDir, gwRunning, wrapperPath); err != nil {
		return "", err
	}
	if !yes {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Type '%s' to confirm: ", canon)
		line, rerr := bufio.NewReader(stdin).ReadString('\n')
		confirm, answered := pystr.Strip(strings.TrimSuffix(line, "\n")), true
		if rerr != nil && line == "" {
			answered = false
			fmt.Fprintln(stdout)
		}
		if !answered || confirm != canon {
			fmt.Fprintln(stdout, "Cancelled.")
			return profileDir, nil
		}
	}
	cleanupGatewayService(ctx, canon, profileDir)
	maybeUnregisterGatewayService(canon)
	if gwRunning {
		stopGatewayProcess(profileDir)
	}
	stopProfileBackends(canon, profileDir)
	stopBotDesktop(ctx, profileDir)
	if err := home.MarkProfileDeleted(profileDir); err != nil {
		return "", err
	}
	notifyMultiplexer(canon)
	identitySettled := purgeIdentity(ctx, canon)
	key := home.Key(profileDir)
	mcp.ShutdownMCPServers(ctx, &key, nil, 15*time.Second)
	if released, err := memoryStoreReleaseAllUnder(profileDir); err == nil && released > 0 {
		fmt.Fprintf(stdout, "✓ Released %d memory-store connection(s) held by this process\n", released)
	}
	if closed := state.CloseAllUnder(profileDir); closed > 0 {
		fmt.Fprintf(stdout, "✓ Released %d session database connection(s) held by this process\n", closed)
	}
	if released, err := releaseProfileLogHandlers(profileDir); err == nil && released > 0 {
		fmt.Fprintf(stdout, "✓ Released %d profile log handler(s) held by this process\n", released)
	}
	if wrapperPath != "" {
		removed, err := profiles.RemoveWrapperScript(canon)
		if err != nil {
			return "", err
		}
		if removed {
			fmt.Fprintf(stdout, "✓ Removed %s\n", wrapperPath)
		}
	}
	removeErr := removeProfileTree(profileDir)
	if removeErr == nil {
		fmt.Fprintf(stdout, "✓ Removed %s\n", profileDir)
	} else {
		fmt.Fprintf(stdout, "⚠ Could not remove %s: %s\n", profileDir, errText(removeErr))
	}
	profiles.RetargetActiveProfile(canon, "default", "✓ Active profile reset to default")
	if removeErr != nil {
		return "", &RuntimeError{Msg: fmt.Sprintf("Could not remove profile directory %s: %s", profileDir, errText(removeErr)), Cause: removeErr}
	}
	fmt.Fprintf(stdout, "\nProfile '%s' deleted.\n", canon)
	if !identitySettled {
		return "", newSettlementPending(canon, profileDir)
	}
	return profileDir, nil
}

// Waits out a live owner then removes the tree with the make writable retry
func removeProfileTree(profileDir string) error {
	unowned, err := WaitProfileUnowned(profileDir, nil)
	if err != nil {
		return err
	}
	if !unowned {
		return &RuntimeError{Msg: "a running gateway still owns this profile; retry the delete once it settles"}
	}
	return rmtreeWithRetry(profileDir, rmtreeMakeWritable)
}

// Message of an error the way Python str() shows an exception, OSError text for filesystem failures
func errText(err error) string {
	var pe *fs.PathError
	var le *os.LinkError
	if errors.As(err, &pe) || errors.As(err, &le) {
		return pyfs.OSErrorText(err)
	}
	return err.Error()
}

// Service manager capable of runtime gateway registration inside a container
type s6Manager interface {
	SupportsRuntimeRegistration() bool
	RegisterProfileGateway(name string, startNow bool) error
	UnregisterProfileGateway(name string) error
}

// The s6 manager inside the container, nil on a host or when detection fails
func s6RuntimeManager() s6Manager {
	kind, err := serviceManagerDetect()
	if err != nil || kind != "s6" {
		return nil
	}
	mgr, err := serviceManagerGet()
	if err != nil || mgr == nil || !mgr.SupportsRuntimeRegistration() {
		return nil
	}
	return mgr
}

// Registers a profile gateway with s6 inside the container, warning on failure and accepting an existing slot
func maybeRegisterGatewayService(profileName string) {
	mgr := s6RuntimeManager()
	if mgr == nil {
		return
	}
	err := mgr.RegisterProfileGateway(profileName, false)
	var already *profiles.ValueError
	if err != nil && !errors.As(err, &already) {
		fmt.Fprintf(stdout, "⚠ Could not register s6 gateway service: %v\n", err)
	}
}

// Tears down a profile's s6 gateway service inside the container, a host no op
func maybeUnregisterGatewayService(profileName string) {
	mgr := s6RuntimeManager()
	if mgr == nil {
		return
	}
	if err := mgr.UnregisterProfileGateway(profileName); err != nil {
		fmt.Fprintf(stdout, "⚠ Could not unregister s6 gateway service: %v\n", err)
	}
}

// Host platform the service cleanup branches on, swapped by tests
var hostSystem = func() string {
	switch runtime.GOOS {
	case "linux":
		return "Linux"
	case "darwin":
		return "Darwin"
	case "windows":
		return "Windows"
	}
	return runtime.GOOS
}

// Disables and removes the profile's user service registration, true when one was removed
func cleanupGatewayService(ctx context.Context, name, profileDir string) bool {
	oldHome, hadHome := os.LookupEnv(home.EnvHome)
	pctx := home.WithOverride(ctx, profileDir)
	os.Setenv(home.EnvHome, profileDir)
	defer func() {
		os.Unsetenv(home.EnvHome)
		if hadHome {
			os.Setenv(home.EnvHome, oldHome)
		}
	}()
	removed, err := cleanupGatewayServiceIn(pctx, profileDir)
	if err != nil {
		fmt.Fprintf(stdout, "⚠ Service cleanup: %s\n", errText(err))
		return false
	}
	return removed
}

// Platform arms of the service cleanup under the profile home
func cleanupGatewayServiceIn(ctx context.Context, profileDir string) (bool, error) {
	run := func(args ...string) error { _, err := runQuiet(10*time.Second, args...); return err }
	switch hostSystem() {
	case "Linux":
		removed := false
		svc := gatewayServiceName(ctx)
		svcFile := filepath.Join(gatewayUserSystemdUnitDir(ctx), svc+".service")
		if pathExists(svcFile) && ownedByProfile(svcFile, profileDir) {
			for _, args := range [][]string{{"systemctl", "--user", "disable", svc}, {"systemctl", "--user", "stop", svc}} {
				if err := run(args...); err != nil {
					return false, err
				}
			}
			if err := unlinkMissingOK(svcFile); err != nil {
				return false, err
			}
			if err := run("systemctl", "--user", "daemon-reload"); err != nil {
				return false, err
			}
			fmt.Fprintf(stdout, "✓ Service %s removed\n", svc)
			removed = true
		}
		system, err := removeSystemSystemdUnit(ctx, profileDir)
		return system || removed, err
	case "Darwin":
		plist := gatewayLaunchdPlistPath(ctx)
		if pathExists(plist) && ownedByProfile(plist, profileDir) {
			if err := run("launchctl", "unload", plist); err != nil {
				return false, err
			}
			if err := unlinkMissingOK(plist); err != nil {
				return false, err
			}
			fmt.Fprintln(stdout, "✓ Launchd service removed")
			return true, nil
		}
		return false, nil
	case "Windows":
		return removeWindowsTask(ctx)
	}
	return false, nil
}

// Stops a running gateway through its pid file, refusing a record stamped with another profile's home
func stopGatewayProcess(profileDir string) {
	pidFile := filepath.Join(profileDir, "gateway.pid")
	if !pathExists(pidFile) {
		return
	}
	err := stopRecordedGateway(profileDir, pidFile)
	if err == nil {
		return
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, fs.ErrPermission) {
		fmt.Fprintln(stdout, "✓ Gateway already stopped")
		return
	}
	fmt.Fprintf(stdout, "⚠ Could not stop gateway: %s\n", errText(err))
}

// Reads the pid record and terminates its gateway gracefully then by force
func stopRecordedGateway(profileDir, pidFile string) error {
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return err
	}
	text := pystr.Strip(strings.TrimPrefix(string(raw), "\ufeff"))
	data := map[string]any{}
	if strings.HasPrefix(text, "{") {
		v, err := pyjson.Loads(text)
		if err != nil {
			return err
		}
		obj, ok := v.(*pyjson.Object)
		if !ok {
			if _, isList := v.([]any); isList {
				return errors.New("list indices must be integers or slices, not str")
			}
			return fmt.Errorf("'%s' object is not subscriptable", pyrepr.TypeName(v))
		}
		data = obj.Vals
	} else {
		n, err := pyInt(text)
		if err != nil {
			return err
		}
		data["pid"] = n
	}
	rawPID, ok := data["pid"]
	if !ok {
		return errors.New("'pid'")
	}
	pid, err := pyInt(rawPID)
	if err != nil {
		return err
	}
	if gatewayRecordedHomeConflicts(data, profileDir) {
		fmt.Fprintf(stdout, "✗ Refusing to stop PID %d: its recorded ICEBATH_HOME belongs to a different profile than %s (stale/poisoned PID record, #89315).\n", pid, profileDir)
		return nil
	}
	var expected *int64
	if st, ok := pyIntOK(data["start_time"]); ok {
		expected = &st
	} else if st, known := gatewayProcessStartTime(pid); known {
		expected = &st
	}
	if err := gatewayTerminatePid(pid, false, nil); err != nil {
		return err
	}
	if waitThenForceKill([]int{pid}, map[int]*int64{pid: expected}, 10*time.Second) {
		fmt.Fprintf(stdout, "✓ Gateway stopped (PID %d)\n", pid)
	} else {
		fmt.Fprintf(stdout, "✓ Gateway force-stopped (PID %d)\n", pid)
	}
	return nil
}

// Python int() of a decoded value, the ValueError text for an unparsable string
func pyInt(v any) (int, error) {
	switch x := v.(type) {
	case int:
		return x, nil
	case int64:
		return int(x), nil
	case float64:
		return int(x), nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		n, err := strconv.Atoi(strings.ReplaceAll(pystr.Strip(x), "_", ""))
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(x))
		}
		return n, nil
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(v))
}

// Integer start time of a pid record, false when absent or null
func pyIntOK(v any) (int64, bool) {
	if v == nil {
		return 0, false
	}
	n, err := pyInt(v)
	return int64(n), err == nil
}
