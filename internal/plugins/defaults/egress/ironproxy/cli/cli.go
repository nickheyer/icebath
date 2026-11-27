// The egress commands: install, set up, start, stop, restart, reload, see, turn off the iron-proxy egress firewall, and where its config is
package cli

import (
	"fmt"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	ironconfig "github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/config"
	ironterminal "github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/terminal"
	"github.com/nickheyer/icebath/internal/tools/redact"
	egressv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/egress/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/egress/v1/egressv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	e := egress{service: egressv1connect.NewEgressServiceClient(conn)}
	root := &cobra.Command{Use: "egress", Short: "Manage the iron-proxy egress credential-injection firewall", Args: cobra.NoArgs}
	root.AddCommand(e.install(), e.setup(),
		&cobra.Command{Use: "start", Short: "Start the managed iron-proxy", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return e.start(cmd) }},
		&cobra.Command{Use: "stop", Short: "Stop the managed iron-proxy", Args: cobra.NoArgs, RunE: e.stop},
		&cobra.Command{Use: "restart", Short: "Restart the managed iron-proxy (stop if running, then start)", Args: cobra.NoArgs, RunE: e.restart},
		&cobra.Command{Use: "reload", Short: "Hot-reload the running daemon's ruleset from proxy.yaml (management API — no restart, no dropped connections)",
			Args: cobra.NoArgs, RunE: e.reload},
		e.status(),
		&cobra.Command{Use: "disable", Short: "Turn off the proxy integration", Args: cobra.NoArgs, RunE: e.disable},
		&cobra.Command{Use: "config", Short: "Print the generated proxy.yaml path", Args: cobra.NoArgs, RunE: e.config})
	return []*cobra.Command{root}
}

type egress struct {
	service egressv1connect.EgressServiceClient
}

func (e egress) install() *cobra.Command {
	force := false
	command := &cobra.Command{Use: "install", Short: "Install the PM-pinned iron-proxy binary", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			installed, err := e.service.Install(cmd.Context(), &egressv1.InstallRequest{Force: force})
			if err != nil {
				c.Say(c.Paint("✗ install failed:", cli.Red) + " " + tui.Reason(err))
				c.Say("  Manual install: https://github.com/ironsh/iron-proxy/releases")
				return cli.Exit(1)
			}
			c.Say(c.Tick() + " installed " + installed.GetBinary() + "  " + version(installed.GetVersion()))
			return nil
		}}
	command.Flags().BoolVar(&force, "force", false, "Verify and repair the managed copy")
	return command
}

func version(text string) string {
	if text == "" {
		return "(version unknown)"
	}
	return text
}

// Table of the mappings, the tokens masked unless asked to show them
func table(c *cli.Console, mappings []*egressv1.Mapping, envHeader, hostsHeader string, show bool) {
	var rows [][]string
	for _, m := range mappings {
		token := m.GetToken()
		if !show {
			token = redact.Ends(token)
		}
		rows = append(rows, []string{c.Paint(m.GetEnv(), cli.Cyan), c.Paint(strings.Join(m.GetHosts(), ", "), cli.Dim), c.Paint(token, cli.Green)})
	}
	c.Table([]string{envHeader, hostsHeader, "Proxy token"}, rows)
}

