package commands

import (
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	mutuallyExclusiveAnnotation = "cobra_annotation_mutually_exclusive"
	oneRequiredAnnotation       = "cobra_annotation_one_required"
	argparseTopAnnotation       = "argparse_top"
)

// ArgparseError is a parse failure reported as usage and the prog error line with exit status 2
type ArgparseError struct {
	Cmd *cobra.Command
	Msg string
	// Leftover arguments the parser could not place, reported against the top command
	Unrecognized bool
}

func (e *ArgparseError) Error() string { return e.Msg }

// Usage line followed by the prog error line, leftover arguments reported by the parser parse_args ran on
func (e *ArgparseError) Stderr() string {
	cmd := e.Cmd
	if e.Unrecognized {
		cmd = argparseTop(cmd)
	}
	return argparseUsage(cmd) + cmd.CommandPath() + ": error: " + e.Msg
}

// Command whose parser runs parse_args, the nearest one carrying its own ArgumentParser or else the root
func argparseTop(cmd *cobra.Command) *cobra.Command {
	for c := cmd; c.HasParent(); c = c.Parent() {
		if c.Annotations[argparseTopAnnotation] != "" {
			return c
		}
	}
	return cmd.Root()
}

// Option strings of a flag joined by slashes, the name argparse gives an optional action
func actionName(f *pflag.Flag) string {
	var names []string
	if f.Shorthand != "" {
		names = append(names, "-"+f.Shorthand)
	}
	names = append(names, "--"+f.Name)
	aliases := append([]string{}, f.Annotations["aliases"]...)
	sort.Strings(aliases)
	for _, a := range aliases {
		names = append(names, "--"+a)
	}
	return strings.Join(names, "/")
}

// Argparse wording of a pflag parse failure, any other error returned unchanged
func argparseFlagError(cmd *cobra.Command, err error) error {
	var invalid *pflag.InvalidValueError
	var required *pflag.ValueRequiredError
	var missing *pflag.NotExistError
	var syntax *pflag.InvalidSyntaxError
	switch {
	case errors.As(err, &invalid):
		return &ArgparseError{Cmd: cmd, Msg: "argument " + actionName(invalid.GetFlag()) + ": " + invalidValueText(invalid)}
	case errors.As(err, &required):
		return &ArgparseError{Cmd: cmd, Msg: "argument " + actionName(required.GetFlag()) + ": expected one argument"}
	case errors.As(err, &missing):
		token := "--" + missing.GetSpecifiedName()
		if short := missing.GetSpecifiedShortnames(); short != "" {
			token = "-" + short
		}
		return argparseUnknownOption(cmd, token)
	case errors.As(err, &syntax):
		return argparseUnknownOption(cmd, syntax.GetSpecifiedFlag())
	}
	return err
}

// Error for an option a command does not declare: what its own parser raises first over the rest of its command line, a refused positional value or the required arguments left unfilled, else the unrecognized option the top parser reports
func argparseUnknownOption(cmd *cobra.Command, token string) error {
	if c, ok := cliRegistry.Load(cmd.Root()); ok {
		cli := c.(*CLI)
		if words, given, reached := cli.ownArguments(cmd); reached {
			specs := cli.positionals[cmd]
			if err := positionalValueError(cmd, specs, words); err != nil {
				return err
			}
			if missing := argparseMissingRequired(cmd, specs, words, func(f *pflag.Flag) bool { return given[f.Name] }); len(missing) > 0 {
				return &ArgparseError{Cmd: cmd, Msg: "the following arguments are required: " + strings.Join(missing, ", ")}
			}
		}
	}
	return &ArgparseError{Cmd: cmd, Unrecognized: true, Msg: "unrecognized arguments: " + token}
}

