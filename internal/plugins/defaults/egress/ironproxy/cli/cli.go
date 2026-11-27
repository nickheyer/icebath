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
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
)

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	e := egress{service: egressv1connect.NewEgressServiceClient(conn)}
	root := &cobra.Command{
		Use:     "egress",
		GroupID: cli.GroupSafety,
		Short:   "Keep your API keys out of Docker sandboxes",
		Long: "The sandbox proxy is iron-proxy, a program that runs on this computer. Docker sandboxes reach providers through it with " +
			"stand-in tokens, and the proxy puts your real API keys in on the way out. A model that a web page or a file tricks never sees " +
			"your real keys. Run setup once, then start.",
		Example: "  icebath egress setup\n  icebath egress start\n  icebath egress status",
		Args:    cobra.NoArgs,
	}
	root.AddCommand(e.install(), e.setup(),
		&cobra.Command{Use: "start", Short: "Start the sandbox proxy", Long: "Starts iron-proxy in the background with the settings that setup wrote.",
			Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return e.start(cmd) }},
		&cobra.Command{Use: "stop", Short: "Stop the sandbox proxy", Long: "Stops the iron-proxy that is running.", Args: cobra.NoArgs, RunE: e.stop},
		&cobra.Command{Use: "restart", Short: "Stop the sandbox proxy if it runs, then start it",
			Long: "Stops the running iron-proxy and starts it again. Use restart after you run setup again or change an API key, because the proxy " +
				"reads your real keys only when it starts.",
			Args: cobra.NoArgs, RunE: e.restart},
		&cobra.Command{Use: "reload", Short: "Apply edited proxy rules without a restart",
			Long: "Makes the running proxy read its rules from proxy.yaml again, without a restart and without dropping connections. New or " +
				"changed API keys still need a restart, because the proxy reads them only when it starts.",
			Args: cobra.NoArgs, RunE: e.reload},
		e.status(),
		&cobra.Command{Use: "disable", Short: "Turn the sandbox proxy off",
			Long: "Turns the sandbox proxy off in your settings. A proxy that is running keeps running until you stop it.", Args: cobra.NoArgs,
			RunE: e.disable},
		&cobra.Command{Use: "config", Short: "Print the path of proxy.yaml", Long: "Prints where the proxy.yaml that setup wrote is kept.",
			Args: cobra.NoArgs, RunE: e.config})
	return []*cobra.Command{root}
}

type egress struct {
	service egressv1connect.EgressServiceClient
}

func (e egress) install() *cobra.Command {
	force := false
	command := &cobra.Command{Use: "install", Short: "Install the pinned version of iron-proxy",
		Long: "Downloads the version of iron-proxy that icebath pins and keeps it with the other programs icebath downloads. With --force, " +
			"install checks the copy you have and repairs it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			installed, err := e.service.Install(cmd.Context(), &egressv1.InstallRequest{Force: force})
			if err != nil {
				c.Say(c.Paint("✗ iron-proxy could not be installed.", cli.Red) + " " + tui.Reason(err))
				c.Say("  You can install it yourself from https://github.com/ironsh/iron-proxy/releases.")
				return cli.Exit(1)
			}
			c.Say(c.Tick() + " Installed iron-proxy at " + installed.GetBinary() + ". " + version(installed.GetVersion()))
			return nil
		}}
	command.Flags().BoolVar(&force, "force", false, "Check the installed copy and repair it")
	return command
}

// Sentence saying which version of iron-proxy is installed
func version(text string) string {
	if text == "" {
		return "Its version is not known."
	}
	return "Its version is " + strings.TrimPrefix(text, "v") + "."
}

// On or Off for a switch, the way the settings say it
func onOff(c *cli.Console, on bool) string {
	if on {
		return c.Paint("On", cli.Green)
	}
	return c.Paint("Off", cli.Dim)
}

// Yes or No, in the case a value is said in
func yesNo(c *cli.Console, yes bool) string {
	if yes {
		return c.Paint("Yes", cli.Green)
	}
	return c.Paint("No", cli.Dim)
}

// Where the keys come from, the way the settings name it
func source(value string) string {
	if value == ironconfig.FromBitwarden {
		return "Bitwarden"
	}
	return "Environment"
}

// Whether the proxy is up, as the end of a sentence
func listens(c *cli.Console, listening bool) string {
	if listening {
		return c.Paint("is listening", cli.Green)
	}
	return c.Paint("is not listening yet", cli.Yellow)
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
	c.Table([]string{envHeader, hostsHeader, "Stand-in token"}, rows)
}

