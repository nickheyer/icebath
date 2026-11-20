package app

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// One add_argument call of a console parser, a positional when flags is empty
type consoleArg struct {
	flags     []string
	dest      string
	nargs     string
	storeTrue bool
	isInt     bool
	def       any
}

// Positional argument with the argparse nargs and default
func consolePositional(dest, nargs string, def any) consoleArg {
	return consoleArg{dest: dest, nargs: nargs, def: def}
}

// Option taking one value, its dest derived from the first long option string
func consoleOption(def any, isInt bool, flags ...string) consoleArg {
	return consoleArg{flags: flags, dest: consoleDest(flags), def: def, isInt: isInt}
}

// store_true option defaulting to False
func consoleFlag(flags ...string) consoleArg {
	return consoleArg{flags: flags, dest: consoleDest(flags), storeTrue: true, def: false}
}

// Dest argparse derives from option strings, the first long one without dashes and hyphens as underscores
func consoleDest(flags []string) string {
	pick := flags[0]
	for _, f := range flags {
		if strings.HasPrefix(f, "--") {
			pick = f
			break
		}
	}
	return strings.ReplaceAll(strings.TrimLeft(pick, "-"), "-", "_")
}

// Name an argparse error gives an action, its option strings joined by slashes or its dest
func (a *consoleArg) name() string {
	if len(a.flags) > 0 {
		return strings.Join(a.flags, "/")
	}
	return a.dest
}

// Regex pattern argparse matches an action against over the A O and dash argument classes
func (a *consoleArg) nargsPattern() string {
	option := len(a.flags) > 0
	switch {
	case a.storeTrue:
		return "([AO]{0})"
	case a.nargs == "?" && option:
		return "(A?)"
	case a.nargs == "?":
		return "(-*A?-*)"
	case a.nargs == "+" && option:
		return "(A+)"
	case a.nargs == "+":
		return "(-*A[A-]*)"
	case option:
		return "([A])"
	}
	return "(-*A-*)"
}

// argparse ArgumentError text, prefixed with the action name when one is attached
func consoleArgError(action *consoleArg, msg string) error {
	if action == nil {
		return &ConsoleCommandError{Msg: msg}
	}
	return &ConsoleCommandError{Msg: "argument " + action.name() + ": " + msg}
}

// One interpretation of an option looking argument string
type consoleOptionTuple struct {
	action        *consoleArg
	optionString  string
	sep, explicit string
	hasExplicit   bool
}

// A help less argparse parser over the specs, the _ArgumentParser of console_engine.py
type consoleParser struct {
	prog    string
	actions []*consoleArg
	options map[string]*consoleArg
	order   []string
}

var consoleNegativeNumber = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)

// Parses args under prog with the specs, every parse error raised as a console command error naming prog
func consoleParse(prog string, args []string, specs ...consoleArg) (map[string]any, error) {
	p := &consoleParser{prog: prog, options: map[string]*consoleArg{}}
	for i := range specs {
		a := &specs[i]
		p.actions = append(p.actions, a)
		for _, f := range a.flags {
			p.options[f] = a
			p.order = append(p.order, f)
		}
	}
	ns, extras, err := p.parseKnown(args)
	if err == nil && len(extras) > 0 {
		err = &ConsoleCommandError{Msg: "unrecognized arguments: " + strings.Join(extras, " ")}
	}
	if err != nil {
		return nil, &ConsoleCommandError{Msg: prog + ": " + err.Error()}
	}
	return ns, nil
}

// Interpretations of an argument string as an option, nil when it is a positional
func (p *consoleParser) parseOptional(arg string) []consoleOptionTuple {
	if arg == "" || arg[0] != '-' {
		return nil
	}
	if a, ok := p.options[arg]; ok {
		return []consoleOptionTuple{{action: a, optionString: arg}}
	}
	if len(arg) == 1 {
		return nil
	}
	if opt, explicit, found := strings.Cut(arg, "="); found {
		if a, ok := p.options[opt]; ok {
			return []consoleOptionTuple{{action: a, optionString: opt, sep: "=", explicit: explicit, hasExplicit: true}}
		}
	}
	if tuples := p.optionTuples(arg); len(tuples) > 0 {
		return tuples
	}
	if consoleNegativeNumber.MatchString(arg) {
		return nil
	}
	if strings.Contains(arg, " ") {
		return nil
	}
	return []consoleOptionTuple{{optionString: arg}}
}

