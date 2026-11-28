// The secrets onepassword commands: set the store up, see it, rotate its token, bind and unbind variables, resolve now, turn it off
package cli

import (
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	onepasswordconfig "github.com/nickheyer/icebath/internal/plugins/defaults/secrets/onepassword/config"
	picked "github.com/nickheyer/icebath/internal/profile"
	onepasswordv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/onepassword/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/onepassword/v1/onepasswordv1connect"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1/secretsv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
)

type commands struct{}

func Register(registry *cli.Registry) {
	registry.Register(commands{})
}

func (commands) Commands(conn *connect.Client) []*cobra.Command {
	o := onepassword{service: onepasswordv1connect.NewOnePasswordServiceClient(conn), stores: secretsv1connect.NewSecretServiceClient(conn)}
	root := &cobra.Command{Use: onepasswordconfig.Name, Aliases: []string{"op", "1password"}, Short: "Load API keys from 1Password",
		Long: "Sets environment variables from op:// references to 1Password items each time icebath starts. icebath reads them with op, the 1Password " +
			"command line tool, signed in to your account or with a service account token. icebath secrets on and off turn it on and off.",
		Args: cobra.NoArgs}
	root.AddCommand(o.setup(), o.status(), o.token(), o.set(), o.remove(), o.sync())
	return []*cobra.Command{root}
}

type onepassword struct {
	service onepasswordv1connect.OnePasswordServiceClient
	// The service every store shares, which fills variables, fetches now and turns stores on and off
	stores secretsv1connect.SecretServiceClient
}

func (o onepassword) setup() *cobra.Command {
	var account, tokenEnv, token, binaryPath string
	command := &cobra.Command{Use: "setup", Short: "Set up 1Password step by step",
		Long: "Checks that op is installed and signs in, with a service account token or your account on this computer, then keeps the token " +
			"or the account and turns 1Password on. Nothing is kept while op can't sign in. Then set each variable from an item with set.",
		Example: "  icebath secrets onepassword setup --account my.1password.com",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			c.Panel("", c.Paint("Set up 1Password", cli.Bold), "", "icebath reads references such as "+c.Paint("op://vault/item/field", cli.Cyan)+
				" with op,", "the 1Password command line tool, installed and signed in on this computer.", "",
				"To install op and sign in, follow "+c.Paint(onepasswordconfig.Docs, cli.Cyan)+".")
			c.Step(1, "Find op")
			set, err := o.service.Setup(cmd.Context(), &onepasswordv1.SetupRequest{Profile: picked.Picked(cmd.Context()), Account: account, TokenEnv: tokenEnv, Token: token, BinaryPath: binaryPath})
			if err != nil {
				return err
			}
			if set.GetBinaryMissing() {
				if set.GetBinaryPath() != "" {
					c.Failure(set.GetBinaryPath() + " is not a program icebath can run.")
				} else {
					c.Failure("op is not installed, or it is not on PATH.")
				}
				c.Info("Install the 1Password command line tool from " + onepasswordconfig.Docs + ".")
				return cli.Exit(1)
			}
			if version := set.GetVersion(); version != "" && version != "version unknown" {
				c.Success("op " + strings.TrimPrefix(version, "op ") + " is installed at " + set.GetBinary() + ".")
			} else {
				c.Success("op is installed at " + set.GetBinary() + ".")
			}
			if account = strings.TrimSpace(account); account != "" {
				c.Info("Account: " + c.Paint(account, cli.Cyan))
			}
			c.Step(2, "Sign in")
			switch {
			case set.GetNotSignedIn() && (strings.TrimSpace(token) != "" || set.GetEnvToken()):
				c.Failure("1Password refused the service account token, so nothing changed.")
				return cli.Exit(1)
			case set.GetNotSignedIn():
				c.Failure("op is not signed in, so nothing changed.")
				c.Info("Run " + c.Paint("op signin", cli.Cyan) + " to sign in, or pass a service account token with --token. Then run setup again.")
				return cli.Exit(1)
			case set.GetSavedToken():
				c.Success("The service account token is kept in " + set.GetEnvPath() + " as " + set.GetTokenEnv() + ", signed in as " + set.GetIdentity() + ".")
			case set.GetEnvToken():
				c.Success("icebath uses the service account token in " + set.GetTokenEnv() + ", signed in as " + set.GetIdentity() + ".")
			default:
				c.Success("icebath uses op, which is signed in as " + set.GetIdentity() + ".")
			}
			c.Blank()
			c.Say("✓ 1Password is on.", cli.Green)
			c.Say("  Set a variable from an item:    " + c.Paint(`icebath secrets onepassword set OPENAI_API_KEY "op://Private/OpenAI/api key"`, cli.Cyan) +
				"\n  See what would be set:          " + c.Paint("icebath secrets onepassword sync", cli.Cyan) +
				"\n  See how it is set up:           " + c.Paint("icebath secrets onepassword status", cli.Cyan))
			return nil
		}}
	command.Flags().StringVar(&account, "account", "", "1Password account, by shorthand or sign-in address, as op --account takes it")
	command.Flags().StringVar(&tokenEnv, "token-env", "", "Variable holding a service account token, instead of "+onepasswordconfig.DefaultTokenEnv)
	command.Flags().StringVar(&token, "token", "", "Service account token to keep in the keys file, instead of typing it")
	command.Flags().StringVar(&binaryPath, "binary-path", "", "Path of the op program, instead of looking for it on PATH")
	return command
}

