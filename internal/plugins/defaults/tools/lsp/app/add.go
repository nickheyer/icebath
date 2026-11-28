package app

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/config"
	lspv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/lsp/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Adds a server of your own in three steps, each one question: its name, the command that starts it, the file types it checks. Nothing is
// kept until the last step succeeds; the new server is then in hand and the note says what was made
func (p *page) add() tea.Cmd {
	f := p.kit.Flow(p.hub, "Add a server")
	name := cards.Field{ID: "name", Label: "Name", Help: "A name for this server, such as gopls.", Check: p.free}
	command := cards.Field{ID: "command", Label: "Command", Help: "The command that starts it, such as gopls serve.", Check: commandLine}
	types := cards.Field{ID: "types", Label: "File types", Help: "The file endings it checks, such as .go, .mod.", Check: endings}
	return f.Ask("", []cards.Field{name}, "", func(named map[string]string) tea.Cmd {
		return f.Ask("", []cards.Field{command}, "", func(run map[string]string) tea.Cmd {
			return f.Ask("", []cards.Field{types}, "Add", func(checks map[string]string) tea.Cmd {
				request := &lspv1.AddRequest{Profile: p.profile, Name: strings.TrimSpace(named["name"]), Command: strings.TrimSpace(run["command"]),
					Types: checks["types"]}
				return tui.Call(p.hub, func() (*lspv1.AddResponse, error) {
					return p.service.Add(p.kit.Env.Ctx, request)
				}, func(*lspv1.AddResponse) tea.Cmd {
					p.focus = request.GetName()
					told := "Added " + request.GetName() + ". It checks " + strs.And(split(request.GetTypes())) + " files."
					return tea.Batch(f.End(), p.hub.Done(told), p.load(), p.options.Load(nil))
				})
			})
		})
	})
}

// What is wrong with a name typed for a new server, empty when it can be one
func (p *page) free(typed string) string {
	typed = strings.TrimSpace(typed)
	switch {
	case typed == "":
		return "Give the server a name."
	case !config.Named(typed):
		return "Use only lower case letters, digits, - and _."
	case p.server(typed) != nil:
		return "There is a server called " + typed + " already. Give yours another name."
	}
	return ""
}

// What is wrong with a command line typed for a server, empty when it splits into words
func commandLine(typed string) string {
	words, err := strs.Shell(typed)
	switch {
	case err != nil:
		return err.Error()
	case len(words) == 0:
		return "Give the command that starts the server."
	}
	return ""
}

// What is wrong with file types typed for a server, empty when there is at least one
func endings(typed string) string {
	if len(split(typed)) == 0 {
		return "Give at least one file ending, such as .go."
	}
	return ""
}

// File types typed, separated by commas or spaces
func split(typed string) []string {
	return strings.FieldsFunc(typed, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
}
