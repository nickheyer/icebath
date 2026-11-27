// The secrets onepassword commands: set the store up, see it, rotate its token, bind and unbind variables, resolve now, turn it off
package cli

import (
	"fmt"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	onepasswordconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/config"
	onepasswordv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/onepassword/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/onepassword/v1/onepasswordv1connect"
	"github.com/nickheyer/icebath/pkg/tui"
)

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	o := onepassword{service: onepasswordv1connect.NewOnePasswordServiceClient(conn)}
	root := &cobra.Command{Use: onepasswordconfig.Name, Aliases: []string{"op", "1password"}, Short: "1Password (op:// references) integration", Args: cobra.NoArgs}
	root.AddCommand(o.setup(), o.status(), o.token(), o.set(), o.remove(), o.sync(), o.disable())
	return []*cobra.Command{root}
}

type onepassword struct {
	service onepasswordv1connect.OnePasswordServiceClient
}

func (o onepassword) setup() *cobra.Command {
	var account, tokenEnv, token, binaryPath string
	command := &cobra.Command{Use: "setup", Short: "Verify the op CLI, set account / token env var, and enable", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			c.Panel("", c.Paint("1Password secret source setup", cli.Bold), "", "Icebath resolves "+c.Paint("op://vault/item/field", cli.Cyan)+
				" references through your", "already-installed, already-authenticated 1Password CLI (`op`).", "",
				"Don't have it yet? Install + sign in: "+c.Paint(onepasswordconfig.Docs, cli.Cyan))
			c.Step(1, "Locate the op CLI")
			set, err := o.service.Setup(cmd.Context(), &onepasswordv1.SetupRequest{Account: account, TokenEnv: tokenEnv, Token: token, BinaryPath: binaryPath})
			if err != nil {
				return err
			}
			if set.GetBinaryMissing() {
				if set.GetBinaryPath() != "" {
					c.Say("  ✗ "+set.GetBinaryPath()+" is not an executable op binary.", cli.Red)
				} else {
					c.Say("  ✗ op not found on PATH.", cli.Red)
				}
				c.Say("  Install the 1Password CLI: " + onepasswordconfig.Docs)
				return cli.Exit(1)
			}
			c.Say("  " + c.Tick() + " " + set.GetBinary() + "  (" + set.GetVersion() + ")")
			if account = strings.TrimSpace(account); account != "" {
				c.Say("  Account: " + c.Paint(account, cli.Cyan))
			}
			c.Step(2, "Authentication")
			switch {
			case set.GetSavedToken():
				c.Say("  " + c.Tick() + " service-account token stored in " + set.GetEnvPath() + " as " + set.GetTokenEnv())
			case set.GetEnvToken():
				c.Say("  " + c.Tick() + " using service-account token from " + set.GetTokenEnv())
			case set.GetIdentity() != "":
				c.Say("  " + c.Tick() + " using existing op session (" + set.GetIdentity() + ")")
			default:
				c.Say("  No service-account token and no active op session detected.", cli.Yellow)
				c.Say("  Either run " + c.Paint("op signin", cli.Cyan) + " (desktop/interactive) or set a service-account token in " + set.GetTokenEnv() +
					", then re-run status.")
			}
			c.Blank()
			c.Say("✓ 1Password secret source is enabled.", cli.Green)
			c.Say("  Map credentials:  " + c.Paint(`icebath secrets onepassword set OPENAI_API_KEY "op://Private/OpenAI/api key"`, cli.Cyan) +
				"\n  Preview:          " + c.Paint("icebath secrets onepassword sync", cli.Cyan) +
				"\n  Status:           " + c.Paint("icebath secrets onepassword status", cli.Cyan))
			return nil
		}}
	command.Flags().StringVar(&account, "account", "", "1Password account shorthand or sign-in address (op --account)")
	command.Flags().StringVar(&tokenEnv, "token-env", "", "Env var holding a service-account token (default "+onepasswordconfig.DefaultTokenEnv+")")
	command.Flags().StringVar(&token, "token", "", "Service-account token to store in .env non-interactively")
	command.Flags().StringVar(&binaryPath, "binary-path", "", "Absolute path to the op binary (skips PATH lookup)")
	return command
}

