package commands

import (
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// One argparse action of a parser in declaration order
type apAction struct {
	Kind     byte
	Opts     []string
	Dest     string
	Metavar  []string
	Nargs    string
	Choices  []string
	Required bool
	Suppress bool
	Mutex    int
	Help     string
	Sub      []apChoice
}

// One subparser choice with its aliases and the help row it lists when Listed
type apChoice struct {
	Name    string
	Aliases []string
	Help    string
	Listed  bool
}

// Argparse shape of one parser, Raw for RawDescriptionHelpFormatter
type apParser struct {
	Raw           bool
	Desc          bool
	Epilog        bool
	MutexRequired []bool
	Acts          []apAction
}

// Action resolved against the cobra tree with the help text it renders
type apItem struct {
	apAction
	hasHelp bool
	subs    []apItem
}

var (
	cliRegistry          sync.Map
	argparseOwn          sync.Map
	apWhitespaceRe       = regexp.MustCompile(`[ \t\n\r\f\v]+`)
	apLongBreakRe        = regexp.MustCompile(`\n\n\n+`)
	apHelpText           = "show this help message and exit"
	apHelpActionTemplate = apAction{Kind: 'h', Opts: []string{"-h", "--help"}, Dest: "help", Nargs: "0"}
)

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// Command path below the root, the argparse tree key
func apPath(cmd *cobra.Command) string {
	return strings.TrimPrefix(strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()), " ")
}

// Flag of a command named by any of its option strings
func apFlag(cmd *cobra.Command, opts []string) *pflag.Flag {
	for _, o := range opts {
		if name, ok := strings.CutPrefix(o, "--"); ok {
			if fl := cmd.Flags().Lookup(name); fl != nil {
				return fl
			}
			if fl := cmd.InheritedFlags().Lookup(name); fl != nil {
				return fl
			}
		}
	}
	for _, o := range opts {
		if short, ok := strings.CutPrefix(o, "-"); ok && len(short) == 1 {
			if fl := cmd.Flags().ShorthandLookup(short); fl != nil {
				return fl
			}
		}
	}
	return nil
}

// Actions of a command from the hermes parser table, or derived from cobra for commands hermes never declared
func apItems(cmd *cobra.Command) ([]apItem, *apParser) {
	p, ok := argparseTree[apPath(cmd)]
	if own, found := argparseOwn.Load(cmd); found {
		p, ok = own.(*apParser), true
	}
	if ok {
		items := make([]apItem, 0, len(p.Acts))
		for _, a := range p.Acts {
			items = append(items, apResolve(cmd, a))
		}
		return items, p
	}
	return apDerive(cmd)
}

// Help of an option from its cobra flag, or from the remainder positional the cobra tree models it as
func apOptionHelp(cmd *cobra.Command, a apAction) (string, bool) {
	if fl := apFlag(cmd, a.Opts); fl != nil {
		return fl.Usage, true
	}
	if c, ok := cliRegistry.Load(cmd.Root()); ok {
		for _, s := range c.(*CLI).positionals[cmd] {
			if s.Remain && s.Name == a.Dest {
				return s.Help, true
			}
		}
	}
	return "", false
}

// Attaches the help text a table action renders, flag help coming from the cobra flag usage
func apResolve(cmd *cobra.Command, a apAction) apItem {
	it := apItem{apAction: a}
	switch a.Kind {
	case 'h':
		it.Help, it.hasHelp = apHelpText, true
	case 'f':
		it.Help, _ = apOptionHelp(cmd, a)
		it.hasHelp = it.Help != ""
	default:
		it.hasHelp = a.Help != ""
	}
	for _, s := range a.Sub {
		if !s.Listed {
			continue
		}
		metavar := s.Name
		if len(s.Aliases) > 0 {
			metavar += " (" + strings.Join(s.Aliases, ", ") + ")"
		}
		it.subs = append(it.subs, apItem{apAction: apAction{Kind: 'c', Dest: s.Name, Metavar: []string{metavar}, Help: s.Help}, hasHelp: s.Help != ""})
	}
	return it
}

