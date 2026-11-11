package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/mattn/go-isatty"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/internal/pydifflib"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Fresh cobra command tree with every host handler bound, assigned by the binary at startup
var ConsoleCommandTree func() *commands.CLI

// ConsoleStatus is the outcome kind of one console line
type ConsoleStatus string

// Outcome kinds of a console line
const (
	ConsoleOK              ConsoleStatus = "ok"
	ConsoleError           ConsoleStatus = "error"
	ConsoleConfirmRequired ConsoleStatus = "confirm_required"
	ConsoleExit            ConsoleStatus = "exit"
	ConsoleClear           ConsoleStatus = "clear"
)

// ConsoleResult is what one console line produced
type ConsoleResult struct {
	Status              ConsoleStatus
	Output              string
	Command             string
	ConfirmationMessage string
}

// Handler of a console command, its output text or a console command error
type consoleHandler func(e *ConsoleEngine, args []string) (string, error)

// ConsoleCommand is one registered console command path
type ConsoleCommand struct {
	Path         []string
	Usage        string
	Summary      string
	Handler      consoleHandler
	Mutating     bool
	Confirmation string
}

// ConsoleEngine is the curated line command executor of the icebath console
type ConsoleEngine struct {
	ctx         context.Context
	OutputLimit int
	History     []string
	commands    map[string]*ConsoleCommand
	order       []string
}

// Map key of a command path, tokens joined by a separator no token holds
func consoleKey(path []string) string { return strings.Join(path, "\x00") }

// Engine with the built in commands, the CLI families and send registered
func NewConsoleEngine(ctx context.Context, outputLimit int) *ConsoleEngine {
	e := &ConsoleEngine{ctx: ctx, OutputLimit: outputLimit, commands: map[string]*ConsoleCommand{}}
	e.registerDefaults()
	return e
}

// Runs one console line, a mutating command asking for confirmation unless confirmed
func (e *ConsoleEngine) Execute(line string, confirmed bool) (ConsoleResult, error) {
	rawLine := pystr.Strip(line)
	if rawLine == "" {
		return ConsoleResult{Status: ConsoleOK}, nil
	}
	result, err := e.execute(rawLine, confirmed)
	var console *ConsoleCommandError
	if errors.As(err, &console) {
		return ConsoleResult{Status: ConsoleError, Output: pystr.Strip(console.Msg), Command: rawLine}, nil
	}
	return result, err
}

// Body of Execute whose console command errors become error results
func (e *ConsoleEngine) execute(rawLine string, confirmed bool) (ConsoleResult, error) {
	tokens, err := consoleSplitLine(rawLine)
	if err != nil {
		return ConsoleResult{}, err
	}
	if len(tokens) > 0 && tokens[0] == "icebath" {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		help, err := e.HelpText("")
		return ConsoleResult{Status: ConsoleOK, Output: help}, err
	}
	if consoleContainsShellSyntax(rawLine, tokens) {
		return ConsoleResult{}, &ConsoleCommandError{Msg: "Icebath Console does not run shell syntax. Use one supported " +
			"Icebath command at a time."}
	}
	if builtin, ok := e.executeBuiltin(tokens); ok {
		if rawLine != "history" && rawLine != "clear" {
			e.History = append(e.History, rawLine)
		}
		return builtin, nil
	}
	command, args, err := e.resolveCommand(tokens)
	if err != nil {
		return ConsoleResult{}, err
	}
	if command.Mutating && !confirmed {
		message := command.Confirmation
		if message == "" {
			message = "Run `" + command.Usage + "`?"
		}
		return ConsoleResult{Status: ConsoleConfirmRequired, Command: rawLine, ConfirmationMessage: message}, nil
	}
	output, err := command.Handler(e, args)
	if err != nil {
		return ConsoleResult{}, err
	}
	output = e.capOutput(pystr.Rstrip(output))
	e.History = append(e.History, rawLine)
	return ConsoleResult{Status: ConsoleOK, Output: output, Command: rawLine}, nil
}

