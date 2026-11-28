package app

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect/v2"
	"go.yaml.in/yaml/v3"

	"github.com/nickheyer/icebath/internal/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/tools"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	providerv1 "github.com/nickheyer/icebath/pkg/gen/icebath/provider/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	slashv1 "github.com/nickheyer/icebath/pkg/gen/icebath/slash/v1"
	toolsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/tools/v1"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

const (
	reconnect   = 3 * time.Second
	completions = 50
)

// What an empty chat says while no provider of its profile is ready: why the agent can't answer and the command that fixes it. Models that
// run on this computer are connected there too
const noProvider = "No provider is ready yet, so the agent can't answer. Type /providers to connect one."

type chat struct {
	desk     *desk
	kit      *tui.Kit
	composer *cards.Composer
	markdown *render.Markdown
	// Lines the person ran in the session after a mark, such as !, the one running last
	runs    []*lineRun
	sheet   *approval
	inquiry *inquiry
	// Answer to one of the agent's questions being typed in the prompt
	typing   *answering
	commands []*slashv1.Command
	// Tools of the server by name, for the title, glyph and subject of each call
	tools map[string]*toolsv1.Tool
	key   string
	id    string
	// Profile the session runs on, the one in use when the chat was opened until the session is made
	profile string
	title   string
	named   string
	// Session the window left with /new, named with the first message of the next so the session hooks hear of it
	left     string
	starting bool
	entering bool
	replying bool
	// Approval prompt the words typed in the prompt deny, and what became of the last one answered while the reply runs
	replyTo string
	outcome string
	unseen  bool
	// Why the agent can't answer in this chat yet, by the one test of whether a provider is ready; empty while it can
	stranded string
	// Messages sent while the session is being made, each with its voice flags, sent in order once it is
	waiting []*agentv1.SendRequest
	epoch   int
	stop    context.CancelFunc
	// Prompt picked to rewind to, which the next line sent replaces, and when esc was last pressed on the empty idle prompt
	editing *rewinding
	primed  time.Time
	// Set while an edit rewinds the session, whose taken back text is not put back in the prompt
	quiet bool

	log      transcript
	folds    map[string]bool
	expanded bool
	thinking string
	rendered map[string][]string
	kept     map[string][]string
	width    int
	lines    []row
	rows     int
	shown    int
	top      int
	follow   bool
	prompt   int
	// What the docks show of the session and whether they are folded to a line, whether a reply runs as the docks are read, and the
	// nudge that reads them at once
	docked  []toolsapp.Docked
	folded  bool
	working atomic.Bool
	poke    chan struct{}
	// Subagents the session's turn runs and the spawn trees that finished, which /agents and /replay show
	spawn spawnState
	// Spawn tree screen while it is open, repainted as events come
	tree *spawnScreen
	// How delegation stands for the session, its caps and its pause, which the status line measures the fan-out against; read as a fan-out
	// starts
	limits   *agentv1.SubagentsResponse
	limiting bool
	// The person spoke the text being submitted, which goes on the voice chat route when it is sent as a message
	voiced bool
}

var _ interface {
	wm.Card
	wm.Keyed
	wm.Glypher
	wm.Statuser
	wm.Cursorer
	wm.Contexter
	wm.Selectable
	wm.Sized
	wm.Kinded
	wm.Answerer
	wm.Grabber
	wm.SheetHolder
} = (*chat)(nil)

func newChat(desk *desk, key string, session *sessionsv1.Session, profile string) *chat {
	kit := desk.kit
	c := &chat{
		desk:     desk,
		kit:      kit,
		composer: cards.NewComposer(kit.Env.Theme, kit.Env.Keys, "", kit.Env.WorkDir),
		markdown: render.NewMarkdown(),
		key:      key,
		id:       session.GetId(),
		profile:  cmp.Or(session.GetProfile(), profile),
		title:    session.GetTitle(),
		folds:    map[string]bool{},
		follow:   true,
		poke:     make(chan struct{}, 1),
	}
	c.composer.SessionID = key
	c.composer.Literal = true
	c.composer.History = kit.History
	c.composer.Hint = c.hint()
	c.composer.OnStop = c.interrupt
	c.composer.Apply = c.take
	return c
}

