package tui

import (
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the form of one icebath command
const runCommandContext = "tui.runcommand"

// Keys of the form of one icebath command beyond the form keys
func registerRunCommandKeys(km *keys.Map) {
	if keys.ContextDocs[runCommandContext] == "" {
		if err := keys.AddContext(runCommandContext, "form of an icebath command"); err != nil {
			panic(err)
		}
	}
	km.Register(keys.Action{ID: "tui.runcommand.line", Context: runCommandContext, Label: "type it", Doc: "type the command line instead of filling the form", Default: []string{"ctrl+t"}})
}

func init() {
	hubRegistry = append(hubRegistry, hubDef{id: "commands", order: 98, title: txt.HubsTitleCommands, short: txt.HubsShortCommands, slash: "run", usage: "[command]",
		open: func(a *app, arg string) tea.Cmd { return a.openRunCommand(arg) }, keys: registerRunCommandKeys})
}

// Screen of every icebath command a person may run, read from the background service: pick one, fill its form, run it here or after exit
func (a *app) runCommand() tea.Cmd { return a.openRunCommand("") }

// Opens the icebath commands screen, at the form of the command the words name when they name one, else filtered by them
func (a *app) openRunCommand(arg string) tea.Cmd {
	var commands []tuigateway.CLICommand
	loaded := false
	var h *cards.Hub
	open := func(i int) tea.Cmd {
		if i < 0 || i >= len(commands) {
			return nil
		}
		return a.commandForm(h, commands[i])
	}
	picker := cards.NewList(a.hubEnv(), cards.ListSpec{
		Title: tr(txt.HubsTitleCommands), Glyph: a.toolkit.Glyphs.Terminal, Key: "tui:commands", Context: hubPickContext, Primary: "hub.pick",
		Columns: []render.Column{{Title: tr(txt.HubsColCommand), Max: 34}, {Title: tr(txt.HubsColWhen)}, {Title: tr(txt.HubsColWhatItDoes), Flex: 1}},
		Yield:   []int{1},
		Rows: func() [][]string {
			out := make([][]string, len(commands))
			for i, c := range commands {
				when := ""
				if c.Class == tuigateway.CLIRunAfterExit {
					when = tr(txt.RunCommandAfterExit)
				}
				out[i] = []string{c.Path, when, c.Short}
			}
			return out
		},
		Detail: func(i int) []string {
			c := commands[i]
			out := []string{"icebath " + c.Path, c.Usage}
			if c.Long != "" {
				out = append(out, "", c.Long)
			}
			return out
		},
		Empty: func() string {
			if !loaded {
				return tr(txt.RunCommandLoading)
			}
			return tr(txt.RunCommandEmpty)
		},
		Pick: true,
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.pick", Label: tr(txt.HintsLabelOpen)}}
		},
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{"hub.pick": func(_ *cards.List, i int) tea.Cmd { return open(i) }},
	})
	h = a.newHub("tui:commands", picker)
	h.Busy(tr(txt.RunCommandLoading))
	load := call(a.gw, tuigateway.CommandsTree, tuigateway.CommandsTreeParams{}, func(r *tuigateway.CommandsTreeResult, err error) tea.Cmd {
		loaded = true
		if err != nil {
			return h.Fail(rpcErrorMessage(err))
		}
		h.Quiet()
		commands = commands[:0]
		for _, c := range r.Commands {
			if c.Class == tuigateway.CLIRunInternal || c.Hidden || c.Group || c.Path == "" || c.Path == "chat" {
				continue
			}
			commands = append(commands, c)
		}
		picker.Refresh()
		arg = strings.Join(strings.Fields(arg), " ")
		if arg == "" {
			return nil
		}
		for i, c := range commands {
			if c.Path == arg {
				picker.Select(i)
				return open(i)
			}
		}
		picker.Filter(arg)
		return nil
	})
	return tea.Batch(a.wm.ShowOverlay(h), load)
}

// One field of a command's form and the command line words its value spells
type commandField struct {
	field cards.Field
	words func(value string) []string
	// Long name of the flag the field sets, empty for a positional argument
	flag string
}

