// The egress commands: install, set up, start, stop, restart, reload, see, turn off the iron-proxy egress firewall, and where its config is
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	ironconfig "github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/config"
	picked "github.com/nickheyer/icebath/internal/profile"
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
	root := &cobra.Command{
		Use:     "egress",
		GroupID: cli.GroupSafety,
		Short:   "Keep your API keys out of Docker sandboxes",
		Long: "The key proxy is iron-proxy, a program that runs on this computer. Docker sandboxes reach providers through it with " +
			"stand-in tokens, and the proxy puts your real API keys in on the way out. A model that a web page or a file tricks never sees " +
			"your real keys. setup sets it up and starts it, the same as turning on Hide your keys in Key proxy, in /sandbox.",
		Example: "  icebath egress setup\n  icebath egress status\n  icebath egress disable",
		Args:    cobra.NoArgs,
	}
	root.AddCommand(e.install(), e.setup(),
		&cobra.Command{Use: "start", Short: "Start the key proxy", Long: "Starts iron-proxy in the background with the settings that setup wrote.",
			Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return e.start(cmd) }},
		&cobra.Command{Use: "stop", Short: "Stop the key proxy", Long: "Stops the iron-proxy that is running.", Args: cobra.NoArgs, RunE: e.stop},
		&cobra.Command{Use: "restart", Short: "Stop the key proxy if it runs, then start it",
			Long: "Stops the running iron-proxy and starts it again. Use restart after you run setup again or change an API key, because the proxy " +
				"reads your real keys only when it starts.",
			Args: cobra.NoArgs, RunE: e.restart},
		&cobra.Command{Use: "reload", Short: "Apply edited proxy rules without a restart",
			Long: "Makes the running proxy read its rules from proxy.yaml again, without a restart and without dropping connections. New or " +
				"changed API keys still need a restart, because the proxy reads them only when it starts.",
			Args: cobra.NoArgs, RunE: e.reload},
		e.status(),
		&cobra.Command{Use: "disable", Short: "Turn the key proxy off and stop it",
			Long: "Turns off Hide your keys and stops the proxy, as turning it off in Key proxy, in /sandbox, does.", Args: cobra.NoArgs,
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
			installed, err := e.service.Install(cmd.Context(), &egressv1.InstallRequest{Profile: picked.Picked(cmd.Context()), Force: force})
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
	command := &cobra.Command{Use: "setup", Short: "Set up the key proxy and turn it on",
		Long: "Finds the API keys of the providers you use, installs iron-proxy, makes its CA certificate and a stand-in token for each provider, " +
			"writes proxy.yaml, then starts the proxy. Run setup again after you add a provider. Tokens you have are kept unless you pass " +
			"--rotate-tokens, so running sandboxes keep working.\n\nA proxy that is running stops while setup writes the new settings, and starts " +
			"again after, unless you pass --no-restart. With --from-bitwarden, setup fails when Bitwarden cannot be reached instead of falling back " +
			"to the environment. Turning on Hide your keys in Key proxy, in /sandbox, does the same.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			ctx := cmd.Context()
			if restart && noRestart {
				return c.Fail("--restart and --no-restart can't be given together.")
			}
			c.Panel("", c.Paint("Setting up the key proxy", cli.Bold), "", "Docker sandboxes reach providers through iron-proxy, a proxy on this",
				"computer that puts in your real API keys on the way out, so an agent",
				"that a web page or a file tricks never sees them.", "",
				c.Paint("iron-proxy is at https://github.com/ironsh/iron-proxy, under the Apache 2.0 license.", cli.Dim))
			if rotate && c.Interactive() {
				status, err := e.service.Status(ctx, &egressv1.StatusRequest{Profile: picked.Picked(ctx)})
				if err != nil {
					return err
				}
				if len(status.GetMappings()) > 0 {
					c.Say(c.Paint("⚠", cli.Yellow) + " --rotate-tokens replaces every stand-in token, so providers refuse every running icebath sandbox until it restarts.")
					if answer, _ := c.Input("Replace the stand-in tokens of every sandbox? Type rotate to go ahead: "); strings.ToLower(strings.TrimSpace(answer)) != "rotate" {
						c.Say("Nothing was changed.", cli.Yellow)
						return cli.Exit(1)
					}
				}
			}
			ok, err := e.turn(ctx, c, &egressv1.TurnRequest{Profile: picked.Picked(ctx), On: true, Again: true, TunnelPort: int32(port),
				FromBitwarden: fromBitwarden, NoBitwarden: noBitwarden, Rotate: rotate, Stopped: noRestart})
			if err != nil || !ok {
				return cmpErr(err)
			}
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
	command.Flags().BoolVar(&restart, "restart", false, "Start the proxy after setup, as setup does unless --no-restart is given")
	command.Flags().BoolVar(&noRestart, "no-restart", false, "Leave the proxy stopped after setup")
	return command
}

// Exit status of a turn that failed: its error, else a plain failure once the reason was said
func cmpErr(err error) error {
	if err != nil {
		return err
	}
	return cli.Exit(1)
}

// Runs a turn of hiding the keys, printing each step as the server tells it and how it ended; whether it did what was asked
func (e egress) turn(ctx context.Context, c *cli.Console, request *egressv1.TurnRequest) (bool, error) {
	stream, err := e.service.Turn(ctx, request)
	if err != nil {
		return false, err
	}
	defer stream.Close()
	step := 0
	for {
		event, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return false, errors.New("The key proxy stopped answering before it said whether it worked.")
		}
		if err != nil {
			return false, err
		}
		switch kind := event.GetKind().(type) {
		case *egressv1.TurnEvent_Line:
			line := kind.Line
			switch line.GetTone() {
			case egressv1.Tone_TONE_STEP:
				step++
				c.Step(step, line.GetText())
			case egressv1.Tone_TONE_GOOD:
				c.Say("  " + c.Tick() + " " + line.GetText())
			case egressv1.Tone_TONE_WARN:
				c.Say("  ⚠ "+line.GetText(), cli.Yellow)
			case egressv1.Tone_TONE_BAD:
				c.Say("  ✗ "+line.GetText(), cli.Red)
			case egressv1.Tone_TONE_DIM:
				c.Say("  "+line.GetText(), cli.Dim)
			default:
				c.Say("  " + line.GetText())
			}
		case *egressv1.TurnEvent_Minted:
			if uncovered := kind.Minted.GetUncovered(); len(uncovered) > 0 {
				c.Say("  " + c.Paint("⚠", cli.Yellow) + " The proxy does not cover these provider variables yet:")
				for _, name := range uncovered {
					c.Say("    - " + name)
				}
				c.Say("  These providers sign their requests or make their own sign-in tokens, such as with SigV4 or service account files, so their "+
					"real keys stay inside the sandbox. The proxy does not protect them.", cli.Dim)
			}
			table(c, kind.Minted.GetMappings(), "Variable", "Sites", false)
		case *egressv1.TurnEvent_Done:
			done := kind.Done
			c.Blank()
			if !done.GetOk() {
				c.Say("✗ "+done.GetNote(), cli.Red)
				return false, nil
			}
			c.Say(c.Paint("✓ ", cli.Green) + done.GetNote())
			return true, nil
		}
	}
}

