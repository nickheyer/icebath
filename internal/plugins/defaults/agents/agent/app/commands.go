package app

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	slashv1 "github.com/nickheyer/icebath/pkg/gen/icebath/slash/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/clip"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

type local struct {
	name    string
	aliases []string
	usage   string
	help    string
	bare    bool
	run     func(c *chat, argument string) tea.Cmd
}

func locals() []local {
	return []local{
		{name: "new", aliases: []string{"clear", "reset"}, help: "Start a new session (fresh session ID + history)", run: (*chat).fresh},
		{name: "redraw", help: "Force a full UI repaint (recovers from terminal drift)", run: (*chat).redraw},
		{name: "history", help: "Show conversation history", run: (*chat).history},
		{name: "resume", usage: "[name]", help: "Resume a previously-named session", run: (*chat).resume},
		{name: "sessions", aliases: []string{"switch", "session"}, help: "Browse and resume previous sessions", run: screen("sessions")},
		{name: "settings", aliases: []string{"config", "preferences"}, help: "Every setting and its value", run: screen("settings")},
		{name: "model", help: "Switch model (session-scoped; --global to persist)", bare: true, run: func(c *chat, _ string) tea.Cmd { return c.pick() }},
		{name: "tools", aliases: []string{"toolsets"}, help: "What the agent can call, by toolset", run: screen("tools")},
		{name: "skin", usage: "[name]", help: "Show or change the display skin/theme", run: (*chat).skin},
		{name: "commands", help: "Browse all commands", run: (*chat).browse},
		{name: "help", usage: "[filter]", help: "Show available commands (/help <text> filters)", run: (*chat).browse},
		{name: "palette", help: "Open the fuzzy command palette", run: func(c *chat, _ string) tea.Cmd { return c.kit.WM.OpenPalette() }},
		{name: "copy", usage: "[number]", help: "Copy the last assistant response to clipboard", run: (*chat).copy},
		{name: "quit", aliases: []string{"exit"}, usage: "[--delete]", help: "Exit (use --delete to also remove session history)", run: (*chat).quit},
	}
}

func screen(id string) func(*chat, string) tea.Cmd {
	return func(c *chat, _ string) tea.Cmd {
		return c.kit.Open(id)
	}
}

func (c *chat) local(text string) (tea.Cmd, bool) {
	word, argument, _ := strings.Cut(strings.TrimSpace(text), " ")
	name, ok := strings.CutPrefix(word, "/")
	if !ok {
		return nil, false
	}
	argument = strings.TrimSpace(argument)
	for _, command := range locals() {
		if command.name != name && !slices.Contains(command.aliases, name) || command.bare && argument != "" {
			continue
		}
		c.note = ""
		return command.run(c, argument), true
	}
	return nil, false
}

func (c *chat) unknown(text string) (string, bool) {
	word, _, _ := strings.Cut(strings.TrimSpace(text), " ")
	name, ok := strings.CutPrefix(word, "/")
	if !ok || name == "" || strings.ContainsAny(name, "/\n") || c.commands == nil {
		return "", false
	}
	for _, command := range c.catalog() {
		if command.GetName() == name || slices.Contains(command.GetAliases(), name) {
			return "", false
		}
	}
	return name, true
}

func (c *chat) catalog() []*slashv1.Command {
	terminal := locals()
	catalog := make([]*slashv1.Command, 0, len(terminal)+len(c.commands))
	for _, command := range terminal {
		catalog = append(catalog, &slashv1.Command{Name: command.name, Aliases: command.aliases, Usage: command.usage, Help: command.help})
	}
	for _, command := range c.commands {
		if !slices.ContainsFunc(terminal, func(l local) bool { return l.name == command.GetName() }) {
			catalog = append(catalog, command)
		}
	}
	slices.SortFunc(catalog, func(a, b *slashv1.Command) int { return strings.Compare(a.GetName(), b.GetName()) })
	return catalog
}

