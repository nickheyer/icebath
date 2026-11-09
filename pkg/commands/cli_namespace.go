package commands

import (
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Argparse style vars of a parsed invocation with flag dests, named positionals and the top level command
func (c *CLI) Namespace(cmd *cobra.Command, args []string) map[string]any {
	ns := map[string]any{}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "help" {
			return
		}
		key := strings.ReplaceAll(f.Name, "-", "_")
		if d := f.Annotations["dest"]; len(d) > 0 {
			key = d[0]
		}
		ns[key] = flagValue(f)
	})
	specs := c.positionals[cmd]
	for i, s := range specs {
		switch {
		case s.Variadic || s.Remain:
			if i < len(args) {
				ns[s.Name] = append([]string{}, args[i:]...)
			} else if s.Default != "" {
				ns[s.Name] = s.Default
			} else {
				ns[s.Name] = nil
			}
		case i < len(args):
			ns[s.Name] = positionalValue(s, args[i])
		case s.Default != "":
			ns[s.Name] = positionalValue(s, s.Default)
		default:
			ns[s.Name] = nil
		}
	}
	path := strings.Fields(c.pathOf(cmd))
	if len(path) > 0 {
		ns["command"] = path[0]
	} else {
		ns["command"] = nil
	}
	return ns
}

// Typed value of one flag, None for an unset flag whose default is empty
func flagValue(f *pflag.Flag) any {
	switch f.Value.Type() {
	case "bool":
		v, _ := strconv.ParseBool(f.Value.String())
		return v
	case "int", "count":
		v, _ := strconv.Atoi(f.Value.String())
		return v
	case "float64":
		v, _ := strconv.ParseFloat(f.Value.String(), 64)
		return v
	case "stringArray", "stringSlice":
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			if !f.Changed && len(sv.GetSlice()) == 0 {
				return nil
			}
			return append([]string{}, sv.GetSlice()...)
		}
	}
	if !f.Changed && f.Value.String() == "" {
		return nil
	}
	return f.Value.String()
}

// Positional token converted the way its argparse type converts it
func positionalValue(s posSpec, raw string) any {
	if s.Int {
		if v, err := strconv.Atoi(raw); err == nil {
			return v
		}
	}
	return raw
}
