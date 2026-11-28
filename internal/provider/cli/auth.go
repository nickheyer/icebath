package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	picked "github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/tools/diff"
	"github.com/nickheyer/icebath/pkg/browser"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/config/v1/configv1connect"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1/providerv1connect"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Names auth commands take for a kind besides those its spec lists
var aliases = map[string]string{"or": "openrouter", "open-router": "openrouter", "grok-oauth": "xai-oauth", "x-ai-oauth": "xai-oauth", "xai-grok-oauth": "xai-oauth"}

// Kinds that sign in with OAuth through a sibling kind, which stores the account
var siblings = map[string]string{"xai": "xai-oauth", "minimax": "minimax-oauth", "minimax-cn": "minimax-oauth", "openai-api": "openai-codex"}

// Strategies of a rotation as the interactive menu offers them: the value, its title and what it does
var strategies = func() [][3]string {
	var out [][3]string
	for _, option := range providerconfig.Strategies() {
		out = append(out, [3]string{fmt.Sprint(option.Value), option.Title, option.About})
	}
	return out
}()

// Title of a rotation order as the settings screen shows it
func strategyTitle(value string) string {
	for _, s := range strategies {
		if s[0] == value {
			return s[1]
		}
	}
	return value
}

// One auth command: its terminal and the services it works through
type auth struct {
	ctx      context.Context
	console  *cli.Console
	out      io.Writer
	errw     io.Writer
	service  providerv1connect.ProviderServiceClient
	settings configv1connect.ConfigServiceClient
	signins  []credentials.SignIn
	// Settings that keep the profile's choice of provider
	choices []string
	// Profile -p picked for the command, which every request names; the server's profile in use when it picked none
	profile string
}

// Prints the reason on the error stream, marked as a failure, and ends the command with status 1
func (a *auth) fail(format string, args ...any) error {
	fmt.Fprintln(a.errw, "✗ "+strings.TrimPrefix(fmt.Sprintf(format, args...), "✗ "))
	return cli.Exit(1)
}

// Provider the setting that keeps the profile's choice names, empty for none
func (a *auth) chosen(key string) (string, error) {
	selected, err := a.settings.Get(a.ctx, &configv1.GetRequest{Profile: a.profile, Key: key})
	if err != nil {
		return "", err
	}
	return strings.Trim(strings.TrimSpace(selected.GetSetting().GetValue()), `"'`), nil
}

// Lines to the output
func (a *auth) say(format string, args ...any) {
	fmt.Fprintf(a.out, format+"\n", args...)
}

func authCommands(conn *connect.Client, signins *SignIns) []*cobra.Command {
	with := func(cmd *cobra.Command) *auth {
		return &auth{ctx: cmd.Context(), console: cli.NewConsole(cmd), out: cmd.OutOrStdout(), errw: cmd.ErrOrStderr(),
			service: providerv1connect.NewProviderServiceClient(conn), settings: configv1connect.NewConfigServiceClient(conn), signins: signins.SignIns(),
			choices: signins.Choices(), profile: picked.Picked(cmd.Context())}
	}
	run := func(do func(a *auth, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error { return do(with(cmd), args) }
	}
	root := &cobra.Command{Use: "auth", GroupID: cli.GroupModels, Short: "Manage your accounts at each provider",
		Long: "An account is one sign-in or API key at a provider. A provider can have several accounts, take turns between them and move " +
			"on when one runs out. With no subcommand, auth lists your accounts and asks what to do.",
		Args: cobra.NoArgs, RunE: run(func(a *auth, _ []string) error { return a.interactive() })}

	var add adding
	addCmd := &cobra.Command{Use: "add <provider>", Short: "Add an account at a provider",
		Long: "Adds an account by signing in or with an API key, asking for what the flags do not give. Without --name, the account is " +
			"named after the account it signs in as, or API key for a first key. --name of an account the provider has signs that account in " +
			"again. --priority places it in the order accounts are tried with the fill_first rotation order, and without it the account goes " +
			"last.\n\n" +
			"For openai-codex, --browser signs in through the browser on http://localhost:1455/auth/callback instead of with a device code. " +
			"The sign-in falls back to the device code when that port is busy. The setting providers.<name>.login_flow does the same.",
		Example: "  icebath auth add anthropic\n  icebath auth add openrouter --type api-key --name work",
		Args:    cobra.ExactArgs(1),
		RunE:    run(func(a *auth, args []string) error { return a.add(args[0], add) })}
	flags := addCmd.Flags()
	flags.StringVar(&add.kind, "type", "", "How the account signs in: oauth or api-key")
	flags.StringVar(&add.name, "name", "", "Name of the account, or of an account the provider has to sign in again")
	flags.BoolVar(&add.rotate, "rotate", false, "Make the new account take turns with the others")
	flags.BoolVar(&add.makeDefault, "default", false, "Make the new account the one the provider uses")
	flags.IntVar(&add.priority, "priority", -1, "Place in the order accounts are tried, 0 for first")
	flags.StringVar(&add.key, "api-key", "", "API key to keep, instead of typing it")
	flags.BoolVar(&add.noBrowser, "no-browser", false, "Do not open a browser to sign in")
	flags.BoolVar(&add.browser, "browser", false, "For openai-codex, sign in through the browser instead of with a code")
	flags.Float64Var(&add.timeout, "timeout", 0, "Time limit in seconds to sign in")

	var onExhausted string
	rotation := &cobra.Command{Use: "rotation <provider> [mode] [accounts...]", Short: "Show or change which accounts take turns",
		Long: "Shows which accounts of the provider take turns, or changes it. mode on makes the accounts you name take turns, every account " +
			"when you name none, and the provider takes turns between them. off stops the ones you name, or with none makes the provider use " +
			"the account in use alone. A rotation order as mode sets which account is used next: fill_first, round_robin, least_used or " +
			"random. --on-exhausted sets what happens when the account in use reaches its limit.",
		Example: "  icebath auth rotation anthropic on work home\n  icebath auth rotation anthropic round_robin",
		Args:    cobra.MinimumNArgs(1), RunE: run(func(a *auth, args []string) error { return a.rotation(args[0], args[1:], onExhausted) })}
	rotation.Flags().StringVar(&onExhausted, "on-exhausted", "", "What happens when the account in use reaches its limit: fail or rotate")

	var all bool
	logout := &cobra.Command{Use: "logout <provider>", Short: "Remove a provider's default account, or all with --all",
		Long: "Removes the account that the provider uses by default. With --all, logout removes every account of the provider and leaves " +
			"out every key or sign-in it would find outside them.",
		Args: cobra.ExactArgs(1),
		RunE: run(func(a *auth, args []string) error { return a.logout(args[0], all) })}
	logout.Flags().BoolVar(&all, "all", false, "Remove every account and saved API key of the provider")

	root.AddCommand(addCmd,
		&cobra.Command{Use: "list [provider]", Short: "List your accounts at each provider",
			Long: "Lists the accounts of every provider, or of the one you name, with their names, which one is the default and which take turns.",
			Args: cobra.MaximumNArgs(1),
			RunE: run(func(a *auth, args []string) error { return a.list(strs.First(args)) })},
		&cobra.Command{Use: "remove <provider> <target>", Short: "Remove one account",
			Long: "Removes one account of the provider, and clears every setting and session that named it. target is the account's name or " +
				"its number in auth list. For a key or sign-in found outside the accounts, remove leaves it out, as auth outside does.",
			Args: cobra.ExactArgs(2),
			RunE: run(func(a *auth, args []string) error { return a.remove(args[0], args[1]) })},
		&cobra.Command{Use: "use <provider> <target>", Short: "Make an account the one a provider uses",
			Long: "Makes the account the one that the provider uses in this profile. Give @rotate as target to take turns between the " +
				"accounts in the rotation instead.",
			Example: "  icebath auth use anthropic work\n  icebath auth use anthropic @rotate",
			Args:    cobra.ExactArgs(2),
			RunE:    run(func(a *auth, args []string) error { return a.use(args[0], args[1]) })},
		&cobra.Command{Use: "rename <provider> <target> <name>", Short: "Rename an account",
			Long: "Gives the account a new name, which every setting and session that named it follows. target is the account's name or its " +
				"number in auth list.",
			Args: cobra.ExactArgs(3),
			RunE: run(func(a *auth, args []string) error { return a.rename(args[0], args[1], args[2]) })},
		rotation,
		&cobra.Command{Use: "outside <provider> [source] [on|off]", Short: "Use a key or sign-in found outside icebath, or not",
			Long: "Lists the keys in variables and the sign-ins of other programs that the provider finds outside its accounts, each on or " +
				"off. Name one with on or off to turn its use on or off.",
			Example: "  icebath auth outside anthropic\n  icebath auth outside anthropic claude-code off",
			Args:    cobra.RangeArgs(1, 3),
			RunE:    run(func(a *auth, args []string) error { return a.outside(args[0], second(args), third(args)) })},
		&cobra.Command{Use: "reset <provider> [target]", Short: "Forget the limits that accounts ran into",
			Long: "Clears the mark that an account reached its limit, so the provider tries it again. With no target, reset clears every " +
				"account of the provider.",
			Args: cobra.RangeArgs(1, 2),
			RunE: run(func(a *auth, args []string) error { return a.reset(args[0], second(args)) })},
		&cobra.Command{Use: "priority <provider> <target> <priority>", Short: "Change the order in which accounts are tried",
			Long: "Moves the account to the place you give, counted from 0. With the fill_first rotation order, the provider tries the " +
				"account at 0 first.",
			Example: "  icebath auth priority anthropic work 0",
			Args:    cobra.ExactArgs(3), RunE: run(func(a *auth, args []string) error { return a.priority(args[0], args[1], args[2]) })},
		&cobra.Command{Use: "refresh <provider> [target]", Short: "Renew the sign-in of an OAuth account",
			Long: "Renews the tokens of an account that signs in with OAuth and ends its wait before the next try. Name the account when the " +
				"provider has more than one.",
			Args: cobra.RangeArgs(1, 2),
			RunE: run(func(a *auth, args []string) error { return a.refresh(args[0], second(args)) })},
		&cobra.Command{Use: "status <provider>", Short: "Show whether you are signed in to a provider",
			Long: "Shows whether the provider's default account is signed in, and why not when it is not.", Args: cobra.MaximumNArgs(1),
			RunE: run(func(a *auth, args []string) error { return a.status(strs.First(args)) })},
		logout,
	)

	var ignored string
	login := &cobra.Command{Use: "login", GroupID: cli.GroupModels, Hidden: true, Args: cobra.NoArgs, Short: "Use icebath auth instead",
		Long: "This command was removed. Use icebath auth to manage your accounts.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "icebath login no longer exists. Add an account with `icebath auth add <provider>`.")
			return nil
		}}
	login.Flags().StringVar(&ignored, "provider", "", "Ignored, use icebath auth add instead")

	var named string
	var every bool
	top := &cobra.Command{Use: "logout [provider]", GroupID: cli.GroupModels, Short: "Remove your account at a provider",
		Long: "Removes the account that the provider uses by default, or with --all every account and saved API key of the provider. " +
			"Without a provider, logout acts on the provider that new sessions use.",
		Args: cobra.MaximumNArgs(1),
		RunE: run(func(a *auth, args []string) error { return a.logout(cmp.Or(strs.First(args), named), every) })}
	top.Flags().StringVar(&named, "provider", "", "Provider to sign out of, instead of the one new sessions use")
	top.Flags().BoolVar(&every, "all", false, "Remove every account and saved API key of the provider, not only the default")
	return []*cobra.Command{root, login, top}
}