// Usage and summary of one command, or the table of every supported command
func (e *ConsoleEngine) HelpText(subject string) (string, error) {
	if subject != "" {
		command, _, err := e.resolveCommand(pystr.SplitWhitespace(subject, -1))
		if err != nil {
			return "", err
		}
		return command.Usage + "\n" + command.Summary, nil
	}
	sorted := make([]*ConsoleCommand, 0, len(e.order))
	for _, key := range e.order {
		sorted = append(sorted, e.commands[key])
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Usage < sorted[j].Usage })
	lines := []string{"Icebath Console", "", "Supported commands:"}
	for _, command := range sorted {
		marker := "  "
		if command.Mutating {
			marker = " *"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", marker, pystr.Ljust(command.Usage, 32), consoleTableSummary(command.Summary, 76)))
	}
	lines = append(lines, "", "* requires confirmation", "Built-ins: help, help <command>, history, clear, exit, quit")
	return strings.Join(lines, "\n"), nil
}

// Registers the built in rows, every CLI family in order and send
func (e *ConsoleEngine) registerDefaults() {
	for _, b := range consoleBuiltinCommands() {
		e.Register(b.path, b.usage, b.summary, b.handler, b.confirmation != "", b.confirmation)
	}
	for _, family := range consoleCLIFamilies {
		consoleRegisterCommandFamily(e, family)
	}
	e.Register([]string{"send"}, "send --to <target> <message>", "Send a message to a configured platform.",
		func(e *ConsoleEngine, args []string) (string, error) { return e.dispatch("send", nil, args, false) },
		true, "Send this message?")
}

// Adds or replaces the command at path
func (e *ConsoleEngine) Register(path []string, usage, summary string, handler consoleHandler, mutating bool, confirmation string) {
	key := consoleKey(path)
	if _, ok := e.commands[key]; !ok {
		e.order = append(e.order, key)
	}
	e.commands[key] = &ConsoleCommand{Path: append([]string{}, path...), Usage: usage, Summary: summary, Handler: handler,
		Mutating: mutating, Confirmation: confirmation}
}

// Help, history, clear and exit, false for any other head token
func (e *ConsoleEngine) executeBuiltin(tokens []string) (ConsoleResult, bool) {
	switch tokens[0] {
	case "help":
		help, err := e.HelpText(pystr.Strip(strings.Join(tokens[1:], " ")))
		var console *ConsoleCommandError
		if errors.As(err, &console) {
			return ConsoleResult{Status: ConsoleError, Output: console.Msg}, true
		}
		return ConsoleResult{Status: ConsoleOK, Output: help}, true
	case "history":
		lines := make([]string, 0, len(e.History))
		for i, command := range e.History {
			lines = append(lines, fmt.Sprintf("%d: %s", i+1, command))
		}
		output := strings.Join(lines, "\n")
		if output == "" {
			output = "No history yet."
		}
		return ConsoleResult{Status: ConsoleOK, Output: output}, true
	case "clear":
		return ConsoleResult{Status: ConsoleClear, Output: "\033[2J\033[H"}, true
	case "exit", "quit":
		return ConsoleResult{Status: ConsoleExit}, true
	}
	return ConsoleResult{}, false
}

// Longest registered prefix of at most three tokens and the remaining arguments, suggesting near misses
func (e *ConsoleEngine) resolveCommand(tokens []string) (*ConsoleCommand, []string, error) {
	if rejected := consoleRejectionFor(tokens); rejected != "" {
		return nil, nil, &ConsoleCommandError{Msg: rejected}
	}
	for size := min(len(tokens), 3); size > 0; size-- {
		if command, ok := e.commands[consoleKey(tokens[:size])]; ok {
			return command, append([]string{}, tokens[size:]...), nil
		}
	}
	available := make([]string, 0, len(e.order))
	for _, key := range e.order {
		available = append(available, strings.Join(e.commands[key].Path, " "))
	}
	probe := tokens[0]
	if len(tokens) > 1 {
		probe = strings.Join(tokens[:2], " ")
	}
	suffix := ""
	if suggestions := pydifflib.GetCloseMatches(probe, available, 3, 0.45); len(suggestions) > 0 {
		suffix = " Did you mean: " + strings.Join(suggestions, ", ") + "?"
	}
	return nil, nil, &ConsoleCommandError{Msg: "Unsupported Icebath Console command: " + probe + "." + suffix}
}

// Output cut at the limit with a note of how much was dropped
func (e *ConsoleEngine) capOutput(output string) string {
	n := pystr.Len(output)
	if n <= e.OutputLimit {
		return output
	}
	return fmt.Sprintf("%s\n... output truncated (%d bytes omitted)", pystr.Slice(output, 0, e.OutputLimit), n-e.OutputLimit)
}

