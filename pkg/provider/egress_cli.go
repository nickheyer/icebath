package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Iron proxy module surface as the egress CLI reaches it, the attributes hermes tests patch
type egressBackend struct {
	installIronProxy           func(force bool) (string, error)
	findIronProxy              func(installIfMissing bool) string
	ironProxyVersion           func(binary string) string
	ensureCACert               func(force bool) (string, string, error)
	discoverProviderMappings   func(availableEnvNames []string) []TokenMapping
	discoverUncoveredProviders func(availableEnvNames []string) []string
	loadMappings               func() ([]TokenMapping, error)
	mergeMappings              func(existing, discovered []TokenMapping, rotate bool) []TokenMapping
	stateDir                   func() (string, error)
	ensureAuditLog             func(path string) error
	buildProxyConfig           func(opts IronProxyConfigOptions) map[string]any
	writeProxyConfig           func(cfg map[string]any) (string, error)
	writeMappings              func(mappings []TokenMapping) (string, error)
	ensureManagementToken      func(force bool) (string, error)
	getStatus                  func() IronProxyStatus
	stopProxy                  func() (bool, error)
	startProxy                 func(opts IronProxyStartOptions) (IronProxyStatus, error)
	reloadProxy                func() error
	fetchBitwardenSecrets      func(opts BitwardenFetchOptions) (map[string]string, []string, error)
}

// Backend bound to the iron proxy of one home
func newEgressBackend(p IronProxy) egressBackend {
	return egressBackend{
		installIronProxy:           func(bool) (string, error) { return p.InstallIronProxy() },
		findIronProxy:              p.FindIronProxy,
		ironProxyVersion:           p.IronProxyVersion,
		ensureCACert:               p.EnsureCACert,
		discoverProviderMappings:   DiscoverProviderMappings,
		discoverUncoveredProviders: DiscoverUncoveredProviders,
		loadMappings:               p.LoadMappings,
		mergeMappings:              MergeMappings,
		stateDir:                   p.stateDir,
		ensureAuditLog:             EnsureAuditLog,
		buildProxyConfig:           p.BuildProxyConfig,
		writeProxyConfig:           p.WriteProxyConfig,
		writeMappings:              p.WriteMappings,
		ensureManagementToken:      p.EnsureManagementToken,
		getStatus:                  p.GetStatus,
		stopProxy:                  p.StopProxy,
		startProxy:                 p.StartProxy,
		reloadProxy:                p.ReloadProxy,
		fetchBitwardenSecrets:      fetchBitwardenSecrets,
	}
}

// One icebath egress invocation, its home, console, namespace and iron proxy backend
type egressCLI struct {
	ctx     context.Context
	home    string
	console *pyrich.Console
	ns      map[string]any
	ip      egressBackend
}

type egressHandler func(cli *egressCLI) (int, error)

// Subcommand of the egress tree with its handler and arguments
type egressSubcommand struct {
	Name      string
	Help      string
	Func      egressHandler
	Arguments []secretsArg
}

// Parser of the egress tree, subcommands keyed by egress_command
type egressParser struct {
	Dest     string
	Commands []egressSubcommand
}

// Attaches the egress subcommand tree to the parent parser in --help order
func registerEgressCLI(parent *egressParser) {
	parent.Dest = "egress_command"
	parent.Commands = append(parent.Commands, []egressSubcommand{
		{"install", "Install the PM-pinned iron-proxy binary", egressCmdInstall, []secretsArg{
			flag("--force", "Verify and repair the managed copy"),
		}},
		{"setup", "Interactive wizard: install + CA + mint tokens + write config", egressCmdSetup, []secretsArg{
			arg("--tunnel-port", fmt.Sprintf("Override the tunnel port (default %d)", DefaultIronTunnelPort)),
			flag("--from-bitwarden", "Treat secrets as managed by Bitwarden — discover provider keys "+
				"from secrets.bitwarden config instead of the current env.  Fails "+
				"loudly if BW is unreachable rather than silently falling back."),
			flag("--no-bitwarden", "Explicitly switch credential_source back to env on re-setup "+
				"(only meaningful when the previous setup used --from-bitwarden)."),
			flag("--rotate-tokens", "Mint fresh proxy tokens for every provider (default is to "+
				"preserve tokens for providers that already had one — avoids "+
				"401-ing already-running sandboxes on re-setup)."),
			flag("--restart", "If a daemon is already running, restart it automatically after "+
				"writing the new config/tokens (non-interactive default on a tty is to ask)."),
			flag("--no-restart", "Do not restart a running daemon after setup; you'll need to run "+
				"`icebath egress restart` yourself for changes to take effect."),
		}},
		{"start", "Start the managed iron-proxy", egressCmdStart, nil},
		{"stop", "Stop the managed iron-proxy", egressCmdStop, nil},
		{"restart", "Restart the managed iron-proxy (stop if running, then start)", egressCmdRestart, nil},
		{"reload", "Hot-reload the running daemon's ruleset from proxy.yaml " +
			"(management API — no restart, no dropped connections)", egressCmdReload, nil},
		{"status", "Show proxy state and mappings", egressCmdStatus, []secretsArg{
			flag("--show-tokens", "Print the proxy tokens (default: redacted prefix only). "+
				"Beware: tokens may persist in your shell history."),
		}},
		{"disable", "Turn off the proxy integration", egressCmdDisable, nil},
		{"config", "Print the generated proxy.yaml path", egressCmdConfig, nil},
	}...)
}