// Builds argparse actions from a cobra command the hermes tree does not carry, such as a plugin command
func apDerive(cmd *cobra.Command) ([]apItem, *apParser) {
	p := &apParser{Desc: cmd.Long != "", Epilog: cmd.Annotations["epilog"] != ""}
	items := []apItem{apResolve(cmd, apHelpActionTemplate)}
	mutex := map[string]int{}
	cmd.Flags().SortFlags = false
	cmd.LocalFlags().VisitAll(func(fl *pflag.Flag) {
		if fl.Name == "help" {
			return
		}
		a := apAction{Kind: 'f', Opts: []string{"--" + fl.Name}, Dest: strings.ReplaceAll(fl.Name, "-", "_"), Suppress: fl.Hidden}
		if d := fl.Annotations["dest"]; len(d) > 0 {
			a.Dest = d[0]
		}
		if fl.Shorthand != "" {
			a.Opts = append([]string{"-" + fl.Shorthand}, a.Opts...)
		}
		switch {
		case fl.Value.Type() == "bool" || fl.Value.Type() == "count":
			a.Nargs = "0"
		case fl.NoOptDefVal != "":
			a.Nargs = "?"
		}
		a.Choices = fl.Annotations["choices"]
		if groups := fl.Annotations["cobra_annotation_mutually_exclusive"]; len(groups) > 0 {
			if _, ok := mutex[groups[0]]; !ok {
				p.MutexRequired = append(p.MutexRequired, false)
				mutex[groups[0]] = len(p.MutexRequired)
			}
			a.Mutex = mutex[groups[0]]
		}
		a.Required = len(fl.Annotations[cobra.BashCompOneRequiredFlag]) > 0
		a.Help = fl.Usage
		items = append(items, apItem{apAction: a, hasHelp: fl.Usage != ""})
	})
	if c, ok := cliRegistry.Load(cmd.Root()); ok {
		for _, s := range c.(*CLI).positionals[cmd] {
			a := apAction{Kind: 'p', Dest: s.Name, Choices: s.Choices, Help: s.Help}
			if s.Metavar != "" {
				a.Metavar = []string{s.Metavar}
			}
			switch {
			case s.Remain:
				a.Nargs = "..."
			case s.Variadic && s.Optional:
				a.Nargs = "*"
			case s.Variadic:
				a.Nargs = "+"
			case s.Optional:
				a.Nargs = "?"
			}
			items = append(items, apItem{apAction: a, hasHelp: s.Help != ""})
		}
	}
	if cmd.HasSubCommands() {
		a := apAction{Kind: 's', Nargs: "A..."}
		for _, sub := range cmd.Commands() {
			if strings.HasPrefix(sub.Name(), "__") {
				continue
			}
			a.Choices = append(a.Choices, sub.Name())
			a.Choices = append(a.Choices, sub.Aliases...)
			a.Sub = append(a.Sub, apChoice{Name: sub.Name(), Aliases: sub.Aliases, Help: sub.Short, Listed: sub.Short != "" && !sub.Hidden})
		}
		items = append(items, apResolve(cmd, a))
	}
	return items, p
}

// Formatter state of one argparse HelpFormatter run
type apFormatter struct {
	prog       string
	width      int
	maxHelpPos int
	indent     int
	maxLen     int
}

func newAPFormatter(prog string) *apFormatter {
	cols, _ := pyshutil.GetTerminalSize(80, 24)
	width := cols - 2
	return &apFormatter{prog: prog, width: width, maxHelpPos: min(24, max(width-20, 4))}
}

func (a *apAction) metavar(def string) []string {
	if a.Metavar != nil {
		return a.Metavar
	}
	if a.Choices != nil {
		return []string{"{" + strings.Join(a.Choices, ",") + "}"}
	}
	return []string{def}
}

