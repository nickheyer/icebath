package app

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/internal/profile"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the spawn tree
const spawnContext = "agent.spawntree"

var spawnKeys = []keys.Action{
	{ID: "agent.spawntree.tail", Context: spawnContext, Label: "follow", Doc: "Follow what the highlighted subagent does as it happens", Default: []string{"enter", "t"}},
	{ID: "agent.spawntree.detail", Context: spawnContext, Label: "details", Doc: "Show everything known about the highlighted subagent", Default: []string{"d"}},
	{ID: "agent.spawntree.steer", Context: spawnContext, Label: "steer", Doc: "Send the highlighted subagent a note it reads after its next tool call", Default: []string{"e"}},
	{ID: "agent.spawntree.kill", Context: spawnContext, Label: "stop", Doc: "Stop the highlighted subagent", Default: []string{"x"}},
	{ID: "agent.spawntree.subtree", Context: spawnContext, Label: "stop all", Doc: "Stop the highlighted subagent and every subagent under it", Default: []string{"X"}},
	{ID: "agent.spawntree.pause", Context: spawnContext, Label: "pause", Doc: "Hold new subagents back, or let them start again", Default: []string{"p"}},
	{ID: "agent.spawntree.sort", Context: spawnContext, Label: "sort", Doc: "Sort the tree by start, status, time or tool calls", Default: []string{"s"}},
	{ID: "agent.spawntree.filter", Context: spawnContext, Label: "filter", Doc: "Show all subagents, or only the working, the failed or those with none under them", Default: []string{"f"}},
	{ID: "agent.spawntree.older", Context: spawnContext, Label: "older tree", Doc: "Show the spawn tree that finished before this one", Default: []string{"["}},
	{ID: "agent.spawntree.newer", Context: spawnContext, Label: "newer tree", Doc: "Show the next finished spawn tree, then the reply running now", Default: []string{"]"}},
	{ID: "agent.spawntree.more", Context: spawnContext, Label: "more", Doc: "Show everything you can do with the subagent and the tree", Default: []string{"m", "ctrl+o"}},
}

var (
	// Ways the tree is sorted and parts of it shown, as the note and the status say them, in the order the keys step through them
	spawnSorts   = []string{"the order they started", "status", "time, slowest first", "tool calls, most first"}
	spawnFilters = []string{"all subagents", "only working subagents", "only failed subagents", "only subagents with none under them"}
	// Order the status sort puts subagents in, those that need a look first
	spawnRank = map[string]int{"running": 0, "queued": 1, "failed": 2, "error": 2, "timeout": 3, "interrupted": 3, "completed": 4}
)

// The spawn tree screen of a chat: the live fan-out of its session or a finished tree, with its controls
type spawnScreen struct {
	chat *chat
	hub  *cards.Hub
	list *cards.List
	// 0 for the live turn, k for the kth newest finished tree of the chat; a tree loaded from disk in place of both
	index  int
	loaded *agentv1.SpawnTree
	sort   int
	filter int
	// How delegation stands for the session, as the server last said
	status *agentv1.SubagentsResponse
	nodes  []*spawnNode
	closed bool
}

func (s *spawnScreen) refresh() {
	if s != nil && s.list != nil && !s.closed {
		s.list.Refresh()
	}
}

// The tree shown: the live one has no snapshot
func (s *spawnScreen) snapshot() *agentv1.SpawnTree {
	if s.loaded != nil {
		return s.loaded
	}
	if s.index > 0 && s.index <= len(s.chat.spawn.trees) {
		return s.chat.spawn.trees[s.index-1]
	}
	return nil
}

func (s *spawnScreen) replay() bool { return s.snapshot() != nil }

// Subagents shown: those of the snapshot, else those of the live turn with those the server runs for the session that it has not told of
// here, such as background delegations
func (s *spawnScreen) agents() []*agentv1.Subagent {
	if snap := s.snapshot(); snap != nil {
		return snap.GetSubagents()
	}
	agents := s.chat.spawn.live()
	for _, live := range s.status.GetSubagents() {
		if !slices.ContainsFunc(agents, func(a *agentv1.Subagent) bool { return a.GetId() == live.GetId() }) {
			agents = append(agents, &agentv1.Subagent{Id: live.GetId(), ParentId: live.GetParentId(), Depth: live.GetDepth(), Goal: live.GetGoal(),
				DelegationId: live.GetDelegationId(), Model: live.GetModel(), Started: live.GetStarted(), Status: live.GetStatus(),
				ToolCount: live.GetToolCount(), Tool: live.GetLastTool(), Transcript: live.GetTranscript(), SessionId: live.GetSessionId(),
				TaskIndex: live.GetTaskIndex(), TaskCount: live.GetTaskCount()})
		}
	}
	return agents
}