// Handler of the subcommand the namespace names, nil when none ran
func (p *egressParser) handler(ns map[string]any) egressHandler {
	name, ok := ns[p.Dest].(string)
	if !ok {
		return nil
	}
	for _, cmd := range p.Commands {
		if cmd.Name == name {
			return cmd.Func
		}
	}
	return nil
}

// argparse style help of the egress tree
func (p *egressParser) printHelp(w io.Writer) {
	names := make([]string, 0, len(p.Commands))
	for _, cmd := range p.Commands {
		names = append(names, cmd.Name)
	}
	choices := "{" + strings.Join(names, ",") + "}"
	var b strings.Builder
	fmt.Fprintf(&b, "usage: icebath egress [-h] %s ...\n\npositional arguments:\n  %s\n", choices, choices)
	for _, cmd := range p.Commands {
		fmt.Fprintf(&b, "    %-20s%s\n", cmd.Name, cmd.Help)
	}
	b.WriteString("\noptions:\n  -h, --help            show this help message and exit\n")
	io.WriteString(w, b.String())
}

// EgressCommand runs icebath egress from the parsed namespace, help when no subcommand was named
func EgressCommand(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	parser := &egressParser{}
	registerEgressCLI(parser)
	handler := parser.handler(ns)
	if handler == nil {
		parser.printHelp(w)
		return 0, nil
	}
	return handler(&egressCLI{ctx: ctx, home: h, console: pyrich.NewConsole(w), ns: ns, ip: newEgressBackend(IronProxy{Home: h})})
}

func egressCmdInstall(cli *egressCLI) (int, error) {
	binary, err := cli.ip.installIronProxy(nsFlag(cli.ns, "force"))
	if err != nil {
		cli.console.Print("[red]✗ install failed:[/red] " + pyrepr.ExceptionMessage(err))
		cli.console.Print("  Manual install: https://github.com/ironsh/iron-proxy/releases")
		return 1, nil
	}
	version := cli.ip.ironProxyVersion(binary)
	if version == "" {
		version = "(version unknown)"
	}
	cli.console.Print(fmt.Sprintf("[green]✓[/green] installed %s  %s", binary, version))
	return 0, nil
}

// Four step wizard, each setup phase printing its own step and aborting the run when it fails
func egressCmdSetup(cli *egressCLI) (int, error) {
	console := cli.console
	console.PrintPanel(pyrich.Panel{Body: "[bold]iron-proxy setup[/bold]\n\n" +
		"Routes outbound sandbox traffic through a local TLS-intercepting\n" +
		"proxy so prompt-injected agents never see real provider API keys.\n\n" +
		"[dim]Project: https://github.com/ironsh/iron-proxy  (Apache-2.0)[/dim]", BorderStyle: "cyan", Fit: true})
	if !setupInstallBinary(cli) {
		return 1, nil
	}
	caCert, caKey, ok := setupCACert(cli)
	if !ok {
		return 1, nil
	}
	mappings, ok, err := setupMintTokens(cli)
	if err != nil || !ok {
		return 1, err
	}
	proxyCfg, ok, err := setupWriteConfig(cli, mappings, caCert, caKey)
	if err != nil || !ok {
		return 1, err
	}
	if err := setupRestartDaemon(cli, proxyCfg); err != nil {
		return 1, err
	}
	console.Print()
	console.Print("[green]✓ iron-proxy is configured.[/green]  Sandboxes will route outbound traffic through it.")
	console.Print("  Start:   [cyan]icebath egress start[/cyan]\n" +
		"  Restart: [cyan]icebath egress restart[/cyan]  (after any re-setup)\n" +
		"  Reload:  [cyan]icebath egress reload[/cyan]   (apply ruleset edits " +
		"in-place, no restart)\n" +
		"  Status:  [cyan]icebath egress status[/cyan]\n" +
		"  Stop:    [cyan]icebath egress stop[/cyan]\n" +
		"  Disable: [cyan]icebath egress disable[/cyan]")
	return 0, nil
}

