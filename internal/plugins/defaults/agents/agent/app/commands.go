package app

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	slashv1 "github.com/nickheyer/icebath/pkg/gen/icebath/slash/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/clip"
)

const recap = 400

type command struct {
	name    string
	aliases []string
	usage   string
	help    string
	run     func(c *chat, argument string) (tea.Cmd, bool)
}

func locals() []command {
	return []command{
		{name: "new", aliases: []string{"clear", "reset"}, usage: "[title]", help: "Start a new session in this window", run: (*chat).fresh},
		{name: "resume", usage: "[title]", help: "Open a past session", run: (*chat).resume},
		{name: "model", usage: "[model|reset] [--provider name] [--reasoning level] [--global|--session]", help: "Pick a model, or switch by name", run: (*chat).model},
		{name: "reasoning", usage: "[level|reset|show|hide|full] [--global]", help: "Set the reasoning effort, or how reasoning is shown", run: (*chat).reasoning},
		{name: "history", usage: "[chars]", help: "Show the session history, each message shortened", run: (*chat).history},
		{name: "copy", usage: "[n]", help: "Copy the last reply, or the nth from last", run: (*chat).copy},
		{name: "help", aliases: []string{"commands"}, usage: "[filter]", help: "List commands", run: (*chat).help},
		{name: "quit", aliases: []string{"exit"}, usage: "[--delete]", help: "Quit; --delete also deletes this session", run: (*chat).quit},
	}
}

func (c *chat) local(text string) (tea.Cmd, bool) {
	word, argument, _ := strings.Cut(strings.TrimSpace(text), " ")
	name, ok := strings.CutPrefix(word, "/")
	if !ok {
		return nil, false
	}
	for _, command := range locals() {
		if command.name == name || slices.Contains(command.aliases, name) {
			return command.run(c, strings.TrimSpace(argument))
		}
	}
	return nil, false
}

func (c *chat) catalog() []*slashv1.Command {
	var catalog []*slashv1.Command
	add := func(command *slashv1.Command) {
		if !slices.ContainsFunc(catalog, func(have *slashv1.Command) bool { return have.GetName() == command.GetName() }) {
			catalog = append(catalog, command)
		}
	}
	for _, command := range locals() {
		add(&slashv1.Command{Name: command.name, Aliases: command.aliases, Usage: command.usage, Help: command.help})
	}
	for _, screen := range c.kit.Screens() {
		if len(screen.Slash) > 0 {
			add(&slashv1.Command{Name: screen.Slash[0], Aliases: screen.Slash[1:], Usage: screen.Usage, Help: screen.Short})
		}
	}
	for _, command := range c.commands {
		add(command)
	}
	slices.SortFunc(catalog, func(a, b *slashv1.Command) int { return strings.Compare(a.GetName(), b.GetName()) })
	return catalog
}

func (c *chat) command(text string) (*slashv1.Command, bool) {
	word, _, _ := strings.Cut(strings.TrimSpace(text), " ")
	name, ok := strings.CutPrefix(word, "/")
	if !ok {
		return nil, false
	}
	for _, command := range c.catalog() {
		if command.GetName() == name || slices.Contains(command.GetAliases(), name) {
			return command, true
		}
	}
	return nil, false
}

func (c *chat) say(command, text string) {
	c.log.note("`" + command + "`\n\n" + text)
}

func (c *chat) fresh(name string) (tea.Cmd, bool) {
	c.reset()
	c.named = name
	c.say(strings.TrimSpace("/new "+name), "New session.")
	return nil, true
}

func (c *chat) resume(name string) (tea.Cmd, bool) {
	if name == "" {
		return c.kit.Open(sessionsScreen, ""), true
	}
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.sessions.List(c.kit.Env.Ctx, &sessionsv1.ListRequest{})
		return func() tea.Cmd {
			if err != nil {
				c.say("/resume "+name, tui.Reason(err))
				return nil
			}
			var found *sessionsv1.Session
			for _, session := range response.GetSessions() {
				switch title := session.GetTitle(); {
				case strings.EqualFold(title, name), session.GetId() == name:
					return c.desk.open(session)
				case found == nil && strings.HasPrefix(strings.ToLower(title), strings.ToLower(name)):
					found = session
				}
			}
			if found == nil {
				c.say("/resume "+name, "No session named "+name+".")
				return nil
			}
			return c.desk.open(found)
		}
	}), true
}

func (c *chat) model(argument string) (tea.Cmd, bool) {
	if argument != "" {
		return nil, false
	}
	return c.pick(), true
}