// Whether a flag type takes a list of values, given once per value
func listFlag(f tuigateway.CLIFlag) bool {
	return f.Repeatable || strings.HasSuffix(f.Type, "Array") || strings.HasSuffix(f.Type, "Slice")
}

// Whether a flag or argument type takes a number
func numberType(t string) bool {
	return strings.HasPrefix(t, "int") || strings.HasPrefix(t, "uint") || strings.HasPrefix(t, "float")
}

// Choices of a field: leaving it out first unless it is required, then the values the parser accepts
func fieldChoices(values []string, required bool) []cards.Choice {
	var out []cards.Choice
	if !required {
		out = append(out, cards.Choice{Value: "", Label: tr(txt.SlashFormLeaveOut)})
	}
	for _, v := range values {
		out = append(out, cards.Choice{Value: v, Label: v})
	}
	return out
}

// Field of a command's form for one of its options, nil words for a value that leaves the option out
func flagFields(f tuigateway.CLIFlag) []commandField {
	name := "--" + f.Name
	field := cards.Field{ID: "flag:" + f.Name, Label: name, Help: f.Help, Required: f.Required, More: f.Hidden || f.Global}
	if f.Default != "" && f.Type != "bool" {
		field.Placeholder = tr(txt.RunCommandDefault, f.Default)
	} else if f.Metavar != "" {
		field.Placeholder = f.Metavar
	}
	value := func(v string) []string {
		if v = strings.TrimSpace(v); v == "" {
			return nil
		}
		return []string{name, v}
	}
	switch {
	case f.Type == "bool":
		field.Kind, field.Value, field.Placeholder = cards.FieldBool, strconv.FormatBool(f.Default == "true"), ""
		return []commandField{{field: field, flag: f.Name, words: func(v string) []string {
			switch {
			case v == strconv.FormatBool(f.Default == "true"):
				return nil
			case v == "true":
				return []string{name}
			}
			return []string{name + "=false"}
		}}}
	case f.Type == "count":
		field.Kind = cards.FieldNumber
		return []commandField{{field: field, flag: f.Name, words: func(v string) []string {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			out := make([]string, 0, max(n, 0))
			for range max(n, 0) {
				out = append(out, name)
			}
			return out
		}}}
	case len(f.Choices) > 0 && !listFlag(f):
		field.Kind, field.Choices = cards.FieldChoice, fieldChoices(f.Choices, f.Required)
		field.Value = field.Choices[0].Value
		if slices.Contains(f.Choices, f.Default) && f.Required {
			field.Value = f.Default
		}
	case listFlag(f):
		field.Kind = cards.FieldText
		field.Help = strings.TrimSpace(f.Help + " " + tr(txt.RunCommandSeveral))
		return []commandField{{field: field, flag: f.Name, words: func(v string) []string {
			var out []string
			for _, part := range strings.Split(v, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, name, part)
				}
			}
			return out
		}}}
	case numberType(f.Type):
		field.Kind = cards.FieldNumber
	default:
		field.Kind = cards.FieldText
	}
	if !f.ValueOptional {
		return []commandField{{field: field, flag: f.Name, words: value}}
	}
	// An option that also stands alone gets a switch for itself and a field for its value
	alone := cards.Field{ID: "flag:" + f.Name + ":on", Label: name, Help: f.Help, Kind: cards.FieldBool, Value: "false", More: field.More}
	field.Label, field.Help, field.More, field.Required = tr(txt.RunCommandValueOf, name), "", true, false
	var given bool
	return []commandField{
		{field: alone, flag: f.Name, words: func(v string) []string {
			given = v == "true"
			return nil
		}},
		{field: field, flag: f.Name, words: func(v string) []string {
			if v = strings.TrimSpace(v); v != "" {
				return []string{name + "=" + v}
			}
			if given {
				return []string{name}
			}
			return nil
		}},
	}
}

