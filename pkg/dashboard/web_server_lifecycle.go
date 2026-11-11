package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"os"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyipaddress"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	PortInUseExitCode   = 75
	portInUseSentinel   = "BACKEND_PORT_IN_USE port=%d"
	orphanExitCeiling   = 10 * time.Second
	dotnetTicksAtEpoch  = 621_355_968_000_000_000
	defaultWatchdogPoll = 2.0
)

// Loopback proxies uvicorn always trusts for forwarded headers
var defaultDashboardForwardedAllowIPs = []string{"127.0.0.1", "::1"}

// Desktop process the start marker names is gone, the ProcessLookupError of a marker probe
type ProcessLookupError struct{ PID int }

func (e *ProcessLookupError) Error() string {
	return fmt.Sprintf("[Errno 3] No such process: %d", e.PID)
}
func (e *ProcessLookupError) PyTypeName() string { return "ProcessLookupError" }

// Whether a parent start marker has a known prefix and a whole value, a split ps date refused
func validParentStartMarker(marker string) bool {
	prefix, value, found := strings.Cut(marker, ":")
	if !found || value == "" || value != pystr.Strip(value) {
		return false
	}
	switch prefix {
	case "linux", "win", "winms":
		return pystr.StrIsDigit(value)
	case "ps":
	default:
		return false
	}
	tokens := pystr.SplitWhitespace(value, -1)
	hasYear, hasTime := false, false
	for _, token := range tokens {
		if pystr.StrIsDigit(token) && len([]rune(token)) == 4 {
			hasYear = true
		}
		if strings.Contains(token, ":") {
			hasTime = true
		}
	}
	return len(tokens) >= 4 && hasYear && hasTime
}

// Whether a marker mismatch proves the Desktop parent was replaced, never for a wall clock ps marker
func parentStartMarkerMismatchIsConclusive(actual, expected string) bool {
	return !(strings.HasPrefix(actual, "ps:") || strings.HasPrefix(expected, "ps:"))
}

// Whether parent markers match across Desktop generations, .NET ticks normalised against Unix milliseconds
func parentStartMarkersMatch(actual, expected string) bool {
	if actual == expected {
		return true
	}
	if !strings.HasPrefix(actual, "win:") || !strings.HasPrefix(expected, "winms:") {
		return false
	}
	ticks, ok := pyint.Parse(strings.TrimPrefix(actual, "win:"))
	if !ok {
		return false
	}
	expectedMS, ok := pyint.Parse(strings.TrimPrefix(expected, "winms:"))
	if !ok {
		return false
	}
	actualMS := new(big.Int).Sub(ticks, big.NewInt(dotnetTicksAtEpoch))
	actualMS.Div(actualMS, big.NewInt(10_000))
	return actualMS.Cmp(expectedMS) == 0
}

// Provider discovery run off the request path so the first provider RPC does not stall
func warmGatewayModule(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			webServerLog().Debug(fmt.Sprintf("warm-up provider discovery failed: %v", r))
		}
	}()
	provider.ListProviders(ctx)
}

// Restart drain timeout in seconds the gateway restart path resolves
func resolveRestartDrainTimeout(ctx context.Context) (float64, error) {
	return gateway.GetRestartDrainTimeout(ctx)
}

// Port the live server socket bound, the fallback before it binds
func readBoundPort(server *UvicornServer, fallback int) int {
	if addr, ok := server.Addr().(*net.TCPAddr); ok && addr != nil {
		return addr.Port
	}
	return fallback
}

// Publishes the bound port through an atomic ready file when ICEBATH_DESKTOP_READY_FILE names one
func writeDashboardReadyFile(ctx context.Context, actualPort int) {
	target := os.Getenv("ICEBATH_DESKTOP_READY_FILE")
	if target == "" {
		return
	}
	err := config.AtomicJSONWriteWith(ctx, target, pyjson.ObjectOf("port", actualPort), pyjson.Separators(",", ":"))
	if err != nil {
		webServerLog().Warn(fmt.Sprintf("Failed to write dashboard ready file %s: %s", pyrepr.Repr(target), err))
	}
}

