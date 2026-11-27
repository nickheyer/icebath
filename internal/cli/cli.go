package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/nickheyer/icebath/internal/launch"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Groups the root's help lists its commands under, most used first; a command that names none is listed under More
const (
	GroupSessions  = "sessions"
	GroupModels    = "models"
	GroupAbilities = "abilities"
	GroupSafety    = "safety"
	GroupSecrets   = "secrets"
	GroupApp       = "app"
	GroupMore      = "more"
)

// Headings of the groups, in the order the root's help shows them
var groups = []*cobra.Group{
	{ID: GroupSessions, Title: "Sessions:"},
	{ID: GroupModels, Title: "Models:"},
	{ID: GroupAbilities, Title: "What the agent can do:"},
	{ID: GroupSafety, Title: "Safety:"},
	{ID: GroupSecrets, Title: "Keys and passwords:"},
	{ID: GroupApp, Title: "This app:"},
	{ID: GroupMore, Title: "More:"},
}

const about = `icebath runs AI agents in your terminal. Each chat is a window, and windows sit side by side.

  icebath                  Open the app on a new chat
  icebath -c [title]       Open your newest session, or the one with this title
  icebath -r <session>     Open the session with this id or title
  icebath chat -q <text>   Send the agent one message and print the reply
  icebath -z <text>        Send one message and print only the final reply
  icebath server           Run icebath as a server that apps connect to
  icebath <command>        Run one command and exit

Put -p NAME before any command to use that profile for the run, and --yolo to run every command of the run without asking. With -c or -r, add --no-restore-cwd to stay in this folder instead of going to the session's.`

// Help of a command: its description wrapped to the terminal, then its usage
const helpTemplate = `{{with (or .Long .Short)}}{{wrapped . | trimTrailingWhitespaces}}

{{end}}{{if or .Runnable .HasSubCommands}}{{.UsageString}}{{end}}`

