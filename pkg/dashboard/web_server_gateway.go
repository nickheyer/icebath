package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Calls into hermes_cli.gateway, update_cmd_fleet and gateway_migrate, packages above this one, assigned in cmd/icebath/wiring.go
var GatewayCLI struct {
	NeedsSudo            func(scope string) bool
	SudoNoninteractiveOk func(ctx context.Context, targetedProbe []string) bool
	RefusalLines         func(ctx context.Context, verb string, homes []string, flagHint, profile string) ([]string, error)
	InstalledServices    func(ctx context.Context, profileDir string) (int, error)
}

const dashboardJSONResponseBodyMaxBytes = 1024 * 1024

// JSON body of an HTTP response read under a hard size cap
func readDashboardJSONResponse(body io.Reader) (any, error) {
	raw, err := io.ReadAll(io.LimitReader(body, dashboardJSONResponseBodyMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > dashboardJSONResponseBodyMaxBytes {
		return nil, fmt.Errorf("dashboard HTTP JSON response exceeded %d bytes", dashboardJSONResponseBodyMaxBytes)
	}
	return pyjson.LoadsBytes(raw)
}

var gatewayHealthSuffixRE = regexp.MustCompile(`/health(/detailed)?$`)

// Liveness and body of the cross container gateway health endpoint, detailed first with the bearer key
func probeGatewayHealth(ctx context.Context) (bool, any) {
	url, ok := GatewayHealthURL()
	if !ok || url == "" {
		return false, nil
	}
	base := gatewayHealthSuffixRE.ReplaceAllString(strings.TrimRight(url, "/"), "")
	apiKey := pystr.Strip(os.Getenv("API_SERVER_KEY"))
	client := &http.Client{Timeout: time.Duration(GatewayHealthTimeout() * float64(time.Second))}
	for _, path := range []string{base + "/health/detailed", base + "/health"} {
		ok, body := probeGatewayHealthPath(ctx, client, path, apiKey)
		if ok {
			return true, body
		}
	}
	return false, nil
}

// One health probe, true with the decoded body on a 200 that parses
func probeGatewayHealthPath(ctx context.Context, client *http.Client, path, apiKey string) (bool, any) {
	req, err := http.NewRequestWithContext(ctx, "GET", path, nil)
	if err != nil {
		return false, nil
	}
	if apiKey != "" && strings.HasSuffix(path, "/health/detailed") {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, nil
	}
	body, err := readDashboardJSONResponse(resp.Body)
	if err != nil {
		return false, nil
	}
	return true, body
}

type platformPortSpec struct {
	key  string
	port int64
}

var portBindingPlatformPorts = map[string]platformPortSpec{
	"webhook": {"port", 8644}, "api_server": {"port", 8642}, "msgraph_webhook": {"port", 8646},
	"feishu": {"webhook_port", 8765}, "wecom_callback": {"port", 8645}, "bluebubbles": {"webhook_port", 8645},
	"sms": {"webhook_port", 8080}, "whatsapp_cloud": {"webhook_port", 8090}, "line": {"port", 8646},
	"teams": {"port", 3978},
}

var platformDeadStates = map[string]bool{"fatal": true, "disconnected": true, "stopped": true}

// Host TCP port per live port binding platform of one profile, from its config then the adapter default
func profilePlatformPorts(ctx context.Context, profileHome string, runtimeStatus *pyjson.Object) *pyjson.Object {
	ports := pyjson.NewObject()
	var platforms *pyjson.Object
	if runtimeStatus != nil {
		platforms, _ = runtimeStatus.GetOr("platforms", nil).(*pyjson.Object)
	}
	if platforms == nil {
		return ports
	}
	var active []string
	for _, name := range platforms.Keys {
		state, ok := platforms.Vals[name].(*pyjson.Object)
		if _, binds := portBindingPlatformPorts[name]; !binds || !ok {
			continue
		}
		if s, isStr := state.GetOr("state", nil).(string); isStr && platformDeadStates[s] {
			continue
		}
		active = append(active, name)
	}
	if len(active) == 0 {
		return ports
	}
	blocks := profilePlatformBlocks(ctx, profileHome)
	for _, name := range active {
		spec := portBindingPlatformPorts[name]
		block := blocks[name]
		extra, _ := block["extra"].(map[string]any)
		raw, ok := block[spec.key]
		if !ok {
			raw, ok = extra[spec.key]
		}
		if !ok {
			raw = spec.port
		}
		n, err := pyint.Int64(raw)
		if err != nil {
			n = spec.port
		}
		ports.Set(name, n)
	}
	return ports
}

// Merged gateway.platforms then top level platforms blocks of a profile config, empty when unreadable
func profilePlatformBlocks(ctx context.Context, profileHome string) map[string]map[string]any {
	blocks := map[string]map[string]any{}
	cfg, err := config.ReadUserConfigRaw(ctx, filepath.Join(profileHome, "config.yaml"))
	if err != nil {
		return map[string]map[string]any{}
	}
	gatewayCfg, _ := cfg["gateway"].(map[string]any)
	for _, src := range []any{gatewayCfg["platforms"], cfg["platforms"]} {
		m, ok := src.(map[string]any)
		if !ok {
			continue
		}
		for platName, platBlock := range m {
			b, ok := platBlock.(map[string]any)
			if !ok {
				continue
			}
			if blocks[platName] == nil {
				blocks[platName] = map[string]any{}
			}
			for k, v := range b {
				blocks[platName][k] = v
			}
		}
	}
	return blocks
}

// Pid and start time of the profile's live gateway, false when none
func profileGatewayWriterIdentity(profileHome string, runtimeStatus *pyjson.Object) (int, any, bool) {
	pid, ok, err := gateway.GetRuntimeStatusRunningPid(runtimeStatus, profileHome)
	if err != nil || !ok {
		return 0, nil, false
	}
	start := gateway.GetProcessStartTime(pid)
	if start == nil {
		return 0, nil, false
	}
	return pid, start, true
}

// Platform entries stamped by the profile's current gateway process, empty without a live writer
func ownedProfilePlatforms(livePid int, liveStart any, live bool, platforms *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	if !live {
		return out
	}
	for _, key := range platforms.Keys {
		value, ok := platforms.Vals[key].(*pyjson.Object)
		if !ok {
			continue
		}
		if pyjson.Equal(value.GetOr("writer_pid", nil), livePid) && pyjson.Equal(value.GetOr("writer_start_time", nil), liveStart) {
			out.Set(key, value)
		}
	}
	return out
}

var collectProfileGatewayTopology = collectProfileGatewayTopologyNow

// Collaborators of the topology scan, the profiles, gateway.status and module attributes hermes tests patch
var (
	topologyProfilesToServe     = profiles.ProfilesToServe
	checkGatewayRunning         = manage.CheckGatewayRunning
	readRuntimeStatus           = gateway.ReadRuntimeStatus
	gatewayWriterIdentityOfHome = profileGatewayWriterIdentity
)

// Profiles, the live gateways serving them, the gateway mode and owned platform maps for the status route
func collectProfileGatewayTopologyNow(ctx context.Context) *pyjson.Object {
	homes, err := topologyProfilesToServe(ctx, true, true, true)
	if err != nil {
		webServerLog().Debug("profile/gateway topology enumeration failed", "error", err)
		return pyjson.ObjectOf("profiles", []any{}, "gateway_mode", "unknown", "gateways", []any{},
			"profile_platforms", pyjson.NewObject())
	}
	gateways := []any{}
	profilePlatforms := pyjson.NewObject()
	multiplex := false
	var standaloneReason any
	for _, sp := range homes {
		var running bool
		if sp.Name == "default" {
			running = checkGatewayRunning(sp.Home)
		} else {
			own, err := hasOwnGateway(sp.Home)
			running = own && err == nil
		}
		if !running {
			continue
		}
		rt := readRuntimeStatus(filepath.Join(sp.Home, "gateway_state.json"))
		served := []any{}
		if rt != nil {
			if list, ok := rt.GetOr("served_profiles", nil).([]any); ok {
				for _, p := range list {
					served = append(served, pyStrOf(p))
				}
			}
		}
		if sp.Name == "default" && len(served) > 1 {
			multiplex = true
		}
		if rt != nil && pyTruthy(rt.GetOr("multiplex_standalone_reason", nil)) {
			standaloneReason = pyStrOf(rt.GetOr("multiplex_standalone_reason", nil))
		}
		owned := pyjson.NewObject()
		if rt != nil {
			if plats, ok := rt.GetOr("platforms", nil).(*pyjson.Object); ok && plats.Len() > 0 {
				pid, start, live := gatewayWriterIdentityOfHome(sp.Home, rt)
				owned = ownedProfilePlatforms(pid, start, live, plats)
				if owned.Len() > 0 {
					profilePlatforms.Set(sp.Name, owned)
				}
			}
		}
		entry := pyjson.ObjectOf("profile", sp.Name, "ports", profilePlatformPorts(ctx, sp.Home, pyjson.ObjectOf("platforms", owned)))
		if len(served) > 0 {
			entry.Set("served_profiles", served)
		}
		gateways = append(gateways, entry)
	}
	mode := "multiple"
	switch {
	case multiplex:
		mode = "multiplex"
	case len(gateways) == 0:
		mode = "none"
	case len(gateways) == 1:
		mode = "single"
	}
	if standaloneReason == gateway.SingleProfileReason || len(homes) < 2 {
		standaloneReason = nil
	}
	names, parked := []any{}, []any{}
	for _, sp := range homes {
		names = append(names, sp.Name)
		if sp.Name != "default" && profiles.ProfileIsParked(sp.Home) {
			parked = append(parked, sp.Name)
		}
	}
	return pyjson.ObjectOf("profiles", names, "parked_profiles", parked, "gateway_mode", mode,
		"multiplex_standalone_reason", standaloneReason, "gateways", gateways, "profile_platforms", profilePlatforms)
}

// Python str of a decoded JSON value
func pyStrOf(v any) string { return pyrepr.Str(v) }

// Python truthiness of a decoded JSON value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case *pyjson.Object:
		return x != nil && x.Len() > 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	return true
}

var topologyCache struct {
	ts   time.Time
	data *pyjson.Object
	fn   uintptr
}

var topologyCacheLock sync.Mutex

var topologyCacheMu sync.Mutex

const topologyCacheTTL = 10 * time.Second

// Cached topology when the same collector produced it within the window
func topologyCacheGet(fn uintptr) *pyjson.Object {
	topologyCacheMu.Lock()
	defer topologyCacheMu.Unlock()
	if topologyCache.fn == fn && time.Since(topologyCache.ts) < topologyCacheTTL {
		return topologyCache.data
	}
	return nil
}

// Topology scan collapsed to one per window across concurrent status polls
func collectProfileGatewayTopologyCached(ctx context.Context) *pyjson.Object {
	fn := collectProfileGatewayTopology
	key := reflect.ValueOf(fn).Pointer()
	if cached := topologyCacheGet(key); cached != nil {
		return cached
	}
	topologyCacheLock.Lock()
	defer topologyCacheLock.Unlock()
	if cached := topologyCacheGet(key); cached != nil {
		return cached
	}
	data := fn(ctx)
	topologyCacheMu.Lock()
	topologyCache.data, topologyCache.fn, topologyCache.ts = data, key, time.Now()
	topologyCacheMu.Unlock()
	return data
}

// Names of the connected gateway platforms of the scoped profile
func loadConfiguredGatewayPlatforms(ctx context.Context) (map[string]bool, error) {
	cfg, err := gateway.LoadGatewayConfig(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, p := range cfg.GetConnectedPlatforms(ctx) {
		out[string(p)] = true
	}
	return out, nil
}

const windows11MinBuild = 22000

var windowsBuildRE = regexp.MustCompile(`(?:^|[^\d])10\.0\.(\d{5,})(?:[^\d]|$)`)

// Windows NT build number from the platform version strings, false when absent
func windowsBuildNumber(version, platformLabel string) (int64, bool) {
	for _, value := range []string{version, platformLabel} {
		if m := windowsBuildRE.FindStringSubmatch(value); m != nil {
			n, err := pyint.Int64(m[1])
			return n, err == nil
		}
	}
	return 0, false
}

var windows10LabelRE = regexp.MustCompile(`^Windows-10-`)

// Host OS display fields, Windows 10 builds from 22000 relabelled Windows 11
func displaySystemPlatform(system, release, version, platformLabel string) *pyjson.Object {
	if system == "Windows" && release == "10" {
		if build, ok := windowsBuildNumber(version, platformLabel); ok && build >= windows11MinBuild {
			if loc := windows10LabelRE.FindStringIndex(platformLabel); loc != nil {
				platformLabel = "Windows-11-" + platformLabel[loc[1]:]
			}
			release = "11"
		}
	}
	return pyjson.ObjectOf("os", system, "os_release", release, "os_version", version, "platform", platformLabel)
}

// Log directory of the dashboard actions, the process home logs dir at import
var actionLogDir = sync.OnceValue(func() string { return filepath.Join(home.Process(), "logs") })

// Detached child of a dashboard action, polled by the status routes
type actionProc interface {
	Pid() int
	Poll() *int
	Terminate() error
}

var (
	actionMu        sync.Mutex
	actionLogFiles  = map[string]string{}
	actionProcs     = map[string]actionProc{}
	actionCommands  = map[string][]string{}
	actionIDs       = map[string]string{}
	actionResults   = map[string]*pyjson.Object{}
	actionLogStarts = map[string]actionLogStart{}
)

type actionLogStart struct {
	pid    int
	offset int64
}

func init() {
	for k, v := range map[string]string{"gateway-restart": "gateway-restart.log", "gateway-start": "gateway-start.log",
		"gateway-stop": "gateway-stop.log", "gateway-migrate": "gateway-migrate.log", "hermes-update": "hermes-update.log"} {
		actionLogFiles[k] = v
	}
	for _, name := range []string{"doctor", "security-audit", "backup", "import", "checkpoints-prune", "skills-install",
		"skills-uninstall", "skills-update", "curator-run", "prompt-size", "dump", "config-migrate", "tools-post-setup"} {
		actionLogFiles[name] = "action-" + name + ".log"
	}
}

// Recorded handle of an action
func actionProcOf(name string) actionProc {
	actionMu.Lock()
	defer actionMu.Unlock()
	return actionProcs[name]
}

// Terminates a live gateway restart child when its Desktop backend shuts down
func terminateDesktopManagedGateway() {
	proc := actionProcOf("gateway-restart")
	if proc == nil {
		return
	}
	if proc.Poll() == nil {
		_ = proc.Terminate()
	}
}

// Named profile selector leading an action argv, empty when none
func namedProfileFromAction(subcommand []string) string {
	if len(subcommand) >= 2 && (subcommand[0] == "-p" || subcommand[0] == "--profile") {
		return pystr.Strip(subcommand[1])
	}
	if len(subcommand) > 0 && strings.HasPrefix(subcommand[0], "--profile=") {
		_, value, _ := strings.Cut(subcommand[0], "=")
		return pystr.Strip(value)
	}
	return ""
}

// True when the action starts the host multiplexer rather than a named profile's own gateway
func isHostGatewaySpawn(subcommand []string) bool {
	profile := namedProfileFromAction(subcommand)
	if profile != "" && profile != "default" {
		return false
	}
	args := subcommand
	if profile != "" {
		if len(args) > 0 && (args[0] == "-p" || args[0] == "--profile") {
			args = args[2:]
		} else if len(args) > 0 && strings.HasPrefix(args[0], "--profile=") {
			args = args[1:]
		}
	}
	return len(args) > 0 && args[0] == "gateway"
}

// Default root and process home, the dotenv sources a named action scrubs, empty when unresolvable
func actionSourceHomes(ctx context.Context) (homes []string) {
	defer func() {
		if recover() != nil {
			homes = nil
		}
	}()
	root, cur := home.Root(), home.Dir(ctx)
	if root == cur {
		return []string{root}
	}
	return []string{root, cur}
}

// Environment of a detached action, scrubbed and pinned to the target home for a named profile
func profileActionEnvironment(ctx context.Context, subcommand []string, envOverrides map[string]string) (map[string]string, error) {
	profile := namedProfileFromAction(subcommand)
	var actionEnv map[string]string
	switch {
	case isHostGatewaySpawn(subcommand):
		env, err := tools.HostGatewayChildEnv(ctx, nil, nil)
		if err != nil {
			return nil, err
		}
		actionEnv = env
	case profile == "":
		actionEnv = tools.EnvironMap()
	default:
		targetHome, err := gatewayResolveProfileDir(profile)
		if err != nil {
			return nil, err
		}
		actionEnv, err = tools.BuildSubprocessEnv(ctx, nil, tools.SubprocessEnvOptions{Base: tools.EnvironMap(), ScrubSecrets: true})
		if err != nil {
			return nil, err
		}
		profileKeys := map[string]bool{}
		for _, k := range provider.ProfileManagedEnvKeys() {
			profileKeys[k] = true
		}
		for _, sourceHome := range actionSourceHomes(ctx) {
			for k := range provider.EnvKeysDefinedInDotenv(filepath.Join(sourceHome, ".env")) {
				profileKeys[k] = true
			}
			for k := range provider.SecretSourceValues(sourceHome) {
				profileKeys[k] = true
			}
		}
		for k := range profileKeys {
			delete(actionEnv, k)
		}
		tools.StripLaunchProfileEnv(nil, actionEnv, targetHome)
		actionEnv[home.EnvHome] = targetHome
		home.ApplySubprocessHomeEnv(ctx, actionEnv)
	}
	actionEnv["ICEBATH_NONINTERACTIVE"] = "1"
	gateway.DropBridgedEnv(actionEnv)
	delete(actionEnv, "_ICEBATH_GATEWAY")
	for k, v := range envOverrides {
		actionEnv[k] = v
	}
	return actionEnv, nil
}

var rootRequiringGatewayVerbs = map[string]bool{"restart": true, "start": true, "stop": true}

// True when the action is a gateway lifecycle verb the systemd scope picker resolves to the system unit
func actionTargetsSystemGateway(ctx context.Context, subcommand []string) (bool, error) {
	if !GatewayCLI.NeedsSudo("system") {
		return false, nil
	}
	i := slices.Index(subcommand, "gateway")
	if i < 0 || i+1 >= len(subcommand) {
		return false, nil
	}
	if !rootRequiringGatewayVerbs[subcommand[i+1]] {
		return false, nil
	}
	profile := namedProfileFromAction(subcommand)
	if profile == "" {
		return gateway.SelectSystemdScope(ctx, false)
	}
	target, err := gatewayResolveProfileDir(profile)
	if err != nil {
		return false, err
	}
	return gateway.SelectSystemdScope(home.WithOverride(ctx, target), false)
}

// Started action child with its exit status reaped in the background
type execActionProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	code int
}

func (p *execActionProc) Pid() int { return p.cmd.Process.Pid }

// Exit code once the child exited, negative for a signal, nil while it runs
func (p *execActionProc) Poll() *int {
	select {
	case <-p.done:
		code := p.code
		return &code
	default:
		return nil
	}
}

// Sends the child SIGTERM, TerminateProcess on Windows
func (p *execActionProc) Terminate() error {
	if p.Poll() != nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		return p.cmd.Process.Kill()
	}
	return p.cmd.Process.Signal(syscall.SIGTERM)
}