// Nodes in view, sorted and filtered as asked
func (s *spawnScreen) prepare() []*spawnNode {
	nodes := flatten(spawnTree(s.agents()))
	now := time.Now()
	switch s.filter {
	case 1:
		nodes = slices.DeleteFunc(nodes, func(n *spawnNode) bool { return !inFlight(n.agent.GetStatus()) })
	case 2:
		nodes = slices.DeleteFunc(nodes, func(n *spawnNode) bool {
			status := n.agent.GetStatus()
			return status != "failed" && status != "error" && status != "timeout" && status != "interrupted"
		})
	case 3:
		nodes = slices.DeleteFunc(nodes, func(n *spawnNode) bool { return len(n.children) > 0 })
	}
	switch s.sort {
	case 1:
		slices.SortStableFunc(nodes, func(x, y *spawnNode) int {
			return cmp.Compare(rankOf(x.agent.GetStatus()), rankOf(y.agent.GetStatus()))
		})
	case 2:
		slices.SortStableFunc(nodes, func(x, y *spawnNode) int {
			a, _ := elapsed(x.agent, now)
			b, _ := elapsed(y.agent, now)
			return cmp.Compare(b, a)
		})
	case 3:
		slices.SortStableFunc(nodes, func(x, y *spawnNode) int { return cmp.Compare(y.agg.tools, x.agg.tools) })
	}
	return nodes
}

// Whether the subagent of the id still runs, as this chat's events or the server says
func (s *spawnScreen) active(id string) bool {
	for _, a := range s.chat.spawn.agents {
		if a.GetId() == id {
			return inFlight(a.GetStatus())
		}
	}
	return slices.ContainsFunc(s.status.GetSubagents(), func(live *agentv1.LiveSubagent) bool { return live.GetId() == id })
}

func rankOf(status string) int {
	if rank, ok := spawnRank[status]; ok {
		return rank
	}
	return 5
}

// Columns of the tree: the subagent's task, what it is doing, its model, its tool calls, how long it ran and the tokens it used
var spawnColumns = []render.Column{{Title: "Subagent", Flex: 3}, {Title: "Status"}, {Title: "Model", Flex: 1}, {Title: "Tools", Right: true},
	{Title: "Time", Right: true}, {Title: "Tokens", Right: true}}

// Opens the spawn tree of the chat: the live reply, or the kth newest finished tree, or a tree read from disk
func (c *chat) openTree(index int, loaded *agentv1.SpawnTree) tea.Cmd {
	kit := c.kit
	s := &spawnScreen{chat: c, index: index, loaded: loaded}
	t := kit.Env.Theme
	s.list = cards.NewList(kit.Env, cards.ListSpec{
		Title: "Spawn tree",
		Glyph: t.Glyphs.Agent,
		Key:   "tui:agents",
		// The keys of the tree are shared rather than its own, so each works while the footer shows only the five most used
		Context: cards.HubContext,
		Shared:  []string{spawnContext},
		Primary: "agent.spawntree.tail",
		Intro:   s.intro,
		Columns: spawnColumns,
		Yield:   []int{2, 5, 3},
		Rows: func() [][]string {
			s.nodes = s.prepare()
			now := time.Now()
			rows := make([][]string, len(s.nodes))
			for i, n := range s.nodes {
				a := n.agent
				took := ""
				if seconds, ok := elapsed(a, now); ok {
					took = spawnDuration(seconds)
				}
				goal := strings.Repeat("  ", int(a.GetDepth())) + statusGlyph(a.GetStatus(), t) + " " + cmp.Or(oneLine(a.GetGoal()), "Subagent")
				rows[i] = []string{goal, statusWord(a.GetStatus()), cmp.Or(a.GetModel(), render.Dim("same as the agent", t)), strconv.Itoa(int(a.GetToolCount())),
					took, spawnTokens(a.GetInputTokens() + a.GetOutputTokens())}
			}
			return rows
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(s.nodes) {
				return nil
			}
			return s.detail(s.nodes[i], i)
		},
		Empty: func() string {
			if s.replay() {
				return "This tree has no subagents."
			}
			return "No subagents in this reply yet. The agent starts them when it splits up a big task."
		},
		Status: s.trailer,
		Banner: s.banner,
		Free: map[string]bool{"agent.spawntree.pause": true, "agent.spawntree.sort": true, "agent.spawntree.filter": true, "agent.spawntree.older": true,
			"agent.spawntree.newer": true, "agent.spawntree.more": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			more := wm.Hint{Action: "agent.spawntree.more"}
			switch {
			case i < 0 && s.replay():
				return []wm.Hint{{Action: "agent.spawntree.older"}, {Action: "agent.spawntree.newer"}, more}
			case i < 0:
				return []wm.Hint{s.pauseHint(), {Action: "agent.spawntree.filter"}, {Action: "agent.spawntree.older"}, more}
			case s.replay():
				return []wm.Hint{{Action: "agent.spawntree.tail", Label: "read transcript"}, {Action: "agent.spawntree.detail"}, {Action: "agent.spawntree.older"},
					{Action: "agent.spawntree.newer"}, more}
			case !inFlight(s.nodes[i].agent.GetStatus()):
				return []wm.Hint{{Action: "agent.spawntree.tail", Label: "read transcript"}, {Action: "agent.spawntree.detail"}, s.pauseHint(),
					{Action: "agent.spawntree.filter"}, more}
			}
			return []wm.Hint{{Action: "agent.spawntree.tail"}, {Action: "agent.spawntree.detail"}, {Action: "agent.spawntree.steer"},
				{Action: "agent.spawntree.kill"}, more}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"agent.spawntree.tail":   func(_ *cards.List, i int) tea.Cmd { return s.tail(s.nodes[i].agent) },
			"agent.spawntree.detail": func(_ *cards.List, i int) tea.Cmd { return s.page(s.nodes[i]) },
			"agent.spawntree.steer":  func(_ *cards.List, i int) tea.Cmd { return s.live(func() tea.Cmd { return s.steer(s.nodes[i].agent) }) },
			"agent.spawntree.kill": func(_ *cards.List, i int) tea.Cmd {
				return s.live(func() tea.Cmd { return s.kill([]*agentv1.Subagent{s.nodes[i].agent}) })
			},
			"agent.spawntree.subtree": func(_ *cards.List, i int) tea.Cmd {
				return s.live(func() tea.Cmd { return s.kill(subtree(s.nodes[i])) })
			},
			"agent.spawntree.pause":  func(*cards.List, int) tea.Cmd { return s.live(s.pause) },
			"agent.spawntree.sort":   func(*cards.List, int) tea.Cmd { return s.resort() },
			"agent.spawntree.filter": func(*cards.List, int) tea.Cmd { return s.refilter() },
			"agent.spawntree.older":  func(*cards.List, int) tea.Cmd { return s.step(1) },
			"agent.spawntree.newer":  func(*cards.List, int) tea.Cmd { return s.step(-1) },
			"agent.spawntree.more": func(_ *cards.List, i int) tea.Cmd {
				if i < 0 || i >= len(s.nodes) {
					return s.more(nil)
				}
				return s.more(s.nodes[i])
			},
		},
	})
	s.list.ToggleDetail()
	s.hub = kit.Hub("tui:agents", s.list)
	s.hub.OnClose(func() tea.Cmd {
		s.closed = true
		if c.tree == s {
			c.tree = nil
		}
		return nil
	})
	c.tree = s
	go s.poll(c.id)
	return kit.WM.ShowOverlay(s.hub)
}