func setupInstallBinary(cli *egressCLI) bool {
	step(cli.console, 1, "Install the iron-proxy binary")
	binary := cli.ip.findIronProxy(false)
	if binary == "" {
		cli.console.Print("  No iron-proxy on PATH — downloading…")
		installed, err := cli.ip.installIronProxy(false)
		if err != nil {
			cli.console.Print(fmt.Sprintf("  [red]✗ install failed: %s[/red]", pyrepr.ExceptionMessage(err)))
			return false
		}
		binary = installed
	}
	version := cli.ip.ironProxyVersion(binary)
	if version == "" {
		version = "(version unknown)"
	}
	cli.console.Print(fmt.Sprintf("  [green]✓[/green] %s  %s", binary, version))
	return true
}

func setupCACert(cli *egressCLI) (string, string, bool) {
	step(cli.console, 2, "Generate a CA cert")
	caCert, caKey, err := cli.ip.ensureCACert(false)
	if err != nil {
		cli.console.Print(fmt.Sprintf("  [red]✗ CA generation failed: %s[/red]", pyrepr.ExceptionMessage(err)))
		return "", "", false
	}
	cli.console.Print(fmt.Sprintf("  [green]✓[/green] %s", caCert))
	return caCert, caKey, true
}

// Sorted env var names of the bearer providers
func bearerProviderNames() []string {
	names := make([]string, 0, len(ironBearerProviders))
	for _, p := range ironBearerProviders {
		names = append(names, p.envName)
	}
	sort.Strings(names)
	return names
}

// Discovers providers, merges with existing tokens rotating on request and prints the table, false when the wizard aborts
func setupMintTokens(cli *egressCLI) ([]TokenMapping, bool, error) {
	console := cli.console
	step(console, 3, "Mint proxy tokens for known providers")
	var availableEnvNames []string
	if nsFlag(cli.ns, "from_bitwarden") {
		availableEnvNames = bitwardenEnvNames(cli)
		if availableEnvNames == nil {
			return nil, false, nil
		}
	} else if loaded := loadEnvFileIntoEnviron(cli.home); loaded > 0 {
		console.Print(fmt.Sprintf("  [dim]Loaded %d provider key name(s) from ~/.icebath/.env for discovery.[/dim]", loaded))
	}
	discovered := cli.ip.discoverProviderMappings(availableEnvNames)
	existing, err := cli.ip.loadMappings()
	if err != nil {
		return nil, false, err
	}
	rotate := nsFlag(cli.ns, "rotate_tokens")
	if rotate && len(existing) > 0 {
		if stdinIsATTY() {
			console.Print("[yellow]⚠[/yellow]  --rotate-tokens will invalidate proxy " +
				"tokens in every running Icebath sandbox.  They will start 401-ing against upstreams until restarted.")
			if egressPrompt(console, "Type 'rotate' to confirm: ") != "rotate" {
				console.Print("[yellow]Cancelled.[/yellow]")
				return nil, false, nil
			}
		}
		if err := backupMappingsBeforeRotation(cli); err != nil {
			console.Print(fmt.Sprintf("  [yellow]Could not back up mappings before rotation: %s[/yellow]", pyrepr.ExceptionMessage(err)))
		}
	} else if rotate {
		console.Print("[dim]Note: --rotate-tokens is a no-op on first-time setup (no existing tokens to rotate).[/dim]")
	}
	mappings := cli.ip.mergeMappings(existing, discovered, rotate)
	if len(mappings) == 0 {
		console.Print("  [yellow]No known provider API keys found in env/Bitwarden.[/yellow]")
		console.Print("  Set at least one of these and rerun setup:")
		for _, envName := range bearerProviderNames() {
			console.Print("    - " + envName)
		}
		return nil, false, nil
	}
	if uncovered := cli.ip.discoverUncoveredProviders(availableEnvNames); len(uncovered) > 0 {
		console.Print()
		console.Print("  [yellow]⚠[/yellow]  Detected provider env vars that the proxy does not yet cover:")
		for _, name := range uncovered {
			console.Print("    - " + name)
		}
		console.Print("  [dim]These providers use request signing or SDK-minted " +
			"OAuth (SigV4, service-account files) and will hold real " +
			"credentials inside the sandbox.  Egress isolation is INCOMPLETE for these.[/dim]")
	}
	console.PrintTable(mappingsTable(mappings, "Provider env", "Upstream hosts", false))
	return mappings, true, nil
}