// Starts argv in cwd with stdin from devnull and output appended to the log
func startActionProcess(argv []string, cwd string, env map[string]string, logFile *os.File, attr *syscall.SysProcAttr) (actionProc, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Stdout, cmd.Stderr = logFile, logFile
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execActionProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			p.code = pysubprocess.Returncode(exitErr)
		} else if err != nil {
			p.code = -1
		}
		close(p.done)
	}()
	return p, nil
}

var actionPopen = startActionProcess

// Background action spawner every route calls, the web server gateway attribute hermes tests replace
var spawnHermesAction = spawnHermesActionProcess

// Spawns the icebath subcommand detached with its output in the action log and records the handle
func spawnHermesActionProcess(ctx context.Context, subcommand []string, name string, envOverrides map[string]string) (actionProc, error) {
	logDir := actionLogDir()
	if err := os.MkdirAll(logDir, 0o777); err != nil {
		return nil, err
	}
	actionMu.Lock()
	logName, ok := actionLogFiles[name]
	actionMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("KeyError: %s", pyQuoteOptional(name, true))
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, logName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	st, err := logFile.Stat()
	if err != nil {
		logFile.Close()
		return nil, err
	}
	sectionStart := st.Size()
	actionID := envOverrides["ICEBATH_ACTION_ID"]
	stamp := time.Now().Format("2006-01-02 15:04:05")
	if actionID != "" {
		stamp += " " + actionID
	}
	if _, err := logFile.WriteString("\n=== " + name + " started " + stamp + " ===\n"); err != nil {
		logFile.Close()
		return nil, err
	}
	if actionID != "" {
		if err := pm.DurableWriteBytes(filepath.Join(logDir, name+".action_id"), []byte(actionID)); err != nil {
			logFile.Close()
			return nil, err
		}
	}
	cmd := pm.InstallRuntimeCommand(ProjectRoot(), subcommand, pm.LaunchCommandOptions{})
	system, err := actionTargetsSystemGateway(ctx, subcommand)
	if err != nil {
		logFile.Close()
		return nil, err
	}
	if system {
		if !GatewayCLI.SudoNoninteractiveOk(ctx, append([]string{"-l", "--"}, cmd...)) {
			message := name + " targets the system-scope gateway service, which requires root, and " +
				"passwordless sudo is unavailable for the dashboard user. Run " +
				"'sudo icebath " + strings.Join(subcommand, " ") + "' on the host, or grant that user NOPASSWD sudo."
			if _, werr := logFile.WriteString(message + "\n"); werr != nil {
				logFile.Close()
				return nil, werr
			}
			logFile.Close()
			return nil, errors.New(message)
		}
		cmd = append([]string{"sudo", "-n"}, cmd...)
	}
	actionEnv, err := profileActionEnvironment(ctx, subcommand, envOverrides)
	if err != nil {
		logFile.Close()
		return nil, err
	}
	proc, err := spawnActionDetached(name, cmd, actionEnv, logFile)
	if err != nil {
		if _, werr := logFile.WriteString(fmt.Sprintf("%s: spawn failed: %s\n", name, pypath.OSErrorText(err))); werr != nil {
			logFile.Close()
			return nil, werr
		}
		logFile.Close()
		return nil, err
	}
	logFile.Close()
	actionMu.Lock()
	defer actionMu.Unlock()
	delete(actionResults, name)
	actionCommands[name] = slices.Clone(subcommand)
	actionProcs[name] = proc
	actionLogStarts[name] = actionLogStart{pid: proc.Pid(), offset: sectionStart}
	if actionID != "" {
		actionIDs[name] = actionID
	} else {
		delete(actionIDs, name)
	}
	return proc, nil
}

