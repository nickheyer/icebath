package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/webbundle"
	"golang.org/x/crypto/scrypt"
)

// Exit status and stderr text a dashboard launch stops with, the carrier of a Python SystemExit
type DashboardExit struct {
	Code int
	Msg  string
}

// The message when the exit carries one, the decimal status otherwise
func (e *DashboardExit) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return strconv.Itoa(e.Code)
}

// Exit with status one and the given stderr message, the shape of a token file refusal
func sshTokenExit(msg string) error { return &DashboardExit{Code: 1, Msg: msg} }

// Inputs of one dashboard or serve launch, the runtime preparation running the web dist step it is handed
type DashboardLaunch struct {
	Namespace       map[string]any
	HeadlessBackend bool
	Stdout, Stderr  io.Writer
	PrepareRuntime  func(ctx context.Context, headlessBackend bool, resolveWebDist func() error) (bool, error)
}

// Everything the dashboard HTTP server is started with
type DashboardServerOptions struct {
	Host                       string
	Port                       int
	OpenBrowser                bool
	AllowPublic                bool
	InitialProfile             string
	Headless                   bool
	Isolated                   bool
	SSHSessionToken            string
	SSHOwnerNonce              string
	StartMCPDiscoveryAfterBind bool
	SSHLockPath                string
	// Stream the human lines and a failed sentinel write go to, stdout being reserved for the machine sentinels
	Stderr io.Writer
}

