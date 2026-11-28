package app

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/config"
	secretsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/secrets/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Opens the variables a store fills from its references: a row per variable with the reference it is filled from, enter changing the
// reference, alt+n adding a variable as a job of two steps, its name then its reference, and ctrl+d removing one after a question
func (p *page) variablesPage(name string) tea.Cmd {
	source := p.find(name)
	if source == nil {
		return nil
	}
	t := p.kit.Env.Theme
	label := source.GetLabel()
	var bindings []*secretsv1.Binding
	var view *cards.List
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Variables",
		Glyph:   t.Glyphs.Keys,
		Key:     Screen + ":" + name + VariablesOf,
		Context: cards.HubContext,
		Primary: "hub.use",
		Pick:    true,
		Search:  "Search the variables",
		Intro: func() []string {
			lines := []string{"The variables " + label + " fills, each from the reference that holds its value."}
			if len(bindings) == 0 {
				lines = append(lines, "No variables yet.")
			}
			return lines
		},
		Columns: []render.Column{{}, {Flex: 1}},
		Rows: func() [][]string {
			bindings = nil
			if now := p.find(name); now != nil {
				bindings = now.GetBindings()
			}
			rows := make([][]string, len(bindings), len(bindings)+1)
			for i, binding := range bindings {
				rows[i] = []string{binding.GetVariable(), render.Dim(binding.GetReference(), t)}
			}
			return append(rows, []string{"Add a variable", render.Dim(t.Glyphs.Forward, t)})
		},
		Detail: func(i int) []string {
			if i >= len(bindings) {
				return []string{"Fill another variable from " + label + "."}
			}
			return []string{"Filled from " + bindings[i].GetReference() + " each time icebath starts, while " + label + " is on."}
		},
		Free: map[string]bool{"hub.add": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			switch {
			case i < 0:
				return []wm.Hint{{Action: "hub.add"}}
			case i >= len(bindings):
				return []wm.Hint{{Action: "hub.use", Label: "add"}}
			}
			return []wm.Hint{{Action: "hub.use", Label: "change"}, {Action: "hub.add"}, {Action: "hub.remove"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				if i >= len(bindings) {
					return p.addVariable(name, view)
				}
				return p.changeReference(name, bindings[i])
			},
			"hub.add": func(*cards.List, int) tea.Cmd { return p.addVariable(name, view) },
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if i >= len(bindings) {
					return nil
				}
				variable := bindings[i].GetVariable()
				question := cards.Question{Text: "Stop filling " + variable + " from " + label + "? Its reference leaves the settings, and the item in " +
					label + " stays.", Verb: "Remove", Deletes: true}
				l.Confirm(question, func() tea.Cmd {
					return p.change(func() (*secretsv1.ChangeResponse, error) {
						return p.service.Unbind(p.kit.Env.Ctx, &secretsv1.UnbindRequest{Profile: p.profile, Name: name, Variable: variable})
					}, nil)
				})
				return nil
			},
		},
	})
	p.watch(view)
	return p.hub.Push(view)
}

// Field of a reference of the store, its line saying where one is found
func referenceField(source *secretsv1.Source, value string) cards.Field {
	scheme := source.GetScheme() + "://"
	return cards.Field{ID: "reference", Label: "Reference", Value: value, Required: true,
		Help: strings.TrimSpace("Where " + source.GetLabel() + " keeps the value, such as " + source.GetExample() + ". " + source.GetFind()),
		Check: func(typed string) string {
			if !strings.HasPrefix(strings.TrimSpace(typed), scheme) {
				return "A reference starts with " + scheme + "."
			}
			return ""
		}}
}

// Adds a variable the store fills: its name, then its reference, kept only once the reference is checked
func (p *page) addVariable(name string, view *cards.List) tea.Cmd {
	source := p.find(name)
	if source == nil {
		return nil
	}
	f := p.kit.Flow(p.hub, "Add a variable")
	variable := cards.Field{ID: "variable", Label: "Variable", Required: true,
		Help: "The name of the variable " + source.GetLabel() + " fills, such as OPENAI_API_KEY.",
		Check: func(typed string) string {
			typed = strings.TrimSpace(typed)
			switch {
			case !config.EnvName(typed):
				return "A variable name has only letters, digits and underscores, and starts with a letter."
			case slices.ContainsFunc(source.GetBindings(), func(b *secretsv1.Binding) bool { return b.GetVariable() == typed }):
				return source.GetLabel() + " fills " + typed + " already."
			}
			return ""
		}}
	return f.Ask("", []cards.Field{variable}, "", func(values map[string]string) tea.Cmd {
		named := strings.TrimSpace(values["variable"])
		return f.Ask("", []cards.Field{referenceField(source, "")}, "Add", func(values map[string]string) tea.Cmd {
			reference := strings.TrimSpace(values["reference"])
			p.hub.Busy("Checking the reference…")
			return tui.Call(p.hub, func() (*secretsv1.ChangeResponse, error) {
				return p.service.Bind(p.kit.Env.Ctx, &secretsv1.BindRequest{Profile: p.profile, Name: name, Variable: named, Reference: reference})
			}, func(response *secretsv1.ChangeResponse) tea.Cmd {
				return tea.Batch(p.hub.Done(response.GetNote()), f.End(), p.load(func() tea.Cmd {
					if now := p.find(name); now != nil {
						view.Select(slices.IndexFunc(now.GetBindings(), func(b *secretsv1.Binding) bool { return b.GetVariable() == named }))
					}
					return nil
				}))
			})
		})
	})
}

// Asks for another reference of a variable the store fills, kept once it is checked
func (p *page) changeReference(name string, binding *secretsv1.Binding) tea.Cmd {
	source := p.find(name)
	if source == nil {
		return nil
	}
	variable := binding.GetVariable()
	field := referenceField(source, binding.GetReference())
	field.Select = true
	return p.kit.Ask(p.hub, variable, field, "Save", func(reference string) tea.Cmd {
		if reference == binding.GetReference() {
			return nil
		}
		return p.change(func() (*secretsv1.ChangeResponse, error) {
			return p.service.Bind(p.kit.Env.Ctx, &secretsv1.BindRequest{Profile: p.profile, Name: name, Variable: variable, Reference: reference})
		}, nil)
	})
}