// Field of a command's form for one of its positional arguments
func argField(i int, arg tuigateway.CLIArg) commandField {
	label := arg.Metavar
	if label == "" {
		label = arg.Name
	}
	field := cards.Field{ID: "arg:" + strconv.Itoa(i), Label: label, Help: arg.Help, Required: !arg.Optional, Kind: cards.FieldText}
	if arg.Default != "" {
		field.Placeholder = tr(txt.RunCommandDefault, arg.Default)
	}
	words := func(v string) []string {
		if v = strings.TrimSpace(v); v != "" {
			return []string{v}
		}
		return nil
	}
	switch {
	case arg.Variadic || arg.Remainder:
		field.Help = strings.TrimSpace(arg.Help + " " + tr(txt.RunCommandSeveralWords))
		words = func(v string) []string {
			out, _ := splitCommandLine(v, runtime.GOOS == "windows")
			return out
		}
	case len(arg.Choices) > 0:
		field.Kind, field.Choices = cards.FieldChoice, fieldChoices(arg.Choices, !arg.Optional)
		field.Value = field.Choices[0].Value
	case numberType(arg.Type):
		field.Kind = cards.FieldNumber
	}
	return commandField{field: field, words: words}
}

// Fields of a command's form in order: its arguments, the options it needs, its other options, then those kept back
func commandFields(c tuigateway.CLICommand) []commandField {
	if c.RawArgs {
		field := cards.Field{ID: "raw", Label: tr(txt.RunCommandArguments), Help: c.Usage, Kind: cards.FieldText}
		return []commandField{{field: field, words: func(v string) []string {
			out, _ := splitCommandLine(v, runtime.GOOS == "windows")
			return out
		}}}
	}
	var args, needed, rest, back []commandField
	for i, arg := range c.Args {
		args = append(args, argField(i, arg))
	}
	for _, f := range c.Flags {
		if f.Name == "help" {
			continue
		}
		for _, cf := range flagFields(f) {
			switch {
			case cf.field.Required:
				needed = append(needed, cf)
			case cf.field.More:
				back = append(back, cf)
			default:
				rest = append(rest, cf)
			}
		}
	}
	return slices.Concat(args, needed, rest, back)
}

// Words of the command line a filled form spells: the command, its options, then its arguments
func commandArgv(c tuigateway.CLICommand, fields []commandField, values map[string]string) []string {
	argv := strings.Fields(c.Path)
	var positional []string
	for _, cf := range fields {
		words := cf.words(values[cf.field.ID])
		if cf.flag != "" {
			argv = append(argv, words...)
		} else {
			positional = append(positional, words...)
		}
	}
	return append(argv, positional...)
}

// Options a filled form sets, by long name
func flagsSet(fields []commandField, values map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, cf := range fields {
		if cf.flag != "" && len(cf.words(values[cf.field.ID])) > 0 {
			out[cf.flag] = true
		}
	}
	return out
}

// Form of one icebath command built from its arguments and options, its usage line above, sent to run here or after exit
func (a *app) commandForm(h *cards.Hub, c tuigateway.CLICommand) tea.Cmd {
	fields := commandFields(c)
	specFields := make([]cards.Field, len(fields))
	for i, cf := range fields {
		specFields[i] = cf.field
	}
	intro := []string{c.Usage}
	if c.Short != "" {
		intro = append(intro, c.Short)
	}
	submit := tr(txt.RunCommandRun)
	if c.Class == tuigateway.CLIRunAfterExit {
		intro = append(intro, tr(txt.RunCommandRunsAfterExit, c.ClassReason))
		submit = tr(txt.RunCommandQueue)
	}
	var form *cards.Form
	spec := cards.FormSpec{Title: c.Path, Intro: intro, Fields: specFields, Submit: submit, Context: runCommandContext,
		Hints: []wm.Hint{{Action: "tui.runcommand.line"}},
		Done: func(f *cards.Form, values map[string]string) tea.Cmd {
			set := flagsSet(fields, values)
			for _, flag := range c.Flags {
				if !set[flag.Name] {
					continue
				}
				for _, other := range flag.Exclusive {
					if other != flag.Name && set[other] {
						f.SetError("flag:"+other, tr(txt.RunCommandExclusive, "--"+flag.Name))
						return nil
					}
				}
			}
			return a.launchCommand(h, c, commandArgv(c, fields, values))
		},
		Pick: func(f *cards.Form, field cards.Field) tea.Cmd {
			choices := make([]hubChoice, len(field.Choices))
			for i, ch := range field.Choices {
				choices[i] = hubChoice{id: ch.Value, title: ch.Label, detail: ch.Meaning}
			}
			return a.hubPick(h, field.Label, choices, field.Value, func(ch hubChoice) tea.Cmd {
				f.SetValue(field.ID, ch.id)
				return nil
			})
		},
		Actions: map[string]func(f *cards.Form) tea.Cmd{
			"tui.runcommand.line": func(f *cards.Form) tea.Cmd {
				line := joinCommandLine(commandArgv(c, fields, f.Values()))
				return a.hubAsk(h, c.Path, tr(txt.RunCommandLine), line, false, func(text string) tea.Cmd { return a.runTypedLine(h, c, text) })
			},
		}}
	form = cards.NewForm(a.hubEnv(), spec)
	return h.Push(form)
}