// Text the empty prompt shows: what typing does, and what a line after the mark of a liner runs in, such as the shell after !
func (c *chat) hint() string {
	hint := "Message the agent · / lists commands"
	for _, liner := range c.desk.liners {
		if liner.Hint != nil {
			if said := liner.Hint(c.kit.Env); said != "" {
				hint += " · " + said
			}
		}
	}
	return hint
}

func (c *chat) Title() string { return cmp.Or(c.title, "New chat") }

// Profile the chat acts on: the one its session runs on now, empty for the one in use before it has a session
func (c *chat) on() string          { return cmp.Or(c.log.state.GetProfile(), c.profile) }
func (c *chat) Key() string         { return c.key }
func (c *chat) Kind() string        { return "Chat" }
func (c *chat) Session() string     { return c.id }
func (c *chat) Glyph() string       { return c.kit.Env.Theme.Glyphs.Session }
func (c *chat) Selectable() bool    { return true }
func (c *chat) MinSize() (int, int) { return 40, 10 }

// Action that compresses the session's conversation now, the /compress of the key map
var compactKeys = []keys.Action{
	{ID: "session.compact", Context: keys.Session, Label: "summarize", Doc: "Summarize older messages to make room, as /compress does"},
}

func (c *chat) Answers(action string) bool {
	return action != "composer.attachments.clear"
}

func (c *chat) Grabs(_, y int) bool {
	return y >= 0 && y < c.shown && c.top+y < len(c.lines) && c.lines[c.top+y].fold != ""
}

func (c *chat) Init() tea.Cmd {
	on := c.on()
	load := tui.Async(func() func() tea.Cmd {
		ctx := c.kit.Env.Ctx
		response, err := c.desk.slash.List(ctx, &slashv1.ListRequest{})
		listed, listErr := c.desk.tools.List(ctx, &toolsv1.ListRequest{})
		var thinking string
		if setting, err := c.desk.settings.Get(ctx, &configv1.GetRequest{Key: agentconfig.Thinking, Profile: on}); err == nil {
			yaml.Unmarshal([]byte(setting.GetSetting().GetValue()), &thinking)
		}
		return func() tea.Cmd {
			if err == nil {
				c.commands = response.GetCommands()
			}
			if listErr == nil {
				c.tools = map[string]*toolsv1.Tool{}
				for _, tool := range listed.GetTools() {
					c.tools[tool.GetName()] = tool
				}
			}
			if c.thinking == "" {
				c.thinking = thinking
			}
			return nil
		}
	})
	if c.id == "" {
		return tea.Batch(load, c.check())
	}
	if c.title == "" {
		return tea.Batch(load, c.watch(), c.retitle())
	}
	return tea.Batch(load, c.watch())
}

// Reads whether the agent can answer in this empty chat: the provider new chats use is ready, or for none any provider is, or the model is a
// shortcut to a server of its own
func (c *chat) check() tea.Cmd {
	on := c.on()
	return tui.Async(func() func() tea.Cmd {
		ctx := c.kit.Env.Ctx
		response, err := c.desk.providers.List(ctx, &providerv1.ListRequest{Profile: on})
		var chosen, model string
		var shortcuts map[string]struct {
			BaseURL string `yaml:"base_url"`
		}
		get := func(key string, into any) {
			if setting, err := c.desk.settings.Get(ctx, &configv1.GetRequest{Key: key, Profile: on}); err == nil {
				yaml.Unmarshal([]byte(setting.GetSetting().GetValue()), into)
			}
		}
		get(agentconfig.Provider, &chosen)
		get(agentconfig.Model, &model)
		get(agentconfig.Aliases, &shortcuts)
		why := ""
		if err == nil {
			why = strandedWhy(response.GetProviders(), chosen)
		}
		// A model shortcut naming a server of its own runs with no provider ready
		for name, shortcut := range shortcuts {
			if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(model)) && strings.TrimSpace(shortcut.BaseURL) != "" {
				why = ""
			}
		}
		return func() tea.Cmd {
			c.stranded = why
			return nil
		}
	})
}