// Repeats a single metavar n times the way argparse expands a non tuple metavar
func apTuple(m []string, n int) []string {
	if len(m) != 1 {
		return m
	}
	out := make([]string, n)
	for i := range out {
		out[i] = m[0]
	}
	return out
}

func (a *apAction) formatArgs(def string) string {
	m := a.metavar(def)
	switch a.Nargs {
	case "":
		return m[0]
	case "?":
		return "[" + m[0] + "]"
	case "*":
		if len(m) == 2 {
			return "[" + m[0] + " [" + m[1] + " ...]]"
		}
		return "[" + m[0] + " ...]"
	case "+":
		t := apTuple(m, 2)
		return t[0] + " [" + t[1] + " ...]"
	case "...":
		return "..."
	case "A...":
		return m[0] + " ..."
	case "==SUPPRESS==":
		return ""
	}
	n := 0
	for _, r := range a.Nargs {
		n = n*10 + int(r-'0')
	}
	return strings.Join(apTuple(m, n), " ")
}

func (a *apAction) invocation() string {
	if len(a.Opts) == 0 {
		return strings.Join(a.metavar(a.Dest), " ")
	}
	if a.Nargs == "0" {
		return strings.Join(a.Opts, ", ")
	}
	return strings.Join(a.Opts, ", ") + " " + a.formatArgs(strings.ToUpper(a.Dest))
}

type apGroup struct {
	required bool
	acts     []*apItem
}

// Usage parts with the index where positionals begin, mutually exclusive groups bracketed together
func apUsageParts(items []apItem, mutexRequired []bool) ([]string, int) {
	var acts []*apItem
	for i := range items {
		if !items[i].Suppress {
			acts = append(acts, &items[i])
		}
	}
	pending := map[*apItem]bool{}
	for _, a := range acts {
		pending[a] = true
	}
	groupOf := func(a *apItem) []*apItem {
		var out []*apItem
		for _, b := range acts {
			if b.Mutex == a.Mutex {
				out = append(out, b)
			}
		}
		return out
	}
	var positionals, optionals []apGroup
	for _, a := range acts {
		if len(a.Opts) != 0 {
			continue
		}
		delete(pending, a)
		if a.Mutex > 0 {
			g := apGroup{required: mutexRequired[a.Mutex-1]}
			for _, b := range groupOf(a) {
				if len(b.Opts) > 0 && pending[b] {
					delete(pending, b)
					g.acts = append(g.acts, b)
				}
			}
			g.acts = append(g.acts, a)
			positionals = append(positionals, g)
		} else {
			positionals = append(positionals, apGroup{acts: []*apItem{a}})
		}
	}
	for _, a := range acts {
		if len(a.Opts) == 0 || !pending[a] {
			continue
		}
		delete(pending, a)
		if a.Mutex > 0 {
			g := apGroup{required: mutexRequired[a.Mutex-1], acts: []*apItem{a}}
			for _, b := range groupOf(a) {
				if len(b.Opts) > 0 && pending[b] {
					delete(pending, b)
					g.acts = append(g.acts, b)
				}
			}
			optionals = append(optionals, g)
		} else {
			optionals = append(optionals, apGroup{acts: []*apItem{a}})
		}
	}
	var parts []string
	posStart := -1
	for i, g := range append(optionals, positionals...) {
		start := len(parts)
		if i == len(optionals) {
			posStart = start
		}
		inGroup := len(g.acts) > 1
		for _, a := range g.acts {
			var part string
			if len(a.Opts) == 0 {
				part = a.formatArgs(a.Dest)
				if inGroup && strings.HasPrefix(part, "[") && strings.HasSuffix(part, "]") {
					part = part[1 : len(part)-1]
				}
			} else {
				if a.Nargs == "0" {
					part = a.Opts[0]
				} else {
					part = a.Opts[0] + " " + a.formatArgs(strings.ToUpper(a.Dest))
				}
				if !(a.Required || g.required || inGroup) {
					part = "[" + part + "]"
				}
			}
			parts = append(parts, part)
		}
		if inGroup {
			if g.required {
				parts[start] = "(" + parts[start]
			} else {
				parts[start] = "[" + parts[start]
			}
			for j := start; j < len(parts)-1; j++ {
				parts[j] += " |"
			}
			if g.required {
				parts[len(parts)-1] += ")"
			} else {
				parts[len(parts)-1] += "]"
			}
		}
	}
	if posStart < 0 {
		posStart = len(parts)
	}
	return parts, posStart
}

