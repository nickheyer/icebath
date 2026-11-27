// The secrets bitwarden commands: set the store up, check it, rotate its token, pull now, turn it off, install bws
package cli

import (
	"cmp"
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	bitwardenconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/config"
	"github.com/nickheyer/icebath/internal/secrets"
	bitwardenv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/bitwarden/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/bitwarden/v1/bitwardenv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
)

const (
	oddToken = "The token does not start with “0.”, so it may not be a Bitwarden Secrets Manager access token."
	usCloud  = "https://vault.bitwarden.com"
)

// Bitwarden regions the region prompt offers by name, address shown and address kept, a new one here appearing in it
var regions = [][3]string{
	{"US cloud, the default", usCloud, ""},
	{"EU cloud", "https://vault.bitwarden.eu", "https://vault.bitwarden.eu"},
}

// Follow up of each cause of a failed project list
var hints = map[string]string{
	"region": "  This usually means the token is for another Bitwarden region. Run icebath secrets bitwarden setup and pick your region, or set " +
		"Server address in /settings under Secret stores › Bitwarden.",
	"token": "  This usually means the access token is wrong or was revoked. Check it in the Bitwarden web app.",
}

// Says where bws is and which version it is
func installed(c *cli.Console, binary, version string) {
	if version == "" || version == "version unknown" {
		c.Success("bws is installed at " + binary + ".")
		return
	}
	c.Success(version + " is installed at " + binary + ".")
}

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	b := bitwarden{service: bitwardenv1connect.NewBitwardenServiceClient(conn)}
	root := &cobra.Command{Use: bitwardenconfig.Name, Aliases: []string{"bw"}, Short: "Load API keys from Bitwarden Secrets Manager",
		Long: "Loads the secrets of one Bitwarden Secrets Manager project as environment variables each time icebath starts. icebath reads them with bws, the " +
			"Bitwarden Secrets Manager command line tool, with the access token of a machine account.",
		Args: cobra.NoArgs}
	root.AddCommand(b.setup(), b.status(), b.token(), b.sync(), b.disable(), b.install())
	return []*cobra.Command{root}
}

type bitwarden struct {
	service bitwardenv1connect.BitwardenServiceClient
}