// Plain JSON .rotated-<ts> copy of mappings.json so the operator can recover tokens by hand
func backupMappingsBeforeRotation(cli *egressCLI) error {
	stateDir, err := cli.ip.stateDir()
	if err != nil {
		return err
	}
	source := filepath.Join(stateDir, "mappings.json")
	if !fileExists(source) {
		return nil
	}
	backup := filepath.Join(stateDir, "mappings.json.rotated-"+time.Now().Format("20060102T150405"))
	if err := pyfs.Copy2(source, backup); err != nil {
		return err
	}
	cli.console.Print(fmt.Sprintf("  [dim]backup: %s[/dim]", backup))
	return nil
}

// Integer of a config value the way int() reads it, the default for a missing one
func cfgInt(cfg map[string]any, key string, def int) (int, error) {
	v, ok := cfg[key]
	if !ok {
		return def, nil
	}
	n, err := pyint.Of(v)
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

// Tunnel port from the flag, else the configured or default one, false after printing a bad flag
func setupTunnelPort(cli *egressCLI, proxyCfg map[string]any) (int, bool, error) {
	if raw := cli.ns["tunnel_port"]; raw != nil && !pyrepr.Equal(raw, 0) {
		n, err := pyint.Of(raw)
		if err != nil {
			return 0, false, err
		}
		port := int(n.Int64())
		if port < 1 || port > 65534 {
			cli.console.Print("  [red]✗ --tunnel-port must be between 1 and 65534 (the plain-HTTP listener uses port+1).[/red]")
			return 0, false, nil
		}
		return port, true, nil
	}
	port, err := cfgInt(proxyCfg, "tunnel_port", DefaultIronTunnelPort)
	if err != nil {
		return 0, false, err
	}
	return port, true, nil
}

// Writes proxy.yaml plus mappings, then enables the integration in config, false when the wizard aborts
func setupWriteConfig(cli *egressCLI, mappings []TokenMapping, caCert, caKey string) (map[string]any, bool, error) {
	console := cli.console
	step(console, 4, "Write config and persist mappings")
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		return nil, false, err
	}
	proxyCfg := secretsSetdefaultMapping(cfg, "proxy")
	tunnelPort, ok, err := setupTunnelPort(cli, proxyCfg)
	if err != nil || !ok {
		return nil, false, err
	}
	proxyCfg["tunnel_port"] = tunnelPort
	allowed := append([]string(nil), DefaultIronAllowedHosts...)
	for _, h := range anyStrings(proxyCfg["extra_allowed_hosts"]) {
		if !containsString(DefaultIronAllowedHosts, h) {
			allowed = append(allowed, h)
		}
	}
	stateDir, err := cli.ip.stateDir()
	if err != nil {
		return nil, false, err
	}
	auditLogPath := filepath.Join(stateDir, "audit.log")
	auditLogOK := true
	if err := cli.ip.ensureAuditLog(auditLogPath); err != nil {
		auditLogOK = false
		console.Print(fmt.Sprintf("  [yellow]⚠ %s[/yellow]", pyrepr.ExceptionMessage(err)))
	}
	denyCIDRs, denySet := proxyCfg["upstream_deny_cidrs"]
	ironCfg := cli.ip.buildProxyConfig(IronProxyConfigOptions{Mappings: mappings, CACert: caCert, CAKey: caKey, TunnelPort: tunnelPort,
		AuditLog: auditLogPath, AllowedHosts: allowed, UpstreamDenyCIDRs: anyStrings(denyCIDRs), DenyCIDRsSet: denySet && denyCIDRs != nil})
	cfgPath, err := cli.ip.writeProxyConfig(ironCfg)
	if err != nil {
		return nil, false, err
	}
	mappingsPath, err := cli.ip.writeMappings(mappings)
	if err != nil {
		return nil, false, err
	}
	if _, err := cli.ip.ensureManagementToken(false); err != nil {
		return nil, false, err
	}
	console.Print(fmt.Sprintf("  [green]✓[/green] config:   %s", cfgPath))
	console.Print(fmt.Sprintf("  [green]✓[/green] mappings: %s", mappingsPath))
	if auditLogOK {
		console.Print(fmt.Sprintf("  [green]✓[/green] audit log: %s "+
			"[dim](reserved — not written by iron-proxy v0.39; "+
			"per-request records land in iron-proxy.log)[/dim]", auditLogPath))
	}
	proxyCfg["enabled"] = true
	for _, entry := range []struct {
		key   string
		value any
	}{{"auto_install", true}, {"enforce_on_docker", true}} {
		if _, has := proxyCfg[entry.key]; !has {
			proxyCfg[entry.key] = entry.value
		}
	}
	existingSource := anyString(proxyCfg["credential_source"])
	switch {
	case nsFlag(cli.ns, "from_bitwarden"):
		proxyCfg["credential_source"] = "bitwarden"
	case nsFlag(cli.ns, "no_bitwarden"):
		proxyCfg["credential_source"] = "env"
		if existingSource == "bitwarden" {
			console.Print("[yellow]Switched credential_source from bitwarden to env.[/yellow]")
		}
	case existingSource == "bitwarden":
		console.Print("[dim]Keeping credential_source=bitwarden from existing config. " +
			"Pass --no-bitwarden to switch to env-based credentials.[/dim]")
	default:
		proxyCfg["credential_source"] = "env"
	}
	if err := secretsSaveConfig(cli.home, cfg); err != nil {
		return nil, false, err
	}
	return proxyCfg, true, nil
}

