package tui

import (
	"regexp"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// One argument of a slash command as its usage line spells it
type usagePart struct {
	// Words the usage shows for the part, without its brackets
	text string
	// Flag the part is, empty for a word
	flag string
	// Whether the flag takes the word after it as its value
	valued  bool
	choices []string
	// Whether the command needs the part
	required bool
	// Whether the part is a small language of its own, several verbs with their own words, typed as it reads
	free   bool
	number bool
}

// Words a usage line names a count with
var usageCountWords = map[string]bool{"n": true, "number": true, "days": true, "lines": true, "chars": true, "count": true, "index": true}

// Index of the bracket closing the group that opens at i, square and angle brackets nesting in each other, the end of the text when none closes it
func closingBracket(rs []rune, i int) int {
	depth := 0
	for j := i; j < len(rs); j++ {
		switch rs[j] {
		case '[', '<':
			depth++
		case ']', '>':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return len(rs)
}

// Pieces of a group split at the bars outside nested brackets, each trimmed
func splitAlternatives(text string) []string {
	var out []string
	depth, start := 0, 0
	rs := []rune(text)
	for i, r := range rs {
		switch r {
		case '[', '<':
			depth++
		case ']', '>':
			depth--
		case '|':
			if depth == 0 {
				out = append(out, strings.TrimSpace(string(rs[start:i])))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(string(rs[start:])))
}

// Part of one bracket group of a usage line, the angle bracket marking it required
func usageGroup(inner string, required bool) usagePart {
	p := usagePart{text: strings.TrimSpace(inner), required: required}
	alts := splitAlternatives(p.text)
	if len(alts) > 1 {
		for _, alt := range alts {
			if alt == "" || strings.ContainsAny(alt, " []<>") {
				p.free = true
				return p
			}
		}
		p.choices = alts
		return p
	}
	words := strings.Fields(p.text)
	switch {
	case strings.ContainsAny(p.text, "[]<>") || len(words) == 0 || len(words) > 2:
		p.free = true
	case len(words) == 2 && strings.HasPrefix(words[0], "-"):
		p.flag, p.valued = words[0], true
	case len(words) == 2:
		p.free = true
	case strings.HasPrefix(words[0], "-"):
		p.flag = words[0]
	default:
		p.number = usageCountWords[strings.ToLower(strings.TrimSuffix(words[0], "..."))]
	}
	return p
}

// Parts of a usage line: each bracket group one part, a bare word a part the command line always carries
func parseUsage(usage string) []usagePart {
	var parts []usagePart
	rs := []rune(strings.TrimSpace(usage))
	for i := 0; i < len(rs); {
		switch r := rs[i]; {
		case r == ' ' || r == '\t':
			i++
		case r == '[' || r == '<':
			end := closingBracket(rs, i)
			parts = append(parts, usageGroup(string(rs[i+1:min(end, len(rs))]), r == '<'))
			i = end + 1
		default:
			j := i
			for j < len(rs) && rs[j] != ' ' {
				j++
			}
			word := string(rs[i:j])
			parts = append(parts, usagePart{text: word, choices: []string{word}, required: true})
			i = j
		}
	}
	return parts
}

// Whether a command with the usage line can run without any argument
func usageOptional(usage string) bool {
	for _, p := range parseUsage(usage) {
		if p.required {
			return false
		}
	}
	return true
}

// Usage suffix the background service closes a catalog description with, the word before the colon in the language of the catalog
var catalogUsageSuffix = regexp.MustCompile(`\s*\(\S+: /(\S+) (.+)\)\s*$`)

// Usage of a catalog command the background service spells inside its description, after the command's own name, and the description without it
func catalogUsage(name, desc string) (string, string) {
	m := catalogUsageSuffix.FindStringSubmatchIndex(desc)
	if m == nil || desc[m[2]:m[3]] != name {
		return "", desc
	}
	return desc[m[4]:m[5]], strings.TrimSpace(desc[:m[0]])
}

// Field of a form for one part of a usage line, the suggestions of the command's first argument shown as its help
func usageField(i int, p usagePart, suggestions []string) cards.Field {
	f := cards.Field{ID: "p" + strconv.Itoa(i), Label: p.text, Required: p.required}
	switch {
	case p.flag != "" && !p.valued:
		f.Kind, f.Label, f.Value = cards.FieldBool, p.flag, "false"
	case p.flag != "":
		f.Kind, f.Label, f.Placeholder = cards.FieldText, p.flag, strings.Fields(p.text)[1]
	case len(p.choices) > 0:
		f.Kind, f.Label = cards.FieldChoice, tr(txt.SlashFormChoice)
		if !p.required {
			f.Choices = append(f.Choices, cards.Choice{Value: "", Label: tr(txt.SlashFormLeaveOut)})
		}
		for _, c := range p.choices {
			f.Choices = append(f.Choices, cards.Choice{Value: c, Label: c})
		}
		f.Value = f.Choices[0].Value
	case p.number:
		f.Kind = cards.FieldNumber
	default:
		f.Kind = cards.FieldText
		if p.free {
			f.Label, f.Help = tr(txt.SlashFormWords), p.text
		}
	}
	if len(suggestions) > 0 && f.Kind == cards.FieldText && f.Help == "" {
		f.Help = tr(txt.SlashFormExamples, strings.Join(suggestions[:min(len(suggestions), 8)], ", "))
	}
	return f
}

// Argument words a filled form spells for the usage parts, in usage order
func usageLine(parts []usagePart, values map[string]string) string {
	var words []string
	for i, p := range parts {
		v := strings.TrimSpace(values["p"+strconv.Itoa(i)])
		switch {
		case p.flag != "" && !p.valued:
			if v == "true" {
				words = append(words, p.flag)
			}
		case p.flag != "" && v != "":
			words = append(words, p.flag, v)
		case v != "":
			words = append(words, v)
		}
	}
	return strings.Join(words, " ")
}

// Runs a slash command picked in the palette: at once when it needs no argument, else after a form of its arguments
func (a *app) paletteSlash(name, usage, help string, suggest func(string) []string) tea.Cmd {
	if usageOptional(usage) {
		return a.slash("/"+name, nil, false)
	}
	parts := parseUsage(usage)
	var suggestions []string
	if suggest != nil {
		suggestions = suggest("")
	}
	fields := make([]cards.Field, len(parts))
	for i, p := range parts {
		var s []string
		if i == 0 {
			s = suggestions
		}
		fields[i] = usageField(i, p, s)
	}
	var h *cards.Hub
	spec := cards.FormSpec{Title: "/" + name, Intro: []string{"/" + name + " " + usage, help}, Fields: fields, Submit: tr(txt.SlashFormRun),
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			line := "/" + name
			if args := usageLine(parts, values); args != "" {
				line += " " + args
			}
			return tea.Batch(wm.RequestDismissOf(h), a.slash(line, nil, false))
		},
		Pick: func(f *cards.Form, field cards.Field) tea.Cmd {
			choices := make([]hubChoice, len(field.Choices))
			for i, c := range field.Choices {
				choices[i] = hubChoice{id: c.Value, title: c.Label, detail: c.Meaning}
			}
			return a.hubPick(h, field.Label, choices, field.Value, func(c hubChoice) tea.Cmd {
				f.SetValue(field.ID, c.id)
				return nil
			})
		}}
	h = a.newHub("tui:slash:"+name, cards.NewForm(a.hubEnv(), spec))
	return a.wm.ShowOverlay(h)
}

// Every command as a palette entry, the terminal UI's own and the background service's, each once, those needing arguments asking for them
func (a *app) slashPaletteCommands() []wm.Command {
	idle := func() bool { return !a.dialogs.waiting() }
	// Commands the palette has entries of its own for: the screens of things to manage, help and quit
	taken := map[string]bool{"quit": true, "help": true}
	for _, d := range hubDefs() {
		taken[d.slash] = true
	}
	var out []wm.Command
	// What each command's entry runs, by name, so an alias entry runs the same
	runs := map[string]func() tea.Cmd{}
	for _, d := range hubDefs() {
		runs[d.slash] = func() tea.Cmd { return d.open(a, "") }
	}
	add := func(name string, text func() string, run func() tea.Cmd) {
		runs[strings.ToLower(name)] = run
		out = append(out, wm.Command{ID: "slash." + name, Title: "/" + name, Text: text, When: idle, Run: run})
	}
	for _, c := range a.localCommands {
		if runs[c.name] == nil {
			runs[c.name] = func() tea.Cmd { return a.paletteSlash(c.name, c.usage, c.helpText(), c.suggest) }
		}
		if taken[c.name] {
			continue
		}
		taken[c.name] = true
		add(c.name, func() string { return "/" + c.name + "  " + c.helpText() }, func() tea.Cmd { return a.paletteSlash(c.name, c.usage, c.helpText(), c.suggest) })
	}
	if a.catalog == nil {
		return out
	}
	for _, category := range a.catalog.Categories {
		for _, pair := range category.Pairs {
			name := strings.TrimPrefix(pair[0], "/")
			canon := strings.TrimPrefix(a.catalog.Canon[strings.ToLower(pair[0])], "/")
			if name == "" || taken[strings.ToLower(name)] || (canon != "" && taken[strings.ToLower(canon)]) {
				continue
			}
			taken[strings.ToLower(name)] = true
			usage, desc := catalogUsage(name, pair[1])
			add(name, func() string { return "/" + name + "  " + desc }, func() tea.Cmd { return a.paletteSlash(name, usage, desc, nil) })
		}
	}
	for _, pair := range a.commandAliases() {
		alias, run := strings.TrimPrefix(pair[0], "/"), runs[strings.ToLower(strings.TrimPrefix(pair[1], "/"))]
		if run != nil && !taken[alias] {
			out = append(out, wm.Command{ID: "slash." + alias, Title: pair[0], Text: func() string { return pair[0] + "  " + aliasOf(pair[1]) }, When: idle, Run: run})
		}
	}
	return out
}