// Every option an abbreviated or attached value option string could mean
func (p *consoleParser) optionTuples(optionString string) []consoleOptionTuple {
	var out []consoleOptionTuple
	prefix, explicit, found := strings.Cut(optionString, "=")
	sep := ""
	if found {
		sep = "="
	}
	if optionString[1] == '-' {
		for _, f := range p.order {
			if strings.HasPrefix(f, prefix) {
				out = append(out, consoleOptionTuple{action: p.options[f], optionString: f, sep: sep, explicit: explicit, hasExplicit: found})
			}
		}
		return out
	}
	short, shortExplicit := optionString[:2], optionString[2:]
	for _, f := range p.order {
		if f == short {
			out = append(out, consoleOptionTuple{action: p.options[f], optionString: f, explicit: shortExplicit, hasExplicit: true})
		} else if strings.HasPrefix(f, prefix) {
			out = append(out, consoleOptionTuple{action: p.options[f], optionString: f, sep: sep, explicit: explicit, hasExplicit: found})
		}
	}
	return out
}

// Count of arguments the pattern of action takes from the start of the class string
func (p *consoleParser) matchArgument(action *consoleArg, pattern string) (int, error) {
	m := regexp.MustCompile("^" + action.nargsPattern()).FindStringSubmatch(pattern)
	if m == nil {
		switch action.nargs {
		case "?":
			return 0, consoleArgError(action, "expected at most one argument")
		case "+":
			return 0, consoleArgError(action, "expected at least one argument")
		}
		return 0, consoleArgError(action, "expected one argument")
	}
	return len(m[1]), nil
}

// Argument counts of the longest prefix of positionals the class string satisfies
func (p *consoleParser) matchPartial(actions []*consoleArg, pattern string) []int {
	for i := len(actions); i > 0; i-- {
		var re strings.Builder
		re.WriteString("^")
		for _, a := range actions[:i] {
			re.WriteString(a.nargsPattern())
		}
		loc := regexp.MustCompile(re.String()).FindStringSubmatchIndex(pattern)
		if loc == nil {
			continue
		}
		result := make([]int, 0, i)
		for g := 1; g <= i; g++ {
			result = append(result, loc[2*g+1]-loc[2*g])
		}
		if loc[1] < len(pattern) && pattern[loc[1]] == 'O' {
			for len(result) > 0 && result[len(result)-1] == 0 {
				result = result[:len(result)-1]
			}
		}
		return result
	}
	return nil
}

// Converted value of one argument string, an int action parsing it the way int() does
func (p *consoleParser) value(action *consoleArg, raw string) (any, error) {
	if !action.isInt {
		return raw, nil
	}
	n, ok := pyint.ParseBase(raw, 10)
	if !ok {
		return nil, consoleArgError(action, "invalid int value: "+pyrepr.StrRepr(raw))
	}
	return n, nil
}