// Button that holds new subagents back, or lets them start again
func (s *spawnScreen) pauseHint() wm.Hint {
	if s.status.GetPaused() {
		return wm.Hint{Action: "agent.spawntree.pause", Label: "let start"}
	}
	return wm.Hint{Action: "agent.spawntree.pause"}
}

// Sorts the tree the next way and says which
func (s *spawnScreen) resort() tea.Cmd {
	s.sort = (s.sort + 1) % len(spawnSorts)
	s.refresh()
	return s.hub.Say("Sorted by "+spawnSorts[s.sort]+".", cards.NoteInfo)
}

// Shows the next part of the tree and says which
func (s *spawnScreen) refilter() tea.Cmd {
	s.filter = (s.filter + 1) % len(spawnFilters)
	s.refresh()
	return s.hub.Say("Showing "+spawnFilters[s.filter]+".", cards.NoteInfo)
}

// Everything the tree does, with the highlighted subagent and with the tree, in a view of its own, each row with the key that does it
// straight from the tree
func (s *spawnScreen) more(n *spawnNode) tea.Cmd {
	var deeds []deed
	title := "Spawn tree"
	if n != nil {
		a := n.agent
		title = cmp.Or(oneLine(a.GetGoal()), "Subagent")
		const own = "This subagent"
		follow := deed{"Follow what it does", own, "agent.spawntree.tail", "follow", func() tea.Cmd { return s.tail(a) }}
		if s.replay() || !inFlight(a.GetStatus()) {
			follow.title, follow.verb = "Read its transcript", "read"
		}
		deeds = append(deeds, follow, deed{"Show everything known about it", own, "agent.spawntree.detail", "show", func() tea.Cmd { return s.page(n) }})
		if !s.replay() && inFlight(a.GetStatus()) {
			deeds = append(deeds,
				deed{"Send it a note", own, "agent.spawntree.steer", "steer", func() tea.Cmd { return s.steer(a) }},
				deed{"Stop it", own, "agent.spawntree.kill", "stop", func() tea.Cmd { return s.kill([]*agentv1.Subagent{a}) }})
		}
		if !s.replay() && len(n.children) > 0 {
			deeds = append(deeds, deed{"Stop it and every subagent under it", own, "agent.spawntree.subtree", "stop all", func() tea.Cmd { return s.kill(subtree(n)) }})
		}
	}
	const all = "The tree"
	if !s.replay() {
		pause := deed{"Hold new subagents back", all, "agent.spawntree.pause", "pause", s.pause}
		if s.status.GetPaused() {
			pause.title, pause.verb = "Let new subagents start again", "let start"
		}
		deeds = append(deeds, pause)
	}
	next := func(list []string, at int) string { return list[(at+1)%len(list)] }
	deeds = append(deeds,
		deed{"Sort by " + next(spawnSorts, s.sort), all, "agent.spawntree.sort", "sort", s.resort},
		deed{"Show " + next(spawnFilters, s.filter), all, "agent.spawntree.filter", "show", s.refilter})
	if s.loaded == nil && s.index < len(s.chat.spawn.trees) {
		deeds = append(deeds, deed{"Show the tree that finished before", all, "agent.spawntree.older", "show", func() tea.Cmd { return s.step(1) }})
	}
	if s.loaded == nil && s.index > 0 {
		deeds = append(deeds, deed{"Show the next tree", all, "agent.spawntree.newer", "show", func() tea.Cmd { return s.step(-1) }})
	}
	return showDeeds(s.chat.kit, s.hub, title, "Everything you can do here. The key beside each one does it straight from the tree.", deeds)
}