// Restart preference of the setup flags, nil when neither --restart nor --no-restart was given
func restartPreference(ns map[string]any) *bool {
	if nsFlag(ns, "no_restart") {
		v := false
		return &v
	}
	if nsFlag(ns, "restart") {
		v := true
		return &v
	}
	return nil
}

// Stops a running daemon and decides whether to start it again with the new config
func setupRestartDaemon(cli *egressCLI, proxyCfg map[string]any) error {
	console := cli.console
	wasRunning := cli.ip.getStatus().PID != 0
	if wasRunning {
		if _, err := cli.ip.stopProxy(); err != nil {
			return err
		}
	}
	doRestart := false
	switch pref := restartPreference(cli.ns); {
	case pref != nil:
		doRestart = *pref
	case wasRunning:
		doRestart = true
		if stdinIsATTY() {
			answer := egressPrompt(console, "  Restart the running proxy now with the new config? [Y/n] ")
			doRestart = answer == "" || answer == "y" || answer == "yes"
		}
	}
	if doRestart {
		newStatus, err := cli.ip.startProxy(IronProxyStartOptions{InstallIfMissing: cfgBool(proxyCfg, "auto_install", true)})
		if err != nil {
			console.Print(fmt.Sprintf("  [yellow]⚠ could not start iron-proxy with the new config: %s[/yellow]", pyrepr.ExceptionMessage(err)))
			console.Print("  Run [cyan]icebath egress start[/cyan] manually before launching new Docker sandboxes.")
			return nil
		}
		listening := "not yet listening"
		if newStatus.Listening {
			listening = "listening"
		}
		verb := "started"
		if wasRunning {
			verb = "restarted"
		}
		console.Print(fmt.Sprintf("  [green]✓[/green] %s iron-proxy with the new config (pid=%d, port=%d, %s)", verb, newStatus.PID, newStatus.TunnelPort, listening))
		return nil
	}
	if wasRunning {
		console.Print("  [yellow]⚠ stopped the running iron-proxy; config or tokens " +
			"changed.  Run [cyan]icebath egress restart[/cyan] (or " +
			"[cyan]start[/cyan]) before launching new Docker sandboxes.[/yellow]")
	}
	return nil
}