func second(args []string) string {
	if len(args) > 1 {
		return args[1]
	}
	return ""
}

func third(args []string) string {
	if len(args) > 2 {
		return args[2]
	}
	return ""
}

// What auth add was asked for
type adding struct {
	kind, name, key     string
	rotate, makeDefault bool
	priority            int
	noBrowser, browser  bool
	timeout             float64
	region              string
}

// Every provider of the profile and every kind it can add
func (a *auth) state() ([]*providerv1.Provider, []*providerv1.Spec, error) {
	listed, err := a.service.List(a.ctx, &providerv1.ListRequest{Profile: a.profile})
	if err != nil {
		return nil, nil, err
	}
	specs, err := a.service.Specs(a.ctx, &providerv1.SpecsRequest{Profile: a.profile})
	if err != nil {
		return nil, nil, err
	}
	return listed.GetProviders(), specs.GetSpecs(), nil
}

// Kind the name means: its own, an alias of it, or one of the names auth takes for it
func kind(specs []*providerv1.Spec, name string) *providerv1.Spec {
	name = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), " ", "-")
	name = cmp.Or(aliases[name], name)
	for _, spec := range specs {
		if spec.GetName() == name {
			return spec
		}
	}
	for _, spec := range specs {
		if slices.Contains(spec.GetAliases(), name) {
			return spec
		}
	}
	return nil
}

// Provider of the profile the name means: one by its name there, else the first of the kind it names, with that kind
func (a *auth) find(name string) (*providerv1.Provider, *providerv1.Spec, []*providerv1.Spec, error) {
	providers, specs, err := a.state()
	if err != nil {
		return nil, nil, nil, err
	}
	normalized := strings.ToLower(strings.TrimSpace(name))
	for _, p := range providers {
		if p.GetName() == normalized {
			return p, kind(specs, p.GetSpec()), specs, nil
		}
	}
	spec := kind(specs, normalized)
	if spec == nil {
		return nil, nil, specs, nil
	}
	for _, p := range providers {
		if p.GetSpec() == spec.GetName() {
			return p, spec, specs, nil
		}
	}
	return nil, spec, specs, nil
}

// Refusal of a name that is no provider of the profile nor a kind it can add, with the closest names
func (a *auth) unknown(name string, specs []*providerv1.Spec) error {
	var known []string
	for _, spec := range specs {
		known = append(known, spec.GetName())
	}
	providers, _, _ := a.state()
	for _, p := range providers {
		known = append(known, p.GetName())
	}
	slices.Sort(known)
	known = slices.Compact(known)
	hint := ""
	if close := diff.Close(name, known, 3, 0.5); len(close) > 0 {
		hint = " Did you mean " + strings.Join(close, ", ") + "?"
	}
	return a.fail("There is no provider named %s.%s Run `icebath auth list` to see yours, or `icebath providers kinds` to see every kind you can add.",
		name, hint)
}