// Reads how delegation stands for the session while the screen is open, the rows repainted each second so running times count
func (s *spawnScreen) poll(session string) {
	kit := s.chat.kit
	for tick := 0; ; tick++ {
		if tick%3 == 0 && session != "" {
			response, err := s.chat.desk.agents.Subagents(kit.Env.Ctx, &agentv1.SubagentsRequest{SessionId: session})
			kit.Apply(func() tea.Cmd {
				if err == nil {
					s.status, s.chat.limits = response, response
				}
				s.refresh()
				return nil
			})
		} else {
			kit.Apply(func() tea.Cmd { s.refresh(); return nil })
		}
		select {
		case <-kit.Env.Ctx.Done():
			return
		case <-time.After(time.Second):
		}
		if !s.onUI(func() bool { return !s.closed }) {
			return
		}
	}
}

// Answer of the question asked on the loop that paints, false once the app stops
func (s *spawnScreen) onUI(ask func() bool) bool {
	kit := s.chat.kit
	answer := make(chan bool, 1)
	kit.Apply(func() tea.Cmd { answer <- ask(); return nil })
	select {
	case yes := <-answer:
		return yes
	case <-kit.Env.Ctx.Done():
		return false
	}
}

// Runs the control on the live reply; a finished tree takes none
func (s *spawnScreen) live(action func() tea.Cmd) tea.Cmd {
	if s.replay() {
		return s.hub.Fail("This tree has finished, so its subagents can't be changed.")
	}
	return action()
}

// The subagent and every subagent under it, the deepest first
func subtree(node *spawnNode) []*agentv1.Subagent {
	var out []*agentv1.Subagent
	var walk func(n *spawnNode)
	walk = func(n *spawnNode) {
		for _, child := range n.children {
			walk(child)
		}
		if inFlight(n.agent.GetStatus()) {
			out = append(out, n.agent)
		}
	}
	walk(node)
	return out
}

// Stops the subagents after a question, as their runs cannot be started again
func (s *spawnScreen) kill(agents []*agentv1.Subagent) tea.Cmd {
	if len(agents) == 0 {
		return s.hub.Fail("Nothing is running there to stop.")
	}
	name := "“" + cmp.Or(oneLine(agents[0].GetGoal()), agents[0].GetId()) + "”"
	stop := func() tea.Cmd {
		if len(agents) > 1 {
			s.hub.Busy("Stopping " + render.Count(len(agents), "subagent") + "…")
		} else {
			s.hub.Busy("Stopping " + name + "…")
		}
		kit := s.chat.kit
		return tui.Async(func() func() tea.Cmd {
			var failed error
			missing := false
			for _, a := range agents {
				response, err := s.chat.desk.agents.Interrupt(kit.Env.Ctx, &agentv1.InterruptRequest{SessionId: s.chat.id, SubagentId: a.GetId()})
				switch {
				case err != nil:
					failed = err
				case !response.GetFound():
					missing = true
				}
			}
			return func() tea.Cmd {
				switch {
				case failed != nil:
					return s.hub.Fail("The subagent did not stop. " + tui.Reason(failed))
				case missing && len(agents) == 1:
					return s.hub.Fail(name + " had already finished.")
				case len(agents) > 1:
					return s.hub.Done("Stopped " + render.Count(len(agents), "subagent") + ".")
				}
				return s.hub.Done("Stopped " + name + ".")
			}
		})
	}
	q := cards.Question{Text: "Stop " + render.Count(len(agents), "subagent") + "? What they did so far still comes back to the agent.", Verb: "Stop"}
	if len(agents) == 1 {
		q.Text = "Stop " + name + "? What it did so far still comes back to the agent."
	}
	s.list.Confirm(q, stop)
	return nil
}

func (s *spawnScreen) pause() tea.Cmd {
	kit := s.chat.kit
	paused := !s.status.GetPaused()
	return tui.Async(func() func() tea.Cmd {
		response, err := s.chat.desk.agents.Pause(kit.Env.Ctx, &agentv1.PauseRequest{Paused: paused})
		return func() tea.Cmd {
			if err != nil {
				return s.hub.Fail(tui.Reason(err))
			}
			if s.status == nil {
				s.status = &agentv1.SubagentsResponse{}
			}
			s.status.Paused = response.GetPaused()
			s.chat.limits = s.status
			s.refresh()
			if response.GetPaused() {
				return s.hub.Done("New subagents are now held back. Those running go on.")
			}
			return s.hub.Done("New subagents can start again.")
		}
	})
}