func (b bitwarden) setup() *cobra.Command {
	var project, token, server string
	command := &cobra.Command{Use: "setup", Short: "Set up Bitwarden step by step",
		Long: "Installs bws, keeps your access token in the keys file, asks for your region and lets you pick a project. Then setup loads the " +
			"project's secrets once to check that everything works. You make an access token in the Bitwarden web app under Secrets Manager, " +
			"Machine accounts, Access tokens.\n\n" +
			"--server-url answers the question about your region: https://vault.bitwarden.com is the US cloud and the default, " +
			"https://vault.bitwarden.eu is the EU cloud, or give the address of your own server.",
		Example: "  icebath secrets bitwarden setup\n  icebath secrets bitwarden setup --server-url https://vault.bitwarden.eu",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			ctx := cmd.Context()
			c.Panel("", c.Paint("Set up Bitwarden Secrets Manager", cli.Bold), "", "To make an access token, open the Bitwarden web app and go to",
				"Secrets Manager › Machine accounts › your account › Access tokens,", "then choose Create access token.", "",
				"Copy the token now. It starts with “"+c.Paint("0.", cli.Cyan)+"”, and Bitwarden shows it only once.")
			c.Step(1, "Install bws")
			state, err := b.service.State(ctx, &bitwardenv1.StateRequest{})
			if err != nil {
				return err
			}
			binary, version := state.GetBinary(), state.GetVersion()
			if binary == "" {
				c.Info("bws is not installed, so icebath downloads it now…")
				done, err := b.service.Install(ctx, &bitwardenv1.InstallRequest{})
				if err != nil {
					c.Failure(secrets.Sentence(tui.Reason(err)))
					c.Info("To install bws yourself, download it from https://github.com/bitwarden/sdk-sm/releases.")
					return cli.Exit(1)
				}
				binary, version = done.GetBinary(), done.GetVersion()
			}
			installed(c, binary, version)
			if !c.Interactive() {
				var missing []string
				for _, given := range [][2]string{{"--access-token", token}, {"--server-url", cmp.Or(strings.TrimSpace(server), os.Getenv(bitwardenconfig.ServerURLEnv))},
					{"--project-id", project}} {
					if strings.TrimSpace(given[1]) == "" {
						missing = append(missing, given[0])
					}
				}
				if len(missing) > 0 {
					c.Failure("Without a terminal to answer questions in, setup needs every flag. Add " + strings.Join(missing, ", ") + ".")
					c.Say("\n  For example:\n    icebath secrets bitwarden setup \\\n      --access-token '0.xxx' \\\n" +
						"      --server-url 'https://vault.bitwarden.com' \\\n      --project-id 'xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx'")
					return cli.Exit(1)
				}
			}
			c.Step(2, "Add your access token")
			token = strings.TrimSpace(token)
			if token == "" {
				typed, ok := c.Secret(fmt.Sprintf("  Paste your access token, which icebath keeps as %s: ", state.GetTokenEnv()))
				if !ok {
					return cli.Exit(1)
				}
				token = strings.TrimSpace(typed)
			}
			if token == "" {
				return c.Fail("  ✗ No token was pasted, so setup stopped.")
			}
			if !strings.HasPrefix(token, "0.") {
				c.Warning(oddToken + " Setup goes on with it.")
			}
			saved, err := b.service.SaveToken(ctx, &bitwardenv1.SaveTokenRequest{Token: token})
			if err != nil {
				return err
			}
			c.Success("The token is kept in " + saved.GetEnvPath() + " as " + saved.GetTokenEnv() + ".")
			c.Step(3, "Pick your Bitwarden region")
			url, chosen := b.region(c, strings.TrimSpace(server), state.GetServerUrl())
			if !chosen {
				return cli.Exit(1)
			}
			if url != "" {
				c.Success("icebath uses the Bitwarden server at " + url + ".")
			} else {
				c.Success("icebath uses the US cloud, " + usCloud + ", which is the default.")
			}
			project = strings.TrimSpace(project)
			test := 4
			if project == "" {
				test = 5
				c.Step(4, "Pick a project")
				if project, err = b.project(c, cmd, token, url); err != nil || project == "" {
					return cli.Exit(1)
				}
			}
			c.Step(test, "Load the secrets once")
			preview, err := b.service.Setup(ctx, &bitwardenv1.SetupRequest{Token: token, ServerUrl: url, ProjectId: project})
			if err != nil {
				return c.Fail("  ✗ The secrets could not be loaded. " + secrets.Sentence(tui.Reason(err)))
			}
			if len(preview.GetRows()) == 0 {
				c.Warning("Bitwarden answered, but the project has no secrets.")
			} else {
				var rows [][]string
				for _, row := range preview.GetRows() {
					rows = append(rows, []string{c.Paint(row.GetName(), cli.Cyan), status(c, row.GetAction())})
				}
				c.Table([]string{"Variable", "What happens"}, rows)
			}
			for _, warning := range preview.GetWarnings() {
				c.Warning(secrets.Sentence(warning))
			}
			c.Blank()
			c.Say(c.Paint("✓ Bitwarden Secrets Manager is on.", cli.Green) + " icebath loaded its secrets now and loads them each time it starts.")
			c.Say("  See how it is set up:  " + c.Paint("icebath secrets bitwarden status", cli.Cyan) + "\n  Load the secrets again: " +
				c.Paint("icebath secrets bitwarden sync", cli.Cyan) + "\n  Turn it off:            " + c.Paint("icebath secrets bitwarden disable", cli.Cyan))
			return nil
		}}
	command.Flags().StringVar(&project, "project-id", "", "Project to use, by id, instead of picking one")
	command.Flags().StringVar(&token, "access-token", "", "Access token to keep in the keys file, instead of typing it")
	command.Flags().StringVar(&server, "server-url", "", "Address of the Bitwarden server, instead of asking for your region")
	return command
}

// What setup's test fetch says of one secret
func status(c *cli.Console, action string) string {
	switch action {
	case "skip_token":
		return c.Paint("Left alone, because it holds the access token", cli.Dim)
	case "override":
		return c.Paint("Set already, and replaced by the value from Bitwarden", cli.Yellow)
	case "skip_set":
		return c.Paint("Set already, and kept", cli.Dim)
	}
	return c.Paint("Set from Bitwarden", cli.Green)
}

