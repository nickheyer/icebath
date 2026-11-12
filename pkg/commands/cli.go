package commands

import (
	"context"
	"errors"
	"fmt"
	"github.com/nickheyer/icebath/pkg/profiles"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydifflib"
	"github.com/nickheyer/icebath/pkg/provider"
)

// CLIHandler runs one bound CLI subcommand
type CLIHandler func(cmd *cobra.Command, args []string) error

// ExitError carries a process exit code and an optional message for stderr
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string {
	if e.Msg == "" {
		return "exit " + strconv.Itoa(e.Code)
	}
	return e.Msg
}

// UnboundCLIError reports a CLI command the host never bound a handler to
type UnboundCLIError struct{ Path string }

func (e *UnboundCLIError) Error() string {
	return "icebath: command `icebath " + e.Path + "` has no handler bound"
}

// FlagPresent is the value an optional argument flag takes when given without a value
const FlagPresent = "__present__"

// CLI is the cobra command tree with handlers bound by path
type CLI struct {
	Root        *cobra.Command
	Profile     string
	handlers    map[string]CLIHandler
	positionals map[*cobra.Command][]posSpec
	flagAliases map[*cobra.Command]map[string]string
	argv        []string
}

type posSpec struct {
	Name     string
	Optional bool
	Variadic bool
	Remain   bool
	Int      bool
	Choices  []string
	Default  string
	Help     string
	Metavar  string
}

// Same positional shown under another name in usage and errors, the argparse metavar
func (s posSpec) withMetavar(metavar string) posSpec {
	s.Metavar = metavar
	return s
}

// Name usage, help and error messages show for the positional
func (s posSpec) display() string {
	if s.Metavar != "" {
		return s.Metavar
	}
	return s.Name
}

// Same positional carrying the help line the positional arguments section shows
func (s posSpec) withHelp(help string) posSpec {
	s.Help = help
	return s
}

// PreArgparseInheritedFlags are consumed before parsing and carried over on relaunch
var PreArgparseInheritedFlags = [][2]string{{"--profile", "1"}, {"-p", "1"}}

// BuiltinSubcommands are the top level commands plugin discovery never needs to resolve
var BuiltinSubcommands = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`acp approvals auth backup bundles checkpoints codex-runtime completion computer-use connectors
		config console cron curator dashboard serve debug doctor dump egress fallback gateway hooks import insights
		gui desktop kanban login logout logs lsp mcp memory migrate moa journey memory-graph learning
		model monitoring pairing pause peer pets plugins portal profile project proxy prompt-size resume send sessions setup
		skin skills slack status tools uninstall update usage vault webhook whatsapp whatsapp-cloud worktree chat secrets security
		browser verify help`) {
		BuiltinSubcommands[n] = true
	}
	cobra.EnableCommandSorting = false
}

var profileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func cfgPathForHelp() string {
	return home.Display(home.Process()) + "/config.yaml"
}

const rootEpilog = `Examples:
    icebath                        Start interactive chat
    icebath chat -q "Hello"        Single query mode
    icebath --tui                  Launch the modern TUI (or set display.interface: tui)
    icebath --cli                  Force the classic REPL (overrides display.interface: tui)
    icebath -c                     Resume the most recent session
    icebath -c "my project"        Resume a session by name (latest in lineage)
    icebath --resume <session_id>  Resume a specific session by ID
    icebath --resume latest        Resume the most recent session (same as -c)
    icebath --tui --resume latest --in ./dir   Resume ./dir's latest session in the TUI
    icebath setup                  Run setup wizard
    icebath logout                 Clear stored authentication
    icebath auth add <provider>    Add a pooled credential
    icebath auth list              List pooled credentials
    icebath auth remove <p> <t>    Remove pooled credential by index, id, or label
    icebath auth reset <p> [t]     Clear exhaustion status for a provider, or one credential
    icebath auth priority <p> <t> <n>  Move a pooled credential to priority n (0 = tried first)
    icebath auth refresh <p> [t]   Refresh a pooled OAuth credential and clear its cooldown
    icebath model                  Select default model
    icebath fallback [list]        Show fallback provider chain
    icebath fallback add           Add a fallback provider (same picker as ` + "`icebath model`" + `)
    icebath fallback remove        Remove a fallback provider from the chain
    icebath config                 View configuration
    icebath config edit            Edit config in $EDITOR
    icebath config set model gpt-4 Set a config value
    icebath gateway                Run messaging gateway
    icebath gateway install        Install gateway background service
    icebath gateway start          Start the installed gateway service
    icebath gateway stop           Stop the gateway service
    icebath gateway status         Show gateway status
    icebath -p <profile> <cmd>     Run any command against a named profile's
                                  home (also --profile) — e.g. icebath -p coder gateway stop
    icebath -s icebath-agent-dev,github-auth
    icebath -w                     Start in isolated git worktree
    icebath sessions list          List past sessions
    icebath sessions browse        Interactive session picker
    icebath sessions rename ID T   Rename/title a session
    icebath logs                   View agent.log (last 50 lines)
    icebath logs -f                Follow agent.log in real time
    icebath logs errors            View errors.log
    icebath logs --since 1h        Lines from the last hour
    icebath debug share             Upload debug report for support
    icebath console                Open the safe Icebath command console
    icebath update                 Update to latest version
    icebath dashboard              Start web UI dashboard (port 9119)
    icebath dashboard --stop       Stop running dashboard processes
    icebath dashboard --status     List running dashboard processes

For more help on a command:
    icebath <command> --help`

