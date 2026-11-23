package tuigateway

// When a CLI command may run relative to a live terminal UI and its background service
type CLIRunClass string

const (
	// Safe to run while the terminal UI and its background service run
	CLIRunInside CLIRunClass = "inside"
	// Must run after the terminal UI exits, it replaces or stops what the UI runs on
	CLIRunAfterExit CLIRunClass = "after_exit"
	// Never offered inside the terminal UI, plumbing other programs call or a command that opens another icebath
	CLIRunInternal CLIRunClass = "internal"
)

func (CLIRunClass) Values() []string { return []string{"inside", "after_exit", "internal"} }

// One option of a CLI command as its parser accepts it
type CLIFlag struct {
	// Long name without dashes
	Name string `json:"name"`
	// One letter short name without the dash
	Shorthand string `json:"shorthand,omitzero"`
	// Other long spellings the parser accepts
	Aliases []string `json:"aliases,omitzero"`
	// Value type, bool, count, string, int, float64, stringArray or another pflag type name
	Type string `json:"type"`
	// Value used when the flag is left out, empty when there is none
	Default string `json:"default,omitzero"`
	// Whether the flag may be given without a value
	ValueOptional bool `json:"value_optional,omitzero"`
	// Whether the flag may be given more than once, each adding a value
	Repeatable bool `json:"repeatable,omitzero"`
	// Only values the parser accepts, empty for free text
	Choices []string `json:"choices,omitzero"`
	// Placeholder the help shows for the value
	Metavar  string `json:"metavar,omitzero"`
	Required bool   `json:"required,omitzero"`
	// Left out of --help
	Hidden bool `json:"hidden,omitzero"`
	// Accepted by every command, the root's persistent flags
	Global bool `json:"global,omitzero"`
	// Flags of its group that may not be given together with it, itself included
	Exclusive []string `json:"exclusive,omitzero"`
	Help      string   `json:"help,omitzero"`
}

// One positional argument of a CLI command in the order the parser reads them
type CLIArg struct {
	Name string `json:"name"`
	// Placeholder the help shows, the name when empty
	Metavar  string `json:"metavar,omitzero"`
	Optional bool   `json:"optional,omitzero"`
	// Takes one or more words
	Variadic bool `json:"variadic,omitzero"`
	// Takes every word after it unparsed, flags included
	Remainder bool `json:"remainder,omitzero"`
	// Value type, string or int
	Type    string   `json:"type"`
	Choices []string `json:"choices,omitzero"`
	Default string   `json:"default,omitzero"`
	Help    string   `json:"help,omitzero"`
}

// One command path of the icebath CLI with its help, arguments, options and run class
type CLICommand struct {
	// Words after icebath, empty for the bare icebath command
	Path    string   `json:"path"`
	Aliases []string `json:"aliases,omitzero"`
	// One line help
	Short string `json:"short,omitzero"`
	// Description --help prints under the usage
	Long string `json:"long,omitzero"`
	// Usage line --help prints, without the leading usage:
	Usage string    `json:"usage"`
	Args  []CLIArg  `json:"args,omitzero"`
	Flags []CLIFlag `json:"flags,omitzero"`
	// Has subcommands
	Group bool `json:"group,omitzero"`
	// Its own parser reads every word, so Flags is not the whole option set and --help is the only full reference
	RawArgs bool `json:"raw_args,omitzero"`
	// Not listed in its parent's --help
	Hidden bool        `json:"hidden,omitzero"`
	Class  CLIRunClass `json:"class"`
	// Why the class is not inside, in plain words
	ClassReason string `json:"class_reason,omitzero"`
	// Flags that make an after_exit command read only, so it runs inside when one is given
	InsideWith []string `json:"inside_with,omitzero"`
	// Plugin that added the command, empty for a built in one
	Plugin string `json:"plugin,omitzero"`
}

// Profile whose plugin commands the tree includes, the connection's own when absent
type CommandsTreeParams struct {
	Profile *string `json:"profile,omitzero"`
}

// Every command path of the icebath CLI in tree order, groups before their subcommands
type CommandsTreeResult struct {
	// Program name a command line starts with
	Program  string       `json:"program"`
	Commands []CLICommand `json:"commands"`
}

// CLI command tree built from the same registration the icebath binary parses with, for the palette's Run command list
var CommandsTree = NewMethod[CommandsTreeParams, CommandsTreeResult]("commands.tree")
