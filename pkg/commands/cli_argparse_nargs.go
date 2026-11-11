package commands

import (
	"errors"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/spf13/cobra"
)

// Stores the name a child subcommand was invoked as under key, the argparse add_subparsers dest
func subDest(cmd *cobra.Command, key string) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations["subdest"] = key
}

// Stores the parser command under key in every namespace below it, the argparse set_defaults of a parser
func parserDefault(cmd *cobra.Command, key string) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations["parser_default"] = key
}

// Reads each repeatable flag as an empty list until it is given, the argparse append with default []
func emptyListDefault(cmd *cobra.Command, names ...string) {
	for _, n := range names {
		_ = cmd.Flags().SetAnnotation(n, "empty_list", []string{"true"})
	}
}

// Reads each optional variadic positional as an empty list when absent, the argparse nargs star positional
func emptyPositionalDefault(cmd *cobra.Command, names ...string) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations["empty_positionals"] = strings.Join(names, ",")
}

// Whether an absent positional of the command reads as an empty list
func emptyPositional(cmd *cobra.Command, name string) bool {
	for _, n := range strings.Split(cmd.Annotations["empty_positionals"], ",") {
		if n == name {
			return true
		}
	}
	return false
}

// Repeatable string flag taking every following non option token, the argparse nargs plus option
func nargsPlusFlag(cmd *cobra.Command, name, help string) {
	cmd.Flags().StringArray(name, nil, help)
	_ = cmd.Flags().SetAnnotation(name, "nargs_plus", []string{"true"})
}

// Whether a token reads as an option to argparse, a dash prefix that is not a negative number
func looksLikeOption(tok string) bool {
	if !strings.HasPrefix(tok, "-") || tok == "-" {
		return false
	}
	_, err := strconv.ParseFloat(tok, 64)
	return err != nil
}

// Spreads the values after each nargs plus flag into one flag occurrence per value
func (c *CLI) expandNargsPlus(argv []string) []string {
	target, _, err := c.Root.Find(argv)
	if err != nil || target == nil {
		return argv
	}
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if tok == "--" {
			return append(out, argv[i:]...)
		}
		name, ok := strings.CutPrefix(tok, "--")
		if !ok || strings.Contains(name, "=") {
			out = append(out, tok)
			continue
		}
		f := target.Flags().Lookup(name)
		if f == nil || f.Annotations["nargs_plus"] == nil {
			out = append(out, tok)
			continue
		}
		j := i + 1
		for j < len(argv) && argv[j] != "--" && !looksLikeOption(argv[j]) {
			out = append(out, "--"+name+"="+argv[j])
			j++
		}
		if j == i+1 {
			out = append(out, tok)
		}
		i = j - 1
	}
	return out
}

// Integer retention window in days, refusing text and negative windows the way the argparse type does
type nonNegativeDays int

func (v *nonNegativeDays) String() string { return strconv.Itoa(int(*v)) }

func (v *nonNegativeDays) Type() string { return "int" }

// Accepts Python int text at or above zero
func (v *nonNegativeDays) Set(text string) error {
	n, ok := pyint.Parse(text)
	if !ok || !n.IsInt64() {
		return errors.New("must be an integer")
	}
	if n.Sign() < 0 {
		return errors.New("retention days must be >= 0 (0 disables that sweep)")
	}
	*v = nonNegativeDays(n.Int64())
	return nil
}

// Registers a retention days flag with its default
func nonNegativeDaysFlag(cmd *cobra.Command, name string, def int, help string) {
	v := nonNegativeDays(def)
	cmd.Flags().Var(&v, name, help)
}
