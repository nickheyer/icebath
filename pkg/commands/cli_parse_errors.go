package commands

import (
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	mutuallyExclusiveAnnotation = "cobra_annotation_mutually_exclusive"
	oneRequiredAnnotation       = "cobra_annotation_one_required"
)

// ArgparseError is a parse failure reported as usage and the prog error line with exit status 2
type ArgparseError struct {
	Cmd *cobra.Command
	Msg string
}

func (e *ArgparseError) Error() string { return e.Msg }

// Usage line followed by the prog error line, the text ArgumentParser.error writes to stderr
func (e *ArgparseError) Stderr() string {
	return argparseUsage(e.Cmd) + e.Cmd.CommandPath() + ": error: " + e.Msg
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
	case errors.As(err, &invalid) && errors.Is(invalid, errHelpRemoved):
		return &ArgparseError{Cmd: cmd, Msg: "unrecognized arguments: " + removedHelpToken(cmd, invalid.GetValue())}
	case errors.As(err, &invalid):
		return &ArgparseError{Cmd: cmd, Msg: "argument " + actionName(invalid.GetFlag()) + ": " + invalidValueText(invalid)}
	case errors.As(err, &required):
		return &ArgparseError{Cmd: cmd, Msg: "argument " + actionName(required.GetFlag()) + ": expected one argument"}
	case errors.As(err, &missing):
		token := "--" + missing.GetSpecifiedName()
		if short := missing.GetSpecifiedShortnames(); short != "" {
			token = "-" + short
		}
		return &ArgparseError{Cmd: cmd, Msg: "unrecognized arguments: " + token}
	case errors.As(err, &syntax):
		return &ArgparseError{Cmd: cmd, Msg: "unrecognized arguments: " + syntax.GetSpecifiedFlag()}
	}
	return err
}

// Command line token that reached a removed help option, the first -h cluster or --help the parse met
func removedHelpToken(cmd *cobra.Command, value string) string {
	if c, ok := cliRegistry.Load(cmd.Root()); ok {
		for _, tok := range c.(*CLI).argv {
			if tok == "--" {
				break
			}
			if tok == "--help" || strings.HasPrefix(tok, "--help=") || strings.HasPrefix(tok, "-h") {
				return tok
			}
		}
	}
	if value == removedHelpBare {
		return "--help"
	}
	return "--help=" + value
}

// Type conversion message argparse prints for a rejected option value
func invalidValueText(invalid *pflag.InvalidValueError) string {
	cause := errors.Unwrap(invalid)
	var num *strconv.NumError
	switch t := invalid.GetFlag().Value.Type(); {
	case t == "bool":
		return "ignored explicit argument " + pyrepr.StrRepr(invalid.GetValue())
	case errors.As(cause, &num) && (t == "int" || t == "count"):
		return "invalid int value: " + pyrepr.StrRepr(invalid.GetValue())
	case errors.As(cause, &num) && t == "float64":
		return "invalid float value: " + pyrepr.StrRepr(invalid.GetValue())
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

// Wraps a command's argument validator with the argparse group checks and the unrecognized option check
func (c *CLI) withArgparseGroups(inner cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := argparseExclusive(cmd); err != nil {
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
			return &ArgparseError{Cmd: cmd, Msg: "unrecognized arguments: " + strings.Join(extras, " ")}
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
		return &ArgparseError{Cmd: cmd, Msg: "unrecognized arguments: " + strings.Join(args, " ")}
	}
	return nil
}