// Provider of the profile the name means, an error when there is none
func (a *auth) provider(name string) (*providerv1.Provider, error) {
	if strings.TrimSpace(name) == "" {
		return nil, a.fail("Name a provider. Run `icebath auth list` to see yours.")
	}
	found, spec, specs, err := a.find(name)
	switch {
	case err != nil:
		return nil, err
	case found != nil:
		return found, nil
	case spec != nil:
		return nil, a.fail("This profile has no %s provider. Add an account with `icebath auth add %s`.", spec.GetDisplayName(), spec.GetName())
	}
	return nil, a.unknown(name, specs)
}

// One login of a provider as the commands list it: an account, or the credential the provider finds outside its accounts
type login struct {
	account *providerv1.Account
	// Variable or program of a credential found outside any account
	outside string
}

// Name a login goes by: an account's key, else a key made from the variable or program
func (l login) name() string {
	if l.account != nil {
		return l.account.GetName()
	}
	return providerconfig.KeyOf(credentials.SourceName(l.outside), "outside", func(string) bool { return false })
}

// Name a login is shown by: an account's own, else the variable or program
func (l login) title() string {
	if l.account != nil {
		return l.account.GetTitle()
	}
	return l.outside
}

// Logins of the provider in the order they are tried: its accounts, then the credential found outside them
func logins(p *providerv1.Provider) []login {
	var out []login
	for _, account := range p.GetAccounts() {
		out = append(out, login{account: account})
	}
	if p.GetOutsideOrigin() != providerv1.Origin_ORIGIN_UNSPECIFIED {
		out = append(out, login{outside: p.GetOutsideSource()})
	}
	return out
}

// Login the target names: by its key, its number from 1, or a name no other login shares; the index and the refusal when none
func target(p *providerv1.Provider, raw string) (int, login, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, login{}, "Name an account of " + p.GetName() + ". Run `icebath auth list " + p.GetName() + "` to see them."
	}
	rows := logins(p)
	for i, row := range rows {
		if row.name() == strings.ToLower(raw) {
			return i + 1, row, ""
		}
	}
	// A key or sign-in from outside the accounts also answers to its variable or program alone, whichever store supplied the variable
	var titled []int
	for i, row := range rows {
		if strings.EqualFold(strings.TrimSpace(row.title()), raw) || row.account == nil && strings.EqualFold(credentials.SourceName(row.outside), raw) {
			titled = append(titled, i)
		}
	}
	switch len(titled) {
	case 1:
		return titled[0] + 1, rows[titled[0]], ""
	case 0:
	default:
		return 0, login{}, fmt.Sprintf("More than one account of %s is named “%s”. Name it by its number instead.", p.GetName(), raw)
	}
	if n, err := strconv.Atoi(raw); err == nil {
		if n >= 1 && n <= len(rows) {
			return n, rows[n-1], ""
		}
		return 0, login{}, fmt.Sprintf("%s has no account number %d. Run `icebath auth list %s` to see them.", p.GetName(), n, p.GetName())
	}
	return 0, login{}, fmt.Sprintf("%s has no account named “%s”. Run `icebath auth list %s` to see them.", p.GetName(), raw, p.GetName())
}

// Sets one setting of the profile to a value, nil putting its default back
func (a *auth) set(key string, value any) error {
	text := "null"
	if value != nil {
		encoded, err := yaml.Marshal(value)
		if err != nil {
			return err
		}
		text = strings.TrimSuffix(string(encoded), "\n")
	}
	_, err := a.settings.Set(a.ctx, &configv1.SetRequest{Profile: a.profile, Key: key, Value: text})
	return err
}

// Values of the provider's own settings by their names below it, as the profile has them
func (a *auth) values(p *providerv1.Provider) map[string]string {
	values := map[string]string{}
	listed, err := a.settings.List(a.ctx, &configv1.ListRequest{Profile: a.profile})
	if err != nil {
		return values
	}
	prefix := p.GetSection() + "."
	for _, setting := range listed.GetSettings() {
		if name, ok := strings.CutPrefix(setting.GetKey(), prefix); ok && !strings.Contains(name, ".") {
			values[name] = setting.GetValue()
		}
	}
	return values
}

// ---- add ----

// Signs a new account of the provider in, or an account already there again when --name names one; the server names a new one as the app
// does, after the name given, who signed in, or the way it signs in
func (a *auth) add(name string, opts adding) error {
	found, spec, specs, err := a.find(name)
	if err != nil {
		return err
	}
	if spec == nil {
		return a.unknown(name, specs)
	}
	asked := strings.ToLower(strings.TrimSpace(opts.kind))
	switch asked {
	case "api_key":
		asked = credentials.APIKey
	case "", credentials.OAuth, credentials.APIKey:
	default:
		if a.way(spec.GetName(), asked) == nil {
			return a.fail("--type takes oauth or api-key, not %q.", opts.kind)
		}
	}
	way := a.way(spec.GetName(), credentials.OAuth)
	if asked == "" {
		asked = credentials.APIKey
		if strings.TrimSpace(opts.key) == "" && way != nil && spec.GetSignsIn() && (spec.GetName() != "copilot" || a.console.Interactive()) {
			asked = credentials.OAuth
		}
		if !spec.GetKeyed() && way != nil {
			asked = credentials.OAuth
		}
	}
	if asked == credentials.OAuth && way == nil {
		sibling := siblings[spec.GetName()]
		if sibling == "" {
			return a.fail("%s", noSignIn(spec.GetName(), a.oauthKinds()))
		}
		a.say("%s signs in through %s, so its sign-in starts now.", spec.GetName(), sibling)
		if spec.GetName() == "minimax-cn" {
			opts.region = "cn"
		}
		return a.add(sibling, opts)
	}
	if found == nil {
		added, err := a.service.Add(a.ctx, &providerv1.AddRequest{Profile: a.profile, Spec: spec.GetName()})
		if err != nil {
			return a.fail("%s", tui.Reason(err))
		}
		found = added.GetProvider()
	}
	title := strings.TrimSpace(opts.name)
	var again *providerv1.Account
	if title != "" {
		if index, row, _ := target(found, title); index > 0 && row.account != nil {
			again, title = row.account, ""
		}
	}
	previous := found.GetAccount()
	var signed credentials.Signed
	if chosen := a.way(spec.GetName(), asked); chosen != nil {
		signed, err = a.signIn(found, chosen, opts)
	} else {
		signed, title, err = a.key(found, opts, title, again != nil)
	}
	if err != nil {
		if _, printed := errors.AsType[cli.Exit](err); printed {
			return err
		}
		lines := credentials.FailureLines(err, found.GetDisplayName(), "icebath auth add "+spec.GetName())
		for i, line := range lines {
			if i == 0 {
				line = "✗ " + line
			}
			fmt.Fprintln(a.errw, line)
		}
		return cli.Exit(1)
	}
	if again != nil {
		if _, err := a.store(found, again.GetName(), "", signed); err != nil {
			return a.fail("%s", tui.Reason(err))
		}
		a.say("✓ Signed in again to the %s account %s.", found.GetDisplayName(), again.GetTitle())
		return a.place(found, again.GetName(), opts, previous)
	}
	account, err := a.store(found, "", title, signed)
	if err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	a.warnSame(found, account.GetName(), signed)
	a.say("✓ Added the %s account %s.", found.GetDisplayName(), account.GetTitle())
	return a.place(found, account.GetName(), opts, previous)
}