// Server address from the flag, BWS_SERVER_URL, the settings or the region menu, false for an empty custom address or an ended input
func (b bitwarden) region(c *cli.Console, given, existing string) (string, bool) {
	if given != "" {
		return given, true
	}
	if env := strings.TrimSpace(os.Getenv(bitwardenconfig.ServerURLEnv)); env != "" {
		c.Info("Your shell sets " + c.Paint(bitwardenconfig.ServerURLEnv, cli.Cyan) + " to " + env + ", so icebath uses that address.")
		return env, true
	}
	if existing != "" {
		c.Info("The address in use now is " + c.Paint(existing, cli.Cyan) + ". Press enter to keep it, or pick another below.")
	}
	var rows [][]string
	for i, region := range regions {
		rows = append(rows, []string{c.Paint(strs.Number(float64(i+1)), cli.Cyan), region[0], region[1]})
	}
	custom := len(regions) + 1
	rows = append(rows, []string{c.Paint(strs.Number(float64(custom)), cli.Cyan), "Your own server", "The address you type next"})
	c.Table([]string{"Number", "Region", "Address"}, rows)
	prompt := "  Pick a region from 1 to " + strs.Number(float64(custom))
	if existing != "" {
		prompt += ", or press enter to keep the one in use"
	}
	picked, ok := c.Index(prompt+": ", custom, existing != "", "  Enter a number.")
	switch {
	case !ok:
		return "", false
	case picked == 0:
		return existing, true
	case picked <= len(regions):
		return regions[picked-1][2], true
	}
	url, ok := c.Input("  Address of your Bitwarden server, such as https://vault.example.com: ")
	url = strings.TrimSpace(url)
	if !ok || url == "" {
		c.Failure("No address was typed, so setup stopped.")
		return "", false
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		c.Warning("The address does not start with http:// or https://, so bws may refuse it.")
	}
	return url, true
}

// Says why a project list failed and what that usually means
func refused(c *cli.Console, failure *bitwardenv1.Failure) {
	c.Failure(secrets.Sentence(failure.GetText()))
	if hint := hints[failure.GetCause()]; hint != "" {
		c.Say(hint, cli.Yellow)
	}
}

// Project picked from those the token sees, empty after saying why there is none to pick
func (b bitwarden) project(c *cli.Console, cmd *cobra.Command, token, server string) (string, error) {
	listed, err := b.service.Projects(cmd.Context(), &bitwardenv1.ProjectsRequest{Token: token, ServerUrl: server})
	if err != nil {
		c.Failure(secrets.Sentence(tui.Reason(err)))
		return "", nil
	}
	if listed.GetFailure() != nil {
		refused(c, listed.GetFailure())
		return "", nil
	}
	projects := listed.GetProjects()
	if len(projects) == 0 {
		c.Warning("This machine account can't see any projects.")
		c.Info("In the Bitwarden web app, open the machine account, then its Projects tab, and give it access to a project.")
		return "", nil
	}
	var rows [][]string
	for i, p := range projects {
		rows = append(rows, []string{c.Paint(strs.Number(float64(i+1)), cli.Cyan), p.GetName(), c.Paint(p.GetId(), cli.Dim)})
	}
	c.Table([]string{"Number", "Project", "ID"}, rows)
	picked, ok := c.Index("  Pick a project from 1 to "+strs.Number(float64(len(projects)))+": ", len(projects), false, "")
	if !ok {
		return "", nil
	}
	return projects[picked-1].GetId(), nil
}

