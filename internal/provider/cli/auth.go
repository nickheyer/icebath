package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/credentials"
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

// Kinds that sign in rather than take a key when auth add is told neither
var signsIn = map[string]bool{"anthropic": true, "openai-codex": true, "xai-oauth": true, "qwen-oauth": true, "minimax-oauth": true}

// Kinds whose sign-ins of other programs icebath may adopt
var adopting = map[string]bool{"anthropic": true, "openai-codex": true, "qwen-oauth": true}

// Strategies of a rotation as the interactive menu offers them
var strategies = [][2]string{{providerconfig.FillFirst, "Use first key until exhausted, then next"}, {providerconfig.RoundRobin, "Cycle through keys evenly"},
	{providerconfig.LeastUsed, "Always pick the least-used key"}, {providerconfig.Random, "Random selection"}}

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
}

// Prints the reason on the error stream and ends the command with status 1
func (a *auth) fail(format string, args ...any) error {
	fmt.Fprintf(a.errw, format+"\n", args...)
	return cli.Exit(1)
}

// Provider the setting that keeps the profile's choice names, empty for none
func (a *auth) chosen(key string) (string, error) {
	selected, err := a.settings.Get(a.ctx, &configv1.GetRequest{Key: key})
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
			choices: signins.Choices()}
	}
	run := func(do func(a *auth, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error { return do(with(cmd), args) }
	}
	root := &cobra.Command{Use: "auth", Short: "Manage named provider logins", Args: cobra.NoArgs, RunE: run(func(a *auth, _ []string) error { return a.interactive() })}

	var add adding
	addCmd := &cobra.Command{Use: "add <provider>", Short: "Add a named login", Args: cobra.ExactArgs(1),
		RunE: run(func(a *auth, args []string) error { return a.add(args[0], add) })}
	flags := addCmd.Flags()
	flags.StringVar(&add.kind, "type", "", "credential type to add: oauth, api-key or api_key")
	flags.StringVar(&add.name, "name", "", "login name, a lowercase slug unique per provider (default: from the account or label)")
	flags.BoolVar(&add.rotate, "rotate", false, "put the new login in the provider's rotation set")
	flags.BoolVar(&add.makeDefault, "default", false, "make the new login the provider's profile default")
	flags.StringVar(&add.label, "label", "", "optional display label")
	flags.IntVar(&add.priority, "priority", -1, "place the new credential at this priority (0 = tried first under fill_first); appends last when omitted")
	flags.StringVar(&add.key, "api-key", "", "API key value (otherwise prompted securely)")
	flags.BoolVar(&add.noBrowser, "no-browser", false, "do not auto-open a browser for OAuth login")
	flags.BoolVar(&add.browser, "browser", false, "openai-codex only: sign in with the browser authorization-code (PKCE) flow on http://localhost:1455/auth/callback "+
		"instead of the default device-code flow; falls back to device code when that port is busy (setting: providers.<name>.login_flow)")
	flags.Float64Var(&add.timeout, "timeout", 0, "OAuth/network timeout in seconds")

	var onExhausted string
	rotation := &cobra.Command{Use: "rotation <provider> [mode] [logins...]", Short: "Show or change the provider's rotation set: on, off, or a strategy",
		Args: cobra.MinimumNArgs(1), RunE: run(func(a *auth, args []string) error { return a.rotation(args[0], args[1:], onExhausted) })}
	rotation.Flags().StringVar(&onExhausted, "on-exhausted", "", "what a pinned login does at its limit outside the terminal UI: fail or rotate")

	var all bool
	logout := &cobra.Command{Use: "logout <provider>", Short: "Remove the provider's default login, or every login with --all", Args: cobra.ExactArgs(1),
		RunE: run(func(a *auth, args []string) error { return a.logout(args[0], all) })}
	logout.Flags().BoolVar(&all, "all", false, "remove every login and saved key of the provider")

	root.AddCommand(addCmd,
		&cobra.Command{Use: "list [provider]", Short: "List logins with their names, defaults, rotation and accounts", Args: cobra.MaximumNArgs(1),
			RunE: run(func(a *auth, args []string) error { return a.list(strs.First(args)) })},
		&cobra.Command{Use: "remove <provider> <target>", Short: "Remove one login by name, index, id, or label", Args: cobra.ExactArgs(2),
			RunE: run(func(a *auth, args []string) error { return a.remove(args[0], args[1]) })},
		&cobra.Command{Use: "use <provider> <target>", Short: "Make a login, or @rotate, the provider's profile default", Args: cobra.ExactArgs(2),
			RunE: run(func(a *auth, args []string) error { return a.use(args[0], args[1]) })},
		&cobra.Command{Use: "rename <provider> <target> <name>", Short: "Rename a login", Args: cobra.ExactArgs(3),
			RunE: run(func(a *auth, args []string) error { return a.rename(args[0], args[1], args[2]) })},
		rotation,
		&cobra.Command{Use: "reset <provider> [target]", Short: "Clear exhaustion status for a provider's credentials (all, or one target)", Args: cobra.RangeArgs(1, 2),
			RunE: run(func(a *auth, args []string) error { return a.reset(args[0], second(args)) })},
		&cobra.Command{Use: "priority <provider> <target> <priority>", Short: "Move a pooled credential to a priority (0 = tried first under fill_first)",
			Args: cobra.ExactArgs(3), RunE: run(func(a *auth, args []string) error { return a.priority(args[0], args[1], args[2]) })},
		&cobra.Command{Use: "refresh <provider> [target]", Short: "Refresh a pooled OAuth credential's tokens and clear its cooldown", Args: cobra.RangeArgs(1, 2),
			RunE: run(func(a *auth, args []string) error { return a.refresh(args[0], second(args)) })},
		&cobra.Command{Use: "status <provider>", Short: "Show auth status for a provider", Args: cobra.MaximumNArgs(1),
			RunE: run(func(a *auth, args []string) error { return a.status(strs.First(args)) })},
		logout,
	)

	var ignored string
	login := &cobra.Command{Use: "login", Hidden: true, Args: cobra.NoArgs,
		Long: "Deprecated. Use `icebath auth` to manage credentials, or start `icebath` and open /providers.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "The 'icebath login' command has been removed.\nUse 'icebath auth' to manage credentials,\n"+
				"or start 'icebath' and open /providers to connect a provider and /model to pick its model.")
			return nil
		}}
	login.Flags().StringVar(&ignored, "provider", "", "(deprecated) provider name; ignored, see `icebath auth add`")

	var named string
	var every bool
	top := &cobra.Command{Use: "logout [provider]", Short: "Clear authentication for an inference provider",
		Long: "Remove the provider's default login, or every login and saved key with --all", Args: cobra.MaximumNArgs(1),
		RunE: run(func(a *auth, args []string) error { return a.logout(cmp.Or(strs.First(args), named), every) })}
	top.Flags().StringVar(&named, "provider", "", "provider to log out from (default: active provider): openai-codex or xai-oauth")
	top.Flags().BoolVar(&every, "all", false, "remove every login and saved key of the provider, not only the default")
	return []*cobra.Command{root, login, top}
}