func (o onepassword) status() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show the 1Password settings and references",
		Long: "Shows the 1Password settings, whether op is installed and every variable that is set from an op:// reference.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			state, err := o.service.State(cmd.Context(), &onepasswordv1.StateRequest{Profile: picked.Picked(cmd.Context())})
			if err != nil {
				return err
			}
			account := state.GetAccount()
			if account == "" {
				account = c.Paint("The one op uses", cli.Dim)
			}
			binary := c.Paint("Not found", cli.Yellow)
			if state.GetBinary() != "" {
				binary = state.GetBinary() + ", " + state.GetVersion()
			}
			reuse := "Not reused"
			if ttl := state.GetCacheTtl(); ttl > 0 {
				reuse = strs.Span(time.Duration(ttl) * time.Second)
			}
			c.Pairs("1Password", [][2]string{
				{"Load from 1Password", c.OnOff(state.GetEnabled())},
				{"Account", account},
				{"Token variable", state.GetTokenEnv()},
				{"Token set", c.YesNo(state.GetTokenSet())},
				{"Replace your values", c.OnOff(state.GetOverride())},
				{"Reuse values for", reuse},
				{"op program", binary},
				{"Variables", strs.Count(len(state.GetReferences()), "variable")},
			})
			if refs := state.GetReferences(); len(refs) > 0 {
				var rows [][]string
				for _, ref := range refs {
					rows = append(rows, []string{c.Paint(ref.GetEnv(), cli.Cyan), ref.GetReference()})
				}
				c.Table([]string{"Variable", "Reference"}, rows)
			}
			if !state.GetEnabled() {
				c.Say("\n  Run " + c.Paint("icebath secrets on onepassword", cli.Cyan) + " to turn 1Password on, or " +
					c.Paint("icebath secrets onepassword setup", cli.Cyan) + " to set it up.")
				return nil
			}
			if state.GetBinary() != "" && !state.GetTokenSet() {
				if state.GetSession() != "" {
					c.Blank()
					c.Success("op is signed in as " + state.GetSession() + ".")
				} else {
					c.Blank()
					c.Warning("op is not signed in and " + state.GetTokenEnv() + " is not set, so icebath skips 1Password and warns you each time it starts.")
				}
			}
			if len(state.GetReferences()) == 0 {
				c.Say("\n  " + c.Paint("No variable is set from 1Password yet.", cli.Yellow) + " Add one with " +
					c.Paint("icebath secrets onepassword set <env_var> <reference>", cli.Cyan) + ".")
			}
			return nil
		}}
}

func (o onepassword) token() *cobra.Command {
	var token string
	noVerify := false
	command := &cobra.Command{Use: "token", Short: "Replace the service account token",
		Long: "Asks for a new service account token, checks it with 1Password and keeps it in the keys file. With --no-verify, token keeps " +
			"it without the check.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			ctx := cmd.Context()
			state, err := o.service.State(ctx, &onepasswordv1.StateRequest{Profile: picked.Picked(ctx)})
			if err != nil {
				return err
			}
			token = strings.TrimSpace(token)
			if token == "" {
				if !c.Interactive() {
					return c.Fail("✗ There is no terminal to type the token in. Pass it with --token.")
				}
				c.Say("Make a new service account token at https://my.1password.com under Developer › Service accounts.\n")
				typed, ok := c.Secret(fmt.Sprintf("Paste the new token, which icebath keeps as %s: ", state.GetTokenEnv()))
				if !ok {
					return cli.Exit(1)
				}
				token = strings.TrimSpace(typed)
			}
			if token == "" {
				return c.Fail("✗ No token was pasted, so nothing changed.")
			}
			if !noVerify {
				c.Say("Checking the token with op…")
			}
			rotated, err := o.service.Token(ctx, &onepasswordv1.TokenRequest{Profile: picked.Picked(ctx), Token: token, Verify: !noVerify})
			if err != nil {
				return err
			}
			switch {
			case rotated.GetBinaryMissing():
				return c.Fail("✗ op is not installed, so the token can't be checked. Install it from " + onepasswordconfig.Docs +
					", or run the command again with --no-verify to keep the token unchecked.")
			case rotated.GetRejected():
				return c.Fail("✗ 1Password refused the new token, so nothing changed.")
			}
			if !noVerify {
				c.Success("1Password accepted the token, which signs in as " + rotated.GetIdentity() + ".")
			}
			c.Success("The token is kept in " + rotated.GetEnvPath() + " as " + rotated.GetTokenEnv() + ", and icebath uses it from now on.")
			if rotated.GetDisabled() {
				c.Warning("1Password is off. Run icebath secrets on onepassword to turn it on.")
			}
			return nil
		}}
	command.Flags().StringVar(&token, "token", "", "New service account token, instead of typing it")
	command.Flags().BoolVar(&noVerify, "no-verify", false, "Keep the token without checking it with 1Password first")
	return command
}