// Top level commands the console never runs
var consoleBlockedTop = map[string]bool{}

func init() {
	for _, name := range strings.Fields("acp chat claw completion dashboard desktop fallback gateway gui login logout model moa " +
		"oneshot proxy serve setup uninstall update whatsapp whatsapp-cloud") {
		consoleBlockedTop[name] = true
	}
}

// Command pairs the console refuses with the reason shown
var consoleBlockedPairs = map[[2]string]string{
	{"config", "edit"}:       "`config edit` opens an editor and is not available in Icebath Console.",
	{"profile", "alias"}:     "`profile alias` creates shell wrappers and is not available in Icebath Console.",
	{"skills", "config"}:     "`skills config` is interactive and is not available in Icebath Console.",
	{"skills", "publish"}:    "`skills publish` is not available in Icebath Console.",
	{"portal", "login"}:      "`portal login` is interactive and is not available in Icebath Console.",
	{"portal", "open"}:       "`portal open` opens a browser and is not available in Icebath Console.",
	{"kanban", "tail"}:       "`kanban tail` streams output and is not available in Icebath Console.",
	{"kanban", "watch"}:      "`kanban watch` streams output and is not available in Icebath Console.",
	{"kanban", "daemon"}:     "`kanban daemon` starts a service and is not available in Icebath Console.",
	{"kanban", "dispatcher"}: "`kanban dispatcher` starts a worker and is not available in Icebath Console.",
	{"kanban", "swarm"}:      "`kanban swarm` starts agent work and is not available in Icebath Console.",
	{"kanban", "decompose"}:  "`kanban decompose` starts agent work and is not available in Icebath Console.",
	{"kanban", "specify"}:    "`kanban specify` starts agent work and is not available in Icebath Console.",
	{"kanban", "gc"}:         "`kanban gc` is not available in Icebath Console.",
	{"sessions", "delete"}:   "`sessions delete` and `sessions prune` are not available in Icebath Console.",
	{"sessions", "prune"}:    "`sessions delete` and `sessions prune` are not available in Icebath Console.",
}

// Refusal text for a flag, a blocked top level command or a blocked pair, empty when allowed
func consoleRejectionFor(tokens []string) string {
	first := tokens[0]
	if strings.HasPrefix(first, "-") {
		return first + " is not available in Icebath Console."
	}
	if consoleBlockedTop[first] {
		return "`icebath " + first + "` is not available in Icebath Console."
	}
	if len(tokens) > 1 {
		return consoleBlockedPairs[[2]string{tokens[0], tokens[1]}]
	}
	return ""
}

// Tokens of a console line split the Windows safe way, unbalanced quotes a console error
func consoleSplitLine(line string) ([]string, error) {
	tokens, err := pyshlex.SplitCommandLine(line)
	if err != nil {
		return nil, &ConsoleCommandError{Msg: "Could not parse command: " + pyrepr.ExceptionMessage(err)}
	}
	return tokens, nil
}

// Shell operators the console refuses as tokens
var consoleShellTokens = map[string]bool{"|": true, "||": true, "&": true, "&&": true, ";": true, ">": true, ">>": true,
	"<": true, "<<": true, "2>": true, "2>>": true}

// Whether the line uses command substitution, a shell operator token or a pipe redirect or separator character
func consoleContainsShellSyntax(line string, tokens []string) bool {
	if strings.Contains(line, "$(") || strings.Contains(line, "`") {
		return true
	}
	for _, token := range tokens {
		if consoleShellTokens[token] {
			return true
		}
	}
	return strings.ContainsAny(line, "|<>;")
}

// Whitespace collapsed summary cut to the limit with an ellipsis
func consoleTableSummary(summary string, limit int) string {
	summary = strings.Join(pystr.SplitWhitespace(summary, -1), " ")
	if pystr.Len(summary) <= limit {
		return summary
	}
	return pystr.Rstrip(pystr.Slice(summary, 0, limit-3)) + "..."
}

// Help text as a one line summary, empty for hidden or run hints
func consoleCleanSummary(text string) string {
	summary := strings.Join(pystr.SplitWhitespace(text, -1), " ")
	if strings.HasPrefix(summary, "Run `icebath ") {
		return ""
	}
	return summary
}