// Starts the action in its own session, on Windows retrying without job breakaway when the job refuses it
func spawnActionDetached(name string, cmd []string, env map[string]string, logFile *os.File) (actionProc, error) {
	if runtime.GOOS != "windows" {
		return actionPopen(cmd, ProjectRoot(), env, logFile, home.DetachAttr(true))
	}
	proc, err := actionPopen(cmd, ProjectRoot(), env, logFile, home.DetachAttr(true))
	if err == nil {
		return proc, nil
	}
	if !errors.Is(err, syscall.Errno(5)) {
		return nil, err
	}
	if _, werr := logFile.WriteString(fmt.Sprintf("%s: breakaway spawn refused (%s); retrying without CREATE_BREAKAWAY_FROM_JOB\n", name, pypath.OSErrorText(err))); werr != nil {
		return nil, werr
	}
	return actionPopen(cmd, ProjectRoot(), env, logFile, home.DetachAttr(false))
}

// Profile a lifecycle verb addresses, the explicit selector else the process profile, empty for a non profile home
func ownProfileSelector(profile string) string {
	if requested := pystr.Strip(profile); requested != "" {
		return requested
	}
	return home.ProfileNameFor(home.Process())
}

// Dash p gateway verb argv of a lifecycle action, a served profile's restart addressing the default multiplexer
func gatewaySubcommand(ctx context.Context, profile, verb string) ([]string, error) {
	profile = ownProfileSelector(profile)
	args, err := profileCLIArgs(profile)
	if err != nil {
		return nil, err
	}
	if profile != "" && verb == "restart" {
		refusal, err := multiplexedProfileRefusal(ctx, profile, verb)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			args = []string{"-p", "default"}
		}
	}
	return append(args, "gateway", verb), nil
}