func egressCmdStart(cli *egressCLI) (int, error) {
	console := cli.console
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		return 1, err
	}
	proxyCfg := secretsMapping(cfg["proxy"])
	if !pyTruthy(proxyCfg["enabled"]) {
		console.Print("[yellow]proxy.enabled is false — run `icebath egress setup` first.[/yellow]")
		return 1, nil
	}
	credentialSource := cfgText(proxyCfg, "credential_source", "env")
	bwCfg, bwPresent := secretsMapping(cfg["secrets"])["bitwarden"].(map[string]any)
	refreshBW := credentialSource == "bitwarden" && bwPresent && pyTruthy(bwCfg["enabled"])
	if credentialSource == "bitwarden" && !refreshBW {
		if cfgBool(proxyCfg, "allow_env_fallback", false) {
			console.Print("[yellow]⚠ credential_source=bitwarden but secrets.bitwarden is disabled or missing — falling back " +
				"to host-env secrets (allow_env_fallback=true).  Rotated Bitwarden keys will NOT propagate.[/yellow]")
		} else {
			return egressRefuse(console,
				"proxy.credential_source is 'bitwarden' but secrets.bitwarden is disabled or missing.",
				"Re-enable it (`secrets.bitwarden.enabled: true`), switch "+
					"back to env credentials with `icebath egress setup "+
					"--no-bitwarden`, or set `proxy.allow_env_fallback: true` to opt into the host-env fallback."), nil
		}
	}
	var bitwardenConfig map[string]any
	if bwPresent {
		bitwardenConfig = bwCfg
	}
	if refreshBW && bwPresent {
		copied := make(map[string]any, len(bwCfg)+1)
		for k, v := range bwCfg {
			copied[k] = v
		}
		copied["allow_env_fallback"] = cfgBool(proxyCfg, "allow_env_fallback", false)
		bitwardenConfig = copied
	}
	if refreshBW {
		bwAccessEnv := cfgText(bitwardenConfig, "access_token_env", "BWS_ACCESS_TOKEN")
		if pystr.Strip(os.Getenv(bwAccessEnv)) == "" {
			return egressRefuse(console,
				fmt.Sprintf("credential_source=bitwarden but %s is not set in the environment.", bwAccessEnv),
				"Either export the access token, or run "+
					"`icebath egress setup --no-bitwarden` to switch back to env-based credentials."), nil
		}
		if !pyTruthy(bitwardenConfig["project_id"]) {
			return egressRefuse(console,
				"credential_source=bitwarden but secrets.bitwarden.project_id is empty.",
				"Run `icebath secrets bitwarden setup` to configure the "+
					"project, or switch back via `icebath egress setup --no-bitwarden`."), nil
		}
	}
	status, err := cli.ip.startProxy(IronProxyStartOptions{InstallIfMissing: cfgBool(proxyCfg, "auto_install", true),
		RefreshSecretsFromBitwarden: refreshBW, BitwardenConfig: bitwardenConfig})
	if err != nil {
		console.Print("[red]✗ failed to start iron-proxy:[/red] " + pyrepr.ExceptionMessage(err))
		return 1, nil
	}
	if status.PID == 0 {
		console.Print("[red]✗ iron-proxy did not come up cleanly[/red]")
		return 1, nil
	}
	listening := "[yellow]not yet listening[/yellow]"
	if status.Listening {
		listening = "[green]listening[/green]"
	}
	console.Print(fmt.Sprintf("[green]✓[/green] iron-proxy running  pid=%d  port=%d  %s", status.PID, status.TunnelPort, listening))
	return 0, nil
}

func egressCmdStop(cli *egressCLI) (int, error) {
	stopped, err := cli.ip.stopProxy()
	if err != nil {
		return 1, err
	}
	if stopped {
		cli.console.Print("[green]✓[/green] iron-proxy stopped")
	} else {
		cli.console.Print("[dim]iron-proxy was not running[/dim]")
	}
	return 0, nil
}

// Stops the daemon when one runs, then starts through every credential source guard
func egressCmdRestart(cli *egressCLI) (int, error) {
	stopped, err := cli.ip.stopProxy()
	if err != nil {
		return 1, err
	}
	if stopped {
		cli.console.Print("[dim]stopped the running iron-proxy[/dim]")
	}
	return egressCmdStart(cli)
}

// Hot reloads the ruleset through the management API, new secrets still needing a restart
func egressCmdReload(cli *egressCLI) (int, error) {
	if err := cli.ip.reloadProxy(); err != nil {
		cli.console.Print("[red]✗ reload failed:[/red] " + pyrepr.ExceptionMessage(err))
		return 1, nil
	}
	cli.console.Print("[green]✓[/green] iron-proxy ruleset reloaded in-place (no restart, connections preserved)")
	cli.console.Print("[dim]Note: new upstream secrets (rotated keys, new providers) " +
		"still need `icebath egress restart` — the daemon reads real " +
		"credentials from its environment at spawn time.[/dim]")
	return 0, nil
}

