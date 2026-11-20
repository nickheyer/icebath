package verify

import "fmt"

// Argument of the verify subcommand, an argparse add_argument call the CLI binding replays
type ParserArgument struct {
	Name     string
	Action   string
	Choices  []string
	Default  any
	Type     string
	Optional bool
	Help     string
}

// verify subcommand parser, the help, description and arguments the CLI binding builds
type Parser struct {
	Name        string
	Help        string
	Description string
	Arguments   []ParserArgument
	JSONHelp    string
}

// Parser for the verify subcommand with every flag, choice, default and help text
func BuildVerifyParser() Parser {
	return Parser{
		Name: "verify",
		Help: "Detect a project's run recipe and smoke-test it",
		Description: "Detect how the current project is built, tested, and started " +
			"(or load the saved manifest at .icebath/environment.json), then " +
			"run a verification pass: bootstrap -> build -> test -> start in " +
			"background -> poll readiness -> teardown.",
		Arguments: []ParserArgument{
			{Name: "path", Optional: true, Help: "Project root to verify (default: current directory)"},
			{Name: "--detect-only", Action: "store_true", Default: false, Help: "Only detect and print the recipe as JSON; run nothing"},
			{Name: "--save", Action: "store_true", Default: false, Help: "Save the recipe as .icebath/environment.json in the project"},
			{Name: "--skip-start", Action: "store_true", Default: false, Help: "Run command phases but skip starting the app / readiness poll"},
			{Name: "--phase", Action: "append", Choices: []string{"bootstrap", "build", "test", "start"}, Help: "Run only the given phase(s); repeatable"},
			{Name: "--port", Type: "int", Help: "Override the port used for the readiness poll"},
			{Name: "--timeout", Type: "float", Default: DefaultPhaseTimeout,
				Help: fmt.Sprintf("Per-phase timeout in seconds (default: %.0f)", DefaultPhaseTimeout)},
			{Name: "--ready-timeout", Type: "float", Default: DefaultReadyTimeout,
				Help: fmt.Sprintf("Readiness poll timeout in seconds (default: %.0f)", DefaultReadyTimeout)},
		},
		JSONHelp: "Emit a machine-readable JSON result",
	}
}