// Applies --rotate, --default and --priority to an account just signed in, and says how to use it
func (a *auth) place(p *providerv1.Provider, name string, opts adding, previous string) error {
	if opts.rotate {
		turned, err := a.service.SetTurns(a.ctx, &providerv1.SetTurnsRequest{Profile: a.profile, Provider: p.GetName(), Account: name, On: true})
		if err != nil {
			return a.fail("%s", tui.Reason(err))
		}
		a.say("%s", turned.GetSaid())
	}
	switch {
	case opts.makeDefault:
		if _, err := a.service.UseAccount(a.ctx, &providerv1.UseAccountRequest{Profile: a.profile, Provider: p.GetName(), Account: name}); err != nil {
			return a.fail("%s", tui.Reason(err))
		}
		a.say("%s now uses the account %s.", p.GetDisplayName(), a.title(p.GetName(), name))
	case previous != "" && previous != name && !opts.rotate:
		a.say("The account in use is unchanged. Use this one with `icebath auth use %s %s`.", p.GetName(), name)
	}
	if opts.priority < 0 {
		return nil
	}
	return a.move(p.GetName(), name, opts.priority)
}

// Name an account of the provider is shown by, read now; its key when it can't be read
func (a *auth) title(provider, name string) string {
	if p, err := a.provider(provider); err == nil {
		for _, account := range p.GetAccounts() {
			if account.GetName() == name {
				return account.GetTitle()
			}
		}
	}
	return name
}

// Moves an account to the priority and says where it landed, and why when not where asked
func (a *auth) move(provider, name string, priority int) error {
	moved, err := a.service.MoveAccount(a.ctx, &providerv1.MoveAccountRequest{Profile: a.profile, Provider: provider, Account: name, Priority: int32(priority)})
	if err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	at := int(moved.GetPriority())
	a.say("✓ The %s account %s now has priority %d, number %d in `icebath auth list %s`.", provider, a.title(provider, name), at, at+1, provider)
	refreshed, _ := a.provider(provider)
	if at != priority {
		fmt.Fprintf(a.errw, "⚠ You asked for priority %d, but it is %d, because %s has only %s.\n", priority, at, provider,
			strs.Count(len(refreshed.GetAccounts()), "account"))
	}
	if strategy := refreshed.GetStrategy(); strategy != "" && strategy != providerconfig.FillFirst {
		fmt.Fprintf(a.errw, "⚠ %s takes turns in the order %s, so priority matters only with Fill first.\n", provider, strategyTitle(strategy))
	}
	return nil
}

// Sign-in of the kind with the --type, nil when its plugin adds none
func (a *auth) way(kind, asked string) *credentials.SignIn {
	for i, way := range a.signins {
		if way.Spec == kind && way.Type == asked {
			return &a.signins[i]
		}
	}
	return nil
}

// Kinds a sign-in exists for, sorted
func (a *auth) oauthKinds() []string {
	var kinds []string
	for _, way := range a.signins {
		if way.Type == credentials.OAuth && !slices.Contains(kinds, way.Spec) {
			kinds = append(kinds, way.Spec)
		}
	}
	slices.Sort(kinds)
	return kinds
}

// What a kind without an OAuth sign-in of icebath signs in with instead, and the command that adds it
func noSignIn(kind string, oauth []string) string {
	switch kind {
	case "bedrock":
		return "Bedrock has no sign-in of its own, because AWS signs it in. Run `aws sso login` or `aws configure`, or set AWS_PROFILE, " +
			"or add a Bedrock API key with `icebath auth add bedrock --type api-key`."
	case "vertex":
		return "Vertex has no sign-in of its own, because Google signs it in. Run `gcloud auth application-default login`, or add a " +
			"service account file with `icebath auth add vertex`."
	}
	return fmt.Sprintf("%s takes only an API key, with no sign-in. Add a key with `icebath auth add %s --type api-key`.\nThese providers "+
		"have a sign-in: %s.", kind, kind, strings.Join(oauth, ", "))
}

// Asks for an API key the way auth add does when no plugin adds its own key prompt; a second key of the provider asks its name too, as
// the app does, unless the name was given or the key signs an account in again. The fields and the name
func (a *auth) key(p *providerv1.Provider, opts adding, title string, again bool) (credentials.Signed, string, error) {
	key := strings.TrimSpace(opts.key)
	if key == "" {
		typed, ok := a.console.Secret("Paste your API key: ")
		if !ok {
			return credentials.Signed{}, title, credentials.ErrCancelled
		}
		key = strings.TrimSpace(typed)
	}
	if key == "" {
		return credentials.Signed{}, title, a.fail("No API key was given, so nothing was added.")
	}
	second := slices.ContainsFunc(p.GetAccounts(), func(account *providerv1.Account) bool { return strings.EqualFold(account.GetTitle(), "API key") })
	if title == "" && !again && second && a.console.Interactive() {
		typed, ok := a.console.Input("Name of this account, such as Work: ")
		if !ok {
			return credentials.Signed{}, title, credentials.ErrCancelled
		}
		title = strings.TrimSpace(typed)
	}
	return credentials.Signed{Fields: map[string]any{providerconfig.APIKeyField: key}}, title, nil
}

// Runs a plugin's sign-in in this terminal
func (a *auth) signIn(p *providerv1.Provider, way *credentials.SignIn, opts adding) (credentials.Signed, error) {
	options := map[string]string{"region": opts.region}
	if opts.browser {
		options["browser"] = "true"
	}
	if opts.key != "" {
		options["key"] = opts.key
	}
	prompter := &credentials.Prompter{
		Ctx: a.ctx,
		Out: a.out,
		Ask: func(label string, secret bool) (string, bool) {
			if secret {
				return a.console.Secret(label)
			}
			return a.console.Input(label)
		},
		Open:     func(link string) error { return browser.Open(a.ctx, link) },
		Browser:  !opts.noBrowser,
		Timeout:  time.Duration(opts.timeout * float64(time.Second)),
		Options:  options,
		Settings: a.values(p),
		Terminal: true,
	}
	return way.Run(prompter)
}

// Keeps what a sign-in gave in the account of the name, or in a new account named by the server when none is named, in one write that
// replaces what the account kept to sign in and the limits it ran into; the account as kept
func (a *auth) store(p *providerv1.Provider, name, title string, signed credentials.Signed) (*providerv1.Account, error) {
	fields := map[string]string{}
	for field, value := range signed.Fields {
		if value == nil {
			continue
		}
		encoded, err := yaml.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[field] = strings.TrimSuffix(string(encoded), "\n")
	}
	kept, err := a.service.Connect(a.ctx, &providerv1.ConnectRequest{Profile: a.profile, Provider: p.GetName(), Account: name, Title: title, Identity: signed.Identity,
		Fields: fields})
	if err != nil {
		return nil, err
	}
	return kept.GetAccount(), nil
}

