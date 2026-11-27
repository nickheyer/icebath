// The secrets bitwarden commands: set the store up, check it, rotate its token, pull now, turn it off, install bws
package cli

import (
	"cmp"
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	bitwardenconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/bitwarden/config"
	bitwardenv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/bitwarden/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/bitwarden/v1/bitwardenv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

const (
	oddToken = "Warning: token doesn't start with '0.' — usually that means you pasted something other than a BSM access token."
	usCloud  = "https://vault.bitwarden.com"
)

// Bitwarden regions the region prompt offers, a new one here appearing in it
var regions = [][2]string{
	{"US Cloud  (https://vault.bitwarden.com — bws default)", ""},
	{"EU Cloud  (https://vault.bitwarden.eu)", "https://vault.bitwarden.eu"},
}

// Follow up of each cause of a failed project list
var hints = map[string]string{
	"region": "  'invalid_client' from the US identity endpoint usually means the token is for a different Bitwarden region.  Re-run " +
		"icebath secrets bitwarden setup and pick EU or self-hosted at the region prompt, or set Bitwarden server in /settings under Secret stores.",
	"token": "  This usually means the access token is wrong or revoked. Double-check it in the Bitwarden web app.",
}

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	b := bitwarden{service: bitwardenv1connect.NewBitwardenServiceClient(conn)}
	root := &cobra.Command{Use: bitwardenconfig.Name, Aliases: []string{"bw"}, Short: "Bitwarden Secrets Manager integration", Args: cobra.NoArgs}
	root.AddCommand(b.setup(), b.status(), b.token(), b.sync(), b.disable(), b.install())
	return []*cobra.Command{root}
}

type bitwarden struct {
	service bitwardenv1connect.BitwardenServiceClient
}

func (b bitwarden) setup() *cobra.Command {
	var project, token, server string
	command := &cobra.Command{Use: "setup", Short: "Interactive wizard: install bws, store access token, pick project", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			ctx := cmd.Context()
			c.Panel("", c.Paint("Bitwarden Secrets Manager setup", cli.Bold), "", "Need an access token? In the Bitwarden web app:",
				"  Secrets Manager → Machine accounts → [your account] →", "  Access tokens → Create access token", "",
				"Copy the token (starts with "+c.Paint("0.", cli.Cyan)+"…) — it cannot be retrieved later.")
			c.Step(1, "Install the bws CLI")
			state, err := b.service.State(ctx, &bitwardenv1.StateRequest{})
			if err != nil {
				return err
			}
			binary, version := state.GetBinary(), state.GetVersion()
			if binary == "" {
				c.Say("  No bws on PATH — downloading…")
				installed, err := b.service.Install(ctx, &bitwardenv1.InstallRequest{})
				if err != nil {
					c.Say("  ✗ Could not install bws: "+tui.Reason(err), cli.Red)
					c.Say("  Manual install: https://github.com/bitwarden/sdk-sm/releases")
					return cli.Exit(1)
				}
				binary, version = installed.GetBinary(), installed.GetVersion()
			}
			c.Say("  " + c.Tick() + " " + binary + "  (" + version + ")")
			if !c.Interactive() {
				var missing []string
				for _, given := range [][2]string{{"--access-token", token}, {"--server-url", cmp.Or(strings.TrimSpace(server), os.Getenv(bitwardenconfig.ServerURLEnv))},
					{"--project-id", project}} {
					if strings.TrimSpace(given[1]) == "" {
						missing = append(missing, given[0])
					}
				}
				if len(missing) > 0 {
					c.Say("  Non-interactive mode (no TTY) requires all setup flags.", cli.Red)
					c.Say("  Missing: " + strings.Join(missing, ", ") + "\n\n  Usage:\n    icebath secrets bitwarden setup \\\n      --access-token '0.xxx' \\\n" +
						"      --server-url 'https://vault.bitwarden.com' \\\n      --project-id 'xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx'")
					return cli.Exit(1)
				}
			}
			c.Step(2, "Provide your access token")
			token = strings.TrimSpace(token)
			if token == "" {
				typed, ok := c.Secret(fmt.Sprintf("  Paste access token (%s): ", state.GetTokenEnv()))
				if !ok {
					return cli.Exit(1)
				}
				token = strings.TrimSpace(typed)
			}
			if token == "" {
				return c.Fail("  Empty token, aborting.")
			}
			if !strings.HasPrefix(token, "0.") {
				c.Say("  "+oddToken+"  Continuing anyway.", cli.Yellow)
			}
			saved, err := b.service.SaveToken(ctx, &bitwardenv1.SaveTokenRequest{Token: token})
			if err != nil {
				return err
			}
			c.Say("  " + c.Tick() + " stored in " + saved.GetEnvPath() + " as " + saved.GetTokenEnv())
			c.Step(3, "Pick a Bitwarden region")
			url, chosen := b.region(c, strings.TrimSpace(server), state.GetServerUrl())
			if !chosen {
				return cli.Exit(1)
			}
			if url != "" {
				c.Say("  " + c.Tick() + " using " + url)
			} else {
				c.Say("  " + c.Tick() + " using bws default (US Cloud, " + usCloud + ")")
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
			c.Step(test, "Test fetch")
			preview, err := b.service.Setup(ctx, &bitwardenv1.SetupRequest{Token: token, ServerUrl: url, ProjectId: project})
			if err != nil {
				return c.Fail("  ✗ Fetch failed: " + tui.Reason(err))
			}
			if len(preview.GetRows()) == 0 {
				c.Say("  Fetch succeeded but the project has no secrets.", cli.Yellow)
			} else {
				var rows [][]string
				for _, row := range preview.GetRows() {
					rows = append(rows, []string{c.Paint(row.GetName(), cli.Cyan), status(c, row.GetAction())})
				}
				c.Table([]string{"Name", "Status"}, rows)
			}
			for _, warning := range preview.GetWarnings() {
				c.Say("  " + c.Paint("warning:", cli.Yellow) + " " + warning)
			}
			c.Blank()
			c.Say(c.Paint("✓ Bitwarden Secrets Manager is enabled.", cli.Green) + "  Secrets are pulled now and at the start of every icebath process.")
			c.Say("  Status:  " + c.Paint("icebath secrets bitwarden status", cli.Cyan) + "\n  Refresh: " + c.Paint("icebath secrets bitwarden sync", cli.Cyan) +
				"\n  Disable: " + c.Paint("icebath secrets bitwarden disable", cli.Cyan))
			return nil
		}}
	command.Flags().StringVar(&project, "project-id", "", "Pre-select a project UUID instead of prompting")
	command.Flags().StringVar(&token, "access-token", "", "Provide the access token non-interactively (will be stored in .env)")
	command.Flags().StringVar(&server, "server-url", "", "Bitwarden region / self-hosted endpoint. Examples: https://vault.bitwarden.com (US, default), "+
		"https://vault.bitwarden.eu (EU), or your self-hosted URL. Skips the interactive region prompt.")
	return command
}