// Profile resolver and multiplexer probe of the lifecycle helpers, the web_server_profiles and module attributes hermes tests patch
var (
	gatewayResolveProfileDir = resolveProfileDir
	profileIsMultiplexed     = func(profile string) bool { return gateway.NamedProfileServedByRunningMultiplexer(profile) }
)

// True for a live gateway of the profile's own, not the multiplexer serving it
func hasOwnGateway(profileDir string) (bool, error) {
	if !checkGatewayRunning(profileDir) {
		return false, nil
	}
	servedPid, _, served, err := gateway.MultiplexerLivenessForProfile(profileDir)
	if err != nil {
		return false, err
	}
	if !served {
		return true, nil
	}
	liveness := gateway.ResolveGatewayLiveness(gateway.LivenessOptions{ProfileDir: profileDir, NoCache: true,
		PidProbe: func(path string) (int, bool, error) { return gateway.GetRunningPid(path, false) }})
	return liveness.Running && !pyjson.Equal(liveness.PID, servedPid), nil
}

// Refusal text for gateway start or stop on a named profile with no gateway of its own, empty otherwise
func multiplexedProfileRefusal(ctx context.Context, profile, verb string) (string, error) {
	requested := ownProfileSelector(profile)
	if requested == "" || pystr.Lower(requested) == "current" || pystr.Lower(requested) == "default" {
		return "", nil
	}
	served := profileIsMultiplexed(requested)
	profileDir, err := gatewayResolveProfileDir(requested)
	if err != nil {
		return "", err
	}
	standalone, err := profiles.ProfileIsStandalone(ctx, profileDir)
	if err != nil {
		return "", err
	}
	if standalone {
		if !served {
			return "", nil
		}
		return gateway.StandaloneRescanMessage(requested), nil
	}
	if verb == "start" && profiles.ProfileIsParked(profileDir) && gateway.HostGatewayNow() != nil {
		return "", nil
	}
	if !served && verb != "start" {
		return "", nil
	}
	own, err := hasOwnGateway(profileDir)
	if err != nil {
		return "", err
	}
	if own {
		return "", nil
	}
	if served {
		if verb == "stop" {
			return "", nil
		}
		return fmt.Sprintf("The default gateway already serves profile '%s' as a multiplexer; "+
			"%s it from the default profile instead of a separate gateway for this profile.", requested, verb), nil
	}
	installed, err := GatewayCLI.InstalledServices(ctx, profileDir)
	if err != nil {
		return "", err
	}
	if installed > 0 {
		return "", nil
	}
	return fmt.Sprintf("Profile '%s' does not get a gateway of its own: one host gateway serves every "+
		"profile. Install or start it from the default profile (icebath gateway install), or fold an "+
		"existing per-profile fleet with `icebath gateway migrate --multiplex`; "+
		"`icebath -p %s gateway install --force` starts a separate one anyway.", requested, requested), nil
}