func (o onepassword) status() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show config + op binary + references", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			state, err := o.service.State(cmd.Context(), &onepasswordv1.StateRequest{})
			if err != nil {
				return err
			}
			account := state.GetAccount()
			if account == "" {
				account = c.Paint("default", cli.Dim)
			}
			binary := c.Paint("not found", cli.Yellow)
			if state.GetBinary() != "" {
				binary = state.GetBinary() + " (" + state.GetVersion() + ")"
			}
			c.Pairs("1Password secret source", [][2]string{
				{"Enabled", c.YesNo(state.GetEnabled())},
				{"Account", account},
				{"Token env var", state.GetTokenEnv()},
				{"Token in env", c.YesNo(state.GetTokenSet())},
				{"Override existing", c.YesNo(state.GetOverride())},
				{"Cache TTL (s)", fmt.Sprint(state.GetCacheTtl())},
				{"op binary", binary},
				{"References", fmt.Sprint(len(state.GetReferences()))},
			})
			if refs := state.GetReferences(); len(refs) > 0 {
				var rows [][]string
				for _, ref := range refs {
					rows = append(rows, []string{c.Paint(ref.GetEnv(), cli.Cyan), ref.GetReference()})
				}
				c.Table([]string{"Env var", "Reference"}, rows)
			}
			if !state.GetEnabled() {
				c.Say("\n  Run " + c.Paint("icebath secrets onepassword setup", cli.Cyan) + " to enable.")
				return nil
			}
			if state.GetBinary() != "" && !state.GetTokenSet() {
				if state.GetSession() != "" {
					c.Say("\n  " + c.Paint("Active op session:", cli.Green) + " " + state.GetSession())
				} else {
					c.Say("\n  No active op session and "+state.GetTokenEnv()+" is unset — Icebath will warn and skip 1Password on next startup.", cli.Yellow)
				}
			}
			if len(state.GetReferences()) == 0 {
				c.Say("\n  " + c.Paint("No references mapped yet.", cli.Yellow) + "  Add one: " + c.Paint("icebath secrets onepassword set ENV_VAR \"op://…\"", cli.Cyan))
			}
			return nil
		}}
}

func (o onepassword) token() *cobra.Command {
	var token string
	noVerify := false
	command := &cobra.Command{Use: "token", Short: "Rotate the service-account token: validate and store it in .env", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			ctx := cmd.Context()
			state, err := o.service.State(ctx, &onepasswordv1.StateRequest{})
			if err != nil {
				return err
			}
			token = strings.TrimSpace(token)
			if token == "" {
				if !c.Interactive() {
					return c.Fail("No TTY — pass the token with --token.")
				}
				c.Say("Create a new service-account token at https://my.1password.com → Developer → Service Accounts.\n")
				typed, ok := c.Secret(fmt.Sprintf("Paste new token (%s): ", state.GetTokenEnv()))
				if !ok {
					return cli.Exit(1)
				}
				token = strings.TrimSpace(typed)
			}
			if token == "" {
				return c.Fail("Empty token, aborting.")
			}
			if !noVerify {
				c.Say("Verifying with `op whoami`…")
			}
			rotated, err := o.service.Token(ctx, &onepasswordv1.TokenRequest{Token: token, Verify: !noVerify})
			if err != nil {
				return err
			}
			switch {
			case rotated.GetBinaryMissing():
				return c.Fail("op CLI not found — install it (" + onepasswordconfig.Docs + ") or re-run with --no-verify to store anyway.")
			case rotated.GetRejected():
				return c.Fail("✗ New token was rejected by op — nothing was changed.")
			}
			if !noVerify {
				c.Say(c.Paint("✓ Token accepted", cli.Green) + " (" + rotated.GetIdentity() + ").")
			}
			c.Say(c.Tick() + " stored in " + rotated.GetEnvPath() + " as " + rotated.GetTokenEnv() + ".  The running icebath uses it now.")
			if rotated.GetDisabled() {
				c.Say("Note: the 1Password integration is currently disabled — run `icebath secrets onepassword setup` to turn it on.", cli.Yellow)
			}
			return nil
		}}
	command.Flags().StringVar(&token, "token", "", "Provide the new token non-interactively (default: masked prompt)")
	command.Flags().BoolVar(&noVerify, "no-verify", false, "Store without probing 1Password first (not recommended)")
	return command
}