// Value an action stores for its argument strings, the argparse _get_values rules
func (p *consoleParser) values(action *consoleArg, args []string) (any, error) {
	switch {
	case action.storeTrue:
		return true, nil
	case len(args) == 0 && action.nargs == "?":
		if s, ok := action.def.(string); ok {
			return p.value(action, s)
		}
		return action.def, nil
	case len(args) == 1 && (action.nargs == "" || action.nargs == "?"):
		return p.value(action, args[0])
	}
	out := make([]any, 0, len(args))
	for _, raw := range args {
		v, err := p.value(action, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// The argparse _parse_known_args loop, the namespace and the arguments no action consumed
func (p *consoleParser) parseKnown(args []string) (map[string]any, []string, error) {
	ns := map[string]any{}
	for _, a := range p.actions {
		ns[a.dest] = a.def
	}
	optionIndices := map[int][]consoleOptionTuple{}
	var classes strings.Builder
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			classes.WriteString("-")
			for range args[i+1:] {
				classes.WriteString("A")
			}
			break
		}
		if tuples := p.parseOptional(args[i]); tuples != nil {
			optionIndices[i] = tuples
			classes.WriteString("O")
		} else {
			classes.WriteString("A")
		}
	}
	pattern := classes.String()
	seen := map[*consoleArg]bool{}
	take := func(action *consoleArg, strs []string) error {
		seen[action] = true
		v, err := p.values(action, strs)
		if err != nil {
			return err
		}
		ns[action.dest] = v
		return nil
	}
	var extras []string
	type pending struct {
		action *consoleArg
		args   []string
	}
	consumeOptional := func(start int) (int, error) {
		tuples := optionIndices[start]
		if len(tuples) > 1 {
			names := make([]string, 0, len(tuples))
			for _, t := range tuples {
				names = append(names, t.optionString)
			}
			return 0, consoleArgError(nil, fmt.Sprintf("ambiguous option: %s could match %s", args[start], strings.Join(names, ", ")))
		}
		t := tuples[0]
		action, optionString, sep, explicit, hasExplicit := t.action, t.optionString, t.sep, t.explicit, t.hasExplicit
		var taken []pending
		stop := 0
		for {
			if action == nil {
				extras = append(extras, args[start])
				return start + 1, nil
			}
			if hasExplicit {
				count, err := p.matchArgument(action, "A")
				if err != nil {
					return 0, err
				}
				if count == 0 && optionString[1] != '-' && explicit != "" {
					if sep != "" || explicit[0] == '-' {
						return 0, consoleArgError(action, "ignored explicit argument "+pyrepr.StrRepr(explicit))
					}
					taken = append(taken, pending{action: action})
					optionString = "-" + explicit[:1]
					if next, ok := p.options[optionString]; ok {
						action = next
						explicit = explicit[1:]
						switch {
						case explicit == "":
							sep, hasExplicit = "", false
						case explicit[0] == '=':
							sep, explicit = "=", explicit[1:]
						default:
							sep = ""
						}
						continue
					}
					extras = append(extras, "-"+explicit)
					stop = start + 1
					break
				}
				if count == 1 {
					stop = start + 1
					taken = append(taken, pending{action: action, args: []string{explicit}})
					break
				}
				return 0, consoleArgError(action, "ignored explicit argument "+pyrepr.StrRepr(explicit))
			}
			begin := start + 1
			count, err := p.matchArgument(action, pattern[begin:])
			if err != nil {
				return 0, err
			}
			stop = begin + count
			taken = append(taken, pending{action: action, args: args[begin:stop]})
			break
		}
		for _, t := range taken {
			if err := take(t.action, t.args); err != nil {
				return 0, err
			}
		}
		return stop, nil
	}
	var positionals []*consoleArg
	for _, a := range p.actions {
		if len(a.flags) == 0 {
			positionals = append(positionals, a)
		}
	}
	consumePositionals := func(start int) (int, error) {
		counts := p.matchPartial(positionals, pattern[start:])
		for i, count := range counts {
			strs := append([]string{}, args[start:start+count]...)
			if strings.Contains(pattern[start:start+count], "-") {
				for j, s := range strs {
					if s == "--" {
						strs = append(strs[:j], strs[j+1:]...)
						break
					}
				}
			}
			start += count
			if err := take(positionals[i], strs); err != nil {
				return 0, err
			}
		}
		positionals = positionals[len(counts):]
		return start, nil
	}
	maxOption := -1
	for i := range optionIndices {
		if i > maxOption {
			maxOption = i
		}
	}
	start := 0
	for start <= maxOption {
		next := start
		for next <= maxOption {
			if _, ok := optionIndices[next]; ok {
				break
			}
			next++
		}
		if start != next {
			end, err := consumePositionals(start)
			if err != nil {
				return nil, nil, err
			}
			if end > start {
				start = end
				continue
			}
			start = end
		}
		if _, ok := optionIndices[start]; !ok {
			extras = append(extras, args[start:next]...)
			start = next
		}
		end, err := consumeOptional(start)
		if err != nil {
			return nil, nil, err
		}
		start = end
	}
	stop, err := consumePositionals(start)
	if err != nil {
		return nil, nil, err
	}
	extras = append(extras, args[stop:]...)
	var required []string
	for _, a := range p.actions {
		if !seen[a] && len(a.flags) == 0 && a.nargs != "?" {
			required = append(required, a.name())
		}
	}
	if len(required) > 0 {
		return nil, nil, consoleArgError(nil, "the following arguments are required: "+strings.Join(required, ", "))
	}
	return ns, extras, nil
}

// Int value of a parsed int argument, its default when unset
func consoleInt(ns map[string]any, dest string) *big.Int {
	switch v := ns[dest].(type) {
	case *big.Int:
		return v
	case int:
		return big.NewInt(int64(v))
	}
	return nil
}

// Whether a parsed int lies outside the inclusive bounds
func consoleIntOutside(n *big.Int, lo, hi int64) bool {
	return n.Cmp(big.NewInt(lo)) < 0 || n.Cmp(big.NewInt(hi)) > 0
}