// Warns when a new login signs in as an account another login of the provider already is
func (a *auth) warnSame(p *providerv1.Provider, name string, signed credentials.Signed) {
	if signed.Identity == "" || p.GetSpec() != "openai-codex" {
		return
	}
	for i, account := range p.GetAccounts() {
		if account.GetName() != name && account.GetIdentity() == signed.Identity {
			fmt.Fprintf(a.errw, "⚠ This is the same OpenAI account as number %d, “%s”, and OpenAI then cancels the older sign-in, so you gain no "+
				"extra usage. Sign in to a different account instead, or keep one with `icebath auth remove %s %d`.\n", i+1, account.GetTitle(),
				p.GetName(), i+1)
			return
		}
	}
}

// ---- list ----

func (a *auth) list(name string) error {
	providers := []*providerv1.Provider{}
	if name != "" {
		p, err := a.provider(name)
		if err != nil {
			return err
		}
		providers = append(providers, p)
	} else {
		listed, err := a.service.List(a.ctx, &providerv1.ListRequest{Profile: a.profile})
		if err != nil {
			return err
		}
		providers = listed.GetProviders()
	}
	for _, p := range providers {
		a.group(p)
	}
	shown := 0
	for _, p := range providers {
		if len(logins(p)) > 0 {
			shown++
		}
		a.leftOut(p)
	}
	if shown == 0 {
		a.say("No accounts yet. Add one with `icebath auth add <provider>`.")
	}
	return nil
}

// Says which keys and sign-ins found outside its accounts the provider leaves out, and how to use one again
func (a *auth) leftOut(p *providerv1.Provider) {
	for _, source := range p.GetIgnored() {
		a.say("%s leaves out %s. Use it again with `icebath auth outside %s %s on`.", p.GetDisplayName(), outsideWords(source), p.GetName(),
			login{outside: source}.name())
	}
}

// A key or sign-in found outside the accounts as a sentence names it
func outsideWords(source string) string {
	if config.EnvName(source) {
		return "the API key in " + source
	}
	return "the sign-in of " + source
}

// Header of a provider's logins: how many, the one it uses or how it takes turns, and what it does when one runs out
func header(p *providerv1.Provider, n int) string {
	uses := "tries each account in order"
	switch {
	case p.GetRotate():
		uses = "takes turns in the order " + strategyTitle(p.GetStrategy())
	case p.GetPinned():
		uses = "uses " + provider.AccountTitle(p, p.GetAccount())
	}
	head := fmt.Sprintf("%s · %s · %s", p.GetName(), strs.Count(n, "account"), uses)
	if p.GetOnExhausted() == providerconfig.Rotate {
		head += " · moves on when one runs out"
	}
	return head
}

// Prints one provider's logins as a table under its header
func (a *auth) group(p *providerv1.Provider) {
	rows := logins(p)
	if len(rows) == 0 {
		return
	}
	a.say("%s", a.console.Paint(header(p, len(rows)), cli.Bold))
	cells := make([][]string, len(rows))
	for i, row := range rows {
		cells[i] = line(p, i+1, row)
	}
	a.console.Table([]string{"#", "Account", "Key", "Sign-in", "Key from", "Signed in as", "Priority", "State"}, cells)
	a.say("")
}

// Where a login's secret comes from: icebath, a variable or another program
func origin(row login) string {
	if row.account == nil {
		return row.outside
	}
	account := row.account
	switch {
	case account.GetKeyEnv() != "":
		return account.GetKeyEnv()
	case account.GetCredentialOrigin() == providerv1.Origin_ORIGIN_ENVIRONMENT, account.GetCredentialOrigin() == providerv1.Origin_ORIGIN_EXTERNAL:
		return account.GetCredentialSource()
	}
	return "icebath"
}

// How a login signs in, in words; nothing yet for no credential
func signInWords(auth string) string {
	switch auth {
	case "":
		return "Nothing yet"
	case "api_key":
		return "API key"
	case "oauth":
		return "Signed in"
	case "aws_sigv4":
		return "AWS credentials"
	case "gcp_oauth":
		return "Google Cloud credentials"
	case "none":
		return "None needed"
	}
	return strings.ReplaceAll(auth, "_", " ")
}

// One row of auth list: its number, name, key, how it signs in, where its key comes from, who it is, its priority and its state, by the one
// function the app's pages use
func line(p *providerv1.Provider, index int, row login) []string {
	signedAs, priority, auth, state := "", "", "api_key", ""
	switch {
	case row.account != nil:
		account := row.account
		auth, signedAs, priority, state = account.GetAuth(), account.GetIdentity(), strconv.Itoa(int(account.GetPriority())), account.GetState()
	case p.GetOutsideUsed():
		state = "In use"
	default:
		state = "On"
	}
	if row.account == nil && p.GetOutsideOrigin() == providerv1.Origin_ORIGIN_EXTERNAL {
		auth = "oauth"
	}
	source := origin(row)
	if source == "GITHUB_TOKEN" || source == "GH_TOKEN" {
		state += ", also used by git"
	}
	return []string{strconv.Itoa(index), row.title(), row.name(), signInWords(auth), source, cmp.Or(signedAs, "Not known"), cmp.Or(priority, "None"), state}
}

// ---- remove, use, rename, rotation, reset, priority, refresh ----

func (a *auth) remove(name, raw string) error {
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	index, row, refused := target(p, raw)
	if index == 0 {
		return a.fail("%s", refused)
	}
	if row.account == nil {
		return a.turnOutside(p, row.outside, false)
	}
	removed, err := a.service.RemoveAccount(a.ctx, &providerv1.RemoveAccountRequest{Profile: a.profile, Provider: p.GetName(), Account: row.account.GetName()})
	if err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	a.say("✓ %s", removed.GetSaid())
	if p.GetPinned() && p.GetAccount() == row.account.GetName() {
		after, err := a.provider(p.GetName())
		if err != nil {
			return err
		}
		if after.GetPinned() {
			a.say("%s now uses the account %s.", p.GetDisplayName(), provider.AccountTitle(after, after.GetAccount()))
		} else {
			a.say("%s now tries each of its accounts in order.", p.GetDisplayName())
		}
	}
	return nil
}