// Asks for the note to steer the subagent with and sends it
func (s *spawnScreen) steer(a *agentv1.Subagent) tea.Cmd {
	// One that is finishing reads no more notes, as the server says of those it runs
	for _, live := range s.status.GetSubagents() {
		if live.GetId() == a.GetId() && !live.GetSteering() {
			return s.hub.Fail("This subagent is finishing, so it reads no more notes.")
		}
	}
	kit := s.chat.kit
	var form *cards.Form
	form = cards.NewForm(kit.Env, cards.FormSpec{
		Title:  "Steer",
		Intro:  []string{"Send “" + cmp.Or(oneLine(a.GetGoal()), a.GetId()) + "” a note. It reads the note after its next tool call, without stopping."},
		Submit: "Send",
		Fields: []cards.Field{{ID: "text", Label: "Note", Required: true, Help: "What the subagent should know, such as which file to look at first."}},
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			text := strings.TrimSpace(values["text"])
			return tea.Batch(s.hub.Close(form), tui.Async(func() func() tea.Cmd {
				response, err := s.chat.desk.agents.Steer(kit.Env.Ctx, &agentv1.SteerRequest{SessionId: s.chat.id, SubagentId: a.GetId(), Text: text})
				return func() tea.Cmd {
					switch {
					case err != nil:
						return s.hub.Fail(tui.Reason(err))
					case !response.GetQueued():
						return s.hub.Fail(cmp.Or(response.GetReason(), "The subagent did not take the note."))
					}
					return s.hub.Done("Note sent. The subagent reads it after its next tool call.")
				}
			}))
		},
	})
	return s.hub.Push(form)
}

// Steps through the finished trees: older away from the live reply, newer back to it
func (s *spawnScreen) step(delta int) tea.Cmd {
	if s.loaded != nil {
		return s.hub.Fail("This tree was opened from a file, so it has no older or newer tree.")
	}
	next := s.index + delta
	switch {
	case next < 0:
		return s.hub.Say("This is the reply running now, the newest there is.", cards.NoteInfo)
	case next > len(s.chat.spawn.trees):
		return s.hub.Say("This is the oldest finished tree this chat keeps.", cards.NoteInfo)
	}
	s.index = next
	s.refresh()
	if next == 0 {
		return s.hub.Say("Showing the reply running now.", cards.NoteInfo)
	}
	return s.hub.Say(fmt.Sprintf("Showing finished tree %d of %d.", next, len(s.chat.spawn.trees)), cards.NoteInfo)
}

// Lines above the tree: which tree it is, how far delegation may go and whether new subagents are held back, and the totals of the tree
func (s *spawnScreen) intro() []string {
	var lines []string
	if snap := s.snapshot(); snap != nil {
		head := "A finished spawn tree"
		if s.index > 0 {
			head = fmt.Sprintf("Finished spawn tree %d of %d", s.index, len(s.chat.spawn.trees))
		}
		if label := snap.GetLabel(); label != "" {
			head += ": " + label
		}
		lines = append(lines, head+".")
	} else {
		line := "The subagents of the reply running now."
		if n := s.status.GetMaxConcurrentChildren(); n > 0 {
			line += fmt.Sprintf(" Up to %d run at once.", n)
		}
		if n := s.status.GetMaxSpawnDepth(); n > 0 {
			line += " They nest up to " + render.Count(int(n), "level") + " deep."
		}
		if s.status.GetPaused() {
			line += " New subagents are held back."
		}
		lines = append(lines, line)
	}
	if tree := spawnTree(s.agents()); len(tree) > 0 {
		lines = append(lines, render.Dim(spawnSummary(spawnTotals(tree)), s.chat.kit.Env.Theme))
	}
	return lines
}

// Trailer of the frame: the sort and the part of the tree shown, each only when it is not the first one
func (s *spawnScreen) trailer() string {
	var parts []string
	if s.filter > 0 {
		parts = append(parts, "Showing "+spawnFilters[s.filter])
	}
	if s.sort > 0 {
		parts = append(parts, "Sorted by "+spawnSorts[s.sort])
	}
	return strings.Join(parts, " · ")
}

// Line above the tree: the session's failed background subagents of the last day, for the live reply
func (s *spawnScreen) banner() string {
	if s.replay() || len(s.status.GetFailed()) == 0 {
		return ""
	}
	failed := s.status.GetFailed()
	first := failed[0]
	said := render.Count(len(failed), "subagent") + " started in the background failed in the last day."
	if len(failed) == 1 {
		said = "A subagent started in the background failed in the last day."
	}
	newest := "The newest, “" + cmp.Or(oneLine(first.GetGoal()), "subagent") + "”, " + strings.ToLower(statusWord(first.GetStatus()))
	if reason := oneLine(first.GetError()); reason != "" {
		newest += ": " + strings.TrimRight(reason, ".")
	}
	return said + " " + newest + "."
}