// NewCLI builds the full icebath command tree with every handler unbound
func NewCLI() *CLI {
	c := &CLI{handlers: map[string]CLIHandler{}, positionals: map[*cobra.Command][]posSpec{}, flagAliases: map[*cobra.Command]map[string]string{}}
	root := &cobra.Command{
		Use:              "icebath",
		Short:            "Icebath Agent - AI assistant with tool-calling capabilities",
		Long:             "Icebath Agent - AI assistant with tool-calling capabilities",
		TraverseChildren: true,
		SilenceUsage:     true,
		SilenceErrors:    true,
		Annotations:      map[string]string{"epilog": rootEpilog},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetUsageTemplate(strings.Replace(root.UsageTemplate(), "{{if .HasAvailableLocalFlags}}\n\nFlags:",
		"{{if .Annotations.positionals}}\n\nPositional Arguments:\n{{.Annotations.positionals}}{{end}}{{if .HasAvailableLocalFlags}}\n\nFlags:", 1) + `{{if .Annotations.epilog}}
{{.Annotations.epilog}}
{{end}}`)
	c.Root = root
	cliRegistry.Store(root, c)
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) { io.WriteString(cmd.OutOrStdout(), argparseHelp(cmd)) })
	root.Args = c.groupArgs()
	root.FParseErrWhitelist.UnknownFlags = true
	root.SetFlagErrorFunc(argparseFlagError)
	root.RunE = func(cmd *cobra.Command, args []string) error { return c.dispatch("chat", cmd, args) }
	c.addRootFlags(root)
	c.rootArgparseShape(root)
	c.addChat(root)
	c.addTreeA(root)
	c.addTreeB(root)
	c.addTreeC(root)
	c.addPM(root)
	c.bindMCP()
	c.bindWebhook()
	c.bindMoA()
	c.bindConfig()
	c.bindSkin()
	c.bindHooks()
	c.bindPlugins()
	c.bindSkillsHub()
	c.bindBundles()
	c.bindProfile()
	c.bindMemory()
	c.finishAll(root)
	c.argparseAbbrev(c.find("skills"))
	c.argparseAbbrev(c.find("bundles"))
	return c
}