// Usage of a command as cobra prints it, its flags wrapped to the terminal
const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Other commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsagesWrapped columns | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Flags of every command:
{{.InheritedFlags.FlagUsagesWrapped columns | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

More help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

For the help of one command, run {{.CommandPath}} [command] --help.{{end}}
`

func init() {
	cobra.AddTemplateFunc("columns", columns)
	cobra.AddTemplateFunc("wrapped", func(text string) string { return wrap(text, columns()) })
}

type Registrant interface {
	Commands(conn *connect.Client) []*cobra.Command
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Commands(conn *connect.Client) []*cobra.Command {
	var commands []*cobra.Command
	for _, registrant := range r.registrants {
		commands = append(commands, registrant.Commands(conn)...)
	}
	return commands
}

type CLI struct {
	Registry
	// Flags of a launch of the app, which the root's help lists, and those taken ahead of the command, which it runs the work around it with
	launch []launch.Flag
	given  launch.Given
	// Serves this process's own services until the context ends, for icebath server
	serve func(ctx context.Context) error
}

func New() *CLI {
	return &CLI{}
}

// Has the root's help list the flags a launch of the app takes, and hands the command those given ahead of it, by their first spelling
func (c *CLI) Launch(flags []launch.Flag, given launch.Given) {
	c.launch, c.given = flags, given
}

// Has icebath server run the server, and the root's help list it
func (c *CLI) Serve(serve func(ctx context.Context) error) {
	c.serve = serve
}

// Whether the words ask for help: -h or --help before any --
func Helped(words []string) bool {
	for _, word := range words {
		switch word {
		case "--":
			return false
		case "-h", "--help":
			return true
		}
	}
	return false
}

func (c *CLI) Run(ctx context.Context, conn *connect.Client, args []string) error {
	root := &cobra.Command{
		Use:           "icebath",
		Short:         "Work with AI agents in your terminal",
		Long:          about,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetHelpTemplate(helpTemplate)
	root.SetUsageTemplate(usageTemplate)
	root.AddGroup(groups...)
	for _, flag := range c.launch {
		long, short := "", ""
		for _, name := range flag.Names {
			switch {
			case strings.HasPrefix(name, "--"):
				long = cmp.Or(long, strings.TrimPrefix(name, "--"))
			case strings.HasPrefix(name, "-"):
				short = cmp.Or(short, strings.TrimPrefix(name, "-"))
			}
		}
		switch {
		case long == "" || root.Flags().Lookup(long) != nil:
		case flag.Value != "":
			root.Flags().StringArrayP(long, short, nil, flag.Help)
		default:
			root.Flags().BoolP(long, short, false, flag.Help)
		}
	}
	root.AddCommand(c.Commands(conn)...)
	if c.serve != nil {
		root.AddCommand(c.server())
	}
	for _, command := range root.Commands() {
		if !root.ContainsGroup(command.GroupID) {
			command.GroupID = GroupMore
		}
	}
	builtins(root)
	help(root)
	root.SetFlagErrorFunc(flagRefusal)
	root.SetArgs(args)
	command, err := root.ExecuteContextC(launch.WithGiven(ctx, c.given))
	return refusal(command, err)
}

// Words of a command as its Use names them, after the path of the command: icebath sessions export <id>
func usage(command *cobra.Command) string {
	line := command.CommandPath()
	if _, rest, ok := strings.Cut(command.Use, " "); ok {
		line += " " + rest
	}
	return line
}

// Count of arguments in words: no arguments, 1 argument, 2 arguments
func arguments(n int) string {
	if n == 0 {
		return "no arguments"
	}
	return strs.Count(n, "argument")
}

// Count of arguments a person gave, in words
func gave(n int) string {
	if n == 0 {
		return "none"
	}
	return strs.Number(float64(n))
}

// Sentence for a refusal of cobra's own, which counts with "arg(s)" and starts in lower case; any other error as it came
func refusal(command *cobra.Command, err error) error {
	if err == nil || command == nil {
		return err
	}
	text := err.Error()
	path := command.CommandPath()
	usageLine := "\nUsage: " + usage(command)
	var n, least, most, got int
	var word, name string
	switch {
	case strings.HasPrefix(text, "unknown command "):
		if _, scanErr := fmt.Sscanf(text, "unknown command %q for %q", &word, &name); scanErr != nil {
			return err
		}
		if !command.HasSubCommands() {
			return fmt.Errorf("%s takes no arguments. Leave out “%s”.%s", path, word, usageLine)
		}
		if suggestions := command.SuggestionsFor(word); len(suggestions) > 0 {
			return fmt.Errorf("%s has no command “%s”. Did you mean %s %s?", path, word, path, strings.Join(suggestions, " or "+path+" "))
		}
		return fmt.Errorf("%s has no command “%s”. Run %s --help to see its commands.", path, word, path)
	case strings.HasPrefix(text, "invalid argument "):
		if _, scanErr := fmt.Sscanf(text, "invalid argument %q for %q", &word, &name); scanErr != nil {
			return err
		}
		var valid []string
		for _, arg := range command.ValidArgs {
			valid = append(valid, strings.SplitN(arg, "\t", 2)[0])
		}
		return fmt.Errorf("%s does not take “%s”. It takes %s.", path, word, strings.Join(valid, ", "))
	case strings.HasPrefix(text, "requires at least "):
		if _, scanErr := fmt.Sscanf(text, "requires at least %d arg(s), only received %d", &n, &got); scanErr != nil {
			return err
		}
		return fmt.Errorf("%s needs at least %s, and you gave %s.%s", path, arguments(n), gave(got), usageLine)
	case strings.HasPrefix(text, "accepts at most "):
		if _, scanErr := fmt.Sscanf(text, "accepts at most %d arg(s), received %d", &n, &got); scanErr != nil {
			return err
		}
		return fmt.Errorf("%s takes at most %s, and you gave %s.%s", path, arguments(n), gave(got), usageLine)
	case strings.HasPrefix(text, "accepts between "):
		if _, scanErr := fmt.Sscanf(text, "accepts between %d and %d arg(s), received %d", &least, &most, &got); scanErr != nil {
			return err
		}
		return fmt.Errorf("%s takes %s to %s arguments, and you gave %s.%s", path, strs.Number(float64(least)), strs.Number(float64(most)),
			gave(got), usageLine)
	case strings.HasPrefix(text, "accepts "):
		if _, scanErr := fmt.Sscanf(text, "accepts %d arg(s), received %d", &n, &got); scanErr != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%s takes no arguments.%s", path, usageLine)
		}
		return fmt.Errorf("%s takes %s, and you gave %s.%s", path, arguments(n), gave(got), usageLine)
	case strings.HasPrefix(text, "required flag(s) "):
		list, ok := strings.CutPrefix(text, `required flag(s) "`)
		if !ok {
			return err
		}
		names := strings.Split(strings.TrimSuffix(list, `" not set`), `", "`)
		for i, flagName := range names {
			names[i] = "--" + flagName
		}
		joined := names[len(names)-1]
		if len(names) > 1 {
			joined = strings.Join(names[:len(names)-1], ", ") + " and " + joined
		}
		return fmt.Errorf("%s needs %s.%s", path, joined, usageLine)
	}
	return err
}