// Usage block of argparse _format_usage including the two trailing newlines
func (f *apFormatter) usage(items []apItem, mutexRequired []bool) string {
	prefix := "usage: "
	var usage string
	if len(items) == 0 {
		usage = f.prog
	} else {
		parts, posStart := apUsageParts(items, mutexRequired)
		all := append([]string{f.prog}, parts...)
		var nonEmpty []string
		for _, p := range all {
			if p != "" {
				nonEmpty = append(nonEmpty, p)
			}
		}
		usage = strings.Join(nonEmpty, " ")
		textWidth := f.width - f.indent
		if runeLen(prefix)+runeLen(usage) > textWidth {
			optParts, posParts := parts[:posStart], parts[posStart:]
			getLines := func(parts []string, indent string, prefix *string) []string {
				var lines, line []string
				indentLen := runeLen(indent)
				lineLen := indentLen - 1
				if prefix != nil {
					lineLen = runeLen(*prefix) - 1
				}
				for _, part := range parts {
					partLen := runeLen(part)
					if lineLen+1+partLen > textWidth && len(line) > 0 {
						lines = append(lines, indent+strings.Join(line, " "))
						line = nil
						lineLen = indentLen - 1
					}
					line = append(line, part)
					lineLen += partLen + 1
				}
				if len(line) > 0 {
					lines = append(lines, indent+strings.Join(line, " "))
				}
				if prefix != nil {
					lines[0] = lines[0][len(indent):]
				}
				return lines
			}
			var lines []string
			progLen := runeLen(f.prog)
			if float64(runeLen(prefix)+progLen) <= 0.75*float64(textWidth) {
				indent := strings.Repeat(" ", runeLen(prefix)+progLen+1)
				switch {
				case len(optParts) > 0:
					lines = getLines(append([]string{f.prog}, optParts...), indent, &prefix)
					lines = append(lines, getLines(posParts, indent, nil)...)
				case len(posParts) > 0:
					lines = getLines(append([]string{f.prog}, posParts...), indent, &prefix)
				default:
					lines = []string{f.prog}
				}
			} else {
				indent := strings.Repeat(" ", runeLen(prefix))
				lines = getLines(append(append([]string{}, optParts...), posParts...), indent, nil)
				if len(lines) > 1 {
					lines = append(getLines(optParts, indent, nil), getLines(posParts, indent, nil)...)
				}
				lines = append([]string{f.prog}, lines...)
			}
			usage = strings.Join(lines, "\n")
		}
	}
	return prefix + usage + "\n\n"
}

// Text block of argparse _format_text, refilled unless the formatter keeps raw descriptions
func (f *apFormatter) text(text string, raw bool) string {
	if strings.Contains(text, "%(prog)") {
		text = strings.ReplaceAll(strings.ReplaceAll(text, "%(prog)s", f.prog), "%%", "%")
	}
	width := max(f.width-f.indent, 11)
	indent := strings.Repeat(" ", f.indent)
	if raw {
		var b strings.Builder
		for _, line := range pystr.Splitlines(text, true) {
			b.WriteString(indent + line)
		}
		return b.String() + "\n\n"
	}
	o := pystr.DefaultTextwrap(width)
	o.InitialIndent, o.SubsequentIndent = indent, indent
	return strings.Join(pystr.TextwrapWrap(strings.TrimSpace(apWhitespaceRe.ReplaceAllString(text, " ")), o), "\n") + "\n\n"
}