func (c *CLI) addRootFlags(root *cobra.Command) {
	f := root.Flags()
	f.BoolP("version", "V", false, "Show version and exit")
	f.StringP("oneshot", "z", "", "One-shot mode: send a single prompt and print ONLY the final response text to stdout. No banner, no spinner, no tool previews, no session_id line. Tools, memory, rules, and AGENTS.md in the CWD are loaded as normal; approvals are auto-bypassed. Intended for scripts / pipes.")
	f.String("usage-file", "", "One-shot mode only: after the run, write a JSON usage report (estimated cost, token counts, model, api_calls) to PATH. The report is written even when the run fails, so pipelines can always account for spend. No effect outside -z/--oneshot.")
	f.StringP("model", "m", "", "Model override for this invocation (e.g. anthropic/claude-sonnet-4.6). Applies to -z/--oneshot and --tui. Also settable via ICEBATH_INFERENCE_MODEL env var.")
	c.inherited(root, "model")
	f.String("provider", "", "Provider override for this invocation (e.g. openrouter, anthropic). Applies to -z/--oneshot and --tui. The persistent provider lives in config.yaml under model.provider — use `icebath setup` or edit the file to change it.")
	c.inherited(root, "provider")
	f.String("reasoning", "", "Reasoning effort for this invocation: none, minimal, low, medium, high, xhigh, max, or ultra. Overrides agent.reasoning_effort in config.yaml for this run only; the persistent level lives there (or per-model under agent.reasoning_overrides).")
	c.inherited(root, "reasoning")
	f.StringP("toolsets", "t", "", "Comma-separated toolsets to enable for this invocation. Applies to -z/--oneshot and --tui.")
	f.StringP("resume", "r", "", "Resume a previous session by ID or title, or pass 'latest' for the most recent session (workspace-scoped, like -c with no name)")
	f.Bool("no-restore-cwd", false, "Don't cd into a resumed session's recorded working directory.")
	f.String("in", "", "Change into DIR before starting or resuming. Combined with '--resume latest' or -c, the most recent session for DIR's workspace is picked, and the session stays in DIR (skips the recorded-cwd restore).")
	f.StringP("continue", "c", "", "Resume a session by name, or the most recent if no name given")
	noOpt(root, "continue", FlagPresent)
	f.BoolP("worktree", "w", false, "Run in an isolated git worktree (for parallel agents)")
	f.Bool("accept-hooks", false, "Auto-approve any unseen shell hooks declared in config.yaml without a TTY prompt.  Equivalent to ICEBATH_ACCEPT_HOOKS=1 or hooks_auto_accept: true in config.yaml.  Use on CI / headless runs that can't prompt.")
	c.inherited(root, "accept-hooks")
	f.StringArrayP("skills", "s", nil, "Preload one or more skills for the session (repeat flag or comma-separate)")
	c.inherited(root, "skills")
	f.Bool("yolo", false, "Bypass all dangerous command approval prompts (use at your own risk)")
	c.inherited(root, "yolo")
	f.Bool("pass-session-id", false, "Include the session ID in the agent's system prompt")
	c.inherited(root, "pass-session-id")
	f.Bool("checkpoints", false, "Enable filesystem checkpoints before destructive file operations (use /rollback to restore)")
	c.inherited(root, "checkpoints")
	f.Bool("ignore-user-config", false, "Ignore "+cfgPathForHelp()+" and fall back to built-in defaults (credentials in .env are still loaded)")
	c.inherited(root, "ignore-user-config")
	f.Bool("ignore-rules", false, "Skip auto-injection of AGENTS.md, SOUL.md, .cursorrules, memory, and preloaded skills")
	c.inherited(root, "ignore-rules")
	f.Bool("safe-mode", false, "Troubleshooting mode: disable ALL customizations — user config, AGENTS.md/memory injection, plugins, and MCP servers (implies --ignore-user-config and --ignore-rules)")
	c.inherited(root, "safe-mode")
	f.Bool("tui", false, "Launch the modern TUI instead of the classic REPL")
	c.inherited(root, "tui")
	f.Bool("native", false, "With --tui: use native terminal scrollback and disable mouse tracking")
	c.inherited(root, "native")
	c.aliasFlag(root, "tui-native", "native")
	f.Bool("cli", false, "Force the classic prompt_toolkit REPL (overrides display.interface=tui)")
	c.inherited(root, "cli")
	f.Bool("dev", false, "With --tui: run TypeScript sources via tsx (skip dist build)")
	c.inherited(root, "dev")
	pf := root.PersistentFlags()
	pf.StringP("profile", "p", "", "Run against a named profile's home (consumed before parsing, also carried over on relaunch)")
	root.RegisterFlagCompletionFunc("profile", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return ProfileNames(), cobra.ShellCompDirectiveNoFileComp
	})
}

// ProfileNames lists default plus every named profile for shell completion
func ProfileNames() []string {
	names := []string{"default"}
	for _, n := range home.ListProfiles(home.Root()) {
		if n != "default" {
			names = append(names, n)
		}
	}
	return names
}