func (e egress) setup() *cobra.Command {
	var port int
	var fromBitwarden, noBitwarden, rotate, restart, noRestart bool
	command := &cobra.Command{Use: "setup", Short: "Interactive wizard: install + CA + mint tokens + write config", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			ctx := cmd.Context()
			c.Panel("", c.Paint("iron-proxy setup", cli.Bold), "", "Routes outbound sandbox traffic through a local TLS-intercepting",
				"proxy so prompt-injected agents never see real provider API keys.", "", c.Paint("Project: https://github.com/ironsh/iron-proxy  (Apache-2.0)", cli.Dim))
			c.Step(1, "Install the iron-proxy binary")
			status, err := e.service.Status(ctx, &egressv1.StatusRequest{})
			if err != nil {
				return err
			}
			binary, found := status.GetBinary(), status.GetVersion()
			if binary == "" {
				c.Say("  No iron-proxy on PATH — downloading…")
				installed, err := e.service.Install(ctx, &egressv1.InstallRequest{})
				if err != nil {
					return c.Fail("  ✗ install failed: " + tui.Reason(err))
				}
				binary, found = installed.GetBinary(), installed.GetVersion()
			}
			c.Say("  " + c.Tick() + " " + binary + "  " + version(found))
			c.Step(2, "Generate a CA cert")
			authority, err := e.service.Authority(ctx, &egressv1.AuthorityRequest{})
			if err != nil {
				return c.Fail("  ✗ CA generation failed: " + tui.Reason(err))
			}
			c.Say("  " + c.Tick() + " " + authority.GetCertificate())
			c.Step(3, "Mint proxy tokens for known providers")
			if rotate && len(status.GetMappings()) > 0 && c.Interactive() {
				c.Say(c.Paint("⚠", cli.Yellow) + "  --rotate-tokens will invalidate proxy tokens in every running Icebath sandbox.  They will start 401-ing " +
					"against upstreams until restarted.")
				if answer, _ := c.Input("Type 'rotate' to confirm: "); strings.ToLower(strings.TrimSpace(answer)) != "rotate" {
					c.Say("Cancelled.", cli.Yellow)
					return cli.Exit(1)
				}
			} else if rotate && len(status.GetMappings()) == 0 {
				c.Say("Note: --rotate-tokens is a no-op on first-time setup (no existing tokens to rotate).", cli.Dim)
			}
			minted, err := e.service.Mint(ctx, &egressv1.MintRequest{FromBitwarden: fromBitwarden, Rotate: rotate})
			if err != nil {
				return err
			}
			if refusal := minted.GetRefusal(); len(refusal) > 0 {
				c.Say("  "+refusal[0], cli.Red)
				for _, line := range refusal[1:] {
					c.Say("  " + line)
				}
				return cli.Exit(1)
			}
			if n := minted.GetPulled(); n > 0 {
				c.Say(fmt.Sprintf("  Pulled %d env names from Bitwarden.", n))
			}
			if n := minted.GetLoaded(); n > 0 {
				c.Say(fmt.Sprintf("  Loaded %d provider key name(s) from the profile's .env for discovery.", n), cli.Dim)
			}
			if minted.GetBackup() != "" {
				c.Say("  backup: "+minted.GetBackup(), cli.Dim)
			}
			if minted.GetBackupError() != "" {
				c.Say("  Could not back up mappings before rotation: "+minted.GetBackupError(), cli.Yellow)
			}
			if len(minted.GetMappings()) == 0 {
				c.Say("  No known provider API keys found in env/Bitwarden.", cli.Yellow)
				c.Say("  Set at least one of these and rerun setup:")
				for _, name := range ironterminal.BearerNames() {
					c.Say("    - " + name)
				}
				return cli.Exit(1)
			}
			if uncovered := minted.GetUncovered(); len(uncovered) > 0 {
				c.Blank()
				c.Say("  " + c.Paint("⚠", cli.Yellow) + "  Detected provider env vars that the proxy does not yet cover:")
				for _, name := range uncovered {
					c.Say("    - " + name)
				}
				c.Say("  These providers use request signing or SDK-minted OAuth (SigV4, service-account files) and will hold real credentials inside the "+
					"sandbox.  Egress isolation is INCOMPLETE for these.", cli.Dim)
			}
			table(c, minted.GetMappings(), "Provider env", "Upstream hosts", false)
			c.Step(4, "Write config and persist mappings")
			written, err := e.service.Write(ctx, &egressv1.WriteRequest{Mappings: minted.GetMappings(), TunnelPort: int32(port), FromBitwarden: fromBitwarden,
				NoBitwarden: noBitwarden})
			if err != nil {
				return err
			}
			if written.GetBadPort() {
				return c.Fail("  ✗ --tunnel-port must be between 1 and 65534 (the plain-HTTP listener uses port+1).")
			}
			if written.GetAuditError() != "" {
				c.Say("  ⚠ "+written.GetAuditError(), cli.Yellow)
			}
			c.Say("  " + c.Tick() + " config:   " + written.GetConfig())
			c.Say("  " + c.Tick() + " mappings: " + written.GetMappings())
			if written.GetAudit() != "" {
				c.Say("  " + c.Tick() + " audit log: " + written.GetAudit() + " " + c.Paint("(reserved — not written by iron-proxy v0.39; per-request records land in iron-proxy.log)", cli.Dim))
			}
			for _, note := range written.GetNotes() {
				c.Say(note, cli.Yellow)
			}
			if err := e.restartAfterSetup(c, cmd, restart, noRestart); err != nil {
				return err
			}
			c.Blank()
			c.Say(c.Paint("✓ iron-proxy is configured.", cli.Green) + "  Sandboxes will route outbound traffic through it.")
			c.Say("  Start:   " + c.Paint("icebath egress start", cli.Cyan) + "\n  Restart: " + c.Paint("icebath egress restart", cli.Cyan) + "  (after any re-setup)\n" +
				"  Reload:  " + c.Paint("icebath egress reload", cli.Cyan) + "   (apply ruleset edits in-place, no restart)\n  Status:  " +
				c.Paint("icebath egress status", cli.Cyan) + "\n  Stop:    " + c.Paint("icebath egress stop", cli.Cyan) + "\n  Disable: " + c.Paint("icebath egress disable", cli.Cyan))
			return nil
		}}
	command.Flags().IntVar(&port, "tunnel-port", 0, fmt.Sprintf("Override the tunnel port (default %d)", ironconfig.DefaultTunnelPort))
	command.Flags().BoolVar(&fromBitwarden, "from-bitwarden", false, "Treat secrets as managed by Bitwarden — discover provider keys from secrets.bitwarden "+
		"config instead of the current env.  Fails loudly if BW is unreachable rather than silently falling back.")
	command.Flags().BoolVar(&noBitwarden, "no-bitwarden", false, "Explicitly switch credential_source back to env on re-setup (only meaningful when the "+
		"previous setup used --from-bitwarden).")
	command.Flags().BoolVar(&rotate, "rotate-tokens", false, "Mint fresh proxy tokens for every provider (default is to preserve tokens for providers that "+
		"already had one — avoids 401-ing already-running sandboxes on re-setup).")
	command.Flags().BoolVar(&restart, "restart", false, "If a daemon is already running, restart it automatically after writing the new config/tokens "+
		"(non-interactive default on a tty is to ask).")
	command.Flags().BoolVar(&noRestart, "no-restart", false, "Do not restart a running daemon after setup; you'll need to run `icebath egress restart` "+
		"yourself for changes to take effect.")
	return command
}