// Positional words and declared flags of a command's own part of the command line, an undeclared option taking no value the way argparse sets it aside, reached false when the command line never arrives at the command
func (c *CLI) ownArguments(cmd *cobra.Command) (words []string, given map[string]bool, reached bool) {
	level := c.Root
	given = map[string]bool{}
	literal := false
	for i := 0; i < len(c.argv); i++ {
		tok := c.argv[i]
		switch {
		case literal:
			if level == cmd {
				words = append(words, tok)
			}
		case tok == "--":
			literal = true
		case strings.HasPrefix(tok, "--"):
			name, _, hasValue := strings.Cut(tok[2:], "=")
			f := level.Flags().Lookup(name)
			if f == nil {
				continue
			}
			if level == cmd {
				given[f.Name] = true
			}
			if !hasValue && f.NoOptDefVal == "" && i+1 < len(c.argv) {
				i++
			}
		case strings.HasPrefix(tok, "-") && len(tok) > 1:
			for j, r := range tok[1:] {
				f := level.Flags().ShorthandLookup(string(r))
				if f == nil {
					break
				}
				if level == cmd {
					given[f.Name] = true
				}
				if f.NoOptDefVal == "" {
					if j+1 == len(tok)-1 && i+1 < len(c.argv) {
						i++
					}
					break
				}
			}
		default:
			if level != cmd {
				if next := findSubcommand(level, tok); next != nil {
					level = next
					continue
				}
			}
			if level == cmd {
				words = append(words, tok)
			}
		}
	}
	return words, given, level == cmd
}

// Type conversion message argparse prints for a rejected option value
func invalidValueText(invalid *pflag.InvalidValueError) string {
	cause := errors.Unwrap(invalid)
	var num *strconv.NumError
	switch t := invalid.GetFlag().Value.Type(); {
	case t == "bool":
		return "ignored explicit argument " + strconv.Quote(invalid.GetValue())
	case errors.As(cause, &num) && (t == "int" || t == "count"):
		return "invalid int value: " + strconv.Quote(invalid.GetValue())
	case errors.As(cause, &num) && t == "float64":
		return "invalid float value: " + strconv.Quote(invalid.GetValue())
	case cause != nil:
		return cause.Error()
	}
	return invalid.Error()
}

// Flags of a command in the order the command line set them
func flagsInSetOrder(fs *pflag.FlagSet) []*pflag.Flag {
	var set []*pflag.Flag
	sorted := fs.SortFlags
	fs.SortFlags = false
	fs.Visit(func(f *pflag.Flag) { set = append(set, f) })
	fs.SortFlags = sorted
	return set
}

// Refuses a flag given after a member of its mutually exclusive group, naming the earlier member
func argparseExclusive(cmd *cobra.Command) error {
	fs := cmd.Flags()
	seen := map[string]bool{}
	for _, f := range flagsInSetOrder(fs) {
		for _, group := range f.Annotations[mutuallyExclusiveAnnotation] {
			for _, peer := range strings.Fields(group) {
				if peer == f.Name || !seen[peer] {
					continue
				}
				return &ArgparseError{Cmd: cmd, Msg: "argument " + actionName(f) + ": not allowed with argument " + actionName(fs.Lookup(peer))}
			}
		}
		seen[f.Name] = true
	}
	return nil
}

// Refuses a parse that gave no member of a required group, naming the visible members
func argparseOneRequired(cmd *cobra.Command) error {
	fs := cmd.Flags()
	var groups []string
	known := map[string]bool{}
	fs.VisitAll(func(f *pflag.Flag) {
		for _, group := range f.Annotations[oneRequiredAnnotation] {
			if !known[group] {
				known[group] = true
				groups = append(groups, group)
			}
		}
	})
	for _, group := range groups {
		given := false
		var shown []string
		for _, name := range strings.Fields(group) {
			f := fs.Lookup(name)
			if f == nil {
				continue
			}
			given = given || f.Changed
			if !f.Hidden {
				shown = append(shown, actionName(f))
			}
		}
		if !given {
			return &ArgparseError{Cmd: cmd, Msg: "one of the arguments " + strings.Join(shown, " ") + " is required"}
		}
	}
	return nil
}

// Declared actions of a command's parser, none for a command the parser table never declared
func apDeclared(cmd *cobra.Command) []apAction {
	if own, found := argparseOwn.Load(cmd); found {
		return own.(*apParser).Acts
	}
	if p, ok := argparseTree[apPath(cmd)]; ok {
		return p.Acts
	}
	return nil
}