func (c *CLI) addChat(root *cobra.Command) {
	chat := c.newCmd(root, "chat", "Interactive chat with the agent", "Start an interactive chat session with Icebath Agent")
	f := chat.Flags()
	f.StringP("query", "q", "", "Query to run. On a real TTY the prompt seeds an interactive session (submitted literally as the first turn); combined with --oneshot or -Q, or on a non-TTY, it answers and exits.")
	f.String("query-file", "", "Read the single query from a file instead of the command line ('-' reads stdin). Safe for arbitrary text: nothing is shell-interpreted, so quotes, $(...), and backticks are preserved verbatim. Mutually exclusive with -q.")
	chat.MarkFlagsMutuallyExclusive("query", "query-file")
	f.Bool("oneshot", false, "With -q/--query-file: answer the query and exit (legacy single-query behavior) instead of seeding an interactive session. Implied on non-TTY stdio and by -Q/--quiet.")
	f.String("image", "", "Optional local image path to attach to a single query")
	f.StringP("model", "m", "", "Model to use (e.g., anthropic/claude-sonnet-4)")
	c.inherited(chat, "model")
	f.StringP("toolsets", "t", "", "Comma-separated toolsets to enable")
	f.String("reasoning", "", "Reasoning effort for this session: none, minimal, low, medium, high, xhigh, max, or ultra. Overrides agent.reasoning_effort for this run only (same levels as the /reasoning slash command).")
	c.inherited(chat, "reasoning")
	f.StringArrayP("skills", "s", nil, "Preload one or more skills for the session (repeat flag or comma-separate)")
	c.inherited(chat, "skills")
	f.String("provider", "", "Inference provider (default: auto). Built-in or a user-defined name from `providers:` in config.yaml.")
	c.inherited(chat, "provider")
	f.String("base-url", "", "OpenAI-compatible endpoint for --provider custom (e.g. http://127.0.0.1:8000/v1).")
	f.String("api-key", "", "API key for this launch only; not persisted.")
	f.BoolP("verbose", "v", false, "Verbose output")
	f.BoolP("quiet", "Q", false, "Quiet mode for programmatic use: suppress banner, spinner, and tool previews. Only output the final response and session info.")
	f.String("format", "text", "Output format for single-query mode (-q). 'text' prints the final response as plain text (default). 'stream-json' emits newline-delimited JSON events (JSONL), implies --quiet, and cannot be combined with --tui.")
	c.choice(chat, "format", "text", "stream-json")
	f.StringP("resume", "r", "", "Resume a previous session by ID (shown on exit), or 'latest' for the most recent session")
	f.Bool("no-restore-cwd", false, "Don't cd into a resumed session's recorded working directory.")
	f.String("in", "", "Change into DIR before starting or resuming (scopes '--resume latest' / -c lookups to DIR's workspace).")
	f.StringP("continue", "c", "", "Resume a session by name, or the most recent if no name given")
	noOpt(chat, "continue", FlagPresent)
	f.Bool("create-if-missing", false, "With -c/--continue <name>: if no session matches the name, create a new session with that title and proceed (instead of failing with a not-found error). Programmatic callers that want 'send to this named thread, making it if needed'.")
	f.BoolP("worktree", "w", false, "Run in an isolated git worktree (for parallel agents on the same repo)")
	f.Bool("accept-hooks", false, "Auto-approve any unseen shell hooks declared in config.yaml without a TTY prompt (see also ICEBATH_ACCEPT_HOOKS env var and hooks_auto_accept: in config.yaml).")
	c.inherited(chat, "accept-hooks")
	f.Bool("checkpoints", false, "Enable filesystem checkpoints before destructive file operations (use /rollback to restore)")
	c.inherited(chat, "checkpoints")
	f.Int("max-turns", 0, "Maximum tool-calling iterations per conversation turn (default: 500, or agent.max_turns in config)")
	f.Float64("run-budget", 0, "Optional wall-clock budget in seconds for each conversation run. At 80% elapsed the agent gets a one-time wrap-up notice, and implicit provider stale timeouts are capped to the remaining budget so one hung call can't consume the run. Unset = off. Also configurable as agent.run_budget_seconds in config.yaml. Intended for one-shot/eval invocations with a hard ceiling.")
	f.Bool("yolo", false, "Bypass all dangerous command approval prompts (use at your own risk)")
	c.inherited(chat, "yolo")
	f.Bool("pass-session-id", false, "Include the session ID in the agent's system prompt")
	c.inherited(chat, "pass-session-id")
	f.Bool("ignore-user-config", false, "Ignore "+cfgPathForHelp()+" and fall back to built-in defaults (credentials in .env are still loaded). Useful for isolated CI runs, reproduction, and third-party integrations.")
	c.inherited(chat, "ignore-user-config")
	f.Bool("ignore-rules", false, "Skip auto-injection of AGENTS.md, SOUL.md, .cursorrules, memory, and preloaded skills. Combine with --ignore-user-config for a fully isolated run.")
	c.inherited(chat, "ignore-rules")
	f.Bool("safe-mode", false, "Troubleshooting mode: disable ALL customizations — user config, AGENTS.md/memory injection, plugins, and MCP servers (implies --ignore-user-config and --ignore-rules). Use to isolate whether a problem comes from your setup or from Icebath itself.")
	c.inherited(chat, "safe-mode")
	f.String("source", "", "Session source tag for filtering (default: cli). Use 'tool' for third-party integrations that should not appear in user session lists.")
	f.Bool("tui", false, "Launch the modern TUI instead of the classic REPL")
	c.inherited(chat, "tui")
	f.Bool("native", false, "Use native terminal scrollback and disable mouse tracking")
	c.inherited(chat, "native")
	c.aliasFlag(chat, "tui-native", "native")
	f.Bool("cli", false, "Force the classic prompt_toolkit REPL (overrides display.interface=tui)")
	c.inherited(chat, "cli")
	f.Bool("dev", false, "With --tui: run TypeScript sources via tsx (skip dist build)")
	c.inherited(chat, "dev")
	c.chatArgparseShape(chat)
}

// Creates a subcommand whose RunE dispatches to the handler bound at its path
func (c *CLI) newCmd(parent *cobra.Command, name, short, long string, aliases ...string) *cobra.Command {
	cmd := &cobra.Command{Use: name, Short: short, Long: long, Aliases: aliases, SilenceUsage: true, SilenceErrors: true}
	cmd.RunE = func(cmd *cobra.Command, args []string) error { return c.dispatch(c.pathOf(cmd), cmd, args) }
	parent.AddCommand(cmd)
	return cmd
}

// Creates a subcommand that prints its help when run without a subcommand
func (c *CLI) helpCmd(parent *cobra.Command, name, short, long string, exitCode int, aliases ...string) *cobra.Command {
	cmd := c.newCmd(parent, name, short, long, aliases...)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := cmd.Help(); err != nil {
			return err
		}
		if exitCode != 0 {
			return &ExitError{Code: exitCode}
		}
		return nil
	}
	return cmd
}