func (o onepassword) set() *cobra.Command {
	return &cobra.Command{Use: "set <env_var> <reference>", Short: "Set a variable from an op:// reference",
		Long:    "Fills the environment variable from a 1Password item each time icebath starts. The reference is checked before it is kept.",
		Example: "  icebath secrets onepassword set OPENAI_API_KEY op://Private/OpenAI/credential",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			bound, err := o.stores.Bind(cmd.Context(), &secretsv1.BindRequest{Profile: picked.Picked(cmd.Context()), Name: onepasswordconfig.Name, Variable: args[0], Reference: args[1]})
			if err != nil {
				return c.Fail("✗ " + strs.Sentence(tui.Reason(err)))
			}
			c.Say(c.Tick() + " " + bound.GetNote())
			return nil
		}}
}

func (o onepassword) remove() *cobra.Command {
	return &cobra.Command{Use: "remove <env_var>", Short: "Stop setting a variable from 1Password",
		Long: "Removes the op:// reference of the environment variable, so icebath no longer sets the variable from 1Password.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := cli.NewConsole(cmd)
			removed, err := o.stores.Unbind(cmd.Context(), &secretsv1.UnbindRequest{Profile: picked.Picked(cmd.Context()), Name: onepasswordconfig.Name, Variable: args[0]})
			if err != nil {
				return c.Fail("✗ " + strs.Sentence(tui.Reason(err)))
			}
			c.Say(c.Tick() + " " + removed.GetNote())
			return nil
		}}
}

// What a sync did or would do with one variable
func action(c *cli.Console, kind string) string {
	switch kind {
	case "exported":
		return c.Paint("Set", cli.Green)
	case "overrode":
		return c.Paint("Set over the value you had", cli.Green)
	case "unresolved":
		return c.Paint("Could not be read, as the warnings below say", cli.Red)
	case "skip_token":
		return c.Paint("Left alone, because it holds the token", cli.Dim)
	case "skip_set":
		return c.Paint("Left alone, because it is set already", cli.Dim)
	case "override":
		return c.Paint("Would be set over the value you have", cli.Green)
	}
	return c.Paint("Would be set", cli.Green)
}

func (o onepassword) sync() *cobra.Command {
	apply := false
	command := &cobra.Command{Use: "sync", Short: "Read the references now and show what would change",
		Long: "Reads every op:// reference now and shows what each one would do. Nothing changes unless you pass --apply, which sets the " +
			"values in the running icebath of this profile.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := cli.NewConsole(cmd)
			synced, err := o.stores.Preview(cmd.Context(), &secretsv1.PreviewRequest{Profile: picked.Picked(cmd.Context()), Name: onepasswordconfig.Name, Apply: apply})
			if err != nil {
				return c.Fail("✗ " + strs.Sentence(tui.Reason(err)))
			}
			if len(synced.GetOutcomes()) == 0 && len(synced.GetWarnings()) == 0 {
				c.Say("No variable is set from 1Password yet. Add one with icebath secrets onepassword set <env_var> <reference>.", cli.Yellow)
				return nil
			}
			var rows [][]string
			set := 0
			for _, row := range synced.GetOutcomes() {
				rows = append(rows, []string{c.Paint(row.GetName(), cli.Cyan), action(c, row.GetAction())})
				if row.GetAction() == "exported" || row.GetAction() == "overrode" {
					set++
				}
			}
			c.Table([]string{"Variable", "What happens"}, rows)
			for _, warning := range synced.GetWarnings() {
				c.Warning(strs.Sentence(warning))
			}
			if apply {
				c.Blank()
				c.Success("Set " + strs.Count(set, "secret") + " in the running icebath of this profile.")
				return nil
			}
			c.Say("\n  Nothing changed, and icebath reads these references by itself each time it starts. To set them now in the running " +
				"icebath of this profile, add " + c.Paint("--apply", cli.Cyan) + ".")
			return nil
		}}
	command.Flags().BoolVar(&apply, "apply", false, "Set the values in the running icebath of this profile")
	return command
}