// Why the agent can't answer on the providers of a profile, by the one test of whether a provider is ready: the provider new chats use,
// else any; empty when it can
func strandedWhy(providers []*providerv1.Provider, chosen string) string {
	if chosen == "" {
		if slices.ContainsFunc(providers, providerapp.Ready) {
			return ""
		}
		return noProvider
	}
	for _, entry := range providers {
		if entry.GetName() != chosen {
			continue
		}
		if providerapp.Ready(entry) {
			return ""
		}
		// The provider words why it is not ready whole, naming itself
		return strs.Filled(entry.GetWhy(), entry.GetDisplayName()+", the provider of new chats, isn't ready, so the agent can't answer.")
	}
	return "The provider of new chats is gone from this profile, so the agent can't answer. Type /model to pick another model."
}

func (c *chat) Status() string {
	parts := []string{c.status(), spawnHUD(c.spawn.live(), c.limits), c.desk.voice.status(c)}
	return strings.Join(slices.DeleteFunc(parts, func(part string) bool { return part == "" }), " · ")
}

// What enter does with a message typed while the agent works, by the value of Enter while busy
var whileBusy = map[string]string{
	agentconfig.BusyQueue:     "enter sends it after this reply",
	agentconfig.BusySteer:     "enter adds it to this reply",
	agentconfig.BusyInterrupt: "enter stops the reply and sends it",
}

// Words of each reasoning effort, as the settings screen names them
var effortWords = func() map[string]string {
	words := map[string]string{}
	for _, option := range provider.EffortOptions(config.Option{}) {
		if option.Value != "" {
			words[option.Value] = option.Title
		}
	}
	return words
}()

// Reasoning effort in words, the value itself for one icebath has no words for
func effortTitle(effort string) string { return cmp.Or(effortWords[effort], effort) }

// What the chat's window says it is doing: waiting on you, working and what enter does meanwhile, else the profile, provider and model,
// effort, approvals skipped, how full the context is, what the session cost and how often it was summarized
func (c *chat) status() string {
	switch {
	case len(c.log.approvals) > 0:
		return "waiting for your approval"
	case c.log.questions != nil:
		return "waiting for your answers"
	case c.log.activity != "":
		return c.log.activity
	case c.log.busy && c.outcome != "":
		return "working · " + c.outcome
	case c.log.busy && !c.composer.Empty() && whileBusy[c.log.state.GetBusyInputMode()] != "":
		return "working · " + whileBusy[c.log.state.GetBusyInputMode()]
	case c.log.busy:
		return "working"
	}
	if status := c.composer.Status(); status != "" {
		return status
	}
	state := c.log.state
	parts := []string{}
	if profile := c.on(); profile != "" {
		parts = append(parts, "@"+profile)
	}
	// The model and the provider that runs it, as a row of Settings names them
	switch provider := cmp.Or(state.GetProviderTitle(), state.GetProvider()); {
	case state.GetModel() != "" && provider != "":
		parts = append(parts, state.GetModel()+" · "+provider)
	case state.GetModel() != "":
		parts = append(parts, state.GetModel())
	case provider != "":
		parts = append(parts, provider)
	}
	switch effort := state.GetReasoningEffort(); effort {
	case "":
	case "none":
		parts = append(parts, strings.ToLower(effortTitle(effort)))
	default:
		parts = append(parts, strings.ToLower(effortTitle(effort))+" effort")
	}
	if state.GetYolo() {
		parts = append(parts, "approvals skipped")
	}
	if dial := contextDial(state); dial != "" {
		parts = append(parts, dial)
	}
	if cost := state.GetCost(); cost > 0 {
		parts = append(parts, money(cost))
	}
	if n := state.GetCompressions(); n > 0 {
		parts = append(parts, "summarized "+times(int(n)))
	}
	return strings.Join(parts, " · ")
}

// How many times something happened, in words for once and twice
func times(n int) string {
	switch n {
	case 1:
		return "once"
	case 2:
		return "twice"
	}
	return strconv.Itoa(n) + " times"
}

