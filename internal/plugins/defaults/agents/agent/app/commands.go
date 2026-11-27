package app

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
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
		{name: "resume", usage: "[title|id]", help: "Open a past session here, by its title or id", run: (*chat).resume},
		{name: "branch", aliases: []string{"fork"}, usage: "[--here] [title]", help: "Branch the current session; this window goes on in the branch", run: (*chat).branch},
		{name: "save", usage: "<json|md|html> [filename] [redact]", help: "Export the current conversation (bare /save shows usage)", run: (*chat).saveTo},
		{name: "rewind", usage: "[N | fork N]", help: "Go back to an earlier prompt to edit it, or fork a new session from before it", run: (*chat).rewind},
		{name: "model", usage: "[model|reset] [--provider name] [--reasoning level] [--global|--session]", help: "Pick a model, or switch by name", run: (*chat).model},
		{name: "reasoning", usage: "[level|reset|show|hide|full] [--global]", help: "Set the reasoning effort, or how reasoning is shown", run: (*chat).reasoning},
		{name: "history", usage: "[chars]", help: "Show the session history, each message shortened", run: (*chat).history},
		{name: "copy", usage: "[n]", help: "Copy the last reply, or the nth from last", run: (*chat).copy},
		{name: "help", aliases: []string{"commands"}, usage: "[filter]", help: "List commands", run: (*chat).help},
		{name: "quit", aliases: []string{"exit"}, usage: "[--delete]", help: "Quit; --delete also deletes this session", run: (*chat).quit},
		{name: "agents", aliases: []string{"tasks"}, usage: "[pause|resume|status]",
			help: "Open the spawn-tree dashboard (live audit + kill/pause controls); pause, resume and status go to the agent", run: (*chat).agents},
		{name: "replay", usage: "[N|last|list|load <path>]", help: "Replay a completed spawn tree", run: (*chat).replayTree},
		{name: "replay-diff", usage: "<baseline> <candidate>", help: "Diff two completed spawn trees (indexes from /replay)", run: (*chat).replayDiff},
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

// Tells the session's pre_command hooks of a slash command the app runs, as it is known and as it was typed; nothing for a window with no
// session yet
func (c *chat) observe(id, text string) {
	known, ok := c.command(text)
	if id == "" || !ok {
		return
	}
	word, argument, _ := strings.Cut(strings.TrimSpace(text), " ")
	request := &agentv1.ObserveRequest{SessionId: id, Command: known.GetName(), Alias: strings.TrimPrefix(word, "/"), Args: strings.TrimSpace(argument)}
	ctx := c.kit.Env.Ctx
	go c.desk.agents.Observe(ctx, request)
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
	running := c.log.busy
	left := cmp.Or(c.id, c.left)
	c.reset()
	c.left = left
	c.named = name
	if running {
		c.say(strings.TrimSpace("/new "+name), "New session started · the previous one keeps running; /sessions lists it.")
		return nil, true
	}
	c.say(strings.TrimSpace("/new "+name), "New session.")
	return nil, true
}

func (c *chat) resume(name string) (tea.Cmd, bool) {
	if name == "" {
		return c.kit.Open(sessionsScreen, ""), true
	}
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.sessions.Resolve(c.kit.Env.Ctx, &sessionsv1.ResolveRequest{Ref: name, Profile: c.on(), Tip: true})
		return func() tea.Cmd {
			if err != nil {
				c.say("/resume "+name, "No session found matching '"+name+"'. /sessions lists them.")
				return nil
			}
			if response.GetSession().GetId() == c.id {
				c.say("/resume "+name, "Already on session "+cmp.Or(response.GetSession().GetTitle(), name)+".")
				return nil
			}
			return c.switchTo(response.GetSession())
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
	return c.confirm("quit --delete", "The session and its messages are deleted for good, then icebath quits.", func() tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			_, err := c.desk.sessions.Delete(c.kit.Env.Ctx, &sessionsv1.DeleteRequest{Id: id})
			return func() tea.Cmd {
				if err != nil {
					c.say("/quit --delete", "The session was not deleted, so icebath stays open: "+tui.Reason(err))
					return nil
				}
				return tea.Quit
			}
		})
	}), true
}