// What setup's test fetch says of one secret
func status(c *cli.Console, action string) string {
	switch action {
	case "skip_token":
		return c.Paint("bootstrap token — never overrides itself", cli.Dim)
	case "override":
		return c.Paint("already set in env (will be overwritten)", cli.Yellow)
	case "skip_set":
		return c.Paint("already set in env (kept)", cli.Dim)
	}
	return c.Paint("new", cli.Green)
}

// Server address from the flag, BWS_SERVER_URL, the settings or the region menu, false for an empty custom address or an ended input
func (b bitwarden) region(c *cli.Console, given, existing string) (string, bool) {
	if given != "" {
		return given, true
	}
	if env := strings.TrimSpace(os.Getenv(bitwardenconfig.ServerURLEnv)); env != "" {
		c.Say("  Detected " + c.Paint(bitwardenconfig.ServerURLEnv, cli.Cyan) + "=" + env + " in your shell — using it.")
		return env, true
	}
	if existing != "" {
		c.Say("  Existing config: " + c.Paint(existing, cli.Cyan) + ". Press Enter to keep, or pick a different option below.")
	}
	var rows [][]string
	for i, region := range regions {
		rows = append(rows, []string{c.Paint(fmt.Sprint(i+1), cli.Cyan), region[0]})
	}
	custom := len(regions) + 1
	rows = append(rows, []string{c.Paint(fmt.Sprint(custom), cli.Cyan), "Self-hosted / custom URL"})
	c.Table([]string{"#", "Region / endpoint"}, rows)
	prompt := fmt.Sprintf("  Select region [1-%d]", custom)
	if existing != "" {
		prompt += " (Enter to keep current)"
	}
	picked, ok := c.Index(prompt+": ", custom, existing != "", "  Enter a number.")
	switch {
	case !ok:
		return "", false
	case picked == 0:
		return existing, true
	case picked <= len(regions):
		return regions[picked-1][1], true
	}
	url, ok := c.Input("  Enter your Bitwarden server URL (e.g. https://vault.example.com): ")
	url = strings.TrimSpace(url)
	if !ok || url == "" {
		c.Say("  Empty URL, aborting.", cli.Red)
		return "", false
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		c.Say("  Warning: URL doesn't start with http:// or https:// — bws may reject it.", cli.Yellow)
	}
	return url, true
}

// Says why a project list failed and what that usually means
func refused(c *cli.Console, failure *bitwardenv1.Failure) {
	c.Say("  "+failure.GetText(), cli.Red)
	if hint := hints[failure.GetCause()]; hint != "" {
		c.Say(hint, cli.Yellow)
	}
}