func second(args []string) string {
	if len(args) > 1 {
		return args[1]
	}
	return ""
}

// What auth add was asked for
type adding struct {
	kind, name, label, key string
	rotate, makeDefault    bool
	priority               int
	noBrowser, browser     bool
	timeout                float64
	region                 string
}

// Every provider of the profile and every kind it can add
func (a *auth) state() ([]*providerv1.Provider, []*providerv1.Spec, error) {
	listed, err := a.service.List(a.ctx, &providerv1.ListRequest{})
	if err != nil {
		return nil, nil, err
	}
	specs, err := a.service.Specs(a.ctx, &providerv1.SpecsRequest{})
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
	return a.fail("Unknown provider '%s'.%s Run `icebath auth` to see the provider list, or `icebath providers kinds` to see every kind a profile can add.", name, hint)
}

// Provider of the profile the name means, an error when there is none
func (a *auth) provider(name string) (*providerv1.Provider, error) {
	if strings.TrimSpace(name) == "" {
		return nil, a.fail("Provider is required.")
	}
	found, spec, specs, err := a.find(name)
	switch {
	case err != nil:
		return nil, err
	case found != nil:
		return found, nil
	case spec != nil:
		return nil, a.fail("Profile has no %s provider. Add a login with `icebath auth add %s`.", spec.GetDisplayName(), spec.GetName())
	}
	return nil, a.unknown(name, specs)
}

// One login of a provider as the commands list it: an account, or the credential the provider finds outside its accounts
type login struct {
	account *providerv1.Account
	// Variable or program of a credential found outside any account
	outside string
}

// Name a login goes by
func (l login) name() string {
	if l.account != nil {
		return l.account.GetName()
	}
	return slug(l.outside)
}

// Logins of the provider in the order they are tried
func logins(p *providerv1.Provider) []login {
	var out []login
	for _, account := range p.GetAccounts() {
		out = append(out, login{account: account})
	}
	origin := p.GetCredentialOrigin()
	if len(out) == 0 && (origin == providerv1.Origin_ORIGIN_ENVIRONMENT || origin == providerv1.Origin_ORIGIN_EXTERNAL) {
		out = append(out, login{outside: p.GetCredentialSource()})
	}
	return out
}

var slugOut = regexp.MustCompile(`[^a-z0-9]+`)

// Lowercase name made of the text, at most 40 characters
func slug(text string) string {
	name := strings.Trim(slugOut.ReplaceAllString(strings.ToLower(strings.TrimSpace(text)), "-"), "-")
	if len(name) > 40 {
		name = strings.TrimRight(name[:40], "-")
	}
	return name
}

// Login the target names: by its name, its number from 1, or a label no other login shares; the index and the refusal when none
func target(p *providerv1.Provider, raw string) (int, login, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, login{}, "No credential target provided."
	}
	rows := logins(p)
	for i, row := range rows {
		if row.name() == strings.ToLower(raw) {
			return i + 1, row, ""
		}
	}
	var labelled []int
	for i, row := range rows {
		if row.account != nil && strings.EqualFold(strings.TrimSpace(row.account.GetTitle()), raw) {
			labelled = append(labelled, i)
		}
	}
	switch len(labelled) {
	case 1:
		return labelled[0] + 1, rows[labelled[0]], ""
	case 0:
	default:
		return 0, login{}, fmt.Sprintf("Ambiguous credential label \"%s\". Use the numeric index or entry id instead.", raw)
	}
	if n, err := strconv.Atoi(raw); err == nil {
		if n >= 1 && n <= len(rows) {
			return n, rows[n-1], ""
		}
		return 0, login{}, fmt.Sprintf("No credential #%d.", n)
	}
	return 0, login{}, fmt.Sprintf("No credential matching \"%s\".", raw)
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
	_, err := a.settings.Set(a.ctx, &configv1.SetRequest{Key: key, Value: text})
	return err
}