// FormatEgressStatusText is the plain text egress status for slash commands, Dashboard and Desktop
func FormatEgressStatusText(h string, showTokens bool) (string, error) {
	cfg, err := secretsLoadConfig(h)
	if err != nil {
		return "", err
	}
	ip := newEgressBackend(IronProxy{Home: h})
	proxyCfg := secretsMapping(cfg["proxy"])
	status := ip.getStatus()
	lines := []string{"Egress proxy status", ""}
	for _, row := range statusRows(proxyCfg, status, func(v bool) string {
		if v {
			return "yes"
		}
		return "no"
	}, func(t string) string { return t }) {
		lines = append(lines, row[0]+": "+row[1])
	}
	lines = append(lines, "Scope: Docker backend only in this release")
	mappings, err := ip.loadMappings()
	if err != nil {
		return "", err
	}
	if len(mappings) > 0 {
		lines = append(lines, "", "Token mappings:")
		for _, m := range mappings {
			tok := m.ProxyToken
			if !showTokens {
				tok = config.MaskSecret(m.ProxyToken, "")
			}
			lines = append(lines, fmt.Sprintf("  - %s: %s (%s)", m.RealEnvName, tok, strings.Join(m.UpstreamHosts, ", ")))
		}
	}
	if uncovered := ip.discoverUncoveredProviders(nil); len(uncovered) > 0 {
		lines = append(lines, "", "Uncovered providers (real credentials still visible inside the sandbox):")
		for _, name := range uncovered {
			lines = append(lines, "  - "+name)
		}
	}
	enabled := pyTruthy(proxyCfg["enabled"])
	if enabled && !status.Configured() {
		lines = append(lines, "", "Next: run `icebath egress setup` to mint tokens and write proxy.yaml.")
	} else if enabled && !(status.PID != 0 && status.Listening) {
		lines = append(lines, "", "Next: run `icebath egress start` before launching Docker sandboxes.")
	}
	return strings.Join(lines, "\n"), nil
}

func egressCmdStatus(cli *egressCLI) (int, error) {
	console := cli.console
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		return 1, err
	}
	proxyCfg := secretsMapping(cfg["proxy"])
	status := cli.ip.getStatus()
	table := &pyrich.Table{HideHeader: true, NoBox: true, PadX: 2, Columns: []pyrich.Column{{Style: "bold"}, {}}}
	for _, row := range statusRows(proxyCfg, status, yn, func(t string) string { return "[dim]" + t + "[/dim]" }) {
		table.AddRow(row[0], row[1])
	}
	console.PrintTable(table)
	mappings, err := cli.ip.loadMappings()
	if err != nil {
		return 1, err
	}
	showTokens := nsFlag(cli.ns, "show_tokens")
	if len(mappings) > 0 {
		console.Print()
		console.Print("[bold]Token mappings[/bold]")
		console.PrintTable(mappingsTable(mappings, "Real env", "Upstream", showTokens))
		if showTokens {
			console.Print("[yellow]⚠[/yellow]  proxy tokens just printed in full — " +
				"they may persist in your shell history.  Consider clearing it after this command.")
		}
	}
	if uncovered := cli.ip.discoverUncoveredProviders(nil); len(uncovered) > 0 {
		console.Print()
		console.Print("[yellow]Uncovered providers[/yellow] (real credentials still visible inside the sandbox):")
		for _, name := range uncovered {
			console.Print("  - " + name)
		}
	}
	return 0, nil
}

// Turns the integration off, a still running daemon reported through the liveness checked status pid
func egressCmdDisable(cli *egressCLI) (int, error) {
	console := cli.console
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		return 1, err
	}
	proxyCfg := secretsSetdefaultMapping(cfg, "proxy")
	if !pyTruthy(proxyCfg["enabled"]) {
		console.Print("[dim]proxy.enabled was already false.[/dim]")
		return 0, nil
	}
	proxyCfg["enabled"] = false
	if err := secretsSaveConfig(cli.home, cfg); err != nil {
		return 1, err
	}
	console.Print("[green]✓[/green] proxy.enabled set to false")
	if cli.ip.getStatus().PID != 0 {
		console.Print("  iron-proxy is still running — stop it with [cyan]icebath egress stop[/cyan] if you want it down too.")
	}
	return 0, nil
}

func egressCmdConfig(cli *egressCLI) (int, error) {
	status := cli.ip.getStatus()
	if status.ConfigPath == "" {
		cli.console.Print("[yellow](no config generated — run `icebath egress setup`)[/yellow]")
		return 1, nil
	}
	cli.console.Print(status.ConfigPath)
	return 0, nil
}

