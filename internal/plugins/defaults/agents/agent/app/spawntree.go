package app

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

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
	{ID: "agent.spawntree.tail", Context: spawnContext, Label: "tail", Doc: "follow the live transcript of the highlighted subagent", Default: []string{"enter", "t"}},
	{ID: "agent.spawntree.detail", Context: spawnContext, Label: "detail", Doc: "everything known of the highlighted subagent", Default: []string{"d"}},
	{ID: "agent.spawntree.steer", Context: spawnContext, Label: "steer", Doc: "send guidance the highlighted subagent reads after its next tool call", Default: []string{"e"}},
	{ID: "agent.spawntree.kill", Context: spawnContext, Label: "stop", Doc: "stop the highlighted subagent", Default: []string{"x"}},
	{ID: "agent.spawntree.subtree", Context: spawnContext, Label: "stop subtree", Doc: "stop the highlighted subagent and every subagent under it", Default: []string{"X"}},
	{ID: "agent.spawntree.pause", Context: spawnContext, Label: "pause", Doc: "hold new subagents back from starting, or let them start again", Default: []string{"p"}},
	{ID: "agent.spawntree.sort", Context: spawnContext, Label: "sort", Doc: "order the tree by spawn order, status, the slowest or the busiest", Default: []string{"s"}},
	{ID: "agent.spawntree.filter", Context: spawnContext, Label: "filter", Doc: "show every subagent, those running, those that failed or the leaves", Default: []string{"f"}},
	{ID: "agent.spawntree.older", Context: spawnContext, Label: "older", Doc: "show the spawn tree that finished before this one", Default: []string{"["}},
	{ID: "agent.spawntree.newer", Context: spawnContext, Label: "newer", Doc: "show the spawn tree that finished after this one, then the live turn", Default: []string{"]"}},
}