// Opens the dashboard URL in a browser a second later, skipped on headless Linux
func maybeOpenBrowser(host string, actualPort int, openBrowser bool, initialProfile string) {
	if !openBrowser {
		return
	}
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		webServerLog().Debug("Skipping browser-open: no DISPLAY or WAYLAND_DISPLAY detected " +
			"(headless Linux). Pass --no-open to suppress this detection.")
		return
	}
	displayHost := host
	if host == "0.0.0.0" || host == "::" {
		displayHost = "127.0.0.1"
	}
	openURL := fmt.Sprintf("http://%s:%d", pyurl.FormatURLHost(displayHost), actualPort)
	if initialProfile != "" {
		openURL += "/?profile=" + pyurl.Quote(initialProfile, "/")
	}
	go func() {
		defer func() { recover() }()
		time.Sleep(time.Second)
		_ = provider.OpenBrowserURL(openURL)
	}()
}

// Whether the exact Desktop process owning this backend is gone, any inconclusive probe keeping it serving
func isServeOrphaned(desktopPID int, expectedStartMarker *string, pidExists func(int) bool, processStartMarker func(int) (string, error)) (orphaned bool) {
	defer func() {
		if r := recover(); r != nil {
			orphaned = false
		}
	}()
	if expectedStartMarker != nil {
		probe := processStartMarker
		if probe == nil {
			probe = currentProcessStartMarker
		}
		actual, err := probe(desktopPID)
		var gone *ProcessLookupError
		if errors.As(err, &gone) {
			return true
		}
		if err == nil {
			if parentStartMarkersMatch(actual, *expectedStartMarker) {
				return false
			}
			if parentStartMarkerMismatchIsConclusive(actual, *expectedStartMarker) {
				return true
			}
		}
	}
	if pidExists == nil {
		pidExists = gateway.PidExists
	}
	return !pidExists(desktopPID)
}

// Start marker of a pid on this platform
func currentProcessStartMarker(pid int) (string, error) { return processStartMarker(pid) }

// Raises SIGTERM in this process with a hard exit backstop after the orphan ceiling
func requestOrphanShutdown() *time.Timer {
	ceiling := time.AfterFunc(orphanExitCeiling, func() { os.Exit(0) })
	raiseSignal(syscall.SIGTERM)
	return ceiling
}

// Duration of a seconds value, capped at the largest duration for infinite or huge values
func secondsDuration(s float64) time.Duration {
	if math.IsInf(s, 1) || s*float64(time.Second) >= math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(s * float64(time.Second))
}

// Exits when the exact Desktop parent that spawned this backend dies, disarmed on inconsistent plumbing
func startParentDeathWatchdog() {
	desktopPID, startMarker, poll, armed := parentDeathWatchdogArgs()
	if !armed {
		return
	}
	go parentDeathLoop(desktopPID, startMarker, poll, func(pid int, marker *string) bool {
		return isServeOrphaned(pid, marker, nil, nil)
	})
}

// Desktop pid, start marker and poll seconds the watchdog tracks from the parent env, false when it stays disarmed
func parentDeathWatchdogArgs() (int, *string, float64, bool) {
	var startMarker, nonce *string
	if v := os.Getenv("ICEBATH_PARENT_START_MARKER"); v != "" {
		startMarker = &v
	}
	if v := os.Getenv("ICEBATH_PARENT_NONCE"); v != "" {
		nonce = &v
	}
	pidBig, ok := pyint.Parse(os.Getenv("ICEBATH_PARENT_PID"))
	if !ok || pidBig.Sign() <= 0 || !pidBig.IsInt64() || pidBig.Int64() > math.MaxInt {
		return 0, nil, 0, false
	}
	desktopPID := int(pidBig.Int64())
	hasMarker := startMarker != nil
	if hasMarker != (nonce != nil) {
		return 0, nil, 0, false
	}
	if hasMarker && (!validParentStartMarker(*startMarker) || *nonce != pystr.Strip(*nonce)) {
		webServerLog().Warn(fmt.Sprintf("Parent-death watchdog disabled: unusable ICEBATH_PARENT_START_MARKER=%s / nonce; "+
			"falling back to no parent tracking for desktop PID %d.", pyrepr.Repr(*startMarker), desktopPID))
		return 0, nil, 0, false
	}
	poll := defaultWatchdogPoll
	rawPoll, set := os.LookupEnv("ICEBATH_SERVE_WATCHDOG_POLL_S")
	if !set {
		rawPoll = "2.0"
	}
	if v, err := pyint.ParseFloat(rawPoll); err == nil {
		poll = 0.5
		if v > 0.5 {
			poll = v
		}
	}
	return desktopPID, startMarker, poll, true
}