// Secret names from Bitwarden for setup --from-bitwarden, nil after printing so the wizard aborts loudly
func bitwardenEnvNames(cli *egressCLI) []string {
	console := cli.console
	cfg, err := secretsLoadConfig(cli.home)
	if err != nil {
		console.Print(fmt.Sprintf("  [red]✗ Could not enumerate Bitwarden secrets: %s[/red]", pyrepr.ExceptionMessage(err)))
		return nil
	}
	bwCfg := sectionCfg(cfg, "bitwarden")
	if !pyTruthy(bwCfg["enabled"]) {
		console.Print("  [red]✗ --from-bitwarden requested but secrets.bitwarden.enabled is false.[/red]")
		console.Print("  Run `icebath secrets bitwarden setup` first, or omit --from-bitwarden.")
		return nil
	}
	tokenEnv := cfgText(bwCfg, "access_token_env", "BWS_ACCESS_TOKEN")
	accessToken := pystr.Strip(os.Getenv(tokenEnv))
	if accessToken == "" {
		console.Print(fmt.Sprintf("  [red]✗ --from-bitwarden requested but %s is not set in the environment.[/red]", tokenEnv))
		return nil
	}
	secrets, _, err := cli.ip.fetchBitwardenSecrets(BitwardenFetchOptions{AccessToken: accessToken, ProjectID: cfgText(bwCfg, "project_id", ""), CacheTTL: 0, UseCache: false, HomePath: cli.home})
	if err != nil {
		console.Print(fmt.Sprintf("  [red]✗ Could not enumerate Bitwarden secrets: %s[/red]", pyrepr.ExceptionMessage(err)))
		console.Print("  Either fix the Bitwarden config and retry, or rerun setup " +
			"without --from-bitwarden (the proxy will read secrets from the host process env at start time).")
		return nil
	}
	names := sortedStringKeys(secrets)
	if len(names) == 0 {
		console.Print("  [red]✗ Bitwarden returned an empty secrets list.[/red]\n" +
			"  Check the project_id in secrets.bitwarden and the BWS access-token's project scope.")
		return nil
	}
	console.Print(fmt.Sprintf("  Pulled %d env names from Bitwarden.", len(names)))
	return names
}

// Backfills known provider keys from the home .env into the process env, never overriding an exported value
func loadEnvFileIntoEnviron(h string) int {
	fileEnv := config.LoadEnv(h)
	known := append(bearerProviderNames(), ironNonBearerProviders...)
	added := 0
	for _, name := range known {
		if pystr.Strip(os.Getenv(name)) != "" {
			continue
		}
		if val := pystr.Strip(fileEnv[name]); val != "" {
			os.Setenv(name, val)
			added++
		}
	}
	return added
}

// Table of the mappings with tokens masked unless asked to show them
func mappingsTable(mappings []TokenMapping, envHeader, hostsHeader string, showTokens bool) *pyrich.Table {
	table := &pyrich.Table{Columns: []pyrich.Column{{Header: envHeader, Style: "cyan"}, {Header: hostsHeader, Style: "dim"}, {Header: "Proxy token", Style: "green"}}}
	for _, m := range mappings {
		tok := m.ProxyToken
		if !showTokens {
			tok = config.MaskSecret(m.ProxyToken, "")
		}
		table.AddRow(m.RealEnvName, strings.Join(m.UpstreamHosts, ", "), tok)
	}
	return table
}

// Prints the refusing to start headline plus an indented remedy, exit code 1
func egressRefuse(console *pyrich.Console, reason, hint string) int {
	console.Print(fmt.Sprintf("[red]✗ Refusing to start: %s[/red]", reason))
	console.Print("  " + hint)
	return 1
}

// Lowered and stripped answer to a prompt, a closed stdin reading as empty
func egressPrompt(console *pyrich.Console, text string) string {
	answer, err := console.Input(text)
	if err != nil {
		return ""
	}
	return strings.ToLower(pystr.Strip(answer))
}

// Label and value rows shared by the rich status table and the plain text variant
func statusRows(proxyCfg map[string]any, status IronProxyStatus, yn func(bool) string, dim func(string) string) [][2]string {
	orDim := func(value, missing string) string {
		if value == "" {
			return dim(missing)
		}
		return value
	}
	process := dim("(stopped)")
	if status.PID != 0 {
		process = fmt.Sprintf("pid %d", status.PID)
	}
	return [][2]string{
		{"Enabled", yn(pyTruthy(proxyCfg["enabled"]))},
		{"Binary", orDim(status.BinaryPath, "(missing)")},
		{"Binary version", orDim(status.BinaryVersion, "(unknown)")},
		{"Config", orDim(status.ConfigPath, "(not generated)")},
		{"CA cert", orDim(status.CACertPath, "(not generated)")},
		{"Tunnel port", fmt.Sprint(status.TunnelPort)},
		{"Process", process},
		{"Listening", yn(status.Listening)},
		{"Credential src", cfgText(proxyCfg, "credential_source", "env")},
		{"Docker enforce", yn(cfgBool(proxyCfg, "enforce_on_docker", true))},
	}
}