// Values of the provider's own settings by their names below it, as the profile has them
func (a *auth) values(p *providerv1.Provider) map[string]string {
	values := map[string]string{}
	listed, err := a.settings.List(a.ctx, &configv1.ListRequest{})
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

// Signs a new login of the provider in, or an account already there again when --name names one
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
			return a.fail("--type takes oauth, api-key or api_key, not %q.", opts.kind)
		}
	}
	way := a.way(spec.GetName(), credentials.OAuth)
	if asked == "" {
		asked = credentials.APIKey
		if strings.TrimSpace(opts.key) == "" && way != nil && (signsIn[spec.GetName()] || spec.GetName() == "copilot" && a.console.Interactive()) {
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
		a.say("%s signs in with OAuth through the provider %s, starting its sign-in.", spec.GetName(), sibling)
		if spec.GetName() == "minimax-cn" {
			opts.region = "cn"
		}
		return a.add(sibling, opts)
	}
	if found == nil {
		added, err := a.service.Add(a.ctx, &providerv1.AddRequest{Spec: spec.GetName()})
		if err != nil {
			return err
		}
		found = added.GetProvider()
	}
	requested := strings.TrimSpace(opts.name)
	var again *providerv1.Account
	if requested != "" {
		for _, account := range found.GetAccounts() {
			if account.GetName() == requested {
				again = account
			}
		}
		if again == nil && !validName(requested) {
			return a.fail("login name %q must be lowercase letters, digits and dashes, starting with a letter or digit, at most 40 characters", requested)
		}
	}
	previous := found.GetAccount()
	if err := a.set(found.GetSection()+"."+providerconfig.Ignored, nil); err != nil {
		return err
	}
	var signed credentials.Signed
	label := strings.TrimSpace(opts.label)
	if chosen := a.way(spec.GetName(), asked); chosen != nil {
		signed, err = a.signIn(found, chosen, opts)
	} else {
		signed, label, err = a.key(found, opts, label)
	}
	if err != nil {
		if failure, ok := errors.AsType[*credentials.AuthError](err); ok {
			return a.fail("Login failed: %s", failure.Message)
		}
		if errors.Is(err, credentials.ErrCancelled) {
			return a.fail("Login cancelled.")
		}
		return err
	}
	if again != nil {
		if err := a.store(found, again.GetName(), signed); err != nil {
			return err
		}
		a.say("Signed in again to the %s login %s.", found.GetName(), again.GetName())
		return a.place(found, again.GetName(), opts, previous)
	}
	accountName := requested
	if accountName == "" {
		accountName = free(found, cmp.Or(slug(signed.Identity), slug(label), found.GetName()+map[bool]string{true: "-oauth", false: "-key"}[asked == credentials.OAuth]))
	}
	title := cmp.Or(label, signed.Identity, accountName)
	account, err := a.service.AddAccount(a.ctx, &providerv1.AddAccountRequest{Provider: found.GetName(), Name: accountName, Title: title})
	if err != nil {
		return err
	}
	if err := a.store(found, account.GetAccount().GetName(), signed); err != nil {
		return err
	}
	a.warnSame(found, account.GetAccount().GetName(), signed)
	a.say("Added the %s login %s.", found.GetName(), account.GetAccount().GetName())
	return a.place(found, account.GetAccount().GetName(), opts, previous)
}

// Applies --rotate, --default and --priority to a login just signed in, and says how to use it
func (a *auth) place(p *providerv1.Provider, name string, opts adding, previous string) error {
	section := p.GetSection() + "." + providerconfig.Accounts + "." + name
	if opts.rotate {
		if err := a.set(section+"."+providerconfig.InRotation, true); err != nil {
			return err
		}
	}
	switch {
	case opts.makeDefault:
		if _, err := a.service.UseAccount(a.ctx, &providerv1.UseAccountRequest{Provider: p.GetName(), Account: name}); err != nil {
			return err
		}
		a.say("%s now uses %s by default.", p.GetName(), name)
	case previous != "" && previous != name:
		a.say("The default login is unchanged. Use this one with `icebath auth use %s %s`.", p.GetName(), name)
	}
	if opts.priority < 0 {
		return nil
	}
	return a.move(p.GetName(), name, opts.priority, "Placed", "at")
}

// Moves a login to the priority and says where it landed, and why when not where asked
func (a *auth) move(provider, name string, priority int, verb, prep string) error {
	moved, err := a.service.MoveAccount(a.ctx, &providerv1.MoveAccountRequest{Provider: provider, Account: name, Priority: int32(priority)})
	if err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	at := int(moved.GetPriority())
	a.say("%s %s credential \"%s\" %s priority %d (#%d in `icebath auth list %s`)", verb, provider, name, prep, at, at+1, provider)
	refreshed, _ := a.provider(provider)
	if at != priority {
		fmt.Fprintf(a.errw, "note: requested priority %d; effective priority is %d because the pool has %d credentials, so it was clamped.\n",
			priority, at, len(refreshed.GetAccounts()))
	}
	if strategy := refreshed.GetStrategy(); strategy != "" && strategy != providerconfig.FillFirst {
		fmt.Fprintf(a.errw, "note: %s uses the %s strategy; priority only orders fill_first selection.\n", provider, strategy)
	}
	return nil
}

// Free name for a new login of the provider from the base, numbered from 2 when taken
func free(p *providerv1.Provider, base string) string {
	base = cmp.Or(slug(base), "login")
	taken := func(name string) bool {
		return slices.ContainsFunc(p.GetAccounts(), func(account *providerv1.Account) bool { return account.GetName() == name })
	}
	for n, name := 2, base; ; n, name = n+1, base+"-"+strconv.Itoa(n) {
		if !taken(name) {
			return name
		}
	}
}