func (b bitwarden) status() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show the Bitwarden settings and check the token",
		Long: "Shows the Bitwarden settings, whether bws is installed and whether the access token works.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			state, err := b.service.State(cmd.Context(), &bitwardenv1.StateRequest{})
			if err != nil {
				return err
			}
			check, err := b.service.Validate(cmd.Context(), &bitwardenv1.ValidateRequest{})
			if err != nil {
				return err
			}
			validation := c.Paint("Not checked. "+check.GetSkipped(), cli.Dim)
			if check.GetSkipped() == "" {
				validation = c.Paint("Works", cli.Green)
				if !check.GetPassed() {
					validation = c.Paint("Refused", cli.Red)
				}
			}
			project := state.GetProjectId()
			if project == "" {
				project = c.Paint("Not set", cli.Dim)
			}
			server := state.GetServerUrl()
			if server == "" {
				server = c.Paint("The US cloud, "+usCloud, cli.Dim)
			}
			binary := c.Paint("Not installed", cli.Yellow)
			if state.GetBinary() != "" {
				binary = state.GetBinary() + ", " + state.GetVersion()
			}
			reuse := "Not reused"
			if ttl := state.GetCacheTtl(); ttl > 0 {
				reuse = strs.Span(time.Duration(ttl) * time.Second)
			}
			c.Pairs("Bitwarden Secrets Manager", [][2]string{
				{"Load from Bitwarden", c.OnOff(state.GetEnabled())},
				{"Token variable", state.GetTokenEnv()},
				{"Token set", c.YesNo(state.GetTokenSet())},
				{"Token check", validation},
				{"Project", project},
				{"Server address", server},
				{"Replace your values", c.OnOff(state.GetOverride())},
				{"Reuse values for", reuse},
				{"Install bws", c.OnOff(state.GetAutoInstall())},
				{"bws program", binary},
			})
			if check.GetOddToken() {
				c.Warning(oddToken)
			}
			if check.GetFailure() != nil {
				refused(c, check.GetFailure())
			}
			if !state.GetEnabled() {
				c.Say("\n  Run " + c.Paint("icebath secrets bitwarden setup", cli.Cyan) + " to turn Bitwarden on.")
				return nil
			}
			if !state.GetTokenSet() {
				c.Blank()
				c.Warning("Bitwarden is on, but " + state.GetTokenEnv() + " is not set, so icebath skips Bitwarden and warns you each time it starts.")
			}
			if state.GetProjectId() == "" {
				c.Blank()
				c.Warning("Bitwarden is on, but no project is chosen, so there is nothing to load.")
			}
			return nil
		}}
}

func (b bitwarden) token() *cobra.Command {
	var token string
	noVerify := false
	command := &cobra.Command{Use: "token", Short: "Replace the access token",
		Long: "Asks for a new access token, checks it with Bitwarden and keeps it in the keys file. With --no-verify, token keeps it without " +
			"the check.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			ctx := cmd.Context()
			state, err := b.service.State(ctx, &bitwardenv1.StateRequest{})
			if err != nil {
				return err
			}
			token = strings.TrimSpace(token)
			if token == "" {
				if !c.Interactive() {
					return c.Fail("✗ There is no terminal to type the token in. Pass it with --access-token.")
				}
				c.Say("Make a new token in the Bitwarden web app under Secrets Manager › Machine accounts › your account › Access tokens, with " +
					"Create access token.\n")
				typed, ok := c.Secret(fmt.Sprintf("Paste the new access token, which icebath keeps as %s: ", state.GetTokenEnv()))
				if !ok {
					return cli.Exit(1)
				}
				token = strings.TrimSpace(typed)
			}
			if token == "" {
				return c.Fail("✗ No token was pasted, so nothing changed.")
			}
			if !strings.HasPrefix(token, "0.") {
				c.Warning(oddToken)
			}
			if !noVerify {
				c.Say("Checking the token with Bitwarden…")
			}
			rotated, err := b.service.Token(ctx, &bitwardenv1.TokenRequest{Token: token, Verify: !noVerify})
			if err != nil {
				return err
			}
			switch {
			case rotated.GetBinaryMissing():
				return c.Fail("✗ bws is not installed, so the token can't be checked. Run the command again with --no-verify to keep it unchecked.")
			case rotated.GetFailure() != nil:
				refused(c, rotated.GetFailure())
				return c.Fail("✗ Bitwarden refused the new token, so nothing changed.")
			}
			if !noVerify {
				c.Success("Bitwarden accepted the token, which can see " + strs.Count(int(rotated.GetProjects()), "project") + ".")
				if hidden := rotated.GetHiddenProject(); hidden != "" {
					c.Warning("The machine account can't see the project " + hidden + ". Give it access in the Bitwarden web app, or run icebath " +
						"secrets bitwarden setup to pick another project.")
				}
			}
			c.Success("The token is kept in " + rotated.GetEnvPath() + " as " + rotated.GetTokenEnv() + ", and icebath uses it from now on.")
			if rotated.GetDisabled() {
				c.Warning("Bitwarden is off. To turn it on, run icebath secrets bitwarden setup, or turn on Load from Bitwarden in /settings under " +
					"Secret stores › Bitwarden.")
			}
			return nil
		}}
	command.Flags().StringVar(&token, "access-token", "", "New access token, instead of typing it")
	command.Flags().BoolVar(&noVerify, "no-verify", false, "Keep the token without checking it with Bitwarden first")
	return command
}