func (c *chat) reasoning(argument string) (tea.Cmd, bool) {
	words := strings.Fields(strings.ToLower(argument))
	global := slices.Contains(words, "--global")
	if words = slices.DeleteFunc(words, func(word string) bool { return word == "--global" }); len(words) != 1 {
		return nil, false
	}
	thinking, told := "", ""
	switch words[0] {
	case "hide":
		thinking, told = agentconfig.ThinkingHidden, "Reasoning hidden."
	case "full":
		thinking, told = agentconfig.ThinkingOpen, "Reasoning expanded."
	case "show":
		thinking, told = agentconfig.ThinkingFolded, "Reasoning collapsed. Click it to expand."
		if c.thinking == agentconfig.ThinkingOpen {
			thinking, told = agentconfig.ThinkingOpen, "Reasoning expanded."
		}
	default:
		return nil, false
	}
	if !global {
		c.thinking = thinking
		c.refold(":thinking")
		c.say("/reasoning "+argument, told)
		return nil, true
	}
	value := strconv.Quote(thinking)
	return tui.Async(func() func() tea.Cmd {
		_, err := c.desk.settings.Set(c.kit.Env.Ctx, &configv1.SetRequest{Key: agentconfig.Thinking, Value: value})
		return func() tea.Cmd {
			if err != nil {
				c.say("/reasoning "+argument, tui.Reason(err))
				return nil
			}
			c.say("/reasoning "+argument, told)
			return c.kit.Changed(agentconfig.Thinking, value)
		}
	}), true
}

func (c *chat) history(argument string) (tea.Cmd, bool) {
	limit, err := strconv.Atoi(argument)
	if err != nil || limit < 80 {
		limit = recap
	}
	var lines []string
	turn := 0
	for _, e := range c.log.entries {
		who := ""
		switch e.message.GetRole() {
		case sessionsv1.Role_ROLE_USER:
			who = "You"
		case sessionsv1.Role_ROLE_ASSISTANT:
			who = "icebath"
		default:
			continue
		}
		turn++
		body := strings.TrimSpace(e.message.GetText())
		switch calls := len(e.message.GetToolCalls()); {
		case body != "":
		case calls == 1:
			body = "(1 tool call)"
		case calls > 1:
			body = fmt.Sprintf("(%d tool calls)", calls)
		default:
			body = "(empty)"
		}
		if utf8.RuneCountInString(body) > limit {
			body = strings.TrimSpace(string([]rune(body)[:limit])) + "…"
		}
		lines = append(lines, fmt.Sprintf("**%s #%d**", who, turn), "", body, "")
	}
	if len(lines) == 0 {
		c.say("/history", "No messages yet.")
		return nil, true
	}
	text, width := strings.Join(lines, "\n"), 0
	var wrapped []string
	page := cards.NewPage(c.kit.Env, cards.PageSpec{
		Title: "History",
		Key:   c.key + ":history",
		Lines: func(w int) []string {
			if w != width {
				width, wrapped = w, strings.Split(strings.Trim(c.markdown.Render(text, w, c.kit.Env.Theme), "\n"), "\n")
			}
			return wrapped
		},
	})
	return c.kit.WM.ShowOverlay(c.kit.Hub(c.key+":history", page)), true
}

func (c *chat) copy(number string) (tea.Cmd, bool) {
	back := 1
	if number != "" {
		var err error
		if back, err = strconv.Atoi(number); err != nil || back < 1 {
			c.say("/copy "+number, "Usage: /copy [n], where 1 is the last reply.")
			return nil, true
		}
	}
	replies := c.log.replies()
	if back > len(replies) {
		c.say(strings.TrimSpace("/copy "+number), "No reply to copy.")
		return nil, true
	}
	c.say(strings.TrimSpace("/copy "+number), "Copied.")
	return clip.Copy(replies[len(replies)-back]), true
}

func (c *chat) help(filter string) (tea.Cmd, bool) {
	var out strings.Builder
	out.WriteString("| Command | Description |\n|---|---|\n")
	found := false
	for _, command := range c.catalog() {
		line := strings.TrimSpace("/" + command.GetName() + " " + command.GetUsage())
		if !strings.Contains(strings.ToLower(line+" "+command.GetHelp()), strings.ToLower(filter)) {
			continue
		}
		found = true
		fmt.Fprintf(&out, "| `%s` | %s |\n", strings.ReplaceAll(line, "|", "\\|"), command.GetHelp())
	}
	if !found {
		c.say("/help "+filter, "No commands match "+filter+".")
		return nil, true
	}
	c.say(strings.TrimSpace("/help "+filter), out.String())
	return nil, true
}

func (c *chat) quit(argument string) (tea.Cmd, bool) {
	id := c.id
	if argument != "--delete" || id == "" {
		return tea.Quit, true
	}
	return tui.Async(func() func() tea.Cmd {
		_, err := c.desk.sessions.Delete(c.kit.Env.Ctx, &sessionsv1.DeleteRequest{Id: id})
		return func() tea.Cmd {
			if err != nil {
				c.say("/quit --delete", tui.Reason(err))
				return nil
			}
			return tea.Quit
		}
	}), true
}