// Refusal text when the gateway the verb would act on has live sessions, empty otherwise
func liveSessionRefusal(ctx context.Context, profile, verb string) (string, error) {
	sel := ownProfileSelector(profile)
	if sel == "" {
		sel = "default"
	}
	target, err := gatewayResolveProfileDir(sel)
	if err != nil {
		return "", err
	}
	homes := []string{target}
	if owner := gateway.HostGatewayNow(); owner != nil {
		homes = append(homes, owner.Home)
	}
	lines, err := GatewayCLI.RefusalLines(ctx, verb, homes, "POST /api/gateway/"+verb+"?interrupt=true", "")
	if err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

// Gateway restart after a config change, a refused or failed spawn reported in the result instead of failing
func restartGatewayAfter(ctx context.Context, profile, what, label string) (*pyjson.Object, error) {
	refusal, err := liveSessionRefusal(ctx, profile, "restart")
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return pyjson.ObjectOf("restart_started", false, "restart_error", refusal, "restart_busy", true), nil
	}
	proc, reused, err := spawnGatewayRestart(ctx, profile, false)
	if err != nil {
		webServerLog().Error(fmt.Sprintf("Failed to auto-restart gateway after %s", what), "error", err)
		return pyjson.ObjectOf("restart_started", false, "restart_error", pyExcStr(err)), nil
	}
	if reused {
		webServerLog().Info(fmt.Sprintf("%s: reusing in-flight gateway restart (pid %d)", label, proc.Pid()))
	}
	return pyjson.ObjectOf("restart_started", true, "restart_action", "gateway-restart", "restart_pid", proc.Pid()), nil
}