var loginName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func validName(name string) bool { return loginName.MatchString(name) }

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
		return "Bedrock has no OAuth sign-in of its own; AWS signs it in. Run `aws sso login` or `aws configure` (or set AWS_PROFILE), " +
			"or add a Bedrock API key with `icebath auth add bedrock --type api-key`."
	case "vertex":
		return "Vertex has no OAuth sign-in of its own; Google signs it in through Application Default Credentials. " +
			"Run `gcloud auth application-default login`, or add a service account JSON with `icebath auth add vertex`."
	}
	return fmt.Sprintf("`%s` signs in with an API key only; it offers no OAuth sign-in. Add a key with `icebath auth add %s --type api-key`. "+
		"OAuth sign-in exists for %s.", kind, kind, strings.Join(oauth, ", "))
}

// Asks for an API key and a label the way auth add does when no plugin adds its own key prompt
func (a *auth) key(p *providerv1.Provider, opts adding, label string) (credentials.Signed, string, error) {
	key := strings.TrimSpace(opts.key)
	if key == "" {
		typed, ok := a.console.Secret("Paste your API key: ")
		if !ok {
			return credentials.Signed{}, label, credentials.ErrCancelled
		}
		key = strings.TrimSpace(typed)
	}
	if key == "" {
		return credentials.Signed{}, label, a.fail("No API key provided.")
	}
	if label == "" && a.console.Interactive() {
		fallback := fmt.Sprintf("api-key-%d", len(p.GetAccounts())+1)
		typed, ok := a.console.Input(fmt.Sprintf("Label (optional, default: %s): ", fallback))
		if !ok {
			return credentials.Signed{}, label, credentials.ErrCancelled
		}
		label = cmp.Or(strings.TrimSpace(typed), fallback)
	}
	return credentials.Signed{Fields: map[string]any{providerconfig.APIKeyField: key}}, label, nil
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
	}
	return way.Run(prompter)
}

// Keeps the fields a sign-in gave in the account
func (a *auth) store(p *providerv1.Provider, name string, signed credentials.Signed) error {
	section := p.GetSection() + "." + providerconfig.Accounts + "." + name
	if err := a.set(section+"."+providerconfig.Status, nil); err != nil {
		return err
	}
	for field, value := range signed.Fields {
		if err := a.set(section+"."+field, value); err != nil {
			return err
		}
	}
	return nil
}