// What is known of the subagent: in full for its own page, a row below zero, else the short form the detail pane shows, which leaves out
// what the row in hand already says
func (s *spawnScreen) detail(n *spawnNode, row int) []string {
	t := s.chat.kit.Env.Theme
	a := n.agent
	full := row < 0
	var lines, meta []string
	if full || s.list.Cut(row, 0) {
		lines = append(lines, render.Heading(cmp.Or(oneLine(a.GetGoal()), "Subagent"), t))
	}
	if full {
		meta = append(meta, statusWord(a.GetStatus()))
	}
	if full || !s.list.Shows(2) {
		meta = append(meta, "Model: "+cmp.Or(a.GetModel(), "the same as the agent"))
	}
	if depth := a.GetDepth(); depth > 0 {
		meta = append(meta, fmt.Sprintf("%d levels down", depth))
	}
	if seconds, ok := elapsed(a, time.Now()); ok && (full || !s.list.Shows(4)) {
		meta = append(meta, "Ran for "+spawnDuration(seconds))
	}
	if calls := a.GetApiCalls(); calls > 0 {
		meta = append(meta, render.Count(int(calls), "step"))
	}
	if len(meta) > 0 {
		lines = append(lines, render.Dim(strings.Join(meta, " · "), t))
	}
	tokens := fmt.Sprintf("Tokens: %s read, %s written", spawnTokens(a.GetInputTokens()), spawnTokens(a.GetOutputTokens()))
	if a.GetReasoningTokens() > 0 {
		tokens += ", " + spawnTokens(a.GetReasoningTokens()) + " reasoning"
	}
	if a.GetCost() > 0 {
		tokens += " · Cost " + render.Money(a.GetCost())
	}
	tools := "Tools: " + render.Count(int(a.GetToolCount()), "call")
	if n.agg.tools != int(a.GetToolCount()) {
		tools += fmt.Sprintf(", %d with the subagents under it", n.agg.tools)
	}
	lines = append(lines, render.Dim(tokens+" · "+tools, t))
	if n.agg.descendants > 0 {
		under := "Under it: " + render.Count(n.agg.descendants, "subagent") + ", " + render.Count(n.agg.depth, "level") + " deep"
		if n.agg.active > 0 {
			under += fmt.Sprintf(", %d working", n.agg.active)
		}
		lines = append(lines, render.Dim(under+", "+spawnTokens(n.agg.input+n.agg.output)+" tokens", t))
	}
	files := func(title string, paths []string) {
		if len(paths) == 0 {
			return
		}
		limit := len(paths)
		if !full {
			limit = min(limit, 4)
		}
		lines = append(lines, "", render.Heading(title, t))
		for _, path := range paths[:limit] {
			lines = append(lines, "  "+path)
		}
		if limit < len(paths) {
			lines = append(lines, render.Dim(fmt.Sprintf("  and %d more", len(paths)-limit), t))
		}
	}
	files("Files read", a.GetFilesRead())
	files("Files written", a.GetFilesWritten())
	var progress *agentProgress
	for _, p := range s.chat.spawn.agents {
		if p.GetId() == a.GetId() {
			progress = p
		}
	}
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		lines = append(lines, "", render.Heading(title, t))
		if !full {
			items = items[max(0, len(items)-3):]
		}
		for _, item := range items {
			lines = append(lines, "  "+item)
		}
	}
	if progress != nil {
		section("Tool calls", progress.tools)
		section("Progress", progress.notes)
		if full {
			section("Reasoning", progress.thinking)
		}
	} else if a.GetTool() != "" {
		section("Tool calls", []string{toolLine(a.GetTool(), a.GetPreview())})
	}
	if tail := a.GetOutputTail(); len(tail) > 0 {
		var out []string
		for _, entry := range tail {
			line := cmp.Or(entry.GetTool(), "tool") + "  " + strings.Join(strings.Fields(entry.GetPreview()), " ")
			if entry.GetError() {
				line = render.Bad(line, t)
			}
			out = append(out, line)
		}
		section("Output", out)
	}
	if summary := strings.TrimSpace(a.GetSummary()); summary != "" {
		lines = append(lines, "", render.Heading("Report", t))
		for _, line := range strings.Split(summary, "\n") {
			lines = append(lines, "  "+line)
		}
	}
	return lines
}

// Page of everything known of the subagent
func (s *spawnScreen) page(n *spawnNode) tea.Cmd {
	page := cards.NewPage(s.chat.kit.Env, cards.PageSpec{
		Title: "Details",
		Key:   "tui:agents:" + n.agent.GetId(),
		Lines: func(w int) []string {
			var out []string
			for _, line := range s.detail(n, -1) {
				out = append(out, render.Wrap(line, max(w, 1)))
			}
			return strings.Split(strings.Join(out, "\n"), "\n")
		},
	})
	return s.hub.Push(page)
}

// Page following the newest part of the subagent's live transcript, read again while it runs
func (s *spawnScreen) tail(a *agentv1.Subagent) tea.Cmd {
	kit := s.chat.kit
	text, session := "Reading the transcript…", s.chat.id
	var page *cards.Page
	read := func() {
		response, err := s.chat.desk.agents.Tail(kit.Env.Ctx, &agentv1.TailRequest{SessionId: session, SubagentId: a.GetId()})
		kit.Apply(func() tea.Cmd {
			switch {
			case err != nil:
				text = "The transcript could not be read. " + tui.Reason(err)
			case !response.GetAvailable():
				text = "This subagent left no transcript."
			default:
				text = response.GetText()
				if response.GetTruncated() {
					text = "Older lines are not shown.\n\n" + text
				}
			}
			return nil
		})
	}
	page = cards.NewPage(kit.Env, cards.PageSpec{
		Title:  "Transcript",
		Key:    "tui:agents:tail:" + a.GetId(),
		Follow: true,
		Lines: func(w int) []string {
			var lines []string
			for _, line := range strings.Split(render.Wrap(cmp.Or(oneLine(a.GetGoal()), "Subagent"), max(w, 1)), "\n") {
				lines = append(lines, render.Heading(line, kit.Env.Theme))
			}
			return append(append(lines, ""), strings.Split(render.Wrap(text, max(w, 1)), "\n")...)
		},
		Init: func(*cards.Page) tea.Cmd {
			id := a.GetId()
			go func() {
				for {
					read()
					select {
					case <-kit.Env.Ctx.Done():
						return
					case <-time.After(1500 * time.Millisecond):
					}
					// Read again while the page shows a subagent that still runs
					if !s.onUI(func() bool { return !s.closed && s.hub.Holds(page) && !s.replay() && s.active(id) }) {
						read()
						return
					}
				}
			}()
			return nil
		},
	})
	return s.hub.Push(page)
}