// Python str of a raised exception, the HTTP detail for an HTTPException
func pyExcStr(err error) string {
	var he *HTTPException
	if errors.As(err, &he) {
		return he.Error()
	}
	return err.Error()
}

var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

const restartReasonMax = 300

// Last line the gateway restart child pid wrote to its log section, empty when unknown
func restartLogLastLine(pid int) string {
	actionMu.Lock()
	start, ok := actionLogStarts["gateway-restart"]
	logName := actionLogFiles["gateway-restart"]
	actionMu.Unlock()
	if !ok || start.pid != pid {
		return ""
	}
	f, err := os.Open(filepath.Join(actionLogDir(), logName))
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.Seek(start.offset, io.SeekStart); err != nil {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(f, 256*1024))
	if err != nil {
		return ""
	}
	section := strings.ToValidUTF8(string(raw), "�")
	header, body, _ := strings.Cut(strings.TrimLeft(section, "\n"), "\n")
	if !strings.HasPrefix(header, "=== gateway-restart started") {
		return ""
	}
	body, _, _ = strings.Cut(body, "\n=== gateway-restart started")
	lines := pystr.Splitlines(body, false)
	for i := len(lines) - 1; i >= 0; i-- {
		line := pystr.Strip(ansiEscapeRE.ReplaceAllString(lines[i], ""))
		if line != "" {
			r := []rune(line)
			if len(r) > restartReasonMax {
				r = r[:restartReasonMax]
			}
			return string(r)
		}
	}
	return ""
}