// Lists the keys and sign-ins the provider finds outside its accounts, each on or off, or turns the use of one on or off
func (a *auth) outside(name, raw, mode string) error {
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(raw) == "" {
		var rows [][]string
		if source := p.GetOutsideSource(); source != "" {
			state := "On"
			if p.GetOutsideUsed() {
				state = "In use"
			}
			rows = append(rows, []string{login{outside: source}.name(), source, state})
		}
		for _, source := range p.GetIgnored() {
			rows = append(rows, []string{login{outside: source}.name(), source, "Off"})
		}
		if len(rows) == 0 {
			a.say("%s finds no key or sign-in outside its accounts.", p.GetDisplayName())
			return nil
		}
		a.console.Table([]string{"Name", "From", "State"}, rows)
		return nil
	}
	// One is named by its variable or program, whichever store supplied the variable, or by the name the list gives it
	source := ""
	for _, have := range append([]string{p.GetOutsideSource()}, p.GetIgnored()...) {
		named := credentials.SourceName(have)
		if named != "" && (strings.EqualFold(named, strings.TrimSpace(raw)) || login{outside: have}.name() == strings.ToLower(strings.TrimSpace(raw))) {
			source = named
		}
	}
	if source == "" {
		return a.fail("%s finds no key or sign-in named “%s” outside its accounts. Run `icebath auth outside %s` to see them.", p.GetName(), raw,
			p.GetName())
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "on":
		return a.turnOutside(p, source, true)
	case "off":
		return a.turnOutside(p, source, false)
	case "":
		return a.fail("Say on or off after %s.", raw)
	}
	return a.fail("%q is neither on nor off.", mode)
}

// Turns the use of a key or sign-in found outside the accounts on or off, by the one control of that choice
func (a *auth) turnOutside(p *providerv1.Provider, source string, on bool) error {
	source = credentials.SourceName(source)
	turned, err := a.service.SetOutside(a.ctx, &providerv1.SetOutsideRequest{Profile: a.profile, Provider: p.GetName(), Source: source, On: on})
	if err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	a.say("✓ %s", turned.GetSaid())
	if !on && config.EnvName(source) && os.Getenv(source) != "" {
		a.say("%s is still set in your shell, but %s leaves it out.", source, p.GetDisplayName())
	}
	return nil
}

func (a *auth) use(name, raw string) error {
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(raw) == "" {
		return a.fail("Name an account, or @rotate. Run `icebath auth list %s` to see them.", p.GetName())
	}
	account := providerconfig.Rotation
	if raw != providerconfig.Rotation {
		index, row, refused := target(p, raw)
		switch {
		case index == 0:
			return a.fail("%s", refused)
		case row.account == nil:
			return a.fail("%s comes from %s outside the accounts, so it can't be the one always used. %s takes it after its accounts while it "+
				"tries every account in order.", row.name(), row.outside, p.GetDisplayName())
		}
		account = row.account.GetName()
	}
	if _, err := a.service.UseAccount(a.ctx, &providerv1.UseAccountRequest{Profile: a.profile, Provider: p.GetName(), Account: account}); err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	if account == providerconfig.Rotation {
		a.say("✓ %s now takes turns between the accounts that take turns, in the order %s.", p.GetDisplayName(), strategyTitle(p.GetStrategy()))
		return nil
	}
	a.say("✓ %s now uses the account %s. Sessions held to another account keep theirs.", p.GetDisplayName(), provider.AccountTitle(p, account))
	return nil
}

func (a *auth) rename(name, raw, to string) error {
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	index, row, refused := target(p, raw)
	switch {
	case index == 0:
		return a.fail("%s", refused)
	case row.account == nil:
		return a.fail("%s comes from %s, not from icebath, so it has no name to change.", row.name(), row.outside)
	}
	renamed, err := a.service.RenameAccount(a.ctx, &providerv1.RenameAccountRequest{Profile: a.profile, Provider: p.GetName(), Account: row.account.GetName(), Name: to})
	if err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	a.say("✓ Renamed the %s account %s to %s.", p.GetDisplayName(), row.account.GetTitle(), renamed.GetAccount().GetTitle())
	return nil
}

func (a *auth) rotation(name string, args []string, onExhausted string) error {
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	if onExhausted = strings.ToLower(strings.TrimSpace(onExhausted)); onExhausted != "" {
		if onExhausted != providerconfig.Fail && onExhausted != providerconfig.Rotate {
			return a.fail("--on-exhausted takes fail or rotate, not %q.", onExhausted)
		}
		if err := a.set(p.GetSection()+"."+providerconfig.OnExhausted, onExhausted); err != nil {
			return a.fail("%s", tui.Reason(err))
		}
		a.say("✓ When an account runs out is now %s for %s.", map[string]string{providerconfig.Fail: "Stop", providerconfig.Rotate: "Move on"}[onExhausted],
			p.GetDisplayName())
	}
	mode := strings.ToLower(strings.TrimSpace(strs.First(args)))
	targets := args[min(len(args), 1):]
	isStrategy := slices.ContainsFunc(strategies, func(s [3]string) bool { return s[0] == mode })
	switch {
	case mode == "" && onExhausted != "":
		return nil
	case mode == "":
		a.say("%s", a.console.Paint(header(p, len(p.GetAccounts())), cli.Bold))
		var rows [][]string
		for _, account := range p.GetAccounts() {
			state := "Off"
			if account.GetRotate() {
				state = "On"
			}
			rows = append(rows, []string{account.GetTitle(), account.GetName(), state})
		}
		if len(rows) == 0 {
			a.say("%s has no accounts yet. Add one with `icebath auth add %s`.", p.GetDisplayName(), p.GetName())
			return nil
		}
		a.console.Table([]string{"Account", "Key", "Takes turns"}, rows)
		return nil
	case isStrategy:
		if err := a.set(p.GetSection()+"."+providerconfig.Strategy, mode); err != nil {
			return a.fail("%s", tui.Reason(err))
		}
		a.say("✓ %s now takes turns in the order %s.", p.GetDisplayName(), strategyTitle(mode))
		return nil
	case mode != "on" && mode != "off":
		return a.fail("%q is not a rotation mode. Use on, off, fill_first, round_robin, least_used or random.", mode)
	}
	on := mode == "on"
	if len(targets) == 0 && !on {
		if !p.GetRotate() {
			a.say("%s does not take turns between accounts.", p.GetDisplayName())
			return nil
		}
		keep := p.GetUsing()
		if keep == "" {
			return a.fail("No %s account is ready to keep. Pick one with `icebath auth use %s <account>`.", p.GetName(), p.GetName())
		}
		if _, err := a.service.UseAccount(a.ctx, &providerv1.UseAccountRequest{Profile: a.profile, Provider: p.GetName(), Account: keep}); err != nil {
			return a.fail("%s", tui.Reason(err))
		}
		a.say("✓ %s no longer takes turns and uses %s. The accounts still take turns once it takes turns again.", p.GetDisplayName(),
			provider.AccountTitle(p, keep))
		return nil
	}
	if len(targets) == 0 {
		for _, account := range p.GetAccounts() {
			targets = append(targets, account.GetName())
		}
	}
	for _, raw := range targets {
		index, row, refused := target(p, raw)
		if index == 0 || row.account == nil {
			return a.fail("%s", cmp.Or(refused, "Only accounts kept by icebath can take turns."))
		}
		turned, err := a.service.SetTurns(a.ctx, &providerv1.SetTurnsRequest{Profile: a.profile, Provider: p.GetName(), Account: row.account.GetName(), On: on})
		if err != nil {
			return a.fail("%s", tui.Reason(err))
		}
		a.say("✓ %s", turned.GetSaid())
	}
	return nil
}