var (
	spawnSorts   = []string{"spawn order", "status", "slowest", "busiest"}
	spawnFilters = []string{"all", "running", "failed", "leaves"}
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
	switch spawnFilters[s.filter] {
	case "running":
		nodes = slices.DeleteFunc(nodes, func(n *spawnNode) bool { return !inFlight(n.agent.GetStatus()) })
	case "failed":
		nodes = slices.DeleteFunc(nodes, func(n *spawnNode) bool {
			status := n.agent.GetStatus()
			return status != "failed" && status != "error" && status != "timeout" && status != "interrupted"
		})
	case "leaves":
		nodes = slices.DeleteFunc(nodes, func(n *spawnNode) bool { return len(n.children) > 0 })
	}
	switch spawnSorts[s.sort] {
	case "status":
		slices.SortStableFunc(nodes, func(x, y *spawnNode) int {
			return cmp.Compare(rankOf(x.agent.GetStatus()), rankOf(y.agent.GetStatus()))
		})
	case "slowest":
		slices.SortStableFunc(nodes, func(x, y *spawnNode) int {
			a, _ := elapsed(x.agent, now)
			b, _ := elapsed(y.agent, now)
			return cmp.Compare(b, a)
		})
	case "busiest":
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

// Opens the spawn tree of the chat: the live turn, or the kth newest finished tree, or a tree read from disk
func (c *chat) openTree(index int, loaded *agentv1.SpawnTree) tea.Cmd {
	kit := c.kit
	s := &spawnScreen{chat: c, index: index, loaded: loaded}
	t := kit.Env.Theme
	s.list = cards.NewList(kit.Env, cards.ListSpec{
		Title:   "Spawn tree",
		Glyph:   t.Glyphs.Agent,
		Key:     "tui:agents",
		Context: spawnContext,
		Primary: "agent.spawntree.tail",
		Columns: []render.Column{{Title: "Agent", Flex: 3}, {Title: "Status"}, {Title: "Model", Flex: 1}, {Title: "Tools"}, {Title: "Time"}, {Title: "Tokens"}},
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
				tools := strconv.Itoa(int(a.GetToolCount()))
				if n.agg.tools != int(a.GetToolCount()) {
					tools += render.Dim(fmt.Sprintf(" (%d)", n.agg.tools), t)
				}
				goal := strings.Repeat("  ", int(a.GetDepth())) + statusGlyph(a.GetStatus(), t) + " " + cmp.Or(a.GetGoal(), "subagent")
				rows[i] = []string{goal, a.GetStatus(), cmp.Or(a.GetModel(), render.Dim("inherit", t)), tools, took,
					spawnTokens(a.GetInputTokens() + a.GetOutputTokens())}
			}
			return rows
		},
		Detail: func(i int) []string {
			if i < 0 || i >= len(s.nodes) {
				return nil
			}
			return s.detail(s.nodes[i], false)
		},
		Empty:  func() string { return "No subagents this turn. Trigger delegate_task to populate the tree." },
		Status: s.trailer,
		Banner: s.banner,
		Free: map[string]bool{"agent.spawntree.pause": true, "agent.spawntree.sort": true, "agent.spawntree.filter": true, "agent.spawntree.older": true,
			"agent.spawntree.newer": true},
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if s.replay() {
				return []wm.Hint{{Action: "agent.spawntree.tail", Label: "transcript"}, {Action: "agent.spawntree.detail"}, {Action: "agent.spawntree.older"},
					{Action: "agent.spawntree.newer"}}
			}
			pause := "pause"
			if s.status.GetPaused() {
				pause = "resume"
			}
			hints := []wm.Hint{{Action: "agent.spawntree.pause", Label: pause}, {Action: "agent.spawntree.sort"}, {Action: "agent.spawntree.filter"}}
			if i >= 0 {
				hints = append([]wm.Hint{{Action: "agent.spawntree.tail"}, {Action: "agent.spawntree.detail"}, {Action: "agent.spawntree.steer"},
					{Action: "agent.spawntree.kill"}, {Action: "agent.spawntree.subtree"}}, hints...)
			}
			return hints
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
			"agent.spawntree.sort":   func(*cards.List, int) tea.Cmd { s.sort = (s.sort + 1) % len(spawnSorts); s.refresh(); return nil },
			"agent.spawntree.filter": func(*cards.List, int) tea.Cmd { s.filter = (s.filter + 1) % len(spawnFilters); s.refresh(); return nil },
			"agent.spawntree.older":  func(*cards.List, int) tea.Cmd { return s.step(1) },
			"agent.spawntree.newer":  func(*cards.List, int) tea.Cmd { return s.step(-1) },
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

// Runs the control on the live turn; a finished tree takes none
func (s *spawnScreen) live(action func() tea.Cmd) tea.Cmd {
	if s.replay() {
		return s.hub.Say("replay mode — controls disabled", cards.NoteFailed)
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

func (s *spawnScreen) kill(agents []*agentv1.Subagent) tea.Cmd {
	if len(agents) == 0 {
		return s.hub.Say("nothing running there to stop", cards.NoteFailed)
	}
	label := "stopping " + cmp.Or(agents[0].GetGoal(), agents[0].GetId())
	if len(agents) > 1 {
		label = fmt.Sprintf("stopping subtree · %d nodes", len(agents))
	}
	s.hub.Busy(label)
	kit := s.chat.kit
	return tui.Async(func() func() tea.Cmd {
		var failed error
		missing := ""
		for _, a := range agents {
			response, err := s.chat.desk.agents.Interrupt(kit.Env.Ctx, &agentv1.InterruptRequest{SessionId: s.chat.id, SubagentId: a.GetId()})
			switch {
			case err != nil:
				failed = err
			case !response.GetFound():
				missing = a.GetId()
			}
		}
		return func() tea.Cmd {
			switch {
			case failed != nil:
				return s.hub.Fail("stop failed: " + failed.Error())
			case missing != "" && len(agents) == 1:
				return s.hub.Fail("not found: " + missing)
			}
			return s.hub.Done(strings.Replace(label, "stopping", "stopped", 1))
		}
	})
}

func (s *spawnScreen) pause() tea.Cmd {
	kit := s.chat.kit
	paused := !s.status.GetPaused()
	return tui.Async(func() func() tea.Cmd {
		response, err := s.chat.desk.agents.Pause(kit.Env.Ctx, &agentv1.PauseRequest{Paused: paused})
		return func() tea.Cmd {
			if err != nil {
				return s.hub.Fail("pause failed")
			}
			if s.status == nil {
				s.status = &agentv1.SubagentsResponse{}
			}
			s.status.Paused = response.GetPaused()
			s.chat.limits = s.status
			s.refresh()
			if response.GetPaused() {
				return s.hub.Done("spawning paused")
			}
			return s.hub.Done("spawning resumed")
		}
	})
}

// Asks for the guidance to steer the subagent with and sends it
func (s *spawnScreen) steer(a *agentv1.Subagent) tea.Cmd {
	kit := s.chat.kit
	var form *cards.Form
	form = cards.NewForm(kit.Env, cards.FormSpec{
		Title:  "Steer " + cmp.Or(a.GetGoal(), a.GetId()),
		Intro:  []string{"The subagent reads this after its next tool call, without being interrupted."},
		Submit: "Send",
		Fields: []cards.Field{{ID: "text", Label: "Guidance", Required: true}},
		Done: func(_ *cards.Form, values map[string]string) tea.Cmd {
			text := strings.TrimSpace(values["text"])
			return tea.Batch(s.hub.Close(form), tui.Async(func() func() tea.Cmd {
				response, err := s.chat.desk.agents.Steer(kit.Env.Ctx, &agentv1.SteerRequest{SessionId: s.chat.id, SubagentId: a.GetId(), Text: text})
				return func() tea.Cmd {
					switch {
					case err != nil:
						return s.hub.Fail(err.Error())
					case !response.GetQueued():
						return s.hub.Fail(cmp.Or(response.GetReason(), "steer rejected"))
					}
					return s.hub.Done("steer queued — arrives after next tool call")
				}
			}))
		},
	})
	return s.hub.Push(form)
}

// Steps through the finished trees: older away from the live turn, newer back to it
func (s *spawnScreen) step(delta int) tea.Cmd {
	if s.loaded != nil {
		return s.hub.Say("a tree read from disk has no neighbours", cards.NoteFailed)
	}
	next := s.index + delta
	if next < 0 || next > len(s.chat.spawn.trees) {
		return nil
	}
	s.index = next
	s.refresh()
	if next == 0 {
		return s.hub.Say("live turn", cards.NoteDone)
	}
	return s.hub.Say(fmt.Sprintf("replay · %d/%d", next, len(s.chat.spawn.trees)), cards.NoteDone)
}

// Trailer of the frame: what is shown, the caps and the pause, the sort and the filter
func (s *spawnScreen) trailer() string {
	parts := []string{}
	if snap := s.snapshot(); snap != nil {
		label := "replay"
		if s.index > 0 {
			label = fmt.Sprintf("replay %d/%d", s.index, len(s.chat.spawn.trees))
		}
		parts = append(parts, label+" · "+snap.GetLabel())
	} else {
		parts = append(parts, "live turn")
		if s.status != nil {
			parts = append(parts, fmt.Sprintf("caps d%d/%d", s.status.GetMaxSpawnDepth(), s.status.GetMaxConcurrentChildren()))
			if s.status.GetPaused() {
				parts = append(parts, "⏸ paused")
			}
		}
	}
	if tree := spawnTree(s.agents()); len(tree) > 0 {
		parts = append(parts, spawnSummary(spawnTotals(tree)))
	}
	parts = append(parts, "sort "+spawnSorts[s.sort], "filter "+spawnFilters[s.filter])
	return strings.Join(parts, " · ")
}

// Line above the tree: the session's failed background delegations of the last day, for the live turn
func (s *spawnScreen) banner() string {
	if s.replay() || len(s.status.GetFailed()) == 0 {
		return ""
	}
	failed := s.status.GetFailed()
	first := failed[0]
	return fmt.Sprintf("Failed delegations · %d · newest: %s · %s · %s", len(failed), cmp.Or(first.GetGoal(), "subagent"), first.GetStatus(),
		cmp.Or(first.GetError(), "-"))
}

// What is known of the subagent; in full for its own page, else the short form the detail pane shows
func (s *spawnScreen) detail(n *spawnNode, full bool) []string {
	t := s.chat.kit.Env.Theme
	a := n.agent
	lines := []string{render.Heading(cmp.Or(a.GetGoal(), "subagent"), t)}
	meta := []string{"depth " + strconv.Itoa(int(a.GetDepth())), "model " + cmp.Or(a.GetModel(), "inherit")}
	if seconds, ok := elapsed(a, time.Now()); ok {
		meta = append(meta, "elapsed "+spawnDuration(seconds))
	}
	if a.GetApiCalls() > 0 {
		meta = append(meta, fmt.Sprintf("%d api calls", a.GetApiCalls()))
	}
	lines = append(lines, render.Dim(strings.Join(meta, " · "), t))
	tokens := fmt.Sprintf("tokens %s in · %s out", spawnTokens(a.GetInputTokens()), spawnTokens(a.GetOutputTokens()))
	if a.GetReasoningTokens() > 0 {
		tokens += " · " + spawnTokens(a.GetReasoningTokens()) + " reasoning"
	}
	if a.GetCost() > 0 {
		tokens += " · " + render.Money(a.GetCost())
	}
	lines = append(lines, render.Dim(tokens, t), render.Dim(fmt.Sprintf("tools %d (subtree %d)", a.GetToolCount(), n.agg.tools), t))
	if n.agg.descendants > 0 {
		lines = append(lines, render.Dim(fmt.Sprintf("subtree %s · d%d · ⚡%d · %s tok", render.Count(n.agg.descendants, "agent"), n.agg.depth, n.agg.active,
			spawnTokens(n.agg.input+n.agg.output)), t))
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
			lines = append(lines, render.Dim(fmt.Sprintf("  …+%d more", len(paths)-limit), t))
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
			section("Thinking", progress.thinking)
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
		lines = append(lines, "", render.Heading("Summary", t))
		for _, line := range strings.Split(summary, "\n") {
			lines = append(lines, "  "+line)
		}
	}
	return lines
}

// Page of everything known of the subagent
func (s *spawnScreen) page(n *spawnNode) tea.Cmd {
	page := cards.NewPage(s.chat.kit.Env, cards.PageSpec{
		Title: cmp.Or(n.agent.GetGoal(), n.agent.GetId()),
		Key:   "tui:agents:" + n.agent.GetId(),
		Lines: func(w int) []string {
			var out []string
			for _, line := range s.detail(n, true) {
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
				text = err.Error()
			case !response.GetAvailable():
				text = "This subagent left no transcript."
			default:
				text = response.GetText()
				if response.GetTruncated() {
					text = "[…older output not shown]\n" + text
				}
			}
			return nil
		})
	}
	page = cards.NewPage(kit.Env, cards.PageSpec{
		Title:  "Transcript · " + cmp.Or(a.GetGoal(), a.GetId()),
		Key:    "tui:agents:tail:" + a.GetId(),
		Follow: true,
		Lines: func(w int) []string {
			return strings.Split(render.Wrap(text, max(w, 1)), "\n")
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
		return c.savedTrees(), true
	case strings.HasPrefix(lower, "load"):
		path := strings.TrimSpace(raw[len("load"):])
		if path == "" {
			return wm.Notify("usage: /replay load <path>", wm.Warn), true
		}
		return c.loadTree(path), true
	}
	if len(c.spawn.trees) == 0 {
		return wm.Notify("no completed spawn trees this session · try /replay list", wm.Info), true
	}
	index := 1
	if raw != "" && lower != "last" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > len(c.spawn.trees) {
			return wm.Notify(fmt.Sprintf("replay: index out of range 1..%d · use /replay list for disk", len(c.spawn.trees)), wm.Warn), true
		}
		index = n
	}
	return c.openTree(index, nil), true
}

// The spawn trees kept on disk for the session, newest first, each opened on enter
func (c *chat) savedTrees() tea.Cmd {
	if c.id == "" {
		return wm.Notify("no archived spawn trees on disk for this session", wm.Info)
	}
	kit := c.kit
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.agents.Trees(kit.Env.Ctx, &agentv1.TreesRequest{SessionId: c.id, Limit: 30})
		return func() tea.Cmd {
			if err != nil {
				return wm.Notify(err.Error(), wm.Error)
			}
			trees := response.GetTrees()
			if len(trees) == 0 {
				return wm.Notify("no archived spawn trees on disk for this session", wm.Info)
			}
			var hub *cards.Hub
			list := cards.NewList(kit.Env, cards.ListSpec{
				Title:   "Archived spawn trees",
				Glyph:   kit.Env.Theme.Glyphs.Agent,
				Key:     "tui:agents:saved",
				Context: cards.HubContext,
				Primary: "hub.use",
				Columns: []render.Column{{Title: "finished"}, {Title: "agents"}, {Title: "label", Flex: 2}, {Title: "path", Flex: 1}},
				Rows: func() [][]string {
					rows := make([][]string, len(trees))
					for i, tree := range trees {
						count := ""
						if i < len(response.GetCounts()) {
							count = fmt.Sprintf("%d subagents", response.GetCounts()[i])
						}
						rows[i] = []string{tree.GetFinished().AsTime().Local().Format("Jan 2 15:04"), count, tree.GetLabel(), render.Dim(tree.GetPath(), kit.Env.Theme)}
					}
					return rows
				},
				Actions: map[string]func(*cards.List, int) tea.Cmd{
					"hub.use": func(_ *cards.List, i int) tea.Cmd {
						return tea.Batch(kit.WM.DismissCard(hub), c.loadTree(trees[i].GetPath()))
					},
				},
			})
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
				return wm.Notify(err.Error(), wm.Error)
			case len(response.GetTree().GetSubagents()) == 0:
				return wm.Notify("snapshot empty or unreadable", wm.Warn)
			}
			return c.openTree(0, response.GetTree())
		}
	})
}

// /replay-diff: two finished trees of this chat side by side, picked by their places
func (c *chat) replayDiff(argument string) (tea.Cmd, bool) {
	words := strings.Fields(argument)
	if len(words) != 2 {
		return wm.Notify("usage: /replay-diff <a> <b>  (e.g. /replay-diff 1 2 for last two)", wm.Warn), true
	}
	pick := func(word string) *agentv1.SpawnTree {
		if n, err := strconv.Atoi(word); err == nil && n >= 1 && n <= len(c.spawn.trees) {
			return c.spawn.trees[n-1]
		}
		return nil
	}
	baseline, candidate := pick(words[0]), pick(words[1])
	if baseline == nil || candidate == nil {
		return wm.Notify(fmt.Sprintf("replay-diff: could not resolve indices · history has %d entries", len(c.spawn.trees)), wm.Warn), true
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
		text := fmt.Sprintf("%-10s %12s %12s   Δ %s%s", name, format(x), format(y), sign, format(max(delta, -delta)))
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
		lines := []string{"", render.Heading(title+" · "+tree.GetLabel(), t), render.Dim(spawnSummary(spawnTotals(nodes)), t)}
		for _, n := range flatten(nodes) {
			lines = append(lines, strings.Repeat("  ", int(n.agent.GetDepth())+1)+statusGlyph(n.agent.GetStatus(), t)+" "+cmp.Or(n.agent.GetGoal(), "subagent"))
		}
		return lines
	}
	lines := []string{render.Heading(fmt.Sprintf("%-10s %12s %12s", "", "A · baseline", "B · candidate"), t),
		metric("agents", float64(ta.descendants), float64(tb.descendants), number),
		metric("depth", float64(ta.depth), float64(tb.depth), number),
		metric("tools", float64(ta.tools), float64(tb.tools), number),
		metric("duration", took(baseline), took(candidate), spawnDuration),
		metric("tokens", float64(ta.input+ta.output), float64(tb.input+tb.output), tokens)}
	lines = append(lines, pane("A · baseline", baseline, a)...)
	lines = append(lines, pane("B · candidate", candidate, b)...)
	page := cards.NewPage(c.kit.Env, cards.PageSpec{
		Title: "Replay diff",
		Key:   "tui:agents:diff",
		Lines: func(int) []string { return lines },
	})
	return c.kit.WM.ShowOverlay(c.kit.Hub("tui:agents:diff", page)), true
}