// Polls until the parent is orphaned, then logs and requests the signal shutdown, returning its hard exit ceiling
func parentDeathLoop(desktopPID int, startMarker *string, poll float64, orphaned func(int, *string) bool) *time.Timer {
	for !orphaned(desktopPID, startMarker) {
		time.Sleep(secondsDuration(poll))
	}
	markerRepr := "None"
	if startMarker != nil {
		markerRepr = pyrepr.Repr(*startMarker)
	}
	webServerLog().Warn(fmt.Sprintf("Parent-death watchdog: desktop PID %d appears orphaned (expected_start_marker=%s); shutting down.",
		desktopPID, markerRepr))
	return requestOrphanShutdown()
}

// Whether an error is the platform's address in use bind failure
func isAddrInUseError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return slices.Contains([]int{int(syscall.EADDRINUSE), 98, 48, 10048}, int(errno))
}

// Whether binding host and port would fail with address in use, other probe failures left to the real bind
func portBindConflict(host string, port int) bool {
	if port == 0 {
		return false
	}
	network := "tcp4"
	if strings.Contains(host, ":") {
		network = "tcp6"
	}
	return probeBindConflict(network, host, port)
}

// Writes a machine parsed sentinel line to the real stdout, falling back to the given writer
func writeMachineSentinelLine(line string, fallback io.Writer) {
	if _, err := os.Stdout.Write([]byte(line + "\n")); err != nil {
		fmt.Fprintln(fallback, line)
	}
}

// Prints the port in use sentinel and a hint naming the likely holders
func reportPortInUse(host string, port int, stdout io.Writer) {
	writeMachineSentinelLine(fmt.Sprintf(portInUseSentinel, port), stdout)
	fmt.Fprintf(stdout, "  Port %d on %s is already in use — likely another "+
		"'icebath serve' / 'icebath dashboard' backend or the Icebath gateway. "+
		"Stop the other process, or pass --port <other> "+
		"(--port 0 picks a free ephemeral port).\n", port, host)
}

// Bounded proxy addresses uvicorn may trust, loopback plus the valid dashboard.trusted_proxies entries
func dashboardForwardedAllowIPs(dashboardConfig map[string]any) []string {
	var entries []any
	switch v := dashboardConfig["trusted_proxies"].(type) {
	case nil:
	case string:
		if v != "" {
			entries = []any{v}
		}
	case []any:
		entries = v
	case []string:
		for _, s := range v {
			entries = append(entries, s)
		}
	default:
		webServerLog().Warn(fmt.Sprintf("dashboard.trusted_proxies must be a list of IP addresses or CIDR networks; "+
			"ignoring %s", pyrepr.Repr(v)))
	}
	trusted := slices.Clone(defaultDashboardForwardedAllowIPs)
	for _, raw := range entries {
		s, isStr := raw.(string)
		if !isStr || pystr.Strip(s) == "" {
			webServerLog().Warn(fmt.Sprintf("Ignoring invalid dashboard.trusted_proxies entry %s; expected an IP "+
				"address or CIDR network", pyrepr.Repr(raw)))
			continue
		}
		entry := pystr.Strip(s)
		normalized, ok := normalizeTrustedProxy(entry)
		if !ok {
			webServerLog().Warn(fmt.Sprintf("Ignoring unsafe dashboard.trusted_proxies entry %s; use a bounded IP "+
				"address or CIDR network, never '*' or a /0 network", pyrepr.Repr(raw)))
			continue
		}
		if !slices.Contains(trusted, normalized) {
			trusted = append(trusted, normalized)
		}
	}
	if !slices.Equal(trusted, defaultDashboardForwardedAllowIPs) {
		webServerLog().Info("Dashboard trusted proxies: " + strings.Join(trusted, ", "))
	}
	return trusted
}