// Dollars with cents, or with four decimals below a cent
func money(dollars float64) string {
	if dollars < 0.01 {
		return fmt.Sprintf("$%.4f", dollars)
	}
	return fmt.Sprintf("$%.2f", dollars)
}

// How full the session's context is: the share, then the size led by ~ when part of it is estimated, of the window; empty while either is
// not known
func contextDial(state *agentv1.State) string {
	used, limit := state.GetContextUsed(), state.GetContextMax()
	if used <= 0 || limit <= 0 {
		return ""
	}
	mark := ""
	if state.GetContextEstimated() {
		mark = "~"
	}
	return fmt.Sprintf("%d%% of context, %s%s of %s", int(float64(used)/float64(limit)*100+0.5), mark, compactNumber(float64(used)), compactNumber(float64(limit)))
}

// Number in a few characters: thousands as k and millions as M with a decimal when it says something
func compactNumber(value float64) string {
	scaled := func(v float64, suffix string) string {
		return strings.TrimSuffix(strconv.FormatFloat(math.Round(v*10)/10, 'f', 1, 64), ".0") + suffix
	}
	switch {
	case value >= 999_950:
		return scaled(value/1_000_000, "M")
	case value >= 999.5:
		return scaled(value/1_000, "k")
	}
	return strconv.FormatFloat(math.Floor(value+0.5), 'f', -1, 64)
}

func (c *chat) Contexts() []string {
	if c.composer.Completing() {
		return []string{keys.Complete, keys.Session, keys.Text}
	}
	return []string{keys.Session, keys.Text}
}

func (c *chat) Cursor() *tea.Cursor {
	cursor := c.composer.Cursor()
	if cursor != nil {
		cursor.Y += c.prompt
	}
	return cursor
}

func (c *chat) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case wm.ClosedMsg:
		c.epoch++
		if c.stop != nil {
			c.stop()
		}
		return c, c.desk.closed(c)
	case wm.FocusMsg:
		if msg.Focused {
			c.unseen = false
		}
		if msg.Focused && c.stranded != "" {
			return c, tea.Batch(c.composer.SetFocus(true), c.check(), c.desk.voice.focused(c))
		}
		if msg.Focused {
			return c, tea.Batch(c.composer.SetFocus(true), c.desk.voice.focused(c))
		}
		return c, c.composer.SetFocus(false)
	case wm.ActionMsg:
		return c, c.action(msg.ID)
	case tea.KeyPressMsg, tea.PasteMsg:
		cmd := c.composer.Update(msg)
		c.complete()
		return c, cmd
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			c.scroll(-3)
		case tea.MouseWheelDown:
			c.scroll(3)
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && c.Grabs(msg.X, msg.Y) {
			id := c.lines[c.top+msg.Y].fold
			c.folds[id] = !c.open(id, c.unfolded(id))
			c.follow = false
		}
	case tui.ThemeMsg:
		c.composer.SetTheme(c.kit.Env.Theme)
		c.rendered = nil
	case cards.SettingChangedMsg:
		switch msg.Key {
		case cards.TerminalShell:
			if !c.replying {
				c.composer.Hint = c.hint()
			}
		case agentconfig.Thinking:
			var thinking string
			if yaml.Unmarshal([]byte(msg.Value), &thinking) == nil && thinking != "" {
				c.thinking = thinking
				c.refold(":thinking")
			}
		}
	case cards.SubmitMsg:
		if msg.Session == c.key {
			return c, c.submit(msg.Text)
		}
	}
	return c, nil
}

func (c *chat) unfolded(id string) bool {
	if strings.HasSuffix(id, ":thinking") {
		return c.thinking == agentconfig.ThinkingOpen
	}
	if id == "spawn" {
		// The live spawn tree shows its subagents until it is folded
		return true
	}
	return c.expanded
}

func (c *chat) refold(suffixes ...string) {
	for id := range c.folds {
		for _, suffix := range suffixes {
			if strings.HasSuffix(id, suffix) {
				delete(c.folds, id)
			}
		}
	}
}