// Names of the required arguments the given words and the flags given reports as set leave unfilled, positionals and options in the parser's declaration order and undeclared ones after them
func argparseMissingRequired(cmd *cobra.Command, specs []posSpec, args []string, given func(*pflag.Flag) bool) []string {
	unfilled := map[int]bool{}
	for _, i := range unfilledPositionals(specs, args) {
		unfilled[i] = true
	}
	marked := func(f *pflag.Flag) bool {
		required := f.Annotations[cobra.BashCompOneRequiredFlag]
		return len(required) > 0 && required[0] == "true"
	}
	var names []string
	listed := map[string]bool{}
	position := 0
	for _, a := range apDeclared(cmd) {
		switch a.Kind {
		case 'p':
			if unfilled[position] {
				names = append(names, specs[position].display())
				delete(unfilled, position)
			}
			position++
		case 'f':
			f := apFlag(cmd, a.Opts)
			if f == nil || listed[f.Name] || cmd.DisableFlagParsing {
				continue
			}
			listed[f.Name] = true
			if !given(f) && (a.Required || marked(f)) {
				names = append(names, strings.Join(a.Opts, "/"))
			}
		}
	}
	if !cmd.DisableFlagParsing {
		fs := cmd.Flags()
		sorted := fs.SortFlags
		fs.SortFlags = false
		fs.VisitAll(func(f *pflag.Flag) {
			if !listed[f.Name] && !given(f) && marked(f) {
				names = append(names, actionName(f))
			}
		})
		fs.SortFlags = sorted
	}
	for i, s := range specs {
		if unfilled[i] {
			names = append(names, s.display())
		}
	}
	return names
}

// Refuses a parse that left a required positional or option unfilled, naming every one of them in a single error
func argparseRequired(cmd *cobra.Command, specs []posSpec, args []string) error {
	if missing := argparseMissingRequired(cmd, specs, args, func(f *pflag.Flag) bool { return f.Changed }); len(missing) > 0 {
		return &ArgparseError{Cmd: cmd, Msg: "the following arguments are required: " + strings.Join(missing, ", ")}
	}
	return nil
}

// Wraps a command's argument validator with the checks every argparse parser runs, in its order: exclusive groups and positional values while consuming, then required arguments, required groups and unrecognized options
func (c *CLI) withArgparseGroups(inner cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := argparseExclusive(cmd); err != nil {
			return err
		}
		specs := c.positionals[cmd]
		if err := positionalValueError(cmd, specs, args); err != nil {
			return err
		}
		if err := argparseRequired(cmd, specs, args); err != nil {
			return err
		}
		if inner != nil {
			if err := inner(cmd, args); err != nil {
				return err
			}
		}
		if err := argparseOneRequired(cmd); err != nil {
			return err
		}
		if extras := c.droppedOptions(cmd); len(extras) > 0 {
			return &ArgparseError{Cmd: cmd, Unrecognized: true, Msg: "unrecognized arguments: " + strings.Join(extras, " ")}
		}
		return nil
	}
}

// Option tokens the root and group levels let through unknown on the way to cmd, the argparse extras
func (c *CLI) droppedOptions(cmd *cobra.Command) []string {
	level := c.Root
	var extras []string
	for i := 0; i < len(c.argv); i++ {
		tok := c.argv[i]
		if tok == "--" {
			break
		}
		if !level.FParseErrWhitelist.UnknownFlags {
			if level == cmd {
				break
			}
			continue
		}
		fs := level.Flags()
		switch {
		case strings.HasPrefix(tok, "--"):
			name, _, hasValue := strings.Cut(tok[2:], "=")
			f := fs.Lookup(name)
			switch {
			case f == nil:
				extras = append(extras, tok)
			case !hasValue && f.NoOptDefVal == "" && i+1 < len(c.argv):
				i++
			}
		case strings.HasPrefix(tok, "-") && len(tok) > 1:
			for j, r := range tok[1:] {
				f := fs.ShorthandLookup(string(r))
				if f == nil {
					extras = append(extras, tok)
					break
				}
				if f.NoOptDefVal == "" {
					if j+1 == len(tok)-1 && i+1 < len(c.argv) {
						i++
					}
					break
				}
			}
		default:
			next := findSubcommand(level, tok)
			if next == nil || level == cmd {
				return extras
			}
			level = next
		}
	}
	return extras
}

// Option tokens of a parse ready argv that no parser level on the way to cmd declares
func (c *CLI) UnknownOptions(argv []string, cmd *cobra.Command) []string {
	c.argv = argv
	return c.droppedOptions(cmd)
}

// Child of a command named or aliased by token, nil when the command has none
func findSubcommand(cmd *cobra.Command, token string) *cobra.Command {
	for _, sub := range cmd.Commands() {
		if sub.Name() == token || sub.HasAlias(token) {
			return sub
		}
	}
	return nil
}

// Refuses any positional on a command that declares none, the argparse unrecognized arguments error
func argparseNoArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return &ArgparseError{Cmd: cmd, Unrecognized: true, Msg: "unrecognized arguments: " + strings.Join(args, " ")}
	}
	return nil
}