// Records the widest invocation the way add_argument does before any section renders
func (f *apFormatter) measure(it *apItem) {
	if it.Suppress {
		return
	}
	f.maxLen = max(f.maxLen, runeLen(it.invocation())+f.indent)
	f.indent += 2
	for i := range it.subs {
		f.maxLen = max(f.maxLen, runeLen(it.subs[i].invocation())+f.indent)
	}
	f.indent -= 2
}

// One action row with its wrapped help and indented sub actions
func (f *apFormatter) action(it *apItem) string {
	helpPos := min(f.maxLen+2, f.maxHelpPos)
	helpWidth := max(f.width-helpPos, 11)
	actionWidth := helpPos - f.indent - 2
	header := it.invocation()
	var b strings.Builder
	indentFirst := 0
	switch {
	case !it.hasHelp:
		b.WriteString(strings.Repeat(" ", f.indent) + header + "\n")
	case runeLen(header) <= actionWidth:
		b.WriteString(strings.Repeat(" ", f.indent) + header + strings.Repeat(" ", actionWidth-runeLen(header)) + "  ")
	default:
		b.WriteString(strings.Repeat(" ", f.indent) + header + "\n")
		indentFirst = helpPos
	}
	if it.hasHelp && strings.TrimSpace(it.Help) != "" {
		lines := pystr.TextwrapWrap(strings.TrimSpace(apWhitespaceRe.ReplaceAllString(it.Help, " ")), pystr.DefaultTextwrap(helpWidth))
		b.WriteString(strings.Repeat(" ", indentFirst) + lines[0] + "\n")
		for _, line := range lines[1:] {
			b.WriteString(strings.Repeat(" ", helpPos) + line + "\n")
		}
	} else if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
	f.indent += 2
	for i := range it.subs {
		b.WriteString(f.action(&it.subs[i]))
	}
	f.indent -= 2
	return b.String()
}

// Section of argparse _Section.format_help, empty when no action renders
func (f *apFormatter) section(heading string, items []*apItem) string {
	f.indent += 2
	var b strings.Builder
	for _, it := range items {
		b.WriteString(f.action(it))
	}
	f.indent -= 2
	if b.Len() == 0 {
		return ""
	}
	return "\n" + heading + ":\n" + b.String() + "\n"
}

// Full help of a command as Python 3.14 ArgumentParser.format_help prints it
func argparseHelp(cmd *cobra.Command) string {
	items, p := apItems(cmd)
	f := newAPFormatter(cmd.CommandPath())
	var b strings.Builder
	b.WriteString(f.usage(items, p.MutexRequired))
	if p.Desc && cmd.Long != "" {
		b.WriteString(f.text(cmd.Long, p.Raw))
	}
	var positionals, optionals []*apItem
	f.indent = 2
	for i := range items {
		f.measure(&items[i])
		if items[i].Suppress {
			continue
		}
		if len(items[i].Opts) == 0 {
			positionals = append(positionals, &items[i])
		} else {
			optionals = append(optionals, &items[i])
		}
	}
	f.indent = 0
	b.WriteString(f.section("positional arguments", positionals))
	b.WriteString(f.section("options", optionals))
	if epilog := cmd.Annotations["epilog"]; p.Epilog && epilog != "" {
		b.WriteString(f.text(epilog, p.Raw))
	}
	help := apLongBreakRe.ReplaceAllString("\n"+b.String()+"\n", "\n\n")
	return strings.Trim(help, "\n") + "\n"
}

// Usage text of a command as ArgumentParser.format_usage prints it
func argparseUsage(cmd *cobra.Command) string {
	items, p := apItems(cmd)
	return strings.Trim(newAPFormatter(cmd.CommandPath()).usage(items, p.MutexRequired), "\n") + "\n"
}