// Subcommand path to help over a family subtree, the choice help winning over the child description
func consoleSummariesFromTree(root *cobra.Command, family string) map[string]string {
	summaries := map[string]string{}
	var walk func(parent *cobra.Command, path []string)
	walk = func(parent *cobra.Command, path []string) {
		for _, child := range parent.Commands() {
			if len(path) == 0 && child.Name() != family {
				continue
			}
			childPath := append(append([]string{}, path...), child.Name())
			choice := ""
			if !child.Hidden {
				choice = child.Short
			}
			summary := consoleCleanSummary(choice)
			if summary == "" {
				summary = consoleCleanSummary(child.Long)
			}
			if _, seen := summaries[consoleKey(childPath)]; summary != "" && !seen {
				summaries[consoleKey(childPath)] = summary
			}
			walk(child, childPath)
		}
	}
	walk(root, nil)
	return summaries
}

var (
	consoleSummaryMu    sync.Mutex
	consoleSummaryCache = map[string]map[string]string{}
)

// Summaries of one family read once per process from an unbound command tree
func consoleSurfaceSummaries(family string) map[string]string {
	consoleSummaryMu.Lock()
	defer consoleSummaryMu.Unlock()
	if cached, ok := consoleSummaryCache[family]; ok {
		return cached
	}
	summaries := consoleSummariesFromTree(commands.NewCLI().Root, family)
	consoleSummaryCache[family] = summaries
	return summaries
}

// One CLI family the console reaches, its registration kind and its paths spec
type consoleFamily struct {
	root, kind, paths string
}

// Families in registration order, each paths spec listing subcommands with a star marking a mutating one
var consoleCLIFamilies = []consoleFamily{
	{"dump", "extracted", "."},
	{"debug", "extracted", "*share, *delete"},
	{"prompt-size", "extracted", "."},
	{"insights", "extracted", "."},
	{"security", "extracted", "audit"},
	{"backup", "extracted", "*."},
	{"import", "extracted", "*."},
	{"config", "extracted", "env-path, check"},
	{"tools", "extracted", "list, *enable, *disable, *post-setup"},
	{"plugins", "extracted", "list, *enable, *disable, *install, *update, *remove"},
	{"skills", "extracted", "browse, search, inspect, list, check, list-modified, diff, *install, *update, *audit, " +
		"*uninstall, *reset, *opt-in, *opt-out, *repair-official, *snapshot export, " +
		"*snapshot import, tap list, *tap add, *tap remove"},
	{"mcp", "extracted", "list, catalog, test, *add, *remove, *install, *login, *reauth, *configure, *picker"},
	{"memory", "extracted", "status, *off, *reset"},
	{"auth", "extracted", "list, status, *reset, *priority, *refresh, *add, *remove, *logout"},
	{"pairing", "extracted", "list, *approve, *revoke, *clear-pending"},
	{"webhook", "extracted", "list, *subscribe, *remove, test"},
	{"hooks", "extracted", "list, *test, *doctor, *revoke"},
	{"slack", "extracted", "manifest"},
	{"profile", "extracted", "list, show, info, *create, *use, *describe, *rename, *delete, *export, *import, " +
		"*install, *update"},
	{"cron", "extracted", "*create, *edit, *remove, *tick"},
	{"portal", "adder", "info, tools"},
	{"project", "builder", "list, show, *create, *add-folder, *remove-folder, *rename, *set-primary, *use, " +
		"*archive, *restore, *bind-board"},
	{"kanban", "builder", "*init, boards list, *boards create, *boards rm, *boards switch, boards current, " +
		"*boards rename, *boards set-workdir, *create, list, show, *assign, *reclaim, *reassign, " +
		"diagnose, *link, *unlink, *claim, *comment, *complete, *edit, *block, *schedule, " +
		"*unblock, *promote, *archive, stats, runs, heartbeat, assignments, context"},
	{"bundles", "registered", "list, show, *create, *delete, *reload"},
	{"checkpoints", "registered", "status, list, *prune, *clear, *clear-legacy"},
	{"curator", "registered", "status, *run, *pause, *resume, *pin, *unpin, *restore, list-archived, *archive, *prune, " +
		"*backup, *rollback"},
	{"pets", "registered", "list, *install, *select, show, *off, *scale, *remove, doctor"},
}