func (c *chat) action(id string) tea.Cmd {
	page := max(c.shown-2, 1)
	switch id {
	case "session.scroll.up":
		c.scroll(-page)
	case "session.scroll.down":
		c.scroll(page)
	case "session.scroll.top":
		c.top, c.follow = 0, false
	case "session.scroll.end":
		c.follow = true
	case "session.expand", "session.collapse":
		c.expanded = id == "session.expand"
		c.refold(":call", ":result")
	case "session.thinking":
		if c.thinking == agentconfig.ThinkingOpen {
			c.thinking = agentconfig.ThinkingFolded
		} else {
			c.thinking = agentconfig.ThinkingOpen
		}
		c.refold(":thinking")
	case "session.stop":
		switch {
		case c.composer.Searching() || c.composer.Completing():
			return c.composer.Action(id)
		// A reply of this chat read aloud stops first, which is all the key does once that reply is no longer written
		case c.desk.voice.hush(c) && !c.log.busy:
			return nil
		case c.replying && c.composer.Empty():
			c.replying, c.composer.Hint = false, c.hint()
			return c.ask()
		case c.typing != nil && c.composer.Empty():
			return c.untype()
		case c.running() != nil && c.composer.Empty():
			return c.stopLine()
		case c.log.busy && !c.replying:
			return c.interrupt()
		case !c.log.busy && c.composer.Empty() && c.id != "":
			return c.escape()
		}
		c.follow = true
	case "agent.yolo":
		return c.yolo()
	case voiceAction:
		return c.desk.voice.toggle(c)
	case "agent.dock":
		c.folded = !c.folded
	case "session.compact":
		if !c.log.busy && c.id != "" {
			return c.send("/compress")
		}
	case "complete.start":
		c.paths()
	case "composer.send":
		c.entering = true
		before := c.composer.Value()
		cmd := c.composer.Action(id)
		c.entering = false
		if now := c.composer.Value(); now != before && now != "" {
			c.complete()
		}
		return cmd
	default:
		before := c.composer.Value()
		cmd := c.composer.Action(id)
		if c.composer.Value() != before {
			c.complete()
		}
		return cmd
	}
	return nil
}

func (c *chat) take(text string, replace int) tea.Cmd {
	typed := []rune(c.composer.Value())
	set := c.composer.SetValue(string(typed[:min(replace, len(typed))]) + text)
	if command, ok := c.command(text); c.entering && replace == 0 && ok && !needsArgument(command.GetUsage()) {
		return tea.Batch(set, c.composer.Send())
	}
	return set
}

