package commands

import (
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Argparse style vars of a parsed invocation, every parser level from the root down with named positionals
func (c *CLI) Namespace(cmd *cobra.Command, args []string) map[string]any {
	ns, given := map[string]any{"command": nil}, map[string]bool{}
	if path := strings.Fields(c.pathOf(cmd)); len(path) > 0 {
		ns["command"] = path[0]
	}
	var chain []*cobra.Command
	for level := cmd; level != nil; level = level.Parent() {
		chain = append([]*cobra.Command{level}, chain...)
	}
	for i, level := range chain {
		if key := level.Annotations["subdest"]; key != "" {
			ns[key] = nil
			if i+1 < len(chain) {
				name := chain[i+1].CalledAs()
				if name == "" {
					name = chain[i+1].Name()
				}
				ns[key] = name
			}
		}
		if key := level.Annotations["parser_default"]; key != "" {
			ns[key] = level
		}
		level.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Name == "help" || (level == c.Root && c.Root.PersistentFlags().Lookup(f.Name) != nil) {
				return
			}
			if f.Annotations["suppress"] != nil && !f.Changed {
				return
			}
			if f.Annotations["store_false"] != nil {
				if f.Changed {
					ns[flagDest(f)], given[flagDest(f)] = false, true
				}
				return
			}
			if !given[flagDest(f)] || f.Changed {
				ns[flagDest(f)] = flagValue(f)
			}
		})
	}
	specs := c.positionals[cmd]
	for i, s := range specs {
		switch {
		case s.Variadic || s.Remain:
			if i < len(args) {
				ns[s.Name] = append([]string{}, args[i:]...)
			} else if s.Default != "" {
				ns[s.Name] = s.Default
			} else if emptyPositional(cmd, s.Name) {
				ns[s.Name] = []string{}
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
	return ns
}

// Namespace key of a flag, its argparse dest
func flagDest(f *pflag.Flag) string {
	if d := f.Annotations["dest"]; len(d) > 0 {
		return d[0]
	}
	return strings.ReplaceAll(f.Name, "-", "_")
}

// Leaves each flag out of the namespace until it is given, the argparse SUPPRESS default
func suppress(cmd *cobra.Command, names ...string) {
	for _, n := range names {
		_ = cmd.Flags().SetAnnotation(n, "suppress", []string{"true"})
	}
}

// Reads each typed flag as None until it is given, the argparse None default of an int or float option
func noneDefault(cmd *cobra.Command, names ...string) {
	for _, n := range names {
		_ = cmd.Flags().SetAnnotation(n, "default_none", []string{"true"})
	}
}

// Optional value flag reading True when given without a value, the argparse nargs question mark with const True
func constTrue(cmd *cobra.Command, name string) {
	noOpt(cmd, name, FlagPresent)
	_ = cmd.Flags().SetAnnotation(name, "const", []string{"true"})
}

// Typed value of one flag, None for an unset flag whose default is empty or None
func flagValue(f *pflag.Flag) any {
	if f.Annotations["default_none"] != nil && !f.Changed {
		return nil
	}
	if f.Annotations["const"] != nil && f.Changed && f.Value.String() == FlagPresent {
		return true
	}
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
				if f.Annotations["empty_list"] != nil {
					return []string{}
				}
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