func (o onepassword) set() *cobra.Command {
	return &cobra.Command{Use: "set <env_var> <reference>", Short: "Map an env var to an op:// reference", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			mapped, err := o.service.Map(cmd.Context(), &onepasswordv1.MapRequest{Env: args[0], Reference: args[1]})
			if err != nil {
				return err
			}
			if mapped.GetReference() == "" {
				for _, warning := range mapped.GetWarnings() {
					c.Say(warning, cli.Red)
				}
				return cli.Exit(1)
			}
			c.Say(c.Tick() + " mapped " + c.Paint(args[0], cli.Cyan) + " → " + mapped.GetReference())
			if !mapped.GetEnabled() {
				c.Say("  " + c.Paint("Note: the integration is disabled — run ", cli.Yellow) + c.Paint("icebath secrets onepassword setup", cli.Cyan) +
					c.Paint(" to turn it on.", cli.Yellow))
			}
			return nil
		}}
}

func (o onepassword) remove() *cobra.Command {
	return &cobra.Command{Use: "remove <env_var>", Short: "Remove an env-var → reference mapping", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			removed, err := o.service.Unmap(cmd.Context(), &onepasswordv1.UnmapRequest{Env: args[0]})
			if err != nil {
				return err
			}
			if !removed.GetMapped() {
				c.Say(args[0]+" is not mapped.", cli.Yellow)
				return cli.Exit(1)
			}
			c.Say(c.Tick() + " removed mapping for " + c.Paint(args[0], cli.Cyan))
			return nil
		}}
}

// What a sync did or would do with one variable
func action(c *cli.Console, kind string) string {
	switch kind {
	case "exported":
		return c.Paint("exported", cli.Green)
	case "skipped":
		return c.Paint("skipped (already set / token var)", cli.Dim)
	case "unresolved":
		return c.Paint("unresolved (see warnings)", cli.Red)
	case "skip_token":
		return c.Paint("skip (token var)", cli.Dim)
	case "skip_set":
		return c.Paint("skip (already set)", cli.Dim)
	case "override":
		return c.Paint("would export", cli.Green) + " (overrides)"
	}
	return c.Paint("would export", cli.Green)
}

func (o onepassword) sync() *cobra.Command {
	apply := false
	command := &cobra.Command{Use: "sync", Short: "Resolve references now and report what changed", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			synced, err := o.service.Sync(cmd.Context(), &onepasswordv1.SyncRequest{Apply: apply})
			if err != nil {
				return c.Fail(tui.Reason(err))
			}
			if len(synced.GetRows()) == 0 && len(synced.GetWarnings()) == 0 {
				c.Say("No op:// references configured.  Add one with `icebath secrets onepassword set ENV_VAR \"op://…\"`.", cli.Yellow)
				return nil
			}
			var rows [][]string
			for _, row := range synced.GetRows() {
				rows = append(rows, []string{c.Paint(row.GetName(), cli.Cyan), action(c, row.GetAction())})
			}
			c.Table([]string{"Env var", "Action"}, rows)
			for _, warning := range synced.GetWarnings() {
				c.Say(c.Paint("warning:", cli.Yellow) + " " + warning)
			}
			if apply {
				c.Say("\n  " + c.Paint(fmt.Sprintf("Exported %d secret(s) into the running icebath for this profile.", synced.GetExported()), cli.Green))
				return nil
			}
			c.Say("\n  This was a dry-run — references resolve automatically in icebath.  Re-run with " + c.Paint("--apply", cli.Cyan) +
				" to set them for this profile in the running icebath now.")
			return nil
		}}
	command.Flags().BoolVar(&apply, "apply", false, "Actually export resolved values into the running icebath for this profile (default: dry-run)")
	return command
}

func (o onepassword) disable() *cobra.Command {
	return &cobra.Command{Use: "disable", Short: "Turn off the 1Password integration", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			if _, err := o.service.Disable(cmd.Context(), &onepasswordv1.DisableRequest{}); err != nil {
				return err
			}
			c.Say(c.Paint("Disabled.", cli.Green) + "  1Password references are no longer resolved, from now on and at every start.\n" +
				"  Your reference mappings are left in the settings — remove them with " + c.Paint("icebath secrets onepassword remove ENV_VAR", cli.Cyan) +
				" if you no longer need them.")
			return nil
		}}
}