// Sentence for a flag that was misspelled, left without its value or given one of the wrong kind
func flagRefusal(command *cobra.Command, err error) error {
	path := command.CommandPath()
	if missing, ok := errors.AsType[*pflag.NotExistError](err); ok {
		flagName := "--" + missing.GetSpecifiedName()
		if missing.GetSpecifiedShortnames() != "" {
			flagName = "-" + missing.GetSpecifiedName()
		}
		return fmt.Errorf("%s has no flag %s. Run %s --help to see its flags.", path, flagName, path)
	}
	if empty, ok := errors.AsType[*pflag.ValueRequiredError](err); ok {
		flagName := "--" + empty.GetSpecifiedName()
		if empty.GetSpecifiedShortnames() != "" {
			flagName = "-" + empty.GetSpecifiedName()
		}
		return fmt.Errorf("%s needs a value after it, such as %s VALUE.", flagName, flagName)
	}
	if wrong, ok := errors.AsType[*pflag.InvalidValueError](err); ok {
		flag := wrong.GetFlag()
		takes := "a value of another kind"
		switch kind := flag.Value.Type(); {
		case kind == "bool":
			takes = "true or false"
		case kind == "duration":
			takes = "a length of time such as 30s, 5m or 2h"
		case strings.HasPrefix(kind, "int"), strings.HasPrefix(kind, "uint"), kind == "count":
			takes = "a whole number"
		case strings.HasPrefix(kind, "float"):
			takes = "a number"
		}
		return fmt.Errorf("“%s” is not a value --%s takes. It takes %s.", wrong.GetValue(), flag.Name, takes)
	}
	if syntax, ok := errors.AsType[*pflag.InvalidSyntaxError](err); ok {
		return fmt.Errorf("“%s” is not a flag. A flag is a name after - or --, such as --help.", syntax.GetSpecifiedFlag())
	}
	return err
}

// The server command, which main runs ahead of the command line and the root's help lists
func (c *CLI) server() *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "Run icebath as a server that apps connect to",
		Long: "Runs icebath as a server on the address in /settings under Server, until you stop it. The app and the commands of icebath " +
			"on another computer reach it after icebath connect, with the server's password. icebath makes a password the first time " +
			"the server starts, and you find it in /settings under Server too.",
		Example: "  icebath server\n  icebath connect http://icebath:PASSWORD@127.0.0.1:7433",
		GroupID: GroupApp,
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return c.serve(cmd.Context()) },
	}
}

// Puts cobra's help and completion commands under This app, in the words of the rest
func builtins(root *cobra.Command) {
	root.SetHelpCommandGroupID(GroupApp)
	root.SetCompletionCommandGroupID(GroupApp)
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	for _, command := range root.Commands() {
		switch command.Name() {
		case "help":
			command.Short = "Show help for any command"
			command.Long = "Shows the help of the command you name, such as icebath help chat. With no command, help shows the list of commands."
			// cobra's own answer to a command that does not exist is "Unknown help topic [`name`]"
			shown := command.Run
			command.Run = func(cmd *cobra.Command, args []string) {
				if found, _, err := cmd.Root().Find(args); found == nil || err != nil {
					cmd.Printf("icebath has no command “%s”. These are the commands it has:\n\n", strings.Join(args, " "))
					_ = cmd.Root().Usage()
					return
				}
				shown(cmd, args)
			}
		case "completion":
			command.Short = "Print a script that completes commands in your shell"
			command.Long = "Prints a script that lets your shell complete icebath commands, flags and arguments when you press tab. The " +
				"help of each shell says how to load it."
			for _, shell := range command.Commands() {
				shell.Short = "Print the completion script for " + shell.Name()
				shell.Long = strings.Replace(shell.Long, "Generate the autocompletion script for", "Prints the completion script for", 1)
				if flag := shell.Flags().Lookup("no-descriptions"); flag != nil {
					flag.Usage = "Leave the descriptions out of the completions"
				}
			}
		}
	}
}

// Gives every command a help flag in the words of the rest, where cobra's own would say "help for name"
func help(command *cobra.Command) {
	if command.Flags().Lookup("help") == nil {
		command.Flags().BoolP("help", "h", false, "Show this help")
		_ = command.Flags().SetAnnotation("help", cobra.FlagSetByCobraAnnotation, []string{"true"})
	}
	for _, sub := range command.Commands() {
		help(sub)
	}
}

// Columns help is wrapped to: the terminal's up to 100, else 80
func columns() int {
	if !term.IsTerminal(os.Stdout.Fd()) {
		return 80
	}
	width, _, err := term.GetSize(os.Stdout.Fd())
	if err != nil || width <= 0 {
		return 80
	}
	return min(width, 100)
}

// Text with each line wrapped at its words to the columns, blank and indented lines kept as written
func wrap(text string, columns int) string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			out = append(out, line)
			continue
		}
		row := ""
		for word := range strings.FieldsSeq(line) {
			switch {
			case row == "":
				row = word
			case utf8.RuneCountInString(row)+1+utf8.RuneCountInString(word) > columns:
				out = append(out, row)
				row = word
			default:
				row += " " + word
			}
		}
		out = append(out, row)
	}
	return strings.Join(out, "\n")
}