// Why the newest restart of the profile gateway failed, nil while it runs, after success or when none started
func gatewayRestartFailure(ctx context.Context, profile string) (*pyjson.Object, error) {
	recent := lastGatewayRestartRecord()
	if recent == nil {
		return nil, nil
	}
	command := []string{}
	for _, arg := range recent.command {
		if arg != "--interrupt" {
			command = append(command, arg)
		}
	}
	want, err := gatewaySubcommand(ctx, profile, "restart")
	if err != nil {
		return nil, err
	}
	if !slices.Equal(command, want) {
		return nil, nil
	}
	exit := recent.proc.Poll()
	if exit == nil || *exit == 0 {
		return nil, nil
	}
	reason := restartLogLastLine(recent.proc.Pid())
	if reason == "" {
		reason = fmt.Sprintf("exit code %d", *exit)
	}
	return pyjson.ObjectOf("pid", recent.proc.Pid(), "exit_code", *exit, "reason", reason), nil
}

var speakSentenceBoundaryRE = regexp2.MustCompile(`(?<=[.!?])(?:\s|\n)|(?:\n\n)`, regexp2.None)

// Text split into provider cap sized pieces on sentence boundaries, whitespace reflowed
func splitTextForSpeakStream(text string, capLen int) []string {
	if capLen <= 0 {
		capLen = 4000
	}
	pieces := []string{}
	buf := []rune{}
	for _, sentence := range splitSentenceBoundaries(text) {
		if pystr.Strip(sentence) == "" {
			continue
		}
		s := []rune(sentence)
		for len(s) > capLen {
			pieces = append(pieces, string(s[:capLen]))
			s = s[capLen:]
		}
		switch {
		case len(buf) > 0 && len(buf)+len(s)+1 > capLen:
			pieces = append(pieces, string(buf))
			buf = s
		case len(buf) > 0:
			buf = append(append(buf, ' '), s...)
		default:
			buf = s
		}
	}
	if len(buf) > 0 {
		pieces = append(pieces, string(buf))
	}
	return pieces
}

// Pieces of text between sentence boundary matches, the re.split of the pattern
func splitSentenceBoundaries(text string) []string {
	runes := []rune(text)
	out := []string{}
	last := 0
	m, _ := speakSentenceBoundaryRE.FindRunesMatch(runes)
	for m != nil {
		out = append(out, string(runes[last:m.RuneIndex]))
		last = m.RuneIndex + m.RuneLength
		m, _ = speakSentenceBoundaryRE.FindNextMatch(m)
	}
	return append(out, string(runes[last:]))
}

var sessionListHeavyFields = []string{"system_prompt", "model_config"}

// Session list rows without the fields no list consumer reads
func stripSessionListRows(sessions []*pyjson.Object) []*pyjson.Object {
	for _, s := range sessions {
		for _, key := range sessionListHeavyFields {
			s.Delete(key)
		}
	}
	return sessions
}