func (e egress) setup() *cobra.Command {
	var port int
	var fromBitwarden, noBitwarden, rotate, restart, noRestart bool
	command := &cobra.Command{Use: "setup", Short: "Set up the sandbox proxy step by step",
		Long: "Installs iron-proxy, makes its CA certificate, makes a stand-in token for each provider you use and writes proxy.yaml. Run setup " +
			"again after you add a provider. Tokens you have are kept unless you pass --rotate-tokens, so running sandboxes keep working.\n\n" +
			"A proxy that is running stops while setup writes the new settings. setup then asks whether to start it again, and " +
			"--restart or --no-restart answers for you. With --from-bitwarden, setup fails when Bitwarden cannot be reached instead of " +
			"falling back to the environment.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			ctx := cmd.Context()
			c.Panel("", c.Paint("Setting up the sandbox proxy", cli.Bold), "", "Docker sandboxes reach providers through iron-proxy, a proxy on this",
				"computer that puts in your real API keys on the way out, so an agent",
				"that a web page or a file tricks never sees them.", "",
				c.Paint("iron-proxy is at https://github.com/ironsh/iron-proxy, under the Apache 2.0 license.", cli.Dim))
			c.Step(1, "Install iron-proxy")
			status, err := e.service.Status(ctx, &egressv1.StatusRequest{})
			if err != nil {
				return err
			}
			binary, found := status.GetBinary(), status.GetVersion()
			if binary == "" {
				c.Say("  iron-proxy is not on your PATH, so icebath downloads it…")
				installed, err := e.service.Install(ctx, &egressv1.InstallRequest{})
				if err != nil {
					return c.Fail("  ✗ iron-proxy could not be installed. " + tui.Reason(err))
				}
				binary, found = installed.GetBinary(), installed.GetVersion()
			}
			c.Say("  " + c.Tick() + " iron-proxy is at " + binary + ". " + version(found))
			c.Step(2, "Make a CA certificate")
			authority, err := e.service.Authority(ctx, &egressv1.AuthorityRequest{})
			if err != nil {
				return c.Fail("  ✗ The CA certificate could not be made. " + tui.Reason(err))
			}
			c.Say("  " + c.Tick() + " The CA certificate is " + authority.GetCertificate() + ".")
			c.Step(3, "Make stand-in tokens for your providers")
			if rotate && len(status.GetMappings()) > 0 && c.Interactive() {
				c.Say(c.Paint("⚠", cli.Yellow) + " --rotate-tokens replaces every stand-in token, so providers refuse every running icebath sandbox until it restarts.")
				if answer, _ := c.Input("Replace the stand-in tokens of every sandbox? Type rotate to go ahead: "); strings.ToLower(strings.TrimSpace(answer)) != "rotate" {
					c.Say("Nothing was changed.", cli.Yellow)
					return cli.Exit(1)
				}
			} else if rotate && len(status.GetMappings()) == 0 {
				c.Say("--rotate-tokens changes nothing on a first setup, because there are no tokens to replace yet.", cli.Dim)
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
				c.Say("  Read the names of " + strs.Count(int(n), "secret") + " from Bitwarden.")
			}
			if n := minted.GetLoaded(); n > 0 {
				c.Say("  Found "+strs.Count(int(n), "provider key")+" in the keys file of this profile.", cli.Dim)
			}
			if minted.GetBackup() != "" {
				c.Say("  Saved the old tokens to "+minted.GetBackup()+".", cli.Dim)
			}
			if minted.GetBackupError() != "" {
				c.Say("  ⚠ The old tokens could not be saved before they were replaced: "+minted.GetBackupError(), cli.Yellow)
			}
			if len(minted.GetMappings()) == 0 {
				c.Say("  ⚠ No API key of a provider the proxy knows was found in the environment or in Bitwarden.", cli.Yellow)
				c.Say("  Set at least one of these, then run setup again:")
				for _, name := range ironterminal.BearerNames() {
					c.Say("    - " + name)
				}
				return cli.Exit(1)
			}
			if uncovered := minted.GetUncovered(); len(uncovered) > 0 {
				c.Blank()
				c.Say("  " + c.Paint("⚠", cli.Yellow) + " The proxy does not cover these provider variables yet:")
				for _, name := range uncovered {
					c.Say("    - " + name)
				}
				c.Say("  These providers sign their requests or make their own sign-in tokens, such as with SigV4 or service account files, so their "+
					"real keys stay inside the sandbox. The proxy does not protect them.", cli.Dim)
			}
			table(c, minted.GetMappings(), "Variable", "Sites", false)
			c.Step(4, "Write proxy.yaml and save the tokens")
			written, err := e.service.Write(ctx, &egressv1.WriteRequest{Mappings: minted.GetMappings(), TunnelPort: int32(port), FromBitwarden: fromBitwarden,
				NoBitwarden: noBitwarden})
			if err != nil {
				return err
			}
			if written.GetBadPort() {
				return c.Fail("  ✗ --tunnel-port must be from 1 to 65,534, because the proxy also listens for plain HTTP on the port after it.")
			}
			if written.GetAuditError() != "" {
				c.Say("  ⚠ "+written.GetAuditError(), cli.Yellow)
			}
			c.Say("  " + c.Tick() + " Settings:  " + written.GetConfig())
			c.Say("  " + c.Tick() + " Tokens:    " + written.GetMappings())
			if written.GetAudit() != "" {
				c.Say("  " + c.Tick() + " Audit log: " + written.GetAudit())
				c.Say("    iron-proxy 0.39 does not write the audit log yet. Each request is recorded in iron-proxy.log.", cli.Dim)
			}
			for _, note := range written.GetNotes() {
				c.Say(note, cli.Yellow)
			}
			if err := e.restartAfterSetup(c, cmd, restart, noRestart); err != nil {
				return err
			}
			c.Blank()
			c.Say(c.Paint("✓ The sandbox proxy is set up.", cli.Green) + " Docker sandboxes send their traffic through it.")
			for _, name := range []string{"start", "restart", "reload", "status", "stop", "disable"} {
				if sub, _, err := cmd.Parent().Find([]string{name}); err == nil && sub.Name() == name {
					c.Say("  " + c.Paint(cli.Pad("icebath egress "+name, 24), cli.Cyan) + sub.Short)
				}
			}
			return nil
		}}
	command.Flags().IntVar(&port, "tunnel-port", 0, fmt.Sprintf("Port that sandboxes reach the proxy on, instead of %d", ironconfig.DefaultTunnelPort))
	command.Flags().BoolVar(&fromBitwarden, "from-bitwarden", false, "Find provider keys in your Bitwarden store instead of the environment")
	command.Flags().BoolVar(&noBitwarden, "no-bitwarden", false, "Read provider keys from the environment again after --from-bitwarden")
	command.Flags().BoolVar(&rotate, "rotate-tokens", false, "Make new tokens for every provider, so running sandboxes must restart")
	command.Flags().BoolVar(&restart, "restart", false, "Start a running proxy again after setup, without asking")
	command.Flags().BoolVar(&noRestart, "no-restart", false, "Leave a running proxy stopped after setup, without asking")
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
		again = !c.Interactive() || c.Confirm("  Start the sandbox proxy again with the new settings? It stopped while setup wrote them.", true)
	}
	if !again {
		if running {
			c.Say("  ⚠ The sandbox proxy was stopped, because its settings or tokens changed. Run "+c.Paint("icebath egress start", cli.Cyan)+
				" before you start new Docker sandboxes.", cli.Yellow)
		}
		return nil
	}
	started, err := e.service.Start(ctx, &egressv1.StartRequest{Setup: true})
	if err != nil {
		c.Say("  ⚠ The sandbox proxy could not start with the new settings. "+tui.Reason(err), cli.Yellow)
		c.Say("  Run " + c.Paint("icebath egress start", cli.Cyan) + " yourself before you start new Docker sandboxes.")
		return nil
	}
	repeat := ""
	if running {
		repeat = " again"
	}
	c.Say(fmt.Sprintf("  %s Started the sandbox proxy%s with the new settings. It runs as process %d on port %d and %s.", c.Tick(), repeat,
		started.GetPid(), started.GetPort(), listens(c, started.GetListening())))
	return nil
}