func (c *CLI) epilog(cmd *cobra.Command, text string) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations["epilog"] = text
}

func (c *CLI) pathOf(cmd *cobra.Command) string {
	return strings.TrimPrefix(strings.TrimPrefix(cmd.CommandPath(), c.Root.Name()), " ")
}

func (c *CLI) inherited(cmd *cobra.Command, name string) {
	_ = cmd.Flags().SetAnnotation(name, "inherit_on_relaunch", []string{"true"})
}

func (c *CLI) choice(cmd *cobra.Command, name string, choices ...string) {
	_ = cmd.Flags().SetAnnotation(name, "choices", choices)
	_ = cmd.RegisterFlagCompletionFunc(name, func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return choices, cobra.ShellCompDirectiveNoFileComp
	})
}

// Accepts a unique prefix of any long option in the subtree the way argparse allow_abbrev does
func (c *CLI) argparseAbbrev(cmd *cobra.Command) {
	known := map[string]bool{"help": true}
	c.Root.PersistentFlags().VisitAll(func(f *pflag.Flag) { known[f.Name] = true })
	cmd.Flags().VisitAll(func(f *pflag.Flag) { known[f.Name] = true })
	cmd.Flags().SetNormalizeFunc(func(f *pflag.FlagSet, name string) pflag.NormalizedName {
		if known[name] {
			return pflag.NormalizedName(name)
		}
		var hits []string
		f.VisitAll(func(fl *pflag.Flag) {
			if strings.HasPrefix(fl.Name, name) {
				hits = append(hits, fl.Name)
			}
		})
		if len(hits) == 1 {
			return pflag.NormalizedName(hits[0])
		}
		return pflag.NormalizedName(name)
	})
	for _, sub := range cmd.Commands() {
		c.argparseAbbrev(sub)
	}
}

func hide(cmd *cobra.Command, names ...string) {
	for _, n := range names {
		if fl := cmd.Flags().Lookup(n); fl != nil {
			fl.Hidden = true
		}
	}
}

func noOpt(cmd *cobra.Command, name, present string) {
	if fl := cmd.Flags().Lookup(name); fl != nil {
		fl.NoOptDefVal = present
	}
}

// Registers an alternate spelling for a flag of this command
func (c *CLI) aliasFlag(cmd *cobra.Command, alias, target string) {
	m, ok := c.flagAliases[cmd]
	if !ok {
		m = map[string]string{}
		c.flagAliases[cmd] = m
		cmd.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
			if t, ok := m[name]; ok {
				return pflag.NormalizedName(t)
			}
			return pflag.NormalizedName(name)
		})
	}
	m[alias] = target
	if fl := cmd.Flags().Lookup(target); fl != nil {
		_ = cmd.Flags().SetAnnotation(target, "aliases", append(fl.Annotations["aliases"], alias))
	}
}

func (c *CLI) pos(cmd *cobra.Command, specs ...posSpec) {
	c.positionals[cmd] = append(c.positionals[cmd], specs...)
}

func p(name string) posSpec     { return posSpec{Name: name} }
func pOpt(name string) posSpec  { return posSpec{Name: name, Optional: true} }
func pMany(name string) posSpec { return posSpec{Name: name, Variadic: true} }
func pAny(name string) posSpec  { return posSpec{Name: name, Optional: true, Variadic: true} }
func pRem(name string) posSpec {
	return posSpec{Name: name, Remain: true, Optional: true, Variadic: true}
}
func pInt(name string) posSpec { return posSpec{Name: name, Int: true} }
func pChoice(name string, choices ...string) posSpec {
	return posSpec{Name: name, Choices: choices}
}
func pOptChoice(name, def string, choices ...string) posSpec {
	return posSpec{Name: name, Optional: true, Default: def, Choices: choices}
}

// Finalizes Use strings, argument validators, and remainder parsing across the tree
func (c *CLI) finishAll(cmd *cobra.Command) {
	specs := c.positionals[cmd]
	if len(specs) > 0 {
		var parts []string
		for i, s := range specs {
			switch {
			case s.Remain:
				parts = append(parts, "[-- "+s.display()+"...]")
			case s.Variadic && s.Optional:
				parts = append(parts, "["+s.display()+" ...]")
			case s.Variadic:
				parts = append(parts, s.display()+" ["+s.display()+" ...]")
			case s.Optional:
				parts = append(parts, "["+s.display()+"]")
			default:
				parts = append(parts, s.display())
			}
			if s.Remain && i == 0 {
				cmd.Flags().SetInterspersed(false)
			}
		}
		cmd.Use = cmd.Name() + " " + strings.Join(parts, " ")
		cmd.Args = c.positionalArgs(specs)
		if block := positionalHelp(specs); block != "" {
			if cmd.Annotations == nil {
				cmd.Annotations = map[string]string{}
			}
			cmd.Annotations["positionals"] = block
		}
	} else if cmd.Args == nil {
		if cmd.DisableFlagParsing {
			cmd.Args = cobra.ArbitraryArgs
		} else if cmd.HasSubCommands() {
			cmd.Args = c.groupArgs()
			cmd.FParseErrWhitelist.UnknownFlags = true
		} else {
			cmd.Args = argparseNoArgs
		}
	}
	cmd.Args = c.withArgparseGroups(cmd.Args)
	if cmd.Annotations != nil && cmd.Annotations["epilog"] != "" && cmd != c.Root {
		cmd.SetUsageTemplate(c.Root.UsageTemplate())
	}
	for _, sub := range cmd.Commands() {
		c.finishAll(sub)
	}
}