func (c *chat) fresh(string) tea.Cmd {
	if c.stop != nil {
		c.stop()
	}
	c.epoch++
	c.id, c.title, c.stop, c.asked, c.warned, c.starting = "", "", nil, nil, nil, false
	c.log, c.rendered = transcript{expanded: c.log.expanded, quiet: c.log.quiet}, nil
	c.top, c.follow = 0, true
	return nil
}

func (c *chat) redraw(string) tea.Cmd {
	c.rendered = nil
	c.kit.WM.Repaint()
	c.note = "redrawn"
	return tea.ClearScreen
}

func (c *chat) history(string) tea.Cmd {
	if len(c.log.entries) == 0 {
		c.note = "nothing in this session yet"
		return nil
	}
	c.top, c.follow = 0, false
	c.note = "at the start of the session"
	if key := c.kit.Env.Keys.Display("session.scroll.end"); key != "" {
		c.note += ", " + key + " to re-follow"
	}
	return nil
}

func (c *chat) resume(name string) tea.Cmd {
	if name == "" {
		return c.kit.Open("sessions")
	}
	return tui.Async(func() func() tea.Cmd {
		response, err := c.sessions.List(c.kit.Env.Ctx, &sessionsv1.ListRequest{})
		return func() tea.Cmd {
			if err != nil {
				c.note = "error: " + err.Error()
				return nil
			}
			var found *sessionsv1.Session
			for _, session := range response.GetSessions() {
				switch title := session.GetTitle(); {
				case strings.EqualFold(title, name), session.GetId() == name:
					return c.open(session)
				case found == nil && strings.HasPrefix(strings.ToLower(title), strings.ToLower(name)):
					found = session
				}
			}
			if found == nil {
				c.note = "no session named " + name
				return nil
			}
			return c.open(found)
		}
	})
}

func (c *chat) skin(name string) tea.Cmd {
	if _, ok := theme.Named(name); !ok {
		c.note = "skins: " + strings.Join(theme.Names(), ", ")
		return nil
	}
	return tui.Theme(name)
}

func (c *chat) browse(filter string) tea.Cmd {
	catalog := c.catalog()
	var hub *cards.Hub
	list := cards.NewList(c.kit.Env, cards.ListSpec{
		Title:   "Commands",
		Key:     "commands",
		Context: cards.HubContext,
		Primary: "hub.use",
		Columns: []render.Column{{Title: "Command"}, {Title: "What it does", Flex: 1}},
		Rows: func() [][]string {
			rows := make([][]string, len(catalog))
			for i, command := range catalog {
				rows[i] = []string{strings.TrimSpace("/" + command.GetName() + " " + command.GetUsage()), command.GetHelp()}
			}
			return rows
		},
		Hints: func(*cards.List, int) []wm.Hint {
			return []wm.Hint{{Action: "hub.use", Label: "type it"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				return tea.Batch(c.kit.WM.DismissCard(hub), c.composer.SetValue("/"+catalog[i].GetName()+" "))
			},
		},
	})
	hub = c.kit.Hub("commands", list)
	list.Filter(filter)
	return tea.Batch(c.kit.WM.ShowOverlay(hub), list.Init())
}

func (c *chat) copy(number string) tea.Cmd {
	back := 1
	if number != "" {
		var err error
		if back, err = strconv.Atoi(number); err != nil || back < 1 {
			c.note = "usage: /copy [number]"
			return nil
		}
	}
	replies := c.log.replies()
	if back > len(replies) {
		c.note = "nothing to copy"
		return nil
	}
	c.note = "copied"
	return clip.Copy(replies[len(replies)-back])
}

func (c *chat) quit(argument string) tea.Cmd {
	id := c.id
	if argument != "--delete" || id == "" {
		return tea.Quit
	}
	return tui.Async(func() func() tea.Cmd {
		_, err := c.sessions.Delete(c.kit.Env.Ctx, &sessionsv1.DeleteRequest{Id: id})
		return func() tea.Cmd {
			if err != nil {
				c.note = "error: " + err.Error()
				return nil
			}
			return tea.Quit
		}
	})
}