// Runs a command line typed in place of the form, which must keep the command's own words at its start
func (a *app) runTypedLine(h *cards.Hub, c tuigateway.CLICommand, text string) tea.Cmd {
	path := strings.Fields(c.Path)
	argv, closed := splitCommandLine(text, runtime.GOOS == "windows")
	switch {
	case !closed:
		return h.Fail(tr(txt.RunCommandUnclosed))
	case len(argv) == 0:
		return nil
	case !slices.Equal(argv[:min(len(argv), len(path))], path):
		return h.Fail(tr(txt.RunCommandOtherCommand, c.Path))
	}
	return a.launchCommand(h, c, argv)
}

// Runs a command line in a terminal window of this space, or queues it to run after exit when it cannot run inside, closing the hub
func (a *app) launchCommand(h *cards.Hub, c tuigateway.CLICommand, argv []string) tea.Cmd {
	path := strings.Fields(c.Path)
	readOnly := slices.ContainsFunc(argv[min(len(path), len(argv)):], func(w string) bool {
		name, _, _ := strings.Cut(w, "=")
		return strings.HasPrefix(name, "--") && slices.Contains(c.InsideWith, name[2:])
	})
	if c.Class == tuigateway.CLIRunAfterExit && !readOnly {
		return tea.Batch(wm.RequestDismissOf(h), a.queueAfterExitCmd(argv, c.ClassReason))
	}
	exe, err := os.Executable()
	if err != nil {
		return h.Fail(err.Error())
	}
	return tea.Batch(wm.RequestDismissOf(h), a.runIcebath(exe, argv))
}

// Runs icebath with the words in a terminal window of this space; the window closes when the command ends, so its output opens in a pager
// or the transcript, and a run that could not start says why in a toast
func (a *app) runIcebath(exe string, argv []string) tea.Cmd {
	line := "icebath " + joinCommandLine(argv)
	return a.runInTerminal(append([]string{exe}, argv...), func(end cards.TerminalEnd) tea.Cmd {
		switch {
		case end.Err != nil:
			return wm.Notify(tr(txt.RunCommandFailed, line, end.Err.Error()), wm.Error)
		case end.Closed:
			return nil
		}
		out := strings.TrimRight(end.Output, "\n ")
		if out == "" {
			out = tr(txt.RunCommandNoOutput, line)
		}
		return a.showOutput(out, line)
	})
}

// Words of a command line the way a shell reads them: spaces part words, quotes keep spaces, and a backslash keeps the next character except
// inside single quotes or, on Windows, where it separates path parts; false when a quote or backslash is left open
func splitCommandLine(s string, windows bool) ([]string, bool) {
	var words []string
	var cur strings.Builder
	in, quote, escaped := false, rune(0), false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped, in = false, true
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"' && r == '"':
			quote = 0
		case r == '\\' && !windows && (quote == 0 || quote == '"'):
			escaped = true
		case quote == '"':
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		words = append(words, cur.String())
	}
	return words, quote == 0 && !escaped
}

// Command line of the words for display, a word that is empty or holds a space or quote put in double quotes
func joinCommandLine(argv []string) string {
	parts := make([]string, len(argv))
	for i, w := range argv {
		parts[i] = w
		if w == "" || strings.ContainsAny(w, " \t\n'\"") {
			parts[i] = `"` + strings.ReplaceAll(w, `"`, `\"`) + `"`
		}
	}
	return strings.Join(parts, " ")
}