// Keeps the finished tree on disk for /replay list, its path noted once saved
func (c *chat) saveTree(tree *agentv1.SpawnTree) tea.Cmd {
	if c.id == "" {
		return nil
	}
	tree.SessionId = c.id
	kit := c.kit
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.agents.SaveTree(kit.Env.Ctx, &agentv1.SaveTreeRequest{Tree: tree})
		return func() tea.Cmd {
			if err == nil {
				tree.Path = response.GetPath()
			}
			return nil
		}
	})
}

// /agents: the spawn tree, bare; its words go to the server
func (c *chat) agents(argument string) (tea.Cmd, bool) {
	if strings.TrimSpace(argument) != "" {
		return nil, false
	}
	return c.openTree(0, nil), true
}

// /replay: a finished tree of this chat by its place, the trees kept on disk for the session, or one read from a file
func (c *chat) replayTree(argument string) (tea.Cmd, bool) {
	raw := strings.TrimSpace(argument)
	lower := strings.ToLower(raw)
	switch {
	case lower == "list" || lower == "ls":
		return c.savedTrees(false), true
	// The trees saved for every session of the profile, not only this one's
	case lower == "all" || lower == "list all" || lower == "ls all":
		return c.savedTrees(true), true
	case strings.HasPrefix(lower, "load"):
		// With no file named, the saved trees are where one is picked
		if path := strings.TrimSpace(raw[len("load"):]); path != "" {
			return c.loadTree(path), true
		}
		return c.savedTrees(false), true
	}
	// With no tree finished in this chat, those saved for the session are the ones there are
	if len(c.spawn.trees) == 0 {
		return c.savedTrees(false), true
	}
	index := 1
	if raw != "" && lower != "last" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > len(c.spawn.trees) {
			return wm.Notify(fmt.Sprintf("Usage: /replay %s. Pick a tree from 1 to %d, where 1 is the last.", replayUsage, len(c.spawn.trees)), wm.Warn), true
		}
		index = n
	}
	return c.openTree(index, nil), true
}

// The spawn trees kept on disk for the session, or for every session of its profile, newest first, each opened on enter
func (c *chat) savedTrees(every bool) tea.Cmd {
	none := "No spawn trees are saved for this session yet. A tree is saved once its reply ends."
	if every {
		none = "No spawn trees are saved yet. A tree is saved once its reply ends."
	}
	if c.id == "" {
		return wm.Notify(none, wm.Info)
	}
	kit := c.kit
	t := kit.Env.Theme
	id := c.id
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.agents.Trees(kit.Env.Ctx, &agentv1.TreesRequest{SessionId: id, Every: every, Limit: 30})
		return func() tea.Cmd {
			if err != nil {
				return wm.Notify("The saved spawn trees could not be read. "+tui.Reason(err), wm.Error)
			}
			trees := response.GetTrees()
			if len(trees) == 0 {
				return wm.Notify(none, wm.Info)
			}
			count := func(i int) string {
				if i < len(response.GetCounts()) {
					return render.Count(int(response.GetCounts()[i]), "subagent")
				}
				return ""
			}
			var hub *cards.Hub
			var list *cards.List
			list = cards.NewList(kit.Env, cards.ListSpec{
				Title:   "Saved spawn trees",
				Glyph:   t.Glyphs.Agent,
				Key:     "tui:agents:saved",
				Context: cards.HubContext,
				Primary: "hub.use",
				Pick:    true,
				Bar:     len(trees) > 8,
				Search:  "Search saved trees",
				Intro:   func() []string { return []string{"The spawn trees saved for this session, newest first."} },
				Columns: []render.Column{{}, {Right: true}, {Flex: 1}},
				Rows: func() [][]string {
					rows := make([][]string, len(trees))
					for i, tree := range trees {
						rows[i] = []string{tree.GetFinished().AsTime().Local().Format("Jan 2 15:04"), render.Dim(count(i), t), render.Dim(tree.GetLabel(), t)}
					}
					return rows
				},
				Words: func(i int) string { return trees[i].GetPath() },
				// What the row in hand does not say: its whole label where the row cuts it, and its file
				Detail: func(i int) []string {
					var lines []string
					if list.Cut(i, 2) {
						lines = append(lines, trees[i].GetLabel())
					}
					return append(lines, render.Dim("File: "+profile.Tilde(trees[i].GetPath()), t))
				},
				Hints: func(_ *cards.List, i int) []wm.Hint {
					if i < 0 {
						return nil
					}
					return []wm.Hint{{Action: "hub.use", Label: "open"}}
				},
				Actions: map[string]func(*cards.List, int) tea.Cmd{
					"hub.use": func(_ *cards.List, i int) tea.Cmd {
						return tea.Batch(kit.WM.DismissCard(hub), c.loadTree(trees[i].GetPath()))
					},
				},
			})
			list.ToggleDetail()
			hub = kit.Hub("tui:agents:saved", list)
			return kit.WM.ShowOverlay(hub)
		}
	})
}