func (b bitwarden) sync() *cobra.Command {
	apply := false
	command := &cobra.Command{Use: "sync", Short: "Load the secrets now and show what would change",
		Long: "Loads the project's secrets now and shows what each one would do. Nothing changes unless you pass --apply, which sets them in " +
			"the running icebath of this profile.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			preview, err := b.service.Sync(cmd.Context(), &bitwardenv1.SyncRequest{Apply: apply})
			if err != nil {
				return c.Fail("✗ " + secrets.Sentence(tui.Reason(err)))
			}
			if len(preview.GetRows()) == 0 {
				c.Warning("The Bitwarden project has no secrets.")
				return nil
			}
			var rows [][]string
			for _, row := range preview.GetRows() {
				rows = append(rows, []string{c.Paint(row.GetName(), cli.Cyan), action(c, row.GetAction())})
			}
			c.Table([]string{"Variable", "What happens"}, rows)
			for _, warning := range preview.GetWarnings() {
				c.Warning(secrets.Sentence(warning))
			}
			if !apply {
				c.Say("\n  Nothing changed, and icebath loads these secrets by itself each time it starts. To set them now in the running icebath " +
					"of this profile, over values you set already, add " + c.Paint("--apply", cli.Cyan) + ".")
				return nil
			}
			c.Blank()
			c.Success("Set " + strs.Count(int(preview.GetExported()), "secret") + " in the running icebath of this profile.")
			return nil
		}}
	command.Flags().BoolVar(&apply, "apply", false, "Set the secrets in the running icebath of this profile")
	return command
}

// What a sync did or would do with one secret
func action(c *cli.Console, kind string) string {
	switch kind {
	case "skip_token":
		return c.Paint("Left alone, because it holds the access token", cli.Dim)
	case "skip_set":
		return c.Paint("Left alone, because it is set already", cli.Dim)
	case "exported":
		return c.Paint("Set", cli.Green)
	case "overrode":
		return c.Paint("Set over the value you had", cli.Green)
	case "override":
		return c.Paint("Would be set over the value you have", cli.Green)
	}
	return c.Paint("Would be set", cli.Green)
}

func (b bitwarden) disable() *cobra.Command {
	return &cobra.Command{Use: "disable", Short: "Turn Bitwarden off",
		Long: "Stops loading secrets from Bitwarden. Your access token stays in the keys file until you remove it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			state, err := b.service.State(cmd.Context(), &bitwardenv1.StateRequest{})
			if err != nil {
				return err
			}
			if _, err := b.service.Disable(cmd.Context(), &bitwardenv1.DisableRequest{}); err != nil {
				return err
			}
			c.Success("Bitwarden is off. icebath no longer loads its secrets, now or when it starts.")
			c.Info("Your access token stays in " + state.GetEnvPath() + ". Remove it from that file, and revoke it in the Bitwarden web app, if " +
				"you want it gone.")
			return nil
		}}
}

func (b bitwarden) install() *cobra.Command {
	force := false
	command := &cobra.Command{Use: "install", Short: "Install the pinned version of bws",
		Long: "Downloads the version of bws that icebath pins and keeps it with the other programs icebath downloads. With --force, " +
			"install checks the copy you have and repairs it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			done, err := b.service.Install(cmd.Context(), &bitwardenv1.InstallRequest{Force: force})
			if err != nil {
				return c.Fail("✗ bws could not be installed. " + secrets.Sentence(tui.Reason(err)))
			}
			installed(c, done.GetBinary(), done.GetVersion())
			return nil
		}}
	command.Flags().BoolVar(&force, "force", false, "Check the installed copy and repair it")
	return command
}