// One subcommand path of a paths spec and whether it mutates
type consolePath struct {
	path     []string
	mutating bool
}

// Paths of a spec, a star marking a mutating path and a dot the bare root
func consolePaths(spec string) []consolePath {
	var out []consolePath
	for _, item := range strings.Split(spec, ",") {
		item = pystr.Strip(item)
		if item == "" {
			continue
		}
		name := strings.TrimLeft(item, "*")
		var path []string
		if name != "." {
			path = strings.Split(name, " ")
		}
		out = append(out, consolePath{path: path, mutating: strings.HasPrefix(item, "*")})
	}
	return out
}

// Registers every path of a family, extracted and registered kinds skipping nested prompts once confirmed
func consoleRegisterCommandFamily(e *ConsoleEngine, family consoleFamily) {
	summaries := consoleSurfaceSummaries(family.root)
	confirmedDefaults := family.kind == "extracted" || family.kind == "registered"
	for _, p := range consolePaths(family.paths) {
		fullPath := append([]string{family.root}, p.path...)
		usage := strings.Join(fullPath, " ")
		fixed := p.path
		handler := func(e *ConsoleEngine, args []string) (string, error) {
			return e.dispatch(family.root, fixed, args, confirmedDefaults)
		}
		summary := summaries[consoleKey(fullPath)]
		if summary == "" {
			summary = "Run `icebath " + usage + "`."
		}
		e.Register(fullPath, usage, summary, handler, p.mutating, "Run `icebath "+usage+"`?")
	}
}

// Parses root, fixed and args with a fresh bound tree and runs the handler with its output captured
func (e *ConsoleEngine) dispatch(root string, fixed, args []string, confirmedDefaults bool) (string, error) {
	cli := ConsoleCommandTree()
	argv := append(append([]string{root}, fixed...), args...)
	var capture *consoleCapture
	defer func() {
		if capture != nil {
			capture.stop()
		}
	}()
	cli.Root.PersistentPreRunE = func(cmd *cobra.Command, posArgs []string) error {
		if err := consoleRejectProfileFlag(cli, argv); err != nil {
			return err
		}
		if confirmedDefaults {
			if err := consoleApplyConfirmedDefaults(cli, cmd, posArgs); err != nil {
				return err
			}
		}
		var err error
		capture, err = startConsoleCapture()
		return err
	}
	cli.Root.SetContext(e.ctx)
	err := cli.ExecuteParsed(argv)
	if capture == nil {
		var parse *commands.ArgparseError
		switch {
		case errors.As(err, &parse) && strings.HasPrefix(parse.Msg, "unrecognized arguments: "):
			return "", &ConsoleCommandError{Msg: cli.Root.Name() + ": " + parse.Msg}
		case errors.As(err, &parse):
			return "", &ConsoleCommandError{Msg: parse.Cmd.CommandPath() + ": " + parse.Msg}
		case err == nil:
			return "", &commands.ExitError{Code: 0}
		}
		return "", err
	}
	stdout, stderr := capture.stop()
	var unbound *commands.UnboundCLIError
	if errors.As(err, &unbound) {
		return "", &ConsoleCommandError{Msg: "No handler is available for that console command."}
	}
	return consoleSettle(0, err, stdout, stderr)
}

// Refuses the root profile flag the bare family parser never defines, as argparse reports an unknown argument
func consoleRejectProfileFlag(cli *commands.CLI, argv []string) error {
	flag := cli.Root.PersistentFlags().Lookup("profile")
	if flag == nil || !flag.Changed {
		return nil
	}
	var extras []string
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		switch {
		case tok == "--":
			i = len(argv)
		case tok == "-p" || tok == "--profile":
			extras = append(extras, argv[i:min(i+2, len(argv))]...)
			i++
		case strings.HasPrefix(tok, "--profile="), strings.HasPrefix(tok, "-p") && !strings.HasPrefix(tok, "--"):
			extras = append(extras, tok)
		}
	}
	return &ConsoleCommandError{Msg: cli.Root.Name() + ": unrecognized arguments: " + strings.Join(extras, " ")}
}

