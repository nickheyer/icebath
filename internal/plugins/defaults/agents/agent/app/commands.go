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
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Characters /history shows of each message unless told otherwise
const recap = 400

type command struct {
	name    string
	aliases []string
	usage   string
	help    string
	run     func(c *chat, argument string) (tea.Cmd, bool)
}

// Usage lines of the commands whose mistakes the chat answers with them, the same words the slash list shows
const (
	rewindUsage     = "[n | fork n]"
	copyUsage       = "[n]"
	replayUsage     = "[n|last|list|load <path>]"
	replayDiffUsage = "<baseline> <candidate>"
	saveUsageLine   = "<json|md|html> [file] [redact]"
)

func locals() []command {
	return []command{
		{name: "new", aliases: []string{"clear", "reset"}, usage: "[title]", help: "Start a new session in this chat", run: (*chat).fresh},
		{name: "resume", usage: "[title|id]", help: "Open a past session in this chat", run: (*chat).resume},
		{name: "branch", aliases: []string{"fork"}, usage: "[--here] [title]", help: "Branch the session and go on in the branch", run: (*chat).branch},
		{name: "save", usage: saveUsageLine, help: "Save the session to a file", run: (*chat).saveTo},
		{name: "rewind", usage: rewindUsage, help: "Go back to an earlier message to change it", run: (*chat).rewind},
		{name: "model", usage: "[model|reset] [--provider name] [--reasoning level] [--login name] [--global|--session] [--refresh]",
			help: "Show or switch the model", run: (*chat).model},
		{name: "reasoning", usage: "[level|reset|show|hide|full] [--global]", help: "Set the reasoning effort or how reasoning shows", run: (*chat).reasoning},
		{name: "history", usage: "[characters]", help: "Show the messages of this session, shortened", run: (*chat).history},
		{name: "copy", usage: copyUsage, help: "Copy the last reply, or the one n back", run: (*chat).copy},
		{name: "help", aliases: []string{"commands"}, usage: "[skills|words]", help: "List the commands, or only the matching ones", run: (*chat).help},
		{name: "quit", aliases: []string{"exit"}, usage: "[--delete]", help: "Quit icebath, and with --delete delete this session", run: (*chat).quit},
		{name: "agents", aliases: []string{"tasks"}, usage: "[pause|resume|status]", help: "Show running subagents, or pause new ones", run: (*chat).agents},
		{name: "replay", usage: replayUsage, help: "Replay a finished spawn tree", run: (*chat).replayTree},
		{name: "replay-diff", usage: replayDiffUsage, help: "Compare two finished spawn trees", run: (*chat).replayDiff},
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
	// icebath's own commands first, then the ones that load a skill, each by name
	slices.SortFunc(catalog, func(a, b *slashv1.Command) int {
		if a.GetSkill() != b.GetSkill() {
			if b.GetSkill() {
				return -1
			}
			return 1
		}
		return strings.Compare(a.GetName(), b.GetName())
	})
	return catalog
}

// Heading a command is listed under where commands and skills are listed together
func heading(command *slashv1.Command) string {
	if command.GetSkill() {
		return "Skills"
	}
	return "Commands"
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
		c.say(strings.TrimSpace("/new "+name), "Started a new session. The one before keeps working, and /sessions lists it.")
		return nil, true
	}
	c.say(strings.TrimSpace("/new "+name), "Started a new session.")
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
				c.say("/resume "+name, "No session matches “"+name+"”. /sessions lists them all.")
				return nil
			}
			if response.GetSession().GetId() == c.id {
				c.say("/resume "+name, "This chat already shows "+cmp.Or(response.GetSession().GetTitle(), name)+".")
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
	// Words of the Reasoning display setting and of its choices, as the settings screen names them
	thinking, shown := "", ""
	switch words[0] {
	case "hide":
		thinking, shown = agentconfig.ThinkingHidden, "Hidden"
	case "full":
		thinking, shown = agentconfig.ThinkingOpen, "Expanded"
	case "show":
		thinking, shown = agentconfig.ThinkingFolded, "Collapsed"
		if c.thinking == agentconfig.ThinkingOpen {
			thinking, shown = agentconfig.ThinkingOpen, "Expanded"
		}
	default:
		return nil, false
	}
	echo := "/reasoning " + argument
	if !global {
		c.thinking = thinking
		c.refold(":thinking")
		told := "Reasoning display is now " + shown + " in this chat."
		if thinking == agentconfig.ThinkingFolded {
			told += " Click a Reasoning line to open it."
		}
		c.say(echo, told)
		return nil, true
	}
	value := strconv.Quote(thinking)
	return tui.Async(func() func() tea.Cmd {
		_, err := c.desk.settings.Set(c.kit.Env.Ctx, &configv1.SetRequest{Key: agentconfig.Thinking, Value: value})
		return func() tea.Cmd {
			if err != nil {
				c.say(echo, "Reasoning display was not changed. "+tui.Reason(err))
				return nil
			}
			c.say(echo, "Reasoning display is now "+shown+" in your settings.")
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
			who = "Agent"
		default:
			continue
		}
		turn++
		body := strings.TrimSpace(e.message.GetText())
		switch calls := len(e.message.GetToolCalls()); {
		case body != "":
		case calls > 0:
			body = "*Used " + render.Count(calls, "tool") + ".*"
		default:
			body = "*Nothing was said.*"
		}
		if utf8.RuneCountInString(body) > limit {
			body = strings.TrimSpace(string([]rune(body)[:limit])) + c.kit.Env.Theme.Glyphs.Ellipsis
		}
		lines = append(lines, fmt.Sprintf("**%s · %d**", who, turn), "", body, "")
	}
	if len(lines) == 0 {
		c.say("/history", "This session has no messages yet.")
		return nil, true
	}
	head := fmt.Sprintf("*Each message is cut after %d characters. /history %d shows more of each.*", limit, max(limit*4, 2000))
	text, width := strings.Join(append([]string{head, ""}, lines...), "\n"), 0
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
	echo := strings.TrimSpace("/copy " + number)
	if number != "" {
		var err error
		if back, err = strconv.Atoi(number); err != nil || back < 1 {
			c.say(echo, "Usage: /copy "+copyUsage+". 1 is the last reply, 2 the one before it.")
			return nil, true
		}
	}
	replies := c.log.replies()
	switch {
	case len(replies) == 0:
		c.say(echo, "There is no reply to copy yet.")
		return nil, true
	case back > len(replies):
		c.say(echo, "This session has only "+render.Count(len(replies), "reply")+". Pick a number from 1 to "+strconv.Itoa(len(replies))+".")
		return nil, true
	case back == 1:
		c.say(echo, "Copied the last reply.")
	default:
		c.say(echo, "Copied reply "+strconv.Itoa(back)+" from the end.")
	}
	return clip.Copy(replies[len(replies)-back]), true
}

// Lists every command with its usage and what it does, icebath's own in one table and the ones that load a skill in another, or those
// whose words hold every word typed; skills alone for the word skills
func (c *chat) help(filter string) (tea.Cmd, bool) {
	only := strings.EqualFold(filter, "skills")
	words := strings.Fields(strings.ToLower(filter))
	if only {
		words = nil
	}
	tables := map[bool]*strings.Builder{false: {}, true: {}}
	for _, command := range c.catalog() {
		line := strings.TrimSpace("/" + command.GetName() + " " + command.GetUsage())
		said := strings.ToLower(line + " " + strings.Join(command.GetAliases(), " ") + " " + command.GetHelp())
		if only && !command.GetSkill() || slices.ContainsFunc(words, func(word string) bool { return !strings.Contains(said, word) }) {
			continue
		}
		fmt.Fprintf(tables[command.GetSkill()], "| `%s` | %s |\n", strings.ReplaceAll(line, "|", "\\|"), command.GetHelp())
	}
	echo := strings.TrimSpace("/help " + filter)
	var out strings.Builder
	for _, skill := range []bool{false, true} {
		if tables[skill].Len() == 0 {
			continue
		}
		head := "| Command | What it does |"
		if skill {
			head = "| Skill | What it is for |"
		}
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(head + "\n|---|---|\n" + tables[skill].String())
	}
	switch {
	case out.Len() > 0:
		c.say(echo, out.String())
	case only:
		c.say(echo, "No skills are installed. /skills installs one.")
	default:
		c.say(echo, "No command matches “"+filter+"”. /help lists them all.")
	}
	return nil, true
}

func (c *chat) quit(argument string) (tea.Cmd, bool) {
	id := c.id
	if argument != "--delete" || id == "" {
		return tea.Quit, true
	}
	return c.confirm("quit --delete", "This deletes the session and all its messages for good, then quits icebath.", func() tea.Cmd {
		return tui.Async(func() func() tea.Cmd {
			_, err := c.desk.sessions.Delete(c.kit.Env.Ctx, &sessionsv1.DeleteRequest{Id: id})
			return func() tea.Cmd {
				if err != nil {
					c.say("/quit --delete", "The session was not deleted, so icebath stays open. "+tui.Reason(err))
					return nil
				}
				return tea.Quit
			}
		})
	}), true
}