// Warns when a new login signs in as an account another login of the provider already is
func (a *auth) warnSame(p *providerv1.Provider, name string, signed credentials.Signed) {
	if signed.Identity == "" || p.GetSpec() != "openai-codex" {
		return
	}
	for i, account := range p.GetAccounts() {
		if account.GetName() != name && account.GetIdentity() == signed.Identity {
			fmt.Fprintf(a.errw, "warning: this login is the same OpenAI account as openai-codex credential #%d (\"%s\"). Both logins share one token "+
				"family, so OpenAI will revoke the older one and you gain no extra quota. Log into a different account instead, or keep just one "+
				"(`icebath auth remove %s %d`).\n", i+1, account.GetTitle(), p.GetName(), i+1)
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
		listed, err := a.service.List(a.ctx, &providerv1.ListRequest{})
		if err != nil {
			return err
		}
		providers = listed.GetProviders()
	}
	for _, p := range providers {
		a.group(p)
	}
	for _, p := range providers {
		if adopting[p.GetSpec()] && a.values(p)["adopt_external_logins"] == "false" {
			a.say("Logins from other CLIs (Codex CLI, Claude Code, Qwen CLI) are not used by %s because its adopt_external_logins setting is off. "+
				"Turn it on in /settings to use them.", p.GetName())
		}
	}
	return nil
}

// Header of a provider's logins: how many, its default, and its rotation when it rotates
func header(p *providerv1.Provider, n int) string {
	def := "none (every login in order)"
	switch {
	case p.GetRotate():
		def = providerconfig.Rotation
	case p.GetPinned():
		def = p.GetAccount()
	}
	noun := "logins"
	if n == 1 {
		noun = "login"
	}
	head := fmt.Sprintf("%s (%d %s, default: %s", p.GetName(), n, noun, def)
	if p.GetRotate() {
		head += ", strategy: " + p.GetStrategy()
	}
	if p.GetOnExhausted() == providerconfig.Rotate {
		head += ", on_exhausted: rotate"
	}
	return head + "):"
}

// Prints one provider's logins
func (a *auth) group(p *providerv1.Provider) {
	rows := logins(p)
	if len(rows) == 0 {
		return
	}
	a.say("%s", header(p, len(rows)))
	for i, row := range rows {
		a.say("%s", line(p, i+1, row))
	}
	a.say("")
}

// Where a login's secret lives: owned by icebath, or linked to a variable or program it is read from
func origin(row login) (string, string) {
	if row.account == nil {
		if row.outside == "Claude Code" || row.outside == "Qwen CLI" {
			return "linked", row.outside
		}
		return "linked", "env:" + row.outside
	}
	account := row.account
	switch {
	case account.GetKeyEnv() != "":
		return "linked", "env:" + account.GetKeyEnv()
	case account.GetCredentialOrigin() == providerv1.Origin_ORIGIN_ENVIRONMENT:
		return "linked", "env:" + account.GetCredentialSource()
	case account.GetCredentialOrigin() == providerv1.Origin_ORIGIN_EXTERNAL:
		return "linked", account.GetCredentialSource()
	}
	return "owned", "icebath"
}

// One row of auth list with its marks
func line(p *providerv1.Provider, index int, row login) string {
	account, priority := "-", 0
	authType := "api_key"
	status, until := "ready", ""
	var marks []string
	active := row.account == nil
	label := ""
	if row.account != nil {
		a := row.account
		authType = cmp.Or(a.GetAuth(), authType)
		account = cmp.Or(a.GetIdentity(), "-")
		priority = int(a.GetPriority())
		status = a.GetStatus()
		if resets := a.GetResets(); resets != nil {
			until = " until " + resets.AsTime().Local().Format("15:04") + " (" + left(resets.AsTime()) + ")"
		}
		if p.GetPinned() && a.GetName() == p.GetAccount() {
			marks = append(marks, "default")
		}
		if a.GetRotate() {
			marks = append(marks, "rotate")
		}
		active = a.GetActive()
		if title := a.GetTitle(); title != a.GetName() {
			label = title
		}
	}
	where, source := origin(row)
	text := fmt.Sprintf("  #%d  %-20s %-7s %-6s %-26s %-28s priority=%d", index, row.name(), authType, where, source, account, priority)
	if len(marks) > 0 {
		text += " [" + strings.Join(marks, ",") + "]"
	}
	if status != "" && status != "ready" {
		text += " " + strings.ReplaceAll(status, "_", " ") + until
	}
	if label != "" {
		text += fmt.Sprintf(" label=%q", label)
	}
	if source == "env:GITHUB_TOKEN" || source == "env:GH_TOKEN" {
		text += " (also used by git)"
	}
	if active {
		text += " ← in use"
	}
	return strings.TrimRight(text, " ")
}

// ---- remove, use, rename, rotation, reset, priority, refresh ----

func (a *auth) remove(name, raw string) error {
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	index, row, refused := target(p, raw)
	if index == 0 {
		return a.fail("%s Provider: %s.", refused, p.GetName())
	}
	if row.account == nil {
		return a.ignore(p, row.outside)
	}
	removed, err := a.service.RemoveAccount(a.ctx, &providerv1.RemoveAccountRequest{Provider: p.GetName(), Account: row.account.GetName()})
	if err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	a.say("Removed %s credential #%d (%s)", p.GetName(), index, row.account.GetTitle())
	for _, variable := range removed.GetCleared() {
		a.say("Cleared %s from .env", variable)
	}
	if p.GetPinned() && p.GetAccount() == row.account.GetName() {
		after, err := a.provider(p.GetName())
		if err != nil {
			return err
		}
		if after.GetPinned() {
			a.say("%s now uses the login %s by default.", p.GetName(), after.GetAccount())
		} else {
			a.say("%s has no default login now.", p.GetName())
		}
	}
	return nil
}

// Leaves a variable or program out of the places the provider takes its credential from, saying what that means
func (a *auth) ignore(p *providerv1.Provider, source string) error {
	left, err := a.service.RemoveAccount(a.ctx, &providerv1.RemoveAccountRequest{Provider: p.GetName(), Source: source})
	if err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	cleared := slices.Contains(left.GetCleared(), source)
	if cleared {
		a.say("Cleared %s from .env", source)
	}
	switch source {
	case "Claude Code":
		a.say("Suppressed claude_code credential — it will not be re-seeded.\nNote: Claude Code credentials still live in ~/.claude/.credentials.json\n" +
			"Run `icebath auth add anthropic` to re-enable if needed.")
	case "Qwen CLI":
		a.say("Suppressed qwen-cli credential — it will not be re-seeded.\nNote: Qwen CLI credentials still live in ~/.qwen/oauth_creds.json\n" +
			"Run `icebath auth add qwen-oauth` to re-enable if needed.")
	default:
		if os.Getenv(source) != "" && !cleared {
			a.say("Note: %s is still set in your shell environment.\n  Unset it there (shell profile, systemd EnvironmentFile, launchd plist, etc.) "+
				"or it will keep being visible to Icebath.\n  The credential is now suppressed — Icebath will ignore %s until you run `icebath auth add %s`.",
				source, source, p.GetName())
		} else {
			a.say("Suppressed env:%s — it will not be re-seeded even if the variable is re-exported later.", source)
		}
	}
	if after, err := a.provider(p.GetName()); err == nil && after.GetCredentialSource() != "" && after.GetCredentialSource() != source && len(after.GetAccounts()) == 0 {
		a.say("%s now takes its credential from %s.", p.GetName(), after.GetCredentialSource())
	}
	return nil
}

func (a *auth) use(name, raw string) error {
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(raw) == "" {
		return a.fail("Name a login or @rotate. Run `icebath auth list %s` to see them.", p.GetName())
	}
	account := providerconfig.Rotation
	if raw != providerconfig.Rotation {
		index, row, refused := target(p, raw)
		switch {
		case index == 0:
			return a.fail("%s", refused)
		case row.account == nil:
			return a.fail("%s is read from %s and is the provider's only credential, so it is already in use.", row.name(), row.outside)
		}
		account = row.account.GetName()
	}
	if _, err := a.service.UseAccount(a.ctx, &providerv1.UseAccountRequest{Provider: p.GetName(), Account: account}); err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	if account == providerconfig.Rotation {
		a.say("%s now rotates through its logins marked rotate (strategy %s).", p.GetName(), p.GetStrategy())
		return nil
	}
	a.say("%s now uses the login %s by default. Sessions pinned to another login keep theirs.", p.GetName(), account)
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
		return a.fail("%s is read from %s, not kept by icebath, so it has no name to change.", row.name(), row.outside)
	case !validName(strings.TrimSpace(to)):
		return a.fail("login name %q must be lowercase letters, digits and dashes, starting with a letter or digit, at most 40 characters", to)
	}
	renamed, err := a.service.RenameAccount(a.ctx, &providerv1.RenameAccountRequest{Provider: p.GetName(), Account: row.account.GetName(), Name: to})
	if err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	a.say("Renamed %s login %s to %s.", p.GetName(), row.account.GetName(), renamed.GetAccount().GetName())
	return nil
}