// Stops a running daemon and starts it again with the new config when asked, by the flags or at the prompt
func (e egress) restartAfterSetup(c *cli.Console, cmd *cobra.Command, restart, noRestart bool) error {
	ctx := cmd.Context()
	status, err := e.service.Status(ctx, &egressv1.StatusRequest{})
	if err != nil {
		return err
	}
	running := status.GetPid() != 0
	if running {
		if _, err := e.service.Stop(ctx, &egressv1.StopRequest{}); err != nil {
			return err
		}
	}
	again := false
	switch {
	case noRestart:
	case restart:
		again = true
	case running:
		again = !c.Interactive() || c.Confirm("  Restart the running proxy now with the new config?", true)
	}
	if !again {
		if running {
			c.Say("  ⚠ stopped the running iron-proxy; config or tokens changed.  Run "+c.Paint("icebath egress restart", cli.Cyan)+" (or "+
				c.Paint("start", cli.Cyan)+") before launching new Docker sandboxes.", cli.Yellow)
		}
		return nil
	}
	started, err := e.service.Start(ctx, &egressv1.StartRequest{Setup: true})
	if err != nil {
		c.Say("  ⚠ could not start iron-proxy with the new config: "+tui.Reason(err), cli.Yellow)
		c.Say("  Run " + c.Paint("icebath egress start", cli.Cyan) + " manually before launching new Docker sandboxes.")
		return nil
	}
	listening, verb := "not yet listening", "started"
	if started.GetListening() {
		listening = "listening"
	}
	if running {
		verb = "restarted"
	}
	c.Say(fmt.Sprintf("  %s %s iron-proxy with the new config (pid=%d, port=%d, %s)", c.Tick(), verb, started.GetPid(), started.GetPort(), listening))
	return nil
}

func (e egress) start(cmd *cobra.Command) error {
	c := cli.NewConsole(cmd)
	started, err := e.service.Start(cmd.Context(), &egressv1.StartRequest{})
	if err != nil {
		return c.Fail("✗ failed to start iron-proxy: " + tui.Reason(err))
	}
	for _, note := range started.GetNotes() {
		c.Say(note, cli.Yellow)
	}
	switch {
	case started.GetRefusal() != "" && started.GetHint() == "":
		c.Say(started.GetRefusal(), cli.Yellow)
		return cli.Exit(1)
	case started.GetRefusal() != "":
		c.Say("✗ Refusing to start: "+started.GetRefusal(), cli.Red)
		c.Say("  " + started.GetHint())
		return cli.Exit(1)
	}
	listening := c.Paint("not yet listening", cli.Yellow)
	if started.GetListening() {
		listening = c.Paint("listening", cli.Green)
	}
	c.Say(fmt.Sprintf("%s iron-proxy running  pid=%d  port=%d  %s", c.Tick(), started.GetPid(), started.GetPort(), listening))
	return nil
}