// Host facts of a dashboard launch, swapped by tests
var (
	dashboardReadServeRecord       = func() *HostRecord { return ReadHostRecord(RoleServe, false) }
	dashboardTokenIsConsistent     = HostRecordTokenIsConsistent
	dashboardProbeOwner            = func(r *HostRecord) *pyjson.Object { return ProbeHostOwner(r, HostProbeTimeout) }
	dashboardIsDesktopOwnedBackend = func() bool { return tools.IsDesktopOwnedBackend(nil) }
	dashboardActiveProfileName     = profiles.GetActiveProfileName
	dashboardOpenBrowser           = provider.OpenBrowserURL
	dashboardTypedArgv             = func() []string { return os.Args[1:] }
	dashboardReexec                = reexecDashboard
	dashboardStartServer           = startDashboardServer
	dashboardRequiresAuth          = webServerRequiresAuth
	dashboardKillStale             = KillStaleDashboardProcesses
	dashboardConsole               = cliui.Default
	dashboardGetpass               = provider.Getpass
	dashboardDiscoverPlugins       = plugins.DiscoverPlugins
	dashboardApplyNofileLimit      = ApplyNofileSoftLimit
	dashboardReadSSHToken          = readSSHSessionTokenFile
	sshOwnerNonceRE                = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// String value of a namespace key, empty for a missing, None or non string value
func dashArgString(ns map[string]any, key string) string {
	s, _ := ns[key].(string)
	return s
}

// Truth of a namespace flag, false when missing
func dashArgBool(ns map[string]any, key string) bool {
	b, _ := ns[key].(bool)
	return b
}

// Integer value of a namespace key, not ok for a missing or non integer value
func dashArgInt(ns map[string]any, key string) (int, bool) {
	n, ok := ns[key].(int)
	return n, ok
}

// Starts the web UI server or, with the status and stop flags, reports or stops the running ones
func CmdDashboard(ctx context.Context, l DashboardLaunch) (int, error) {
	err := cmdDashboard(ctx, l)
	var exit *DashboardExit
	if errors.As(err, &exit) {
		if exit.Msg != "" {
			fmt.Fprintln(l.Stderr, exit.Msg)
		}
		return exit.Code, nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func cmdDashboard(ctx context.Context, l DashboardLaunch) error {
	ns := l.Namespace
	tokenFile := dashArgString(ns, "ssh_session_token_file")
	if err := dashboardLifecycleFlags(ctx, l, tokenFile); err != nil {
		return err
	}
	headless := l.HeadlessBackend
	nonce, err := dashboardValidateServeArgs(ctx, l, headless, tokenFile)
	if err != nil {
		return err
	}
	dashboardSanitizeDesktopEnv(headless)
	if err := attachToHostBackend(ctx, l, headless); err != nil {
		return err
	}
	if err := routeNamedProfileDashboard(ctx, l, headless, nonce, tokenFile); err != nil {
		return err
	}
	dashboardApplyNofileLimit(nil)
	token := ""
	if tokenFile != "" {
		if token, err = dashboardReadSSHToken(tokenFile); err != nil {
			return err
		}
	}
	discoveryAfterBind, err := l.PrepareRuntime(ctx, headless, func() error { return resolveDashboardWebDist(ctx, l, headless) })
	if err != nil {
		return err
	}
	if err := maybeSetupDashboardAuthInteractively(ctx, l); err != nil {
		return err
	}
	if headless {
		observability.BeginProcess(ctx, "serve")
	}
	port, _ := dashArgInt(ns, "port")
	lockPath := ""
	if tokenFile != "" {
		lockPath = filepath.Join(filepath.Dir(tokenFile), tools.BackendLockName)
	}
	return dashboardStartServer(ctx, DashboardServerOptions{
		Host:                       dashArgString(ns, "host"),
		Port:                       port,
		OpenBrowser:                !dashArgBool(ns, "no_open"),
		AllowPublic:                dashArgBool(ns, "insecure"),
		InitialProfile:             dashArgString(ns, "open_profile"),
		Headless:                   headless,
		Isolated:                   dashArgBool(ns, "isolated"),
		SSHSessionToken:            token,
		SSHOwnerNonce:              nonce,
		StartMCPDiscoveryAfterBind: discoveryAfterBind,
		SSHLockPath:                lockPath,
		Stderr:                     l.Stderr,
	})
}

// Reports or stops running dashboards for the status and stop flags and ends the launch there
func dashboardLifecycleFlags(ctx context.Context, l DashboardLaunch, tokenFile string) error {
	status, stop := dashArgBool(l.Namespace, "status"), dashArgBool(l.Namespace, "stop")
	if tokenFile != "" && (status || stop) {
		return &DashboardExit{Code: 1, Msg: "--ssh-session-token-file cannot be used with --status or --stop"}
	}
	if status {
		reportDashboardStatus(ctx, l.Stdout)
		return &DashboardExit{Code: 0}
	}
	if stop {
		ownHome := home.Dir(ctx)
		if len(dashboardFindStale(ctx, nil, ownHome)) == 0 {
			fmt.Fprintln(l.Stdout, "No icebath dashboard processes running for this profile.")
			return &DashboardExit{Code: 0}
		}
		result := dashboardKillStale(ctx, l.Stdout, "requested via --stop", false, nil, ownHome)
		if len(result.Failed) > 0 {
			return &DashboardExit{Code: 1}
		}
		return &DashboardExit{Code: 0}
	}
	return nil
}

// Checks the headless serve arguments and returns the Desktop SSH owner nonce, empty when none was given
func dashboardValidateServeArgs(ctx context.Context, l DashboardLaunch, headless bool, tokenFile string) (string, error) {
	if headless {
		if err := config.RequireParseableUserConfig(ctx, home.Dir(ctx), dashArgBool(l.Namespace, "ignore_user_config")); err != nil {
			var invalid *config.InvalidUserConfigError
			if !errors.As(err, &invalid) {
				return "", err
			}
			fmt.Fprintf(l.Stderr, "Error: %s\n", invalid.Error())
			return "", &DashboardExit{Code: 2}
		}
	}
	nonce := dashArgString(l.Namespace, "ssh_owner_nonce")
	if nonce != "" && !sshOwnerNonceRE.MatchString(nonce) {
		return "", &DashboardExit{Code: 1, Msg: "--ssh-owner-nonce must be 16 lowercase hex characters"}
	}
	if tokenFile != "" && !headless {
		return "", &DashboardExit{Code: 1, Msg: "--ssh-session-token-file is only valid with icebath serve"}
	}
	return nonce, nil
}

// Drops the headless variable a Desktop owned shell passes down, a standalone browser dashboard must not obey it
func dashboardSanitizeDesktopEnv(headless bool) {
	if !headless {
		os.Unsetenv("ICEBATH_SERVE_HEADLESS")
	}
}

// Live host serve record with a bound port and a consistent token to attach to, nil when there is none
func hostBackendAttachment() (record *HostRecord) {
	defer func() {
		if recover() != nil {
			record = nil
		}
	}()
	r := dashboardReadServeRecord()
	if r == nil || r.Port == nil || *r.Port == 0 {
		return nil
	}
	if !dashboardTokenIsConsistent(r) {
		return nil
	}
	return r
}

// Which of host and port the operator typed, read from the process argv when none is given
func explicitEndpointFlags(argv []string) map[string]bool {
	if argv == nil {
		argv = dashboardTypedArgv()
	}
	typed := map[string]bool{}
	for _, token := range argv {
		name := strings.SplitN(token, "=", 2)[0]
		if name == "--host" || name == "--port" {
			typed[name[2:]] = true
		}
	}
	return typed
}

// Why a typed host or port cannot be served by the recorded owner, empty when it can
func endpointConflict(ns map[string]any, record *HostRecord, typed map[string]bool) string {
	recordPort := 0
	if record.Port != nil {
		recordPort = *record.Port
	}
	if typed["port"] {
		if wanted, ok := dashArgInt(ns, "port"); ok && wanted > 0 && wanted != recordPort {
			return fmt.Sprintf("--port %d (the host owner is on port %d)", wanted, recordPort)
		}
	}
	if typed["host"] {
		wantedHost := dashArgString(ns, "host")
		ownerHost := record.Host
		if ownerHost == "" {
			ownerHost = "127.0.0.1"
		}
		loopback := wantedHost == "127.0.0.1" || wantedHost == "localhost" || wantedHost == "::1"
		wildcard := ownerHost == "0.0.0.0" || ownerHost == "::" || ownerHost == "*"
		if !(wantedHost == ownerHost || (wildcard && loopback)) {
			return fmt.Sprintf("--host %s (the host owner is bound to %s)", wantedHost, ownerHost)
		}
	}
	return ""
}

// Reports the live host backend and exits zero instead of binding again, an unservable typed endpoint exits 78
func attachToHostBackend(ctx context.Context, l DashboardLaunch, headless bool) error {
	ns := l.Namespace
	if dashArgBool(ns, "isolated") || dashboardIsDesktopOwnedBackend() {
		return nil
	}
	record := hostBackendAttachment()
	if record == nil {
		return nil
	}
	identity := dashboardProbeOwner(record)
	if identity == nil {
		return nil
	}
	w := l.Stdout
	if conflict := endpointConflict(ns, record, explicitEndpointFlags(nil)); conflict != "" {
		fmt.Fprintf(w, "Refusing to start: this host is already served by %s.\n", DescribeHostRecord(record))
		fmt.Fprintf(w, "  You asked for %s.\n", conflict)
		fmt.Fprintln(w, "  Stop that backend, or drop the flag to use the running one.")
		return &DashboardExit{Code: GatewayFatalConfigExitCode}
	}
	if !headless && !pyTruthy(identity.GetOr("servesSpa", nil)) {
		fmt.Fprintf(w, "Refusing to start: this host is already served by %s, which is a headless `icebath serve` backend with no dashboard UI.\n",
			DescribeHostRecord(record))
		fmt.Fprintln(w, "  Stop it and run `icebath dashboard`, or use --isolated for a dedicated server.")
		return &DashboardExit{Code: GatewayFatalConfigExitCode}
	}
	wanted := dashArgString(ns, "open_profile")
	if wanted == "" {
		wanted = dashboardActiveProfileName(ctx)
	}
	url := fmt.Sprintf("http://%s:%d/?profile=%s", pyurl.FormatURLHost(HostDialHost(record)), *record.Port, wanted)
	kind := "dashboard"
	if headless {
		kind = "backend"
	}
	fmt.Fprintf(w, "Icebath %s already running on this host: PID %d, port %d.\n", kind, record.PID, *record.Port)
	fmt.Fprintf(w, "  Managing profile '%s': %s\n", wanted, url)
	if !headless && !dashArgBool(ns, "no_open") {
		_ = dashboardOpenBrowser(url)
	}
	return &DashboardExit{Code: 0}
}

// Replaces this process with argv under env, on Windows runs it as a child and exits with its status
func reexecDashboard(argv []string, env map[string]string) error {
	envList := make([]string, 0, len(env))
	for k, v := range env {
		envList = append(envList, k+"="+v)
	}
	sort.Strings(envList)
	if dashboardGOOS == "windows" {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = envList
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			return err
		}
		return &DashboardExit{Code: cmd.ProcessState.ExitCode()}
	}
	return syscall.Exec(argv[0], argv, envList)
}

// Sends a named profile launch to the single machine dashboard by re-executing pinned to the default profile
func routeNamedProfileDashboard(ctx context.Context, l DashboardLaunch, headless bool, sshOwnerNonce, tokenFile string) error {
	ns := l.Namespace
	launchProfile := dashboardActiveProfileName(ctx)
	if launchProfile == "default" || launchProfile == "custom" || dashArgBool(ns, "isolated") ||
		dashArgString(ns, "open_profile") != "" || dashboardIsDesktopOwnedBackend() {
		return nil
	}
	fmt.Fprintf(l.Stdout, "Routing to the machine dashboard (profile '%s' preselected). Use --isolated for a dedicated per-profile server.\n", launchProfile)
	self, err := dashboardSelfExecutable()
	if err != nil {
		return err
	}
	subcommand := "dashboard"
	if headless {
		subcommand = "serve"
	}
	port, _ := dashArgInt(ns, "port")
	argv := []string{self, "-p", "default", subcommand, "--port", strconv.Itoa(port), "--host", dashArgString(ns, "host"), "--open-profile", launchProfile}
	if sshOwnerNonce != "" {
		argv = append(argv, "--ssh-owner-nonce", sshOwnerNonce)
	}
	if tokenFile != "" {
		argv = append(argv, "--ssh-session-token-file", tokenFile)
	}
	for _, flag := range []string{"no_open", "insecure"} {
		if dashArgBool(ns, flag) {
			argv = append(argv, "--"+strings.ReplaceAll(flag, "_", "-"))
		}
	}
	env, err := tools.BuildSubprocessEnv(ctx, sdkToolsEnv(ctx, home.Dir(ctx)), tools.SubprocessEnvOptions{ScrubSecrets: false, InheritProfileHome: false})
	if err != nil {
		return err
	}
	env["ICEBATH_HOME"] = home.Root()
	return dashboardReexec(argv, env)
}

// Resolves the dashboard bundle before the server starts, a missing bundle ending the launch
func resolveDashboardWebDist(ctx context.Context, l DashboardLaunch, headless bool) error {
	if headless {
		os.Setenv("ICEBATH_SERVE_HEADLESS", "1")
		return nil
	}
	dist, err := webbundle.Dir(ctx, webbundle.Dashboard, l.Stdout)
	if err != nil {
		fmt.Fprintf(l.Stdout, "✗ %s\n", err)
		return &DashboardExit{Code: 1}
	}
	fmt.Fprintf(l.Stdout, "→ Serving the dashboard bundle at %s\n", dist)
	return nil
}

// Prints the message and ends the launch with status one
func cancelDashboard(w io.Writer, message string) error {
	fmt.Fprintln(w, message)
	return &DashboardExit{Code: 1}
}

// Scrypt hash of a dashboard password in the scrypt$n$r$p$salt$key text form with a fresh random salt
func hashDashboardPassword(password string) (string, error) {
	const n, r, p, keyLen, saltBytes = 1 << 14, 8, 1, 32, 16
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := scrypt.Key([]byte(password), salt, n, r, p, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("scrypt$%d$%d$%d$%s$%s", n, r, p, base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(dk)), nil
}

// Offers an interactive operator a username and password setup when the bind requires auth and no provider exists
func maybeSetupDashboardAuthInteractively(ctx context.Context, l DashboardLaunch) error {
	w := l.Stdout
	host := dashArgString(l.Namespace, "host")
	if host == "" {
		host = "127.0.0.1"
	}
	required, err := dashboardRequiresAuth(ctx, host)
	if err != nil {
		return err
	}
	if !required {
		return nil
	}
	if len(ListDashboardAuthProviders(ctx, "")) > 0 {
		return nil
	}
	con := dashboardConsole()
	if !(con.StdinIsTTY() && con.StdoutIsTTY()) {
		return nil
	}
	prompts := &cliui.Console{In: con.In, Out: w, Getenv: con.Getenv, StdinTTY: con.StdinTTY, StdoutTTY: con.StdoutTTY, TermSize: con.TermSize}
	fmt.Fprintf(w, "\n⚠ Dashboard authentication is required for this configuration (%s).\n", host)
	fmt.Fprintln(w, "  Non-loopback binds and configured external dashboard.public_url values require authentication (--insecure does not bypass this).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  How do you want to authenticate the dashboard?")
	fmt.Fprintln(w, "    [1] Username & password (quickest; for a trusted LAN / VPN)")
	fmt.Fprint(w, "    [2] OAuth via Nous Portal (run `icebath dashboard register`)\n    [3] Cancel\n\n")
	answer, err := prompts.Input("  Choice [1]: ")
	if err != nil {
		return cancelDashboard(w, "\n  Cancelled.")
	}
	choice := pystr.Strip(answer)
	if choice == "" {
		choice = "1"
	}
	if choice == "2" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  Run this on the host where the dashboard lives, then start the dashboard again:\n"+
			"    icebath dashboard register\n"+
			"  It provisions a Nous Portal OAuth client and writes ICEBATH_DASHBOARD_OAUTH_CLIENT_ID into ~/.icebath/.env for you.\n"+
			"  Docs: https://hermes-agent.nousresearch.com/docs/user-guide/features/web-dashboard#authentication-gated-mode")
		return &DashboardExit{Code: 0}
	}
	if choice != "1" {
		return cancelDashboard(w, "  Cancelled.")
	}
	fmt.Fprintln(w)
	username, err := prompts.LineInput("  Username [admin]: ")
	if err != nil {
		return cancelDashboard(w, "\n  Cancelled.")
	}
	if username = pystr.Strip(username); username == "" {
		username = "admin"
	}
	password, err := dashboardGetpass("  Password: ")
	if err != nil {
		return cancelDashboard(w, "\n  Cancelled.")
	}
	confirm, err := dashboardGetpass("  Confirm password: ")
	if err != nil {
		return cancelDashboard(w, "\n  Cancelled.")
	}
	if password == "" {
		return cancelDashboard(w, "  ✗ Empty password — aborting.")
	}
	if password != confirm {
		return cancelDashboard(w, "  ✗ Passwords don't match — aborting.")
	}
	passwordHash, err := hashDashboardPassword(password)
	if err != nil {
		return cancelDashboard(w, fmt.Sprintf("  ✗ Could not load the password provider: %s", err))
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return err
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	h := home.Dir(ctx)
	save := func() error {
		cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
		if err != nil {
			return err
		}
		dashboard, ok := cfg["dashboard"].(map[string]any)
		if !ok {
			dashboard = map[string]any{}
			cfg["dashboard"] = dashboard
		}
		basic, ok := dashboard["basic_auth"].(map[string]any)
		if !ok {
			basic = map[string]any{}
			dashboard["basic_auth"] = basic
		}
		basic["username"] = username
		basic["password_hash"] = passwordHash
		basic["password"] = ""
		existing := ""
		if pyTruthy(basic["secret"]) {
			existing = pyrepr.Str(basic["secret"])
		}
		if pystr.Strip(existing) == "" {
			basic["secret"] = secret
		}
		if plugins.EnsureBasicAuthPluginEnabledInConfig(cfg) {
			fmt.Fprintln(w, "  ✓ Re-enabled the bundled 'basic' auth plugin (was in plugins.disabled)")
		}
		return config.SaveConfig(ctx, h, cfg, config.SaveOptions{})
	}
	if err := save(); err != nil {
		return cancelDashboard(w, fmt.Sprintf("  ✗ Failed to write config.yaml: %s", err))
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(w, "  ⚠ Plugin re-discovery failed (%v); the gate may still fail closed. Set the password again or restart the dashboard.\n", r)
			}
		}()
		dashboardDiscoverPlugins(ctx, true)
	}()
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  ✓ Username/password auth configured (user: %s).\n", username)
	fmt.Fprintln(w, "    Saved to config.yaml under dashboard.basic_auth.")
	fmt.Fprint(w, "    Sign in at the dashboard with these credentials.\n\n")
	return nil
}