func (a *auth) reset(name, raw string) error {
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(raw) == "" {
		cleared, err := a.service.Reset(a.ctx, &providerv1.ResetRequest{Profile: a.profile, Provider: p.GetName()})
		if err != nil {
			return a.fail("%s", tui.Reason(err))
		}
		a.say("✓ Cleared the limits of %s of %s.", strs.Count(int(cleared.GetCleared()), "account"), p.GetName())
		return nil
	}
	index, row, refused := target(p, raw)
	if index == 0 || row.account == nil {
		return a.fail("%s", cmp.Or(refused, "Only accounts kept by icebath have limits to clear."))
	}
	if _, err := a.service.Reset(a.ctx, &providerv1.ResetRequest{Profile: a.profile, Provider: p.GetName(), Account: row.account.GetName()}); err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	a.say("✓ Cleared the limits of the %s account %s.", p.GetName(), row.account.GetTitle())
	return nil
}

func (a *auth) priority(name, raw, place string) error {
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	index, row, refused := target(p, raw)
	if index == 0 || row.account == nil {
		return a.fail("%s", cmp.Or(refused, "Only accounts kept by icebath have a priority."))
	}
	n, err := strconv.Atoi(strings.TrimSpace(place))
	if err != nil {
		return a.fail("The priority must be a whole number, not %q.", place)
	}
	return a.move(p.GetName(), row.account.GetName(), n)
}

func (a *auth) refresh(name, raw string) error {
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	rows := logins(p)
	var index int
	var row login
	switch {
	case len(rows) == 0:
		return a.fail("%s has no accounts. Add one with `icebath auth add %s`.", p.GetName(), p.GetName())
	case strings.TrimSpace(raw) == "" && len(rows) != 1:
		return a.fail("%s has %s, so name the one to renew by its number or name. Run `icebath auth list %s` to see them.", p.GetName(),
			strs.Count(len(rows), "account"), p.GetName())
	case strings.TrimSpace(raw) == "":
		index, row = 1, rows[0]
	default:
		var refused string
		if index, row, refused = target(p, raw); index == 0 {
			return a.fail("%s", refused)
		}
	}
	account, label := "", row.name()
	switch {
	case row.account != nil:
		account, label = row.account.GetName(), row.account.GetTitle()
	case !p.GetOutsideUsed():
		return a.fail("%s is not what %s uses now, so there is nothing of it to renew.", row.outside, p.GetName())
	}
	renewed, err := a.service.Renew(a.ctx, &providerv1.RenewRequest{Profile: a.profile, Provider: p.GetName(), Account: account})
	if err != nil {
		if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() == connect.CodeUnimplemented {
			return a.fail("The %s account %s can't be renewed, because it is not a sign-in that renews.", p.GetName(), label)
		}
		return a.fail("The sign-in of the %s account %s could not be renewed: %s. Sign in again with `icebath auth add %s --type oauth`.",
			p.GetDisplayName(), label, strings.TrimSuffix(tui.Reason(err), "."), p.GetName())
	}
	status := cmp.Or(renewed.GetAccount().GetStatus(), "ready")
	if status == "ready" {
		a.say("✓ Renewed the sign-in of the %s account %s.", p.GetName(), label)
		return nil
	}
	a.say("Took the newest sign-in of the %s account %s, which is still %s.", p.GetName(), label, strings.ReplaceAll(status, "_", " "))
	return nil
}

// ---- status, logout ----

func (a *auth) status(name string) error {
	if strings.TrimSpace(name) == "" {
		return a.fail("Name a provider, such as `icebath auth status openai-codex`.")
	}
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	status, err := a.service.Status(a.ctx, &providerv1.StatusRequest{Profile: a.profile, Provider: p.GetName()})
	if err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	if !status.GetLoggedIn() {
		a.say("%s is not signed in. %s", p.GetName(), cmp.Or(status.GetError(), "Add an account with `icebath auth add "+p.GetName()+"`."))
	} else {
		a.say("✓ %s is signed in.", p.GetName())
	}
	for _, detail := range status.GetDetails() {
		a.say("  %s: %s", detail.GetName(), detail.GetValue())
	}
	if !status.GetLoggedIn() {
		a.leftOut(p)
	}
	return nil
}

// Most sign-ins of other programs a provider is asked to leave out one after another, beside its variables
const outsidePrograms = 8

// Removes the provider's default login, or every login with all, and its saved keys; the profile's provider of sessions goes back to
// none when it named this one and nothing of it is left
func (a *auth) logout(name string, all bool) error {
	if strings.TrimSpace(name) == "" {
		for _, key := range a.choices {
			chosen, err := a.chosen(key)
			if err != nil {
				return err
			}
			if name = chosen; name != "" {
				break
			}
		}
		if name == "" {
			a.say("No provider is signed in, so there is nothing to sign out of.")
			return nil
		}
	}
	p, _, specs, err := a.find(name)
	if err != nil {
		return err
	}
	if p == nil {
		if kind(specs, name) != nil {
			a.say("%s has no account to remove.", name)
			return nil
		}
		return a.unknown(name, specs)
	}
	label := p.GetDisplayName()
	removed := false
	if !all {
		switch {
		case p.GetRotate():
			a.say("%s takes turns between several accounts. Remove one with `icebath auth remove %s <target>`, or all with `icebath logout %s --all`.",
				label, p.GetName(), p.GetName())
			return nil
		case p.GetAccount() != "":
			gone, err := a.service.RemoveAccount(a.ctx, &providerv1.RemoveAccountRequest{Profile: a.profile, Provider: p.GetName(), Account: p.GetAccount()})
			if err != nil {
				return a.fail("%s", tui.Reason(err))
			}
			a.say("✓ %s", gone.GetSaid())
			after, err := a.provider(p.GetName())
			if err != nil {
				return err
			}
			if left := len(after.GetAccounts()); left > 0 {
				if after.GetPinned() {
					a.say("%s now uses the account %s.", label, provider.AccountTitle(after, after.GetAccount()))
				}
				a.say("%s still has %s. The provider that new sessions use is unchanged.", label, strs.Count(left, "account"))
				return nil
			}
			removed = true
		}
	} else {
		for _, account := range p.GetAccounts() {
			if _, err := a.service.RemoveAccount(a.ctx, &providerv1.RemoveAccountRequest{Profile: a.profile, Provider: p.GetName(), Account: account.GetName()}); err != nil {
				return a.fail("%s", tui.Reason(err))
			}
			removed = true
		}
		after, err := a.provider(p.GetName())
		if err != nil {
			return err
		}
		var leave []string
		for _, variable := range after.GetKeyEnv() {
			if os.Getenv(variable) != "" {
				leave = append(leave, variable)
			}
		}
		// One the provider leaves out already changes nothing, so it is no sign-out
		for _, source := range leave {
			if source == "" || slices.Contains(after.GetIgnored(), source) {
				continue
			}
			if _, err := a.service.SetOutside(a.ctx, &providerv1.SetOutsideRequest{Profile: a.profile, Provider: p.GetName(), Source: source}); err != nil {
				return a.fail("%s", tui.Reason(err))
			}
			removed = true
		}
		// Whatever the provider still finds outside its accounts is left out too, one at a time as each stops being the one it finds: a
		// variable the profile's .env or a secret store supplies, or the sign-in of another program, whether it is ready to use it or not
		for range len(after.GetKeyEnv()) + outsidePrograms {
			now, err := a.provider(p.GetName())
			if err != nil {
				return err
			}
			source := credentials.SourceName(now.GetOutsideSource())
			if source == "" || slices.Contains(now.GetIgnored(), source) {
				break
			}
			if _, err := a.service.SetOutside(a.ctx, &providerv1.SetOutsideRequest{Profile: a.profile, Provider: p.GetName(), Source: source}); err != nil {
				return a.fail("%s", tui.Reason(err))
			}
			removed = true
		}
		if _, err := a.service.UseAccount(a.ctx, &providerv1.UseAccountRequest{Profile: a.profile, Provider: p.GetName()}); err != nil {
			return a.fail("%s", tui.Reason(err))
		}
	}
	reset := false
	for _, key := range a.choices {
		chosen, err := a.chosen(key)
		if err != nil {
			return err
		}
		if chosen != p.GetName() {
			continue
		}
		if err := a.set(key, nil); err != nil {
			return err
		}
		reset = true
	}
	if !removed && !reset {
		a.say("%s has no account to remove.", label)
		return nil
	}
	a.say("✓ Signed out of %s.", label)
	switch {
	case !reset:
		a.say("The provider that new sessions use is unchanged.")
	case os.Getenv("OPENROUTER_API_KEY") != "":
		a.say("icebath now uses OpenRouter, because OPENROUTER_API_KEY is set.")
	default:
		a.say("Connect another provider with `icebath auth add <provider>`.")
	}
	return nil
}