// Canonical text of a bounded network or an address, false for a /0 network or a malformed entry
func normalizeTrustedProxy(entry string) (string, bool) {
	if strings.Contains(entry, "/") {
		network, ok := pyipaddress.IPNetwork(entry, false)
		if !ok || network.Bits() == 0 {
			return "", false
		}
		return network.Masked().String(), true
	}
	addr, ok := pyipaddress.IPAddress(entry)
	if !ok {
		return "", false
	}
	return addr.String(), true
}

// Lifespan mode of a listener, auto running the app lifespan and off skipping it
type LifespanMode string

const (
	LifespanAuto LifespanMode = "auto"
	LifespanOff  LifespanMode = "off"
)

// Options of a listener, a nil graceful timeout meaning three seconds and a zero one meaning no limit
type ListenerOptions struct {
	AuthRequired            bool
	WSPing                  *[2]*time.Duration
	TimeoutGracefulShutdown *time.Duration
	Lifespan                LifespanMode
}

const defaultListenerGracefulShutdown = 3 * time.Second

// Uvicorn config and server of a listener, deflate only for remote peers and pings only off loopback
func BuildListenerServer(ctx context.Context, servedApp ASGIApp, host string, port int, opts ListenerOptions) (*UvicornConfig, *UvicornServer) {
	dashCfg := dashboardConfigSection(ctx)
	wsPingSetting := func(key string) *time.Duration { return wsPingSettingOf(dashCfg, key) }
	perMessageDeflate := opts.AuthRequired || !gateway.LoopbackHostValues[host]
	wsPing := opts.WSPing
	if wsPing == nil {
		if gateway.LoopbackHostValues[host] {
			wsPing = &[2]*time.Duration{nil, nil}
		} else {
			wsPing = &[2]*time.Duration{wsPingSetting("ws_ping_interval"), wsPingSetting("ws_ping_timeout")}
		}
	}
	installUvicornConsoleHandlers()
	configureUvicornLogLevel(logging.LevelWarning)
	graceful := defaultListenerGracefulShutdown
	if opts.TimeoutGracefulShutdown != nil {
		graceful = *opts.TimeoutGracefulShutdown
	}
	lifespan := opts.Lifespan
	if lifespan == "" {
		lifespan = LifespanAuto
	}
	cfg := &UvicornConfig{App: servedApp, Host: host, Port: port, ProxyHeaders: opts.AuthRequired,
		ForwardedAllowIPs: dashboardForwardedAllowIPs(dashCfg), WSPingInterval: wsPing[0], WSPingTimeout: wsPing[1],
		WSPerMessageDeflate: perMessageDeflate, WSMaxSize: gateway.DesktopAttachmentWSMaxBytes,
		TimeoutGracefulShutdown: graceful, Lifespan: lifespan}
	return cfg, NewUvicornServer(cfg)
}

// Dashboard section of the loaded config, empty when the config cannot be read
func dashboardConfigSection(ctx context.Context) map[string]any {
	h := home.Dir(ctx)
	if cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup()); err == nil {
		if section, ok := cfg["dashboard"].(map[string]any); ok && len(section) > 0 {
			return section
		}
	}
	return map[string]any{}
}

// WebSocket ping seconds of a dashboard setting, twenty when absent or not a number
func wsPingSettingOf(dashCfg map[string]any, key string) *time.Duration {
	d := 20 * time.Second
	if raw, ok := dashCfg[key]; ok {
		if f, err := pyint.Float(raw); err == nil {
			d = secondsDuration(f)
		}
	}
	return &d
}