func (e egress) stop(cmd *cobra.Command, _ []string) error {
	c := cli.NewConsole(cmd)
	stopped, err := e.service.Stop(cmd.Context(), &egressv1.StopRequest{})
	if err != nil {
		return err
	}
	if stopped.GetStopped() {
		c.Say(c.Tick() + " iron-proxy stopped")
	} else {
		c.Say("iron-proxy was not running", cli.Dim)
	}
	return nil
}

// Stops the daemon when one runs, then starts it through every check of the key source
func (e egress) restart(cmd *cobra.Command, _ []string) error {
	c := cli.NewConsole(cmd)
	stopped, err := e.service.Stop(cmd.Context(), &egressv1.StopRequest{})
	if err != nil {
		return err
	}
	if stopped.GetStopped() {
		c.Say("stopped the running iron-proxy", cli.Dim)
	}
	return e.start(cmd)
}

func (e egress) reload(cmd *cobra.Command, _ []string) error {
	c := cli.NewConsole(cmd)
	if _, err := e.service.Reload(cmd.Context(), &egressv1.ReloadRequest{}); err != nil {
		return c.Fail("✗ reload failed: " + tui.Reason(err))
	}
	c.Say(c.Tick() + " iron-proxy ruleset reloaded in-place (no restart, connections preserved)")
	c.Say("Note: new upstream secrets (rotated keys, new providers) still need `icebath egress restart` — the daemon reads real credentials from its "+
		"environment at spawn time.", cli.Dim)
	return nil
}

func (e egress) status() *cobra.Command {
	show := false
	command := &cobra.Command{Use: "status", Short: "Show proxy state and mappings", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			status, err := e.service.Status(cmd.Context(), &egressv1.StatusRequest{})
			if err != nil {
				return err
			}
			or := func(value, missing string) string {
				if value == "" {
					return c.Paint(missing, cli.Dim)
				}
				return value
			}
			process := c.Paint("(stopped)", cli.Dim)
			if status.GetPid() != 0 {
				process = fmt.Sprintf("pid %d", status.GetPid())
			}
			c.Pairs("Egress proxy", [][2]string{
				{"Enabled", c.YesNo(status.GetEnabled())},
				{"Binary", or(status.GetBinary(), "(missing)")},
				{"Binary version", or(status.GetVersion(), "(unknown)")},
				{"Config", or(status.GetConfig(), "(not generated)")},
				{"CA cert", or(status.GetAuthority(), "(not generated)")},
				{"Tunnel port", fmt.Sprint(status.GetPort())},
				{"Process", process},
				{"Listening", c.YesNo(status.GetListening())},
				{"Credential src", status.GetCredentialSource()},
				{"Docker enforce", c.YesNo(status.GetEnforce())},
			})
			if mappings := status.GetMappings(); len(mappings) > 0 {
				c.Blank()
				c.Say("Token mappings", cli.Bold)
				table(c, mappings, "Real env", "Upstream", show)
				if show {
					c.Say(c.Paint("⚠", cli.Yellow) + "  proxy tokens just printed in full — they may persist in your shell history.  Consider clearing it after this command.")
				}
			}
			if uncovered := status.GetUncovered(); len(uncovered) > 0 {
				c.Blank()
				c.Say(c.Paint("Uncovered providers", cli.Yellow) + " (real credentials still visible inside the sandbox):")
				for _, name := range uncovered {
					c.Say("  - " + name)
				}
			}
			return nil
		}}
	command.Flags().BoolVar(&show, "show-tokens", false, "Print the proxy tokens (default: redacted prefix only). Beware: tokens may persist in your shell history.")
	return command
}

func (e egress) disable(cmd *cobra.Command, _ []string) error {
	c := cli.NewConsole(cmd)
	disabled, err := e.service.Disable(cmd.Context(), &egressv1.DisableRequest{})
	if err != nil {
		return err
	}
	if !disabled.GetWasEnabled() {
		c.Say("proxy.enabled was already false.", cli.Dim)
		return nil
	}
	c.Say(c.Tick() + " proxy.enabled set to false")
	if disabled.GetRunning() {
		c.Say("  iron-proxy is still running — stop it with " + c.Paint("icebath egress stop", cli.Cyan) + " if you want it down too.")
	}
	return nil
}

func (e egress) config(cmd *cobra.Command, _ []string) error {
	c := cli.NewConsole(cmd)
	status, err := e.service.Status(cmd.Context(), &egressv1.StatusRequest{})
	if err != nil {
		return err
	}
	if status.GetConfig() == "" {
		c.Say("(no config generated — run `icebath egress setup`)", cli.Yellow)
		return cli.Exit(1)
	}
	c.Say(status.GetConfig())
	return nil
}