func (e egress) start(cmd *cobra.Command) error {
	c := cli.NewConsole(cmd)
	started, err := e.service.Start(cmd.Context(), &egressv1.StartRequest{Profile: picked.Picked(cmd.Context())})
	if err != nil {
		return c.Fail("✗ The key proxy could not start. " + tui.Reason(err))
	}
	for _, note := range started.GetNotes() {
		c.Say(note, cli.Yellow)
	}
	switch {
	case started.GetRefusal() != "" && started.GetHint() == "":
		c.Say("✗ "+started.GetRefusal(), cli.Red)
		return cli.Exit(1)
	case started.GetRefusal() != "":
		c.Say("✗ The key proxy did not start. "+started.GetRefusal(), cli.Red)
		c.Say("  " + started.GetHint())
		return cli.Exit(1)
	}
	c.Say(fmt.Sprintf("%s The key proxy runs as process %d on port %d and %s.", c.Tick(), started.GetPid(), started.GetPort(),
		listens(c, started.GetListening())))
	return nil
}

func (e egress) stop(cmd *cobra.Command, _ []string) error {
	c := cli.NewConsole(cmd)
	stopped, err := e.service.Stop(cmd.Context(), &egressv1.StopRequest{Profile: picked.Picked(cmd.Context())})
	if err != nil {
		return err
	}
	if stopped.GetStopped() {
		c.Say(c.Tick() + " Stopped the key proxy.")
	} else {
		c.Say("The key proxy was not running.", cli.Dim)
	}
	return nil
}

// Stops the daemon when one runs, then starts it through every check of the key source
func (e egress) restart(cmd *cobra.Command, _ []string) error {
	c := cli.NewConsole(cmd)
	stopped, err := e.service.Stop(cmd.Context(), &egressv1.StopRequest{Profile: picked.Picked(cmd.Context())})
	if err != nil {
		return err
	}
	if stopped.GetStopped() {
		c.Say("Stopped the key proxy.", cli.Dim)
	}
	return e.start(cmd)
}

func (e egress) reload(cmd *cobra.Command, _ []string) error {
	c := cli.NewConsole(cmd)
	if _, err := e.service.Reload(cmd.Context(), &egressv1.ReloadRequest{Profile: picked.Picked(cmd.Context())}); err != nil {
		return c.Fail("✗ The rules could not be reloaded. " + tui.Reason(err))
	}
	c.Say(c.Tick() + " The key proxy read its rules again, without a restart and without dropping connections.")
	c.Say("New or changed API keys still need `icebath egress restart`, because the proxy reads them only when it starts.", cli.Dim)
	return nil
}

func (e egress) status() *cobra.Command {
	show := false
	command := &cobra.Command{Use: "status", Short: "Show the state of the key proxy and its tokens",
		Long: "Shows whether iron-proxy is installed and running, where its files are, and which environment variable each stand-in token " +
			"replaces. Tokens are shortened unless you pass --show-tokens.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			status, err := e.service.Status(cmd.Context(), &egressv1.StatusRequest{Profile: picked.Picked(cmd.Context())})
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
			c.Pairs("Key proxy", [][2]string{
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
	ok, err := e.turn(cmd.Context(), cli.NewConsole(cmd), &egressv1.TurnRequest{Profile: picked.Picked(cmd.Context())})
	if err != nil || !ok {
		return cmpErr(err)
	}
	return nil
}

func (e egress) config(cmd *cobra.Command, _ []string) error {
	c := cli.NewConsole(cmd)
	status, err := e.service.Status(cmd.Context(), &egressv1.StatusRequest{Profile: picked.Picked(cmd.Context())})
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