func (a *auth) rotation(name string, args []string, onExhausted string) error {
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	if onExhausted = strings.ToLower(strings.TrimSpace(onExhausted)); onExhausted != "" {
		if onExhausted != providerconfig.Fail && onExhausted != providerconfig.Rotate {
			return a.fail("on_exhausted takes fail or rotate, got %q", onExhausted)
		}
		if err := a.set(p.GetSection()+"."+providerconfig.OnExhausted, onExhausted); err != nil {
			return err
		}
		a.say("%s.%s: %s", p.GetSection(), providerconfig.OnExhausted, onExhausted)
	}
	mode := strings.ToLower(strings.TrimSpace(strs.First(args)))
	targets := args[min(len(args), 1):]
	isStrategy := slices.ContainsFunc(strategies, func(s [2]string) bool { return s[0] == mode })
	switch {
	case mode == "":
		if p, err = a.provider(name); err != nil {
			return err
		}
		a.say("%s", header(p, len(p.GetAccounts())))
		for _, account := range p.GetAccounts() {
			state := "off"
			if account.GetRotate() {
				state = "on"
			}
			a.say("  %-20s rotate=%s", account.GetName(), state)
		}
		return nil
	case isStrategy:
		if err := a.set(p.GetSection()+"."+providerconfig.Strategy, mode); err != nil {
			return err
		}
		a.say("Set %s rotation strategy to %s.", p.GetName(), mode)
		return nil
	case mode != "on" && mode != "off":
		return a.fail("Unknown rotation mode %q. Use on, off, fill_first, round_robin, least_used or random.", mode)
	}
	on := mode == "on"
	if len(targets) == 0 && !on {
		if !p.GetRotate() {
			a.say("%s is not rotating.", p.GetName())
			return nil
		}
		keep := p.GetAccount()
		if keep == "" {
			return a.fail("No %s login is ready to keep. Pick one with `icebath auth use %s <login>`.", p.GetName(), p.GetName())
		}
		if _, err := a.service.UseAccount(a.ctx, &providerv1.UseAccountRequest{Provider: p.GetName(), Account: keep}); err != nil {
			return a.fail("%s", tui.Reason(err))
		}
		a.say("%s stopped rotating and uses %s. Logins keep their rotate marks for next time.", p.GetName(), keep)
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
			return a.fail("%s", cmp.Or(refused, "Only logins kept by icebath take part in a rotation."))
		}
		if err := a.set(p.GetSection()+"."+providerconfig.Accounts+"."+row.account.GetName()+"."+providerconfig.InRotation, on); err != nil {
			return err
		}
		a.say("%s login %s: rotate=%s", p.GetName(), row.account.GetName(), mode)
	}
	if on {
		if _, err := a.service.UseAccount(a.ctx, &providerv1.UseAccountRequest{Provider: p.GetName(), Account: providerconfig.Rotation}); err != nil {
			return a.fail("%s", tui.Reason(err))
		}
		a.say("%s now rotates through its rotate logins (strategy %s).", p.GetName(), p.GetStrategy())
	}
	return nil
}

func (a *auth) reset(name, raw string) error {
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(raw) == "" {
		cleared, err := a.service.Reset(a.ctx, &providerv1.ResetRequest{Provider: p.GetName()})
		if err != nil {
			return a.fail("%s", tui.Reason(err))
		}
		a.say("Reset status on %d %s credentials", cleared.GetCleared(), p.GetName())
		return nil
	}
	index, row, refused := target(p, raw)
	if index == 0 || row.account == nil {
		return a.fail("%s Provider: %s.", cmp.Or(refused, "Only logins kept by icebath have a status to reset."), p.GetName())
	}
	if _, err := a.service.Reset(a.ctx, &providerv1.ResetRequest{Provider: p.GetName(), Account: row.account.GetName()}); err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	a.say("Reset status on %s credential #%d (%s)", p.GetName(), index, row.account.GetTitle())
	return nil
}