// Aligned name and help rows of the positionals that carry help, empty when none does
func positionalHelp(specs []posSpec) string {
	width := 0
	for _, s := range specs {
		if s.Help != "" && len(s.display()) > width {
			width = len(s.display())
		}
	}
	var rows []string
	for _, s := range specs {
		if s.Help != "" {
			rows = append(rows, "  "+s.display()+strings.Repeat(" ", width-len(s.display()))+"   "+s.Help)
		}
	}
	return strings.Join(rows, "\n")
}

// Validates positional count, choices, and integer typed positionals like argparse
func (c *CLI) positionalArgs(specs []posSpec) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		min, max := 0, 0
		unbounded := false
		for _, s := range specs {
			if !s.Optional {
				min++
			}
			if s.Variadic || s.Remain {
				unbounded = true
			}
			max++
		}
		if len(args) < min {
			var missing []string
			for i := len(args); i < len(specs); i++ {
				if !specs[i].Optional {
					missing = append(missing, specs[i].display())
				}
			}
			return &ArgparseError{Cmd: cmd, Msg: "the following arguments are required: " + strings.Join(missing, ", ")}
		}
		if !unbounded && len(args) > max {
			return &ArgparseError{Cmd: cmd, Msg: "unrecognized arguments: " + strings.Join(args[max:], " ")}
		}
		for i, a := range args {
			if i >= len(specs) {
				break
			}
			s := specs[i]
			if s.Variadic || s.Remain {
				break
			}
			if len(s.Choices) > 0 && !contains(s.Choices, a) {
				return &ArgparseError{Cmd: cmd, Msg: fmt.Sprintf("argument %s: invalid choice: '%s' (choose from %s)", s.display(), a, quotedList(s.Choices))}
			}
			if s.Int {
				if _, err := strconv.Atoi(a); err != nil {
					return &ArgparseError{Cmd: cmd, Msg: fmt.Sprintf("argument %s: invalid int value: '%s'", s.display(), a)}
				}
			}
		}
		return nil
	}
}

func quotedList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = "'" + s + "'"
	}
	return strings.Join(q, ", ")
}

// Rejects an unknown subcommand with the short three line message instead of a name dump
func (c *CLI) groupArgs() cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		if cmd == c.Root {
			if c.hasChatIntent(cmd) {
				return nil
			}
		}
		return c.unknownCommand(cmd, args[0])
	}
}

func (c *CLI) hasChatIntent(root *cobra.Command) bool {
	for _, n := range []string{"resume", "continue"} {
		if fl := root.Flags().Lookup(n); fl != nil && fl.Changed {
			return true
		}
	}
	return false
}

func (c *CLI) unknownCommand(cmd *cobra.Command, value string) error {
	prog := cmd.CommandPath()
	lines := []string{fmt.Sprintf("%s: '%s' is not a `%s` command.", prog, value, prog)}
	close := pydifflib.GetCloseMatches(value, subcommandChoices(cmd), 3, 0.6)
	ctx := context.Background()
	install := pluginCommandInstallHint(ctx, prog, value)
	moved := ""
	if install == "" {
		moved = provider.MovedCommandHint(ctx, prog, value, "")
	}
	switch {
	case install != "":
		lines = append(lines, fmt.Sprintf("The '%s' memory plugin is not installed. Install it with: %s", value, install))
	case moved != "":
		lines = append(lines, moved)
	case len(close) > 0:
		lines = append(lines, "Did you mean: "+strings.Join(close, ", ")+"?")
	}
	lines = append(lines, fmt.Sprintf("Run `%s --help` to see all commands.", prog))
	return &ExitError{Code: 2, Msg: strings.Join(lines, "\n")}
}

// Bind attaches a handler to the command at path and reports whether that path exists
func (c *CLI) Bind(path string, h CLIHandler) bool {
	if c.find(path) == nil {
		return false
	}
	c.handlers[path] = h
	return true
}

func (c *CLI) find(path string) *cobra.Command {
	if path == "" {
		return c.Root
	}
	cmd, _, err := c.Root.Find(strings.Fields(path))
	if err != nil || cmd == nil || c.pathOf(cmd) != path {
		return nil
	}
	return cmd
}