// Whether a command can't run without something typed after it: its usage names an argument in angle brackets outside every optional
// part, so "[enable|disable <tool>...]" asks for nothing and "<message>" does
func needsArgument(usage string) bool {
	depth := 0
	for _, r := range usage {
		switch r {
		case '[':
			depth++
		case ']':
			depth = max(depth-1, 0)
		case '<':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

func (c *chat) complete() {
	value := c.composer.Value()
	typed, typing := strings.CutPrefix(value, "/")
	if !typing || strings.Contains(typed, "\n") {
		return
	}
	if strings.Contains(typed, " ") {
		if command, ok := c.command(value); ok && command.GetUsage() != "" {
			c.composer.Offer(value, []cards.Offer{{Display: "/" + command.GetName() + " " + command.GetUsage(), Meta: command.GetHelp()}}, 0)
		}
		return
	}
	// A name a screen answers that no list shows is offered nothing else, so enter runs it as typed
	if slices.ContainsFunc(c.kit.Screens(), func(screen tui.Screen) bool { return slices.Contains(screen.Aliases, typed) }) {
		c.composer.Offer(value, nil, 0)
		return
	}
	var offers []cards.Offer
	for _, command := range c.catalog() {
		name := command.GetName()
		if !strings.HasPrefix(name, typed) {
			for _, alias := range command.GetAliases() {
				if strings.HasPrefix(alias, typed) {
					name = alias
					break
				}
			}
		}
		if strings.HasPrefix(name, typed) {
			offers = append(offers, cards.Offer{Text: "/" + name + " ", Display: "/" + name, Meta: command.GetHelp(), Group: heading(command)})
		}
	}
	c.composer.Offer(value, offers, 0)
}

func (c *chat) paths() {
	value := c.composer.Value()
	start := strings.LastIndexAny(value, " \t\n") + 1
	offers := cards.PathOffers(value[start:], c.kit.Env.WorkDir(), cards.PathFile, completions)
	c.composer.Offer(value, offers, utf8.RuneCountInString(value[:start]))
}

func (c *chat) scroll(rows int) {
	last := max(len(c.lines)-c.shown, 0)
	c.top = min(max(c.top+rows, 0), last)
	c.follow = c.top == last
}

func (c *chat) View(w, h int) string {
	c.composer.Fit(w)
	c.composer.SetBusy(c.log.busy)
	prompt := min(c.composer.Height(), max(h-3, 1))
	dock := c.dock(w, h)
	c.rows = max(h-prompt-1-len(dock), 1)
	c.prompt = c.rows + 1
	c.shown = c.rows
	if c.sheet != nil {
		c.shown = max(c.rows-len(c.sheet.lines(max(w-4, 1)))-2, 1)
	}
	if c.inquiry != nil {
		_, rows := c.inquiry.FitIn(max(w-4, 1), c.rows)
		c.shown = max(c.rows-rows-2, 1)
	}
	if w != c.width {
		c.width, c.rendered = w, nil
	}
	c.kept = map[string][]string{}
	if c.lines = c.paint(w); len(c.lines) == 0 && c.stranded != "" {
		c.lines = []row{{}}
		for _, line := range strings.Split(render.Wrap(c.stranded, max(w-1, 8)), "\n") {
			c.lines = append(c.lines, row{text: render.Dim(line, c.kit.Env.Theme)})
		}
	}
	c.rendered = c.kept
	last := max(len(c.lines)-c.shown, 0)
	if c.follow {
		c.top = last
	}
	c.top = min(c.top, last)
	body := make([]string, c.rows)
	for i := range c.shown {
		if c.top+i < len(c.lines) {
			body[i] = c.lines[c.top+i].text
		}
	}
	above := cards.OverlayPopup(append(append(body, dock...), ""), c.composer.Popup(w))
	return strings.Join(append(above, c.composer.View(w, prompt)), "\n")
}

func (c *chat) submit(text string) tea.Cmd {
	c.follow = true
	if c.typing != nil {
		typed := c.typing
		c.typing, c.composer.Hint = nil, c.hint()
		return tea.Batch(c.respond(typed.set, typed.question, append(typed.picked, strings.TrimSpace(text))), c.inquire())
	}
	if c.replying {
		request := c.replyTo
		c.replying, c.replyTo, c.composer.Hint = false, "", c.hint()
		return c.decide(request, agentv1.ApprovalScope_APPROVAL_SCOPE_UNSPECIFIED, strings.TrimSpace(text))
	}
	if liner, line, ok := c.desk.liner(text); ok {
		return c.runLine(liner, line)
	}
	// A command the app runs itself is told to the session's pre_command hooks, as one the server runs is
	id := c.id
	if cmd, ok := c.local(text); ok {
		c.observe(id, text)
		return cmd
	}
	if cmd, ok := c.kit.Slash(text); ok {
		c.observe(id, text)
		return cmd
	}
	// A stop phrase typed alone while voice input is on turns it off instead of going to the model
	if c.desk.voice.typedStop(c, text) {
		return c.desk.voice.stopped(text)
	}
	if c.editing != nil && !strings.HasPrefix(text, "/") {
		return c.resend(text)
	}
	return c.send(text)
}

// Sends a line to the session: a command, or a message on the voice chat route when the person spoke it; every message tells the model
// when the person cut a spoken reply off before it
func (c *chat) send(text string) tea.Cmd {
	request := &agentv1.SendRequest{Text: text, Voice: c.voiced}
	if _, command := c.command(text); !command {
		request.Interrupted = c.desk.voice.cut()
	}
	return c.post(request)
}

func (c *chat) post(request *agentv1.SendRequest) tea.Cmd {
	if c.starting {
		c.waiting = append(c.waiting, request)
		return nil
	}
	text := request.GetText()
	id, epoch, named, profile, left := c.id, c.epoch, c.named, c.profile, c.left
	c.starting = id == ""
	return tui.Async(func() func() tea.Cmd {
		ctx := c.kit.Env.Ctx
		var err error
		if id == "" {
			request.Left = left
			var response *sessionsv1.CreateResponse
			if response, err = c.desk.sessions.Create(ctx, &sessionsv1.CreateRequest{Directory: c.kit.Env.WorkDir(), Profile: profile}); err == nil {
				id, profile = response.GetSession().GetId(), response.GetSession().GetProfile()
			}
			if err == nil && named != "" {
				_, err = c.desk.sessions.Rename(ctx, &sessionsv1.RenameRequest{Id: id, Title: named})
			}
			if err == nil {
				request.ClientEnv, request.ClientHost = c.desk.shell(ctx, profile)
			}
		}
		if err == nil {
			request.SessionId = id
			if !c.desk.told(id) {
				request.Launch = c.desk.asked.Launch()
			}
			_, err = c.desk.agents.Send(ctx, request)
		}
		if err == nil {
			// The session ends as its hooks know it once the app exits
			c.desk.asked.Hold(id)
		}
		return func() tea.Cmd {
			if epoch != c.epoch {
				return nil
			}
			// The message went through the session store, which may have something to tell, such as a database it set aside as it opened
			cmds := []tea.Cmd{c.desk.hear()}
			if c.starting && id != "" {
				c.id, c.named, c.profile = id, "", profile
				cmds = append(cmds, c.watch())
			}
			if err == nil && request.Left != "" {
				c.left = ""
			}
			c.starting = false
			if err != nil {
				said := "**Your message was not sent.** " + strs.Sentence(tui.Reason(err))
				if c.composer.Empty() {
					said += " It is back in the message box."
					cmds = append(cmds, c.composer.SetValue(text))
				}
				c.log.note(said)
			}
			waiting := c.waiting
			c.waiting = nil
			for _, next := range waiting {
				cmds = append(cmds, c.post(next))
			}
			if strings.HasPrefix(text, "/") && err == nil {
				cmds = append(cmds, c.retitle())
			}
			return tea.Batch(cmds...)
		}
	})
}

func (c *chat) retitle() tea.Cmd {
	id, epoch := c.id, c.epoch
	if id == "" {
		return nil
	}
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.sessions.Get(c.kit.Env.Ctx, &sessionsv1.GetRequest{Id: id})
		return func() tea.Cmd {
			if err == nil && epoch == c.epoch && response.GetSession().GetTitle() != "" {
				c.title = response.GetSession().GetTitle()
			}
			return nil
		}
	})
}

func (c *chat) interrupt() tea.Cmd {
	id := c.id
	if id == "" {
		return nil
	}
	return tui.Async(func() func() tea.Cmd {
		_, err := c.desk.agents.Stop(c.kit.Env.Ctx, &agentv1.StopRequest{SessionId: id})
		return func() tea.Cmd {
			if err != nil {
				c.log.note("**The reply did not stop.** " + tui.Reason(err))
			}
			return nil
		}
	})
}

func (c *chat) reset() {
	c.desk.leave(c)
	c.unedit()
	if c.stop != nil {
		c.stop()
	}
	c.epoch++
	c.stopLine()
	c.runs = nil
	c.id, c.title, c.named, c.left, c.stop, c.starting, c.waiting = "", "", "", "", nil, false, nil
	c.log, c.folds, c.rendered = transcript{}, map[string]bool{}, nil
	c.top, c.follow = 0, true
	c.docked = nil
	c.spawn = spawnState{}
	c.working.Store(false)
}

func (c *chat) watch() tea.Cmd {
	if c.stop != nil {
		c.stop()
	}
	ctx, stop := context.WithCancel(c.kit.Env.Ctx)
	c.stop = stop
	id, epoch := c.id, c.epoch
	shown := c.desk.showing(c)
	go c.docking(ctx, id, epoch)
	return tea.Batch(shown, func() tea.Msg {
		stream, err := c.desk.agents.Watch(ctx, &agentv1.WatchRequest{SessionId: id, Client: c.desk.client})
		if err == nil {
			defer stream.Close()
		}
		first := true
		for err == nil {
			var response *agentv1.WatchResponse
			if response, err = stream.Receive(); err == nil {
				replay := first
				first = false
				c.kit.Apply(func() tea.Cmd { return c.received(epoch, response, replay) })
			}
		}
		if ctx.Err() == nil {
			c.kit.Apply(func() tea.Cmd { return c.dropped(epoch, err) })
		}
		return nil
	})
}

func (c *chat) received(epoch int, response *agentv1.WatchResponse, replay bool) tea.Cmd {
	if epoch != c.epoch {
		return nil
	}
	var kept tea.Cmd
	if replay {
		c.log.replay()
		kept = c.keptLines(epoch)
	}
	busy := c.log.busy
	taken := c.log.apply(response)
	if !replay {
		// Voice reads the reply to a message sent from this chat aloud as it is written, by its own rule
		c.desk.voice.reply(c, response, busy, c.log.busy)
	}
	if title := response.GetTitle(); title != "" {
		c.title = title
	}
	// The conversation went on in a new session after its context was compressed, which the chat follows
	continued := false
	if next := response.GetContinued(); next != "" && next != c.id {
		c.id, continued = next, true
	}
	if c.quiet && response.GetRewind() != nil {
		c.quiet, taken = false, ""
	}
	var cmds []tea.Cmd
	if response.GetRewind() != nil {
		c.folds = map[string]bool{}
	}
	if event := response.GetSubagent(); event != nil {
		if len(c.spawn.agents) == 0 && !replay {
			cmds = append(cmds, c.delegationLimits())
		}
		if tree := c.spawn.apply(event); tree != nil {
			cmds = append(cmds, c.saveTree(tree))
		}
		c.tree.refresh()
	}
	if event := response.GetExhausted(); event != nil && !replay {
		cmds = append(cmds, c.exhausted(event))
	}
	if taken != "" && c.composer.Empty() {
		cmds = append(cmds, c.composer.SetValue(taken))
	}
	if continued {
		cmds = append(cmds, c.desk.remember(c))
	}
	if busy != c.log.busy {
		c.working.Store(c.log.busy)
		c.nudge()
	}
	if busy && !c.log.busy {
		c.unseen = c.kit.Focused() != wm.Card(c)
		cmds = append(cmds, c.retitle(), c.desk.remember(c))
		// The fan-out of the turn that ended is kept for /replay, on disk as well
		if tree := c.spawn.finish(c.id); tree != nil {
			cmds = append(cmds, c.saveTree(tree))
			c.tree.refresh()
		}
	}
	return tea.Batch(append(cmds, kept, c.ask(), c.inquire())...)
}

func (c *chat) dropped(epoch int, err error) tea.Cmd {
	if epoch != c.epoch {
		return nil
	}
	c.log.busy = false
	if connect.CodeOf(err) == connect.CodeNotFound {
		c.log.note("**This session was deleted.** Send a message to start a new one.")
		c.id, c.title, c.stop = "", "", nil
		return tea.Batch(c.ask(), c.inquire())
	}
	return tui.Async(func() func() tea.Cmd {
		select {
		case <-time.After(reconnect):
		case <-c.kit.Env.Ctx.Done():
		}
		return func() tea.Cmd {
			if epoch != c.epoch || c.kit.Env.Ctx.Err() != nil {
				return nil
			}
			return c.watch()
		}
	})
}

// Variables of this shell a new session of the profile takes as its client's, with the name of this machine: the behaviour variables,
// the toolchain and the names the profile's client variables setting lists
func (d *desk) shell(ctx context.Context, profile string) (map[string]string, string) {
	var extra []string
	if setting, err := d.settings.Get(ctx, &configv1.GetRequest{Key: terminalconfig.ClientEnvPassthrough, Profile: profile}); err == nil {
		yaml.Unmarshal([]byte(setting.GetSetting().GetValue()), &extra)
	}
	host, _ := os.Hostname()
	return tools.ClientVariables(extra), host
}
