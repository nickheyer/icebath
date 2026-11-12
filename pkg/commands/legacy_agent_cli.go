package commands

import (
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// LegacyAgentProg is the program name the legacy single query runner parses and reports errors under
const LegacyAgentProg = "icebath-agent"

const legacyAgentDescription = "Legacy single-query Icebath Agent runner. For the full CLI use `icebath`."

type legacyAgentAction struct {
	opts []string
	flag string
	kind byte
	def  string
	help string
}

var legacyAgentActions = []legacyAgentAction{
	{opts: []string{"-h", "--help"}, flag: "help", kind: 'h', help: apHelpText},
	{opts: []string{"--version"}, flag: "version", kind: 'v', help: "show program's version number and exit"},
	{opts: []string{"--query", "-q"}, flag: "query", kind: 's', help: "natural-language query to run"},
	{opts: []string{"--model"}, flag: "model", kind: 's', help: "model id (provider/model)"},
	{opts: []string{"--api-key", "--api_key"}, flag: "api-key", kind: 's', help: "API key for the model endpoint"},
	{opts: []string{"--base-url", "--base_url"}, flag: "base-url", kind: 's', help: "model API base URL"},
	{opts: []string{"--max-turns", "--max_turns"}, flag: "max-turns", kind: 'i', def: "10", help: "maximum API call iterations (default: 10)"},
	{opts: []string{"--enabled-toolsets", "--enabled_toolsets"}, flag: "enabled-toolsets", kind: 's', help: "comma-separated toolsets to enable"},
	{opts: []string{"--disabled-toolsets", "--disabled_toolsets"}, flag: "disabled-toolsets", kind: 's', help: "comma-separated toolsets to disable"},
	{opts: []string{"--list-tools", "--list_tools"}, flag: "list-tools", kind: 't', help: "list available tools and exit"},
	{opts: []string{"--save-trajectories", "--save_trajectories"}, flag: "save-trajectories", kind: 't', help: "append the conversation to trajectory JSONL files"},
	{opts: []string{"--save-sample", "--save_sample"}, flag: "save-sample", kind: 't', help: "save one trajectory sample to a UUID-named file"},
	{opts: []string{"--verbose"}, flag: "verbose", kind: 't', help: "verbose logging"},
	{opts: []string{"--log-prefix-chars", "--log_prefix_chars"}, flag: "log-prefix-chars", kind: 'i', def: "20", help: "characters shown in tool-call log previews (default: 20)"},
}

var legacyNegativeNumberRe = regexp.MustCompile(`^-\.?\p{Nd}`)

// LegacyAgentArgs are the parsed legacy runner options, nil where the option keeps its None default
type LegacyAgentArgs struct {
	Prompt                                           []string
	Query, APIKey, EnabledToolsets, DisabledToolsets *string
	Model, BaseURL                                   string
	MaxTurns, LogPrefixChars                         int
	ListTools, SaveTrajectories, SaveSample, Verbose bool
}

// LegacyAgentParser is the legacy runner argument parser, a cobra command parsed with argparse semantics
type LegacyAgentParser struct {
	Cmd     *cobra.Command
	version string
	out     io.Writer
	args    *LegacyAgentArgs
}

type legacyOptionTuple struct {
	action      *legacyAgentAction
	option      string
	sep         string
	explicit    string
	hasExplicit bool
}

// Cobra command carrying every legacy runner flag, its help rendered the way argparse formats this parser
func NewLegacyAgentParser(version string, stdout io.Writer) *LegacyAgentParser {
	p := &LegacyAgentParser{version: version, out: stdout}
	cmd := &cobra.Command{Use: LegacyAgentProg, Long: legacyAgentDescription, SilenceUsage: true, SilenceErrors: true}
	cmd.SetOut(stdout)
	table := &apParser{Desc: true}
	for i := range legacyAgentActions {
		a := &legacyAgentActions[i]
		short := ""
		for _, o := range a.opts {
			if len(o) == 2 {
				short = o[1:]
			}
		}
		switch a.kind {
		case 's':
			cmd.Flags().StringP(a.flag, short, a.def, a.help)
		case 'i':
			def, _ := strconv.Atoi(a.def)
			cmd.Flags().IntP(a.flag, short, def, a.help)
		default:
			cmd.Flags().BoolP(a.flag, short, false, a.help)
		}
		act := apAction{Kind: 'f', Opts: a.opts, Dest: strings.ReplaceAll(a.flag, "-", "_")}
		switch a.kind {
		case 'h':
			act = apHelpActionTemplate
		case 'v', 't':
			act.Nargs = "0"
		}
		table.Acts = append(table.Acts, act)
		if a.kind == 'v' {
			table.Acts = append(table.Acts, apAction{Kind: 'p', Dest: "prompt", Nargs: "*", Help: "query to run (same as --query)"})
		}
	}
	argparseOwn.Store(cmd, table)
	p.Cmd = cmd
	return p
}

// Parses argv like ArgumentParser.parse_args, help and version printed with an exit status 0 error
func (p *LegacyAgentParser) Parse(argv []string) (*LegacyAgentArgs, error) {
	if err := p.parse(argv); err != nil {
		return nil, err
	}
	return p.args, nil
}

// Usage and the prog error line on stderr with exit status 2, the error ArgumentParser.error raises
func (p *LegacyAgentParser) Error(msg string) error {
	return &ArgparseError{Cmd: p.Cmd, Msg: msg}
}

// Writes the full argparse help of the parser
func (p *LegacyAgentParser) PrintHelp(w io.Writer) {
	io.WriteString(w, argparseHelp(p.Cmd))
}

func (p *LegacyAgentParser) lookup(option string) *legacyAgentAction {
	for i := range legacyAgentActions {
		for _, o := range legacyAgentActions[i].opts {
			if o == option {
				return &legacyAgentActions[i]
			}
		}
	}
	return nil
}

// Every option string a token may abbreviate or carry glued, in the order the parser registered them
func (p *LegacyAgentParser) optionTuples(arg string) []legacyOptionTuple {
	var out []legacyOptionTuple
	prefix, value, hasSep := strings.Cut(arg, "=")
	sep := ""
	if hasSep {
		sep = "="
	}
	shortPrefix, shortExplicit := arg, ""
	if r := []rune(arg); len(r) > 2 {
		shortPrefix, shortExplicit = string(r[:2]), string(r[2:])
	}
	for i := range legacyAgentActions {
		a := &legacyAgentActions[i]
		for _, o := range a.opts {
			switch {
			case arg[1] != '-' && o == shortPrefix:
				out = append(out, legacyOptionTuple{action: a, option: o, explicit: shortExplicit, hasExplicit: true})
			case strings.HasPrefix(o, prefix):
				out = append(out, legacyOptionTuple{action: a, option: o, sep: sep, explicit: value, hasExplicit: hasSep})
			}
		}
	}
	return out
}

// Option interpretations of one token, nil when argparse reads it as a positional
func (p *LegacyAgentParser) parseOptional(arg string) []legacyOptionTuple {
	if arg == "" || arg[0] != '-' {
		return nil
	}
	if a := p.lookup(arg); a != nil {
		return []legacyOptionTuple{{action: a, option: arg}}
	}
	if len([]rune(arg)) == 1 {
		return nil
	}
	if name, value, ok := strings.Cut(arg, "="); ok {
		if a := p.lookup(name); a != nil {
			return []legacyOptionTuple{{action: a, option: name, sep: "=", explicit: value, hasExplicit: true}}
		}
	}
	if tuples := p.optionTuples(arg); len(tuples) > 0 {
		return tuples
	}
	if legacyNegativeNumberRe.MatchString(arg) || strings.Contains(arg, " ") {
		return nil
	}
	return []legacyOptionTuple{{option: arg}}
}

type legacyTaken struct {
	action *legacyAgentAction
	args   []string
}

// Runs one option action the way take_action does, help and version ending the parse at once
func (p *LegacyAgentParser) take(t legacyTaken) error {
	fs := p.Cmd.Flags()
	name := strings.Join(t.action.opts, "/")
	switch t.action.kind {
	case 'h':
		p.PrintHelp(p.out)
		return &ExitError{Code: 0}
	case 'v':
		f := newAPFormatter(p.Cmd.CommandPath())
		io.WriteString(p.out, strings.Trim(apLongBreakRe.ReplaceAllString(f.text(p.version, false), "\n\n"), "\n")+"\n")
		return &ExitError{Code: 0}
	case 't':
		return fs.Set(t.action.flag, "true")
	case 'i':
		n, ok := pyint.Parse(t.args[0])
		if !ok {
			return p.Error("argument " + name + ": invalid int value: " + pyrepr.Repr(t.args[0]))
		}
		value := int64(math.MaxInt)
		switch {
		case n.IsInt64() && n.Int64() <= math.MaxInt && n.Int64() >= math.MinInt:
			value = n.Int64()
		case n.Sign() < 0:
			value = math.MinInt
		}
		return fs.Set(t.action.flag, strconv.FormatInt(value, 10))
	}
	return fs.Set(t.action.flag, t.args[0])
}

// Consumes one option and its argument at index, returning the next unconsumed index
func (p *LegacyAgentParser) consumeOptional(argv []string, pattern string, index int, tuples []legacyOptionTuple, extras *[]string) (int, error) {
	if len(tuples) > 1 {
		var names []string
		for _, t := range tuples {
			names = append(names, t.option)
		}
		return 0, p.Error("ambiguous option: " + argv[index] + " could match " + strings.Join(names, ", "))
	}
	t := tuples[0]
	var taken []legacyTaken
	stop := index + 1
	for {
		if t.action == nil {
			*extras = append(*extras, argv[index])
			return index + 1, nil
		}
		takesValue := t.action.kind == 's' || t.action.kind == 'i'
		name := strings.Join(t.action.opts, "/")
		if t.hasExplicit {
			if !takesValue && t.option[1] != '-' && t.explicit != "" {
				if t.sep != "" || t.explicit[0] == '-' {
					return 0, p.Error("argument " + name + ": ignored explicit argument " + pyrepr.Repr(t.explicit))
				}
				taken = append(taken, legacyTaken{action: t.action})
				r := []rune(t.explicit)
				option := "-" + string(r[0])
				next := p.lookup(option)
				if next == nil {
					*extras = append(*extras, "-"+t.explicit)
					stop = index + 1
					break
				}
				rest := string(r[1:])
				t = legacyOptionTuple{action: next, option: option}
				switch {
				case rest == "":
				case rest[0] == '=':
					t.sep, t.explicit, t.hasExplicit = "=", rest[1:], true
				default:
					t.explicit, t.hasExplicit = rest, true
				}
				continue
			}
			if takesValue {
				taken = append(taken, legacyTaken{action: t.action, args: []string{t.explicit}})
				stop = index + 1
				break
			}
			return 0, p.Error("argument " + name + ": ignored explicit argument " + pyrepr.Repr(t.explicit))
		}
		start := index + 1
		if !takesValue {
			taken = append(taken, legacyTaken{action: t.action})
			stop = start
			break
		}
		if start >= len(pattern) || pattern[start] != 'A' {
			return 0, p.Error("argument " + name + ": expected one argument")
		}
		taken = append(taken, legacyTaken{action: t.action, args: []string{argv[start]}})
		stop = start + 1
		break
	}
	for _, t := range taken {
		if err := p.take(t); err != nil {
			return 0, err
		}
	}
	return stop, nil
}

// Matches the zero or more prompt positional at index, deferring an empty match that an option follows
func (p *LegacyAgentParser) consumePositionals(argv []string, pattern string, index int, consumed *bool, prompt *[]string) int {
	if *consumed {
		return index
	}
	end := index
	for end < len(pattern) && (pattern[end] == 'A' || pattern[end] == '-') {
		end++
	}
	if end == index && end < len(pattern) && pattern[end] == 'O' {
		return index
	}
	args := append([]string{}, argv[index:end]...)
	if strings.Contains(pattern[index:end], "-") {
		for i, a := range args {
			if a == "--" {
				args = append(args[:i], args[i+1:]...)
				break
			}
		}
	}
	*consumed, *prompt = true, args
	return end
}

// Classifies argv and consumes optionals and the prompt positional alternately like parse_known_args
func (p *LegacyAgentParser) parse(argv []string) error {
	for i := range legacyAgentActions {
		fl := p.Cmd.Flags().Lookup(legacyAgentActions[i].flag)
		fl.Value.Set(fl.DefValue)
		fl.Changed = false
	}
	indices := map[int][]legacyOptionTuple{}
	var b strings.Builder
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--" {
			b.WriteByte('-')
			b.WriteString(strings.Repeat("A", len(argv)-i-1))
			break
		}
		if tuples := p.parseOptional(argv[i]); tuples != nil {
			indices[i] = tuples
			b.WriteByte('O')
		} else {
			b.WriteByte('A')
		}
	}
	pattern := b.String()
	maxOption := -1
	for i := range indices {
		maxOption = max(maxOption, i)
	}
	prompt := []string{}
	var extras []string
	consumed := false
	index := 0
	for index <= maxOption {
		next := index
		for next <= maxOption && indices[next] == nil {
			next++
		}
		if index != next {
			end := p.consumePositionals(argv, pattern, index, &consumed, &prompt)
			if end > index {
				index = end
				continue
			}
		}
		if indices[index] == nil {
			extras = append(extras, argv[index:next]...)
			index = next
		}
		stop, err := p.consumeOptional(argv, pattern, index, indices[index], &extras)
		if err != nil {
			return err
		}
		index = stop
	}
	stop := p.consumePositionals(argv, pattern, index, &consumed, &prompt)
	extras = append(extras, argv[stop:]...)
	if len(extras) > 0 {
		return p.Error("unrecognized arguments: " + strings.Join(extras, " "))
	}
	p.args = p.collect(prompt)
	return nil
}

// Namespace of the parse read back from the cobra flags, unset options left nil
func (p *LegacyAgentParser) collect(prompt []string) *LegacyAgentArgs {
	fs := p.Cmd.Flags()
	optional := func(name string) *string {
		if !fs.Changed(name) {
			return nil
		}
		v, _ := fs.GetString(name)
		return &v
	}
	str := func(name string) string { v, _ := fs.GetString(name); return v }
	integer := func(name string) int { v, _ := fs.GetInt(name); return v }
	boolean := func(name string) bool { v, _ := fs.GetBool(name); return v }
	return &LegacyAgentArgs{
		Prompt: prompt, Query: optional("query"), APIKey: optional("api-key"),
		EnabledToolsets: optional("enabled-toolsets"), DisabledToolsets: optional("disabled-toolsets"),
		Model: str("model"), BaseURL: str("base-url"), MaxTurns: integer("max-turns"), LogPrefixChars: integer("log-prefix-chars"),
		ListTools: boolean("list-tools"), SaveTrajectories: boolean("save-trajectories"), SaveSample: boolean("save-sample"), Verbose: boolean("verbose"),
	}
}