// Unbound lists every runnable command path without a handler
func (c *CLI) Unbound() []string {
	var out []string
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd != c.Root && cmd.Runnable() {
			if _, ok := c.handlers[c.pathOf(cmd)]; !ok {
				out = append(out, c.pathOf(cmd))
			}
		}
		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}
	walk(c.Root)
	sort.Strings(out)
	return out
}

func (c *CLI) dispatch(path string, cmd *cobra.Command, args []string) error {
	if err := c.validateChoices(cmd); err != nil {
		return err
	}
	h, ok := c.handlers[path]
	if !ok {
		return &UnboundCLIError{Path: path}
	}
	return h(cmd, args)
}

func (c *CLI) validateChoices(cmd *cobra.Command) error {
	var err error
	cmd.Flags().VisitAll(func(fl *pflag.Flag) {
		if err != nil || !fl.Changed {
			return
		}
		choices := fl.Annotations["choices"]
		if len(choices) == 0 {
			return
		}
		if sl, ok := fl.Value.(pflag.SliceValue); ok {
			for _, v := range sl.GetSlice() {
				if !contains(choices, v) {
					err = fmt.Errorf("argument --%s: invalid choice: '%s' (choose from %s)", fl.Name, v, quotedList(choices))
				}
			}
			return
		}
		if !contains(choices, fl.Value.String()) {
			err = fmt.Errorf("argument --%s: invalid choice: '%s' (choose from %s)", fl.Name, fl.Value.String(), quotedList(choices))
		}
	})
	return err
}

// AddPluginCommand registers a plugin provided top level command
func (c *CLI) AddPluginCommand(cmd *cobra.Command) {
	c.Root.AddCommand(cmd)
	c.finishAll(cmd)
}

// InheritedFlags lists the top level flags tagged to carry over on relaunch
func (c *CLI) InheritedFlags() []string {
	var out []string
	c.Root.Flags().VisitAll(func(fl *pflag.Flag) {
		if fl.Annotations["inherit_on_relaunch"] != nil {
			out = append(out, "--"+fl.Name)
		}
	})
	for _, pre := range PreArgparseInheritedFlags {
		out = append(out, pre[0])
	}
	return out
}

// TopLevelValueFlagSets returns the required value and optional value flags of the root
func (c *CLI) TopLevelValueFlagSets() (map[string]bool, map[string]bool) {
	required, optional := map[string]bool{}, map[string]bool{}
	c.Root.Flags().VisitAll(func(fl *pflag.Flag) {
		if t := fl.Value.Type(); t == "bool" || t == "count" || c.Root.PersistentFlags().Lookup(fl.Name) != nil {
			return
		}
		target := required
		if fl.NoOptDefVal != "" {
			target = optional
		}
		target["--"+fl.Name] = true
		if fl.Shorthand != "" {
			target["-"+fl.Shorthand] = true
		}
	})
	return required, optional
}

// CommandArgv returns the subcommand and its arguments without the top level flags
func (c *CLI) CommandArgv(argv []string) []string {
	required, optional := c.TopLevelValueFlagSets()
	for _, pre := range PreArgparseInheritedFlags {
		if pre[1] == "1" {
			required[pre[0]] = true
		}
	}
	for i := 0; i < len(argv); {
		tok := argv[i]
		if tok == "--" {
			return argv[i+1:]
		}
		if !strings.HasPrefix(tok, "-") {
			return argv[i:]
		}
		if !strings.Contains(tok, "=") && (required[tok] || optional[tok]) && i+1 < len(argv) {
			i += 2
		} else {
			i++
		}
	}
	return nil
}

// FirstPositional returns the first subcommand token of argv or empty
func (c *CLI) FirstPositional(argv []string) (string, bool) {
	if rest := c.CommandArgv(argv); len(rest) > 0 {
		return rest[0], true
	}
	return "", false
}

// PluginCLIDiscoveryNeeded reports whether argv may name a plugin provided command
func (c *CLI) PluginCLIDiscoveryNeeded(argv []string) bool {
	first, ok := c.FirstPositional(argv)
	return ok && !BuiltinSubcommands[first]
}

func insideMCPAddArgs(argv []string, index int) bool {
	mcp := -1
	for i := 0; i < index; i++ {
		if argv[i] == "mcp" {
			mcp = i
			break
		}
	}
	if mcp < 0 {
		return false
	}
	for i := mcp + 1; i < index; i++ {
		if argv[i] == "add" {
			return true
		}
	}
	return false
}

func looksLikeOptionValue(v string) bool {
	return strings.HasPrefix(v, "-") || strings.Contains(v, ":")
}