func (a *auth) priority(name, raw, place string) error {
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	index, row, refused := target(p, raw)
	if index == 0 || row.account == nil {
		return a.fail("%s Provider: %s.", cmp.Or(refused, "Only logins kept by icebath have a priority."), p.GetName())
	}
	n, err := strconv.Atoi(strings.TrimSpace(place))
	if err != nil {
		return a.fail("auth priority needs an integer priority, got %q", place)
	}
	return a.move(p.GetName(), row.account.GetName(), n, "Set", "to")
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
		return a.fail("No %s credentials in the pool.", p.GetName())
	case strings.TrimSpace(raw) == "" && len(rows) != 1:
		return a.fail("%s has %d credentials; pass an index, entry id, or exact label (see `icebath auth list %s`).", p.GetName(), len(rows), p.GetName())
	case strings.TrimSpace(raw) == "":
		index, row = 1, rows[0]
	default:
		var refused string
		if index, row, refused = target(p, raw); index == 0 {
			return a.fail("%s Provider: %s.", refused, p.GetName())
		}
	}
	account, label := "", row.name()
	if row.account != nil {
		account, label = row.account.GetName(), row.account.GetTitle()
	}
	renewed, err := a.service.Renew(a.ctx, &providerv1.RenewRequest{Provider: p.GetName(), Account: account})
	if err != nil {
		if failure, ok := errors.AsType[*connect.Error](err); ok && failure.Code() == connect.CodeUnimplemented {
			return a.fail("%s credential #%d (%s) is not a refreshable OAuth credential.", p.GetName(), index, label)
		}
		return a.fail("Could not renew the %s sign-in for credential #%d (%s); %s. Sign in again with `icebath auth add %s --type oauth`.",
			p.GetDisplayName(), index, label, strings.TrimSuffix(tui.Reason(err), "."), p.GetName())
	}
	status := cmp.Or(renewed.GetAccount().GetStatus(), "ready")
	if status == "ready" {
		a.say("Refreshed %s credential #%d (%s); status: ok", p.GetName(), index, label)
		return nil
	}
	a.say("Adopted current tokens for %s credential #%d (%s); status still: %s", p.GetName(), index, label, status)
	return nil
}

// ---- status, logout ----

func (a *auth) status(name string) error {
	if strings.TrimSpace(name) == "" {
		return a.fail("Provider is required. Example: `icebath auth status openai-codex`.")
	}
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	status, err := a.service.Status(a.ctx, &providerv1.StatusRequest{Provider: p.GetName()})
	if err != nil {
		return a.fail("%s", tui.Reason(err))
	}
	if !status.GetLoggedIn() {
		text := p.GetName() + ": logged out"
		if why := status.GetError(); why != "" {
			text += " (" + why + ")"
		}
		a.say("%s", text)
	} else {
		a.say("%s: logged in", p.GetName())
	}
	for _, detail := range status.GetDetails() {
		a.say("  %s: %s", detail.GetName(), detail.GetValue())
	}
	if !status.GetLoggedIn() && adopting[p.GetSpec()] && a.values(p)["adopt_external_logins"] == "false" {
		a.say("Logins from other CLIs (Codex CLI, Claude Code, Qwen CLI) are not used by %s because its adopt_external_logins setting is off.", p.GetName())
	}
	return nil
}

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
			a.say("No provider is currently logged in.")
			return nil
		}
	}
	p, _, specs, err := a.find(name)
	if err != nil {
		return err
	}
	if p == nil {
		if kind(specs, name) != nil {
			a.say("No auth state found for %s.", name)
			return nil
		}
		return a.fail("Unknown provider: %s", name)
	}
	label := p.GetDisplayName()
	removed := false
	if !all {
		switch {
		case p.GetRotate():
			a.say("%s takes turns between several logins. Remove one with `icebath auth remove %s <login>`, or all with `icebath logout %s --all`.",
				label, p.GetName(), p.GetName())
			return nil
		case p.GetAccount() != "":
			if _, err := a.service.RemoveAccount(a.ctx, &providerv1.RemoveAccountRequest{Provider: p.GetName(), Account: p.GetAccount()}); err != nil {
				return a.fail("%s", tui.Reason(err))
			}
			a.say("Removed the %s login %s.", p.GetName(), p.GetAccount())
			after, err := a.provider(p.GetName())
			if err != nil {
				return err
			}
			if left := len(after.GetAccounts()); left > 0 {
				if after.GetPinned() {
					a.say("%s now uses the login %s by default.", p.GetName(), after.GetAccount())
				}
				a.say("%s still has %d login(s). Model provider configuration was unchanged.", label, left)
				return nil
			}
			removed = true
		}
	} else {
		for _, account := range p.GetAccounts() {
			if _, err := a.service.RemoveAccount(a.ctx, &providerv1.RemoveAccountRequest{Provider: p.GetName(), Account: account.GetName()}); err != nil {
				return a.fail("%s", tui.Reason(err))
			}
			removed = true
		}
		after, err := a.provider(p.GetName())
		if err != nil {
			return err
		}
		ignored := slices.Clone(after.GetIgnored())
		for _, variable := range after.GetKeyEnv() {
			if os.Getenv(variable) != "" && !slices.Contains(ignored, variable) {
				ignored = append(ignored, variable)
			}
		}
		if source := after.GetCredentialSource(); after.GetCredentialOrigin() == providerv1.Origin_ORIGIN_EXTERNAL && !slices.Contains(ignored, source) {
			ignored = append(ignored, source)
		}
		if len(ignored) > len(after.GetIgnored()) {
			if err := a.set(p.GetSection()+"."+providerconfig.Ignored, ignored); err != nil {
				return err
			}
			removed = true
		}
		// Variables the profile's .env or a secret store supplies are left out too, one at a time as each stops being the one in use
		for range len(after.GetKeyEnv()) + 1 {
			now, err := a.provider(p.GetName())
			if err != nil {
				return err
			}
			variable, _, _ := strings.Cut(now.GetCredentialSource(), " (from ")
			if now.GetCredentialOrigin() != providerv1.Origin_ORIGIN_ENVIRONMENT || variable == "" || slices.Contains(now.GetIgnored(), variable) {
				break
			}
			if err := a.set(p.GetSection()+"."+providerconfig.Ignored, append(slices.Clone(now.GetIgnored()), variable)); err != nil {
				return err
			}
			removed = true
		}
		if err := a.set(p.GetSection()+"."+providerconfig.Account, nil); err != nil {
			return err
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
		a.say("No auth state found for %s.", label)
		return nil
	}
	a.say("Logged out of %s.", label)
	switch {
	case !reset:
		a.say("Model provider configuration was unchanged.")
	case os.Getenv("OPENROUTER_API_KEY") != "":
		a.say("Icebath will use OpenRouter for inference.")
	default:
		a.say("Run `icebath providers` or configure an API key to use Icebath.")
	}
	return nil
}