func (e egress) start(cmd *cobra.Command) error {
	c := cli.NewConsole(cmd)
	started, err := e.service.Start(cmd.Context(), &egressv1.StartRequest{})
	if err != nil {
		return c.Fail("✗ The sandbox proxy could not start. " + tui.Reason(err))
	}
	for _, note := range started.GetNotes() {
		c.Say(note, cli.Yellow)
	}
	switch {
	case started.GetRefusal() != "" && started.GetHint() == "":
		c.Say("✗ "+started.GetRefusal(), cli.Red)
		return cli.Exit(1)
	case started.GetRefusal() != "":
		c.Say("✗ The sandbox proxy did not start. "+started.GetRefusal(), cli.Red)
		c.Say("  " + started.GetHint())
		return cli.Exit(1)
	}
	c.Say(fmt.Sprintf("%s The sandbox proxy runs as process %d on port %d and %s.", c.Tick(), started.GetPid(), started.GetPort(),
		listens(c, started.GetListening())))
	return nil
}

func (e egress) stop(cmd *cobra.Command, _ []string) error {
	c := cli.NewConsole(cmd)
	stopped, err := e.service.Stop(cmd.Context(), &egressv1.StopRequest{})
	if err != nil {
		return err
	}
	if stopped.GetStopped() {
		c.Say(c.Tick() + " Stopped the sandbox proxy.")
	} else {
		c.Say("The sandbox proxy was not running.", cli.Dim)
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
		c.Say("Stopped the sandbox proxy.", cli.Dim)
	}
	return e.start(cmd)
}