// ScanProfileFlag finds the profile flag in argv as name, tokens consumed, and index
func (c *CLI) ScanProfileFlag(argv []string) (string, int, int, error) {
	required, optional := c.TopLevelValueFlagSets()
	sawSub := false
	for i := 0; i < len(argv); {
		arg := argv[i]
		if arg == "--" || (arg == "--args" && insideMCPAddArgs(argv, i)) {
			break
		}
		if (arg == "--profile" || arg == "-p") && i+1 < len(argv) {
			raw := argv[i+1]
			value := strings.ToLower(strings.TrimSpace(raw))
			if profileNameRe.MatchString(value) {
				return value, 2, i, nil
			}
			if !sawSub && !looksLikeOptionValue(raw) {
				return "", 0, -1, &ExitError{Code: 2, Msg: "Error: " + profiles.InvalidProfileNameError(raw).Error() + "\nRun `icebath profile list` to see your profiles."}
			}
			break
		}
		if strings.HasPrefix(arg, "--profile=") {
			return strings.ToLower(strings.TrimSpace(strings.SplitN(arg, "=", 2)[1])), 1, i, nil
		}
		takesValue := !strings.Contains(arg, "=") && i+1 < len(argv) && (required[arg] || (optional[arg] && !strings.HasPrefix(argv[i+1], "-")))
		if !takesValue && !strings.HasPrefix(arg, "-") {
			sawSub = true
		}
		if takesValue {
			i += 2
		} else {
			i++
		}
	}
	return "", 0, -1, nil
}

var sessionNameSubcommands = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`chat model gateway setup whatsapp whatsapp-cloud login logout auth status cron doctor config pairing skills
		tools mcp sessions insights update uninstall profile dashboard serve webapp desktop gui honcho claw plugins security acp webhook peer
		memory dump debug backup import completion logs usage`) {
		sessionNameSubcommands[n] = true
	}
}

// CoalesceSessionNameArgs joins unquoted multi word session names after the resume flags
func CoalesceSessionNameArgs(argv []string) []string {
	var out []string
	for i := 0; i < len(argv); {
		tok := argv[i]
		if tok == "-c" || tok == "--continue" || tok == "-r" || tok == "--resume" {
			out = append(out, tok)
			i++
			var parts []string
			for i < len(argv) && !strings.HasPrefix(argv[i], "-") && !sessionNameSubcommands[argv[i]] {
				parts = append(parts, argv[i])
				i++
			}
			if len(parts) > 0 {
				out = append(out, strings.Join(parts, " "))
			}
			continue
		}
		out = append(out, tok)
		i++
	}
	return out
}

// Joins an optional value flag with its following token so pflag sees one argument
func (c *CLI) joinOptionalValues(argv []string) []string {
	cmd, _, err := c.Root.Traverse(argv)
	if err != nil || cmd == nil {
		cmd = c.Root
	}
	lookup := func(tok string) *pflag.Flag {
		if strings.HasPrefix(tok, "--") {
			return cmd.Flags().Lookup(tok[2:])
		}
		if len(tok) == 2 && tok[0] == '-' {
			return cmd.Flags().ShorthandLookup(tok[1:])
		}
		return nil
	}
	var out []string
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if fl := lookup(tok); fl != nil && fl.NoOptDefVal != "" && fl.Value.Type() != "bool" && fl.Value.Type() != "count" && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") && !sessionNameSubcommands[argv[i+1]] {
			out = append(out, "--"+fl.Name+"="+argv[i+1])
			i++
			continue
		}
		out = append(out, tok)
	}
	return out
}

// Execute parses argv, strips the profile flag, and runs the resolved handler
func (c *CLI) Execute(argv []string) error {
	name, consume, index, err := c.ScanProfileFlag(argv)
	if err != nil {
		return err
	}
	if consume > 0 {
		c.Profile = name
		argv = append(append([]string{}, argv[:index]...), argv[index+consume:]...)
	}
	argv = c.joinOptionalValues(CoalesceSessionNameArgs(argv))
	argv = c.expandNargsPlus(rewriteMCPAddArgs(argv))
	c.argv = argv
	c.Root.SetArgs(argv)
	return argparseFlagError(c.Root, c.Root.Execute())
}

// Turns the mcp add passthrough marker into the plain argument separator
func rewriteMCPAddArgs(argv []string) []string {
	for i, tok := range argv {
		if tok == "--args" && insideMCPAddArgs(argv, i) {
			out := append([]string{}, argv...)
			out[i] = "--"
			return out
		}
		if tok == "--" {
			break
		}
	}
	return argv
}

// Positional returns the named positional argument or its declared default
func (c *CLI) Positional(cmd *cobra.Command, args []string, name string) string {
	for i, s := range c.positionals[cmd] {
		if s.Name != name {
			continue
		}
		if i < len(args) {
			return args[i]
		}
		return s.Default
	}
	return ""
}

// ExitCode maps an execution error to the process exit status and stderr text
func ExitCode(err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	var ae *ArgparseError
	if errors.As(err, &ae) {
		return 2, ae.Stderr()
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code, ee.Msg
	}
	return 1, err.Error()
}

// Run executes argv and exits the process with the mapped status
func (c *CLI) Run(argv []string) {
	code, msg := ExitCode(c.Execute(argv))
	if msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	os.Exit(code)
}

func (c *CLI) profileArgs(cmd *cobra.Command) {
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return ProfileNames(), cobra.ShellCompDirectiveNoFileComp
	}
}