// Project picked from those the token sees, empty after saying why there is none to pick
func (b bitwarden) project(c *cli.Console, cmd *cobra.Command, token, server string) (string, error) {
	listed, err := b.service.Projects(cmd.Context(), &bitwardenv1.ProjectsRequest{Token: token, ServerUrl: server})
	if err != nil {
		c.Say("  "+tui.Reason(err), cli.Red)
		return "", nil
	}
	if listed.GetFailure() != nil {
		refused(c, listed.GetFailure())
		return "", nil
	}
	projects := listed.GetProjects()
	if len(projects) == 0 {
		c.Say("  No projects visible to this machine account.", cli.Yellow)
		c.Say("  In the Bitwarden web app, open the machine account → Projects tab and grant it access to at least one project.")
		return "", nil
	}
	var rows [][]string
	for i, p := range projects {
		rows = append(rows, []string{c.Paint(fmt.Sprint(i+1), cli.Cyan), p.GetName(), c.Paint(p.GetId(), cli.Dim)})
	}
	c.Table([]string{"#", "Name", "ID"}, rows)
	picked, ok := c.Index(fmt.Sprintf("  Select project [1-%d]: ", len(projects)), len(projects), false, "")
	if !ok {
		return "", nil
	}
	return projects[picked-1].GetId(), nil
}

func (b bitwarden) status() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show config + binary + token validation status", Args: cobra.NoArgs,
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
			validation := c.Paint("not checked", cli.Dim) + " (" + check.GetSkipped() + ")"
			if check.GetSkipped() == "" {
				validation = c.Paint("passed", cli.Green)
				if !check.GetPassed() {
					validation = c.Paint("failed", cli.Red)
				}
			}
			project := state.GetProjectId()
			if project == "" {
				project = c.Paint("(unset)", cli.Dim)
			}
			server := state.GetServerUrl()
			if server == "" {
				server = c.Paint("default (US Cloud, "+usCloud+")", cli.Dim)
			}
			binary := c.Paint("not installed", cli.Yellow)
			if state.GetBinary() != "" {
				binary = state.GetBinary() + " (" + state.GetVersion() + ")"
			}
			c.Pairs("Bitwarden Secrets Manager", [][2]string{
				{"Enabled", c.YesNo(state.GetEnabled())},
				{"Token env var", state.GetTokenEnv()},
				{"Token in env", c.YesNo(state.GetTokenSet())},
				{"Token validation", validation},
				{"Project ID", project},
				{"Server URL", server},
				{"Override existing", c.YesNo(state.GetOverride())},
				{"Cache TTL (s)", fmt.Sprint(state.GetCacheTtl())},
				{"Auto-install", c.YesNo(state.GetAutoInstall())},
				{"bws binary", binary},
			})
			if check.GetOddToken() {
				c.Say("  "+oddToken+"  Continuing anyway.", cli.Yellow)
			}
			if check.GetFailure() != nil {
				refused(c, check.GetFailure())
			}
			if !state.GetEnabled() {
				c.Say("\n  Run " + c.Paint("icebath secrets bitwarden setup", cli.Cyan) + " to enable.")
				return nil
			}
			if !state.GetTokenSet() {
				c.Say("\n  Enabled but "+state.GetTokenEnv()+" is not set — Icebath will skip BSM and warn on next startup.", cli.Yellow)
			}
			if state.GetProjectId() == "" {
				c.Say("\n  Enabled but no project_id — nothing to fetch.", cli.Yellow)
			}
			return nil
		}}
}