// Reads a kept tree from its file and opens it
func (c *chat) loadTree(path string) tea.Cmd {
	kit := c.kit
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.agents.LoadTree(kit.Env.Ctx, &agentv1.LoadTreeRequest{Path: path})
		return func() tea.Cmd {
			switch {
			case err != nil:
				return wm.Notify("The spawn tree could not be read. "+tui.Reason(err), wm.Error)
			case len(response.GetTree().GetSubagents()) == 0:
				return wm.Notify("That file holds no spawn tree icebath can read.", wm.Warn)
			}
			return c.openTree(0, response.GetTree())
		}
	})
}

// /replay-diff: two finished trees of this chat side by side, picked by their places
func (c *chat) replayDiff(argument string) (tea.Cmd, bool) {
	words := strings.Fields(argument)
	usage := "Usage: /replay-diff " + replayDiffUsage + ". For example, /replay-diff 2 1 compares the last tree with the one before it."
	if len(words) != 2 {
		return wm.Notify(usage, wm.Warn), true
	}
	pick := func(word string) *agentv1.SpawnTree {
		if n, err := strconv.Atoi(word); err == nil && n >= 1 && n <= len(c.spawn.trees) {
			return c.spawn.trees[n-1]
		}
		return nil
	}
	baseline, candidate := pick(words[0]), pick(words[1])
	switch {
	case len(c.spawn.trees) < 2:
		return wm.Notify("This chat has "+render.Count(len(c.spawn.trees), "finished spawn tree")+". Comparing needs two.", wm.Warn), true
	case baseline == nil || candidate == nil:
		return wm.Notify(fmt.Sprintf("Pick two trees from 1 to %d, where 1 is the last. %s", len(c.spawn.trees), usage), wm.Warn), true
	}
	t := c.kit.Env.Theme
	a, b := spawnTree(baseline.GetSubagents()), spawnTree(candidate.GetSubagents())
	ta, tb := spawnTotals(a), spawnTotals(b)
	took := func(tree *agentv1.SpawnTree) float64 {
		if tree.GetStarted() == nil || tree.GetFinished() == nil {
			return 0
		}
		return tree.GetFinished().AsTime().Sub(tree.GetStarted().AsTime()).Seconds()
	}
	metric := func(name string, x, y float64, format func(float64) string) string {
		delta := y - x
		sign := "+"
		if delta < 0 {
			sign = "-"
		}
		text := fmt.Sprintf("%-10s %12s %12s   %s%s", name, format(x), format(y), sign, format(max(delta, -delta)))
		switch {
		case delta > 0:
			return render.Warm(text, t)
		case delta < 0:
			return render.Good(text, t)
		}
		return text
	}
	number := func(f float64) string { return strconv.Itoa(int(f)) }
	tokens := func(f float64) string { return spawnTokens(int64(f)) }
	pane := func(title string, tree *agentv1.SpawnTree, nodes []*spawnNode) []string {
		lines := []string{"", render.Heading(title+": "+tree.GetLabel(), t), render.Dim(spawnSummary(spawnTotals(nodes)), t)}
		for _, n := range flatten(nodes) {
			lines = append(lines, strings.Repeat("  ", int(n.agent.GetDepth())+1)+statusGlyph(n.agent.GetStatus(), t)+" "+cmp.Or(oneLine(n.agent.GetGoal()), "Subagent"))
		}
		return lines
	}
	first, second := "Tree "+words[0], "Tree "+words[1]
	lines := []string{render.Dim("How the second tree differs from the first.", t), "",
		render.Heading(fmt.Sprintf("%-10s %12s %12s   %s", "", first, second, "Change"), t),
		metric("Subagents", float64(ta.descendants), float64(tb.descendants), number),
		metric("Depth", float64(ta.depth), float64(tb.depth), number),
		metric("Tool calls", float64(ta.tools), float64(tb.tools), number),
		metric("Time", took(baseline), took(candidate), spawnDuration),
		metric("Tokens", float64(ta.input+ta.output), float64(tb.input+tb.output), tokens)}
	lines = append(lines, pane(first, baseline, a)...)
	lines = append(lines, pane(second, candidate, b)...)
	page := cards.NewPage(c.kit.Env, cards.PageSpec{
		Title: "Spawn trees compared",
		Key:   "tui:agents:diff",
		Lines: func(int) []string { return lines },
	})
	return c.kit.WM.ShowOverlay(c.kit.Hub("tui:agents:diff", page)), true
}