// ---- the bare auth command ----

// Every provider's logins and what signs the cloud ones in, then a choice of what to change
func (a *auth) interactive() error {
	a.say("Credential Pool Status")
	a.say("%s", strings.Repeat("=", 50))
	if err := a.list(""); err != nil {
		return err
	}
	a.clouds()
	a.say("")
	choices := []string{"Add a credential", "Remove a credential", "Reset cooldowns for a provider", "Set rotation strategy for a provider", "Exit"}
	a.say("What would you like to do?")
	for i, choice := range choices {
		a.say("  %d. %s", i+1, choice)
	}
	choice, ok := a.console.Input("\nChoice: ")
	if !ok {
		return nil
	}
	switch strings.TrimSpace(choice) {
	case "1":
		return a.interactiveAdd()
	case "2":
		return a.interactiveRemove()
	case "3":
		p, ok, err := a.pick("Provider to reset cooldowns for")
		if err != nil || !ok {
			return err
		}
		return a.reset(p, "")
	case "4":
		return a.interactiveStrategy()
	}
	return nil
}

// Status of the providers a cloud's own credential chain signs in, which keep no login of icebath
func (a *auth) clouds() {
	listed, err := a.service.List(a.ctx, &providerv1.ListRequest{})
	if err != nil {
		return
	}
	for _, p := range listed.GetProviders() {
		if len(p.GetAccounts()) > 0 || p.GetCredentialOrigin() != providerv1.Origin_ORIGIN_EXTERNAL {
			continue
		}
		status, err := a.service.Status(a.ctx, &providerv1.StatusRequest{Provider: p.GetName()})
		if err != nil || len(status.GetDetails()) == 0 {
			continue
		}
		a.say("%s (%s):", p.GetName(), p.GetCredentialSource())
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
	a.say("\nKnown providers: %s", strings.Join(kinds, ", "))
	if len(names) > 0 {
		a.say("In this profile: %s", strings.Join(names, ", "))
	}
	typed, ok := a.console.Input(prompt + ": ")
	return strings.TrimSpace(typed), ok, nil
}

func (a *auth) interactiveAdd() error {
	name, ok, err := a.pick("Provider to add credential for")
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
			a.say("\n%s supports both API keys and OAuth login.", spec.GetName())
			a.say("  1. API key (paste a key from the provider dashboard)")
			a.say("  2. OAuth login (authenticate via browser)")
			choice, ok := a.console.Input("Type [1/2]: ")
			if !ok {
				return nil
			}
			if strings.TrimSpace(choice) == "2" {
				opts.kind = credentials.OAuth
			}
		}
	}
	label, ok := a.console.Input("Label / account name (optional): ")
	if !ok {
		return nil
	}
	opts.label = strings.TrimSpace(label)
	return a.add(name, opts)
}

func (a *auth) interactiveRemove() error {
	name, ok, err := a.pick("Provider to remove credential from")
	if err != nil || !ok {
		return err
	}
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	rows := logins(p)
	if len(rows) == 0 {
		a.say("No credentials for %s.", p.GetName())
		return nil
	}
	for i, row := range rows {
		a.say("%s", line(p, i+1, row))
	}
	typed, ok := a.console.Input("Remove #, id, or label (blank to cancel): ")
	if !ok || strings.TrimSpace(typed) == "" {
		return nil
	}
	return a.remove(p.GetName(), typed)
}

func (a *auth) interactiveStrategy() error {
	name, ok, err := a.pick("Provider to set strategy for")
	if err != nil || !ok {
		return err
	}
	p, err := a.provider(name)
	if err != nil {
		return err
	}
	current := p.GetStrategy()
	a.say("\nCurrent strategy for %s: %s\n", p.GetName(), current)
	for i, s := range strategies {
		mark := ""
		if s[0] == current {
			mark = " ←"
		}
		a.say("  %d. %-15s — %s%s", i+1, s[0], s[1], mark)
	}
	n, ok := a.console.Index("\nStrategy [1-4]: ", len(strategies), true, "")
	if !ok || n == 0 {
		return nil
	}
	if err := a.set(p.GetSection()+"."+providerconfig.Strategy, strategies[n-1][0]); err != nil {
		return err
	}
	a.say("Set %s strategy to: %s", p.GetName(), strategies[n-1][0])
	return nil
}

// Wait left as the two largest of days, hours, minutes and seconds
func left(until time.Time) string {
	remaining := int(math.Ceil(time.Until(until).Seconds()))
	if remaining <= 0 {
		return "ready to retry"
	}
	values := []int{remaining / 86400, remaining % 86400 / 3600, remaining % 3600 / 60, remaining % 60}
	units := []string{"d", "h", "m", "s"}
	start := slices.IndexFunc(values, func(v int) bool { return v != 0 })
	var parts []string
	for i := start; i < len(values) && i < start+2; i++ {
		parts = append(parts, strconv.Itoa(values[i])+units[i])
	}
	return strings.Join(parts, " ") + " left"
}