// Sets the boolean flag whose argparse dest is dest on any parser level of cmd, false when none defines it
func consoleSetDest(cmd *cobra.Command, dest string) bool {
	for level := cmd; level != nil; level = level.Parent() {
		var hit *pflag.Flag
		level.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if hit == nil && consoleFlagDest(f) == dest {
				hit = f
			}
		})
		if hit != nil {
			hit.Value.Set("true")
			hit.Changed = true
			return true
		}
	}
	return false
}

// Namespace key argparse gives a flag, its dest annotation or its name with hyphens as underscores
func consoleFlagDest(f *pflag.Flag) string {
	if d := f.Annotations["dest"]; len(d) > 0 {
		return d[0]
	}
	return strings.ReplaceAll(f.Name, "-", "_")
}

// Skips the nested prompts of a command the console already confirmed, the _apply_confirmed_defaults rules
func consoleApplyConfirmedDefaults(cli *commands.CLI, cmd *cobra.Command, args []string) error {
	ns := cli.Namespace(cmd, args)
	words := strings.Fields(strings.TrimPrefix(cmd.CommandPath(), cli.Root.Name()))
	root, sub := "", ""
	if len(words) > 0 {
		root = words[0]
	}
	if len(words) > 1 {
		sub = words[1]
	}
	if _, ok := ns["yes"]; ok {
		consoleSetDest(cmd, "yes")
	}
	if root == "import" {
		consoleSetDest(cmd, "force")
	}
	if root == "checkpoints" && (sub == "prune" || sub == "clear" || sub == "clear-legacy") {
		consoleSetDest(cmd, "force")
	}
	if root == "plugins" && sub == "install" && !pyrepr.Truthy(ns["enable"]) && !pyrepr.Truthy(ns["no_enable"]) {
		consoleSetDest(cmd, "no_enable")
	}
	if root == "auth" && sub == "add" {
		authType := ns["type"]
		if (authType == "api-key" || authType == "api_key") && !pyrepr.Truthy(ns["api_key"]) {
			return &ConsoleCommandError{Msg: "auth add --type api-key requires --api-key in Icebath Console."}
		}
	}
	if root == "profile" && sub == "import" && ns["name"] != nil {
		return nil
	}
	if root == "skills" && (sub == "install" || sub == "reset" || sub == "opt-out" || sub == "repair-official") {
		consoleSetDest(cmd, "yes")
	}
	if root == "memory" && sub == "reset" {
		consoleSetDest(cmd, "yes")
	}
	return nil
}

// ConsoleREPLOptions are the streams of a console REPL and whether it prompts, nil meaning the process streams and a tty probe
type ConsoleREPLOptions struct {
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	Interactive *bool
}

// Runs the local icebath console REPL until exit or end of input
func RunConsoleREPL(ctx context.Context, o ConsoleREPLOptions) (int, error) {
	stdin, stdout, stderr := o.Stdin, o.Stdout, o.Stderr
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	interactive := false
	if o.Interactive != nil {
		interactive = *o.Interactive
	} else if f, ok := stdin.(*os.File); ok {
		interactive = isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
	}
	engine := NewConsoleEngine(ctx, 20000)
	reader := bufio.NewReader(stdin)
	readline := func() string {
		line, _ := reader.ReadString('\n')
		return line
	}
	say := func(text, end string) {
		if interactive {
			fmt.Fprint(stdout, text+end)
		}
	}
	say("Icebath Console. Type `help` for commands, `exit` to quit.", "\n")
	for {
		say("icebath> ", "")
		line := readline()
		if line == "" {
			say("", "\n")
			return 0, nil
		}
		result, err := engine.Execute(line, false)
		if err != nil {
			return 1, err
		}
		if result.Status == ConsoleConfirmRequired {
			if !interactive {
				fmt.Fprintln(stderr, "Confirmation required: "+result.ConfirmationMessage)
				return 1, nil
			}
			fmt.Fprint(stdout, result.ConfirmationMessage+" [y/N] ")
			answer := pystr.Lower(pystr.Strip(readline()))
			if answer != "y" && answer != "yes" {
				fmt.Fprintln(stdout, "Cancelled.")
				continue
			}
			if result, err = engine.Execute(result.Command, true); err != nil {
				return 1, err
			}
		}
		if result.Output != "" {
			if result.Status == ConsoleError {
				fmt.Fprintln(stderr, result.Output)
			} else {
				fmt.Fprintln(stdout, result.Output)
			}
		}
		if result.Status == ConsoleExit {
			return 0, nil
		}
	}
}