// ---- the bare auth command ----

// Every provider's logins and what signs the cloud ones in, then a choice of what to change
func (a *auth) interactive() error {
	a.say("%s", a.console.Paint("Your accounts", cli.Bold))
	a.say("")
	if err := a.list(""); err != nil {
		return err
	}
	a.clouds()
	choice, ok := a.console.Choose("What do you want to do?", []string{"Add an account", "Remove an account", "Clear the limits of a provider's accounts",
		"Choose a provider's rotation order"})
	if !ok {
		return nil
	}
	switch choice {
	case 0:
		return a.interactiveAdd()
	case 1:
		return a.interactiveRemove()
	case 2:
		p, ok, err := a.pick("Provider whose limits to clear")
		if err != nil || !ok {
			return err
		}
		return a.reset(p, "")
	}
	return a.interactiveStrategy()
}

// Status of the providers a cloud's own credential chain signs in, which keep no login of icebath
func (a *auth) clouds() {
	listed, err := a.service.List(a.ctx, &providerv1.ListRequest{Profile: a.profile})
	if err != nil {
		return
	}
	for _, p := range listed.GetProviders() {
		if len(p.GetAccounts()) > 0 || p.GetCredentialOrigin() != providerv1.Origin_ORIGIN_EXTERNAL {
			continue
		}
		status, err := a.service.Status(a.ctx, &providerv1.StatusRequest{Profile: a.profile, Provider: p.GetName()})
		if err != nil || len(status.GetDetails()) == 0 {
			continue
		}
		a.say("%s, signed in by %s:", p.GetName(), p.GetCredentialSource())
		for _, detail := range status.GetDetails() {
			a.say("  %s: %s", detail.GetName(), detail.GetValue())
		}
		a.say("")
	}
}

// Provider typed at the prompt, the known ones listed first; false at the end of the input
func (a *auth) pick(prompt string) (string, bool, error) {
	providers, specs, err := a.state()
	if err != nil {
		return "", false, err
	}
	var kinds, names []string
	for _, spec := range specs {
		kinds = append(kinds, spec.GetName())
	}
	for _, p := range providers {
		names = append(names, p.GetName())
	}
	slices.Sort(kinds)
	a.say("\nKinds you can add: %s", strings.Join(kinds, ", "))
	if len(names) > 0 {
		a.say("Providers of this profile: %s", strings.Join(names, ", "))
	}
	typed, ok := a.console.Input(prompt + ": ")
	return strings.TrimSpace(typed), ok, nil
}

func (a *auth) interactiveAdd() error {
	name, ok, err := a.pick("Provider to add an account to")
	if err != nil || !ok {
		return err
	}
	_, spec, specs, err := a.find(name)
	if err != nil {
		return err
	}
	if spec == nil {
		return a.unknown(name, specs)
	}
	opts := adding{kind: credentials.APIKey, priority: -1}
	if a.way(spec.GetName(), credentials.OAuth) != nil {
		if !spec.GetKeyed() {
			opts.kind = credentials.OAuth
		} else {
			a.say("\n%s takes an API key or a sign-in.", spec.GetName())
			a.say("  1. API key, pasted from the provider's website")
			a.say("  2. Sign in through your browser")
			choice, ok := a.console.Input("Number [1/2]: ")
			if !ok {
				return nil
			}
			if strings.TrimSpace(choice) == "2" {
				opts.kind = credentials.OAuth
			}
		}
	}
	return a.add(name, opts)
}

func (a *auth) interactiveRemove() error {
	name, ok, err := a.pick("Provider to remove an account from")
	if err != nil || !ok {
		return err
	}
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	rows := logins(p)
	if len(rows) == 0 {
		a.say("%s has no accounts to remove.", p.GetName())
		return nil
	}
	a.group(p)
	typed, ok := a.console.Input("Account to remove, by its number or name, or empty to cancel: ")
	if !ok || strings.TrimSpace(typed) == "" {
		return nil
	}
	return a.remove(p.GetName(), typed)
}

func (a *auth) interactiveStrategy() error {
	name, ok, err := a.pick("Provider whose rotation order to choose")
	if err != nil || !ok {
		return err
	}
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	current := p.GetStrategy()
	a.say("\nRotation order of %s: %s\n", p.GetName(), strategyTitle(current))
	for i, s := range strategies {
		mark := ""
		if s[0] == current {
			mark = " · In use"
		}
		a.say("  %d. %-12s %s%s", i+1, s[1], s[2], mark)
	}
	n, ok := a.console.Index(fmt.Sprintf("\nNumber from 1 to %d, or empty to keep it: ", len(strategies)), len(strategies), true, "")
	if !ok || n == 0 {
		return nil
	}
	if err := a.set(p.GetSection()+"."+providerconfig.Strategy, strategies[n-1][0]); err != nil {
		return err
	}
	a.say("✓ %s now takes turns in the order %s.", p.GetName(), strategies[n-1][1])
	return nil
}

// Wait left in words, rounded to the unit a person reads it in
func left(until time.Time) string {
	remaining := time.Duration(math.Ceil(time.Until(until).Seconds())) * time.Second
	switch {
	case remaining <= 0:
		return "ready now"
	case remaining >= 2*24*time.Hour:
		remaining = remaining.Round(24 * time.Hour)
	case remaining >= 2*time.Hour:
		remaining = remaining.Round(time.Hour)
	case remaining >= 2*time.Minute:
		remaining = remaining.Round(time.Minute)
	}
	return strs.Span(remaining) + " left"
}