func (e egress) reload(cmd *cobra.Command, _ []string) error {
	c := cli.NewConsole(cmd)
	if _, err := e.service.Reload(cmd.Context(), &egressv1.ReloadRequest{}); err != nil {
		return c.Fail("✗ The rules could not be reloaded. " + tui.Reason(err))
	}
	c.Say(c.Tick() + " The sandbox proxy read its rules again, without a restart and without dropping connections.")
	c.Say("New or changed API keys still need `icebath egress restart`, because the proxy reads them only when it starts.", cli.Dim)
	return nil
}

func (e egress) status() *cobra.Command {
	show := false
	command := &cobra.Command{Use: "status", Short: "Show the state of the sandbox proxy and its tokens",
		Long: "Shows whether iron-proxy is installed and running, where its files are, and which environment variable each stand-in token " +
			"replaces. Tokens are shortened unless you pass --show-tokens.",
		Args: cobra.NoArgs,
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
			process := c.Paint("Not running", cli.Dim)
			if status.GetPid() != 0 {
				process = fmt.Sprintf("Running as process %d", status.GetPid())
			}
			c.Pairs("Sandbox proxy", [][2]string{
				{"Hide your keys", onOff(c, status.GetEnabled())},
				{"Program", or(status.GetBinary(), "Not installed")},
				{"Version", or(strings.TrimPrefix(status.GetVersion(), "v"), "Not known")},
				{"Settings file", or(status.GetConfig(), "Not written yet")},
				{"CA certificate", or(status.GetAuthority(), "Not made yet")},
				{"Port", fmt.Sprint(status.GetPort())},
				{"Process", process},
				{"Listening", yesNo(c, status.GetListening())},
				{"Key source", source(status.GetCredentialSource())},
				{"Required for Docker", onOff(c, status.GetEnforce())},
			})
			if mappings := status.GetMappings(); len(mappings) > 0 {
				c.Blank()
				c.Say("Stand-in tokens", cli.Bold)
				table(c, mappings, "Variable", "Sites", show)
				if show {
					c.Say(c.Paint("⚠", cli.Yellow) + " The stand-in tokens are printed in full above, and your shell history may keep them. Clear it after this command.")
				}
			}
			if uncovered := status.GetUncovered(); len(uncovered) > 0 {
				c.Blank()
				c.Say(c.Paint("Providers the proxy does not cover", cli.Yellow) + ". Their real keys are still visible inside the sandbox:")
				for _, name := range uncovered {
					c.Say("  - " + name)
				}
			}
			return nil
		}}
	command.Flags().BoolVar(&show, "show-tokens", false, "Print the tokens in full, where your shell history may keep them")
	return command
}

func (e egress) disable(cmd *cobra.Command, _ []string) error {
	c := cli.NewConsole(cmd)
	disabled, err := e.service.Disable(cmd.Context(), &egressv1.DisableRequest{})
	if err != nil {
		return err
	}
	if !disabled.GetWasEnabled() {
		c.Say("The sandbox proxy was already off.", cli.Dim)
		return nil
	}
	c.Say(c.Tick() + " Turned off Hide your keys in /settings under Sandbox proxy.")
	if disabled.GetRunning() {
		c.Say("  iron-proxy still runs. Stop it with " + c.Paint("icebath egress stop", cli.Cyan) + " if you want it stopped too.")
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
		c.Say("✗ There is no proxy.yaml yet. Run `icebath egress setup` to write it.", cli.Red)
		return cli.Exit(1)
	}
	c.Say(status.GetConfig())
	return nil
}