func (b bitwarden) token() *cobra.Command {
	var token string
	noVerify := false
	command := &cobra.Command{Use: "token", Short: "Rotate the access token: validate a new one and store it in .env", Args: cobra.NoArgs,
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
					return c.Fail("No TTY — pass the token with --access-token.")
				}
				c.Say("Create a new token in the Bitwarden web app:\n  Secrets Manager → Machine accounts → [your account] → Access tokens → Create access token\n")
				typed, ok := c.Secret(fmt.Sprintf("Paste new access token (%s): ", state.GetTokenEnv()))
				if !ok {
					return cli.Exit(1)
				}
				token = strings.TrimSpace(typed)
			}
			if token == "" {
				return c.Fail("Empty token, aborting.")
			}
			if !strings.HasPrefix(token, "0.") {
				c.Say(oddToken, cli.Yellow)
			}
			if !noVerify {
				c.Say("Verifying against Bitwarden…")
			}
			rotated, err := b.service.Token(ctx, &bitwardenv1.TokenRequest{Token: token, Verify: !noVerify})
			if err != nil {
				return err
			}
			switch {
			case rotated.GetBinaryMissing():
				return c.Fail("bws binary not available — cannot verify.  Re-run with --no-verify to store anyway.")
			case rotated.GetFailure() != nil:
				refused(c, rotated.GetFailure())
				return c.Fail("✗ New token was rejected — nothing was changed.")
			}
			if !noVerify {
				plural := "s"
				if rotated.GetProjects() == 1 {
					plural = ""
				}
				c.Say(fmt.Sprintf("%s (%d project%s visible).", c.Paint("✓ Token accepted", cli.Green), rotated.GetProjects(), plural))
				if hidden := rotated.GetHiddenProject(); hidden != "" {
					c.Say("Warning: configured project "+hidden+" is not visible to this machine account.  Grant it access in the Bitwarden web app or "+
						"re-run `icebath secrets bitwarden setup` to pick a different project.", cli.Yellow)
				}
			}
			c.Say(c.Tick() + " stored in " + rotated.GetEnvPath() + " as " + rotated.GetTokenEnv() + ".  The running icebath uses it now.")
			if rotated.GetDisabled() {
				c.Say("Note: the Bitwarden integration is currently disabled — run `icebath secrets bitwarden setup` (or turn on Bitwarden secrets in "+
					"/settings) to turn it on.", cli.Yellow)
			}
			return nil
		}}
	command.Flags().StringVar(&token, "access-token", "", "Provide the new token non-interactively (default: masked prompt)")
	command.Flags().BoolVar(&noVerify, "no-verify", false, "Store without probing Bitwarden first (not recommended)")
	return command
}

func (b bitwarden) sync() *cobra.Command {
	apply := false
	command := &cobra.Command{Use: "sync", Short: "Fetch secrets now and report what changed", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			preview, err := b.service.Sync(cmd.Context(), &bitwardenv1.SyncRequest{Apply: apply})
			if err != nil {
				return c.Fail(tui.Reason(err))
			}
			if len(preview.GetRows()) == 0 {
				c.Say("No secrets in project.", cli.Yellow)
				return nil
			}
			var rows [][]string
			for _, row := range preview.GetRows() {
				rows = append(rows, []string{c.Paint(row.GetName(), cli.Cyan), action(c, row.GetAction())})
			}
			c.Table([]string{"Name", "Action"}, rows)
			for _, warning := range preview.GetWarnings() {
				c.Say(c.Paint("warning:", cli.Yellow) + " " + warning)
			}
			if !apply {
				c.Say("\n  This was a dry-run — secrets are picked up automatically by icebath.  Re-run with " + c.Paint("--apply", cli.Cyan) +
					" to set them for this profile in the running icebath now, over values set already.")
				return nil
			}
			c.Say(fmt.Sprintf("\n  %s", c.Paint(fmt.Sprintf("Exported %d secret(s) into the running icebath for this profile.", preview.GetExported()), cli.Green)))
			return nil
		}}
	command.Flags().BoolVar(&apply, "apply", false, "Actually export the secrets into the running icebath for this profile (default: dry-run)")
	return command
}

// What a sync did or would do with one secret
func action(c *cli.Console, kind string) string {
	switch kind {
	case "skip_token":
		return c.Paint("skip (bootstrap token)", cli.Dim)
	case "skip_set":
		return c.Paint("skip (already set)", cli.Dim)
	case "exported":
		return c.Paint("exported", cli.Green)
	case "overrode":
		return c.Paint("exported", cli.Green) + " (overrode)"
	case "override":
		return c.Paint("would export", cli.Green) + " (overrides)"
	}
	return c.Paint("would export", cli.Green)
}

func (b bitwarden) disable() *cobra.Command {
	return &cobra.Command{Use: "disable", Short: "Turn off the Bitwarden integration", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			state, err := b.service.State(cmd.Context(), &bitwardenv1.StateRequest{})
			if err != nil {
				return err
			}
			if _, err := b.service.Disable(cmd.Context(), &bitwardenv1.DisableRequest{}); err != nil {
				return err
			}
			c.Say(c.Paint("Disabled.", cli.Green) + "  Bitwarden secrets are no longer pulled, from now on and at every start.\n" +
				"  Your access token is left in " + state.GetEnvPath() + " — remove it manually if you also want to revoke the credential.")
			return nil
		}}
}

func (b bitwarden) install() *cobra.Command {
	force := false
	command := &cobra.Command{Use: "install", Short: "Install the PM-pinned bws binary", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			installed, err := b.service.Install(cmd.Context(), &bitwardenv1.InstallRequest{Force: force})
			if err != nil {
				return c.Fail("Install failed: " + tui.Reason(err))
			}
			c.Say(c.Tick() + " " + installed.GetBinary() + "  (" + installed.GetVersion() + ")")
			return nil
		}}
	command.Flags().BoolVar(&force, "force", false, "Verify and repair the managed copy")
	return command
}
