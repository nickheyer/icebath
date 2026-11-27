package app

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

const (
	// Newest lines of thinking, progress notes and tool steps each subagent keeps
	thinkingTail = 6
	notesTail    = 6
	toolsTail    = 8
	// Finished spawn trees a chat keeps for /replay
	treesKept = 10
)

// What a chat knows of one subagent: the fields its events carried, merged, and the newest of its thinking, notes and tool steps
type agentProgress struct {
	*agentv1.Subagent
	thinking, notes, tools []string
}

// Subagents of the fan-out the session runs now, in tree order, and the finished trees, newest first
type spawnState struct {
	agents []*agentProgress
	trees  []*agentv1.SpawnTree
}

// Moves one subagent by its lifecycle event; the tree finished, when the event completes a subagent of a tree already kept, so it is saved
// again
func (s *spawnState) apply(event *agentv1.Subagent) *agentv1.SpawnTree {
	id := cmp.Or(event.GetId(), fmt.Sprintf("%s:%d", event.GetDelegationId(), event.GetTaskIndex()))
	i := slices.IndexFunc(s.agents, func(a *agentProgress) bool { return a.GetId() == id })
	var kept *agentv1.SpawnTree
	var base *agentProgress
	switch {
	case i >= 0:
		base = s.agents[i]
	default:
		for _, tree := range s.trees {
			if k := slices.IndexFunc(tree.GetSubagents(), func(a *agentv1.Subagent) bool { return a.GetId() == id }); k >= 0 {
				kept, base = tree, &agentProgress{Subagent: tree.GetSubagents()[k]}
				break
			}
		}
	}
	kind := event.GetKind()
	if base == nil {
		if kind != "spawn_requested" && kind != "start" {
			return nil
		}
		base = &agentProgress{Subagent: &agentv1.Subagent{Id: id, Status: "running", TaskCount: 1, Started: cmp.Or(event.GetStarted(), timestamppb.Now())}}
	}
	next := merged(base.Subagent, event)
	next.Id = id
	text := strings.TrimSpace(event.GetText())
	switch kind {
	case "spawn_requested", "start":
		if !finalStatus(base.GetStatus()) {
			next.Status = map[bool]string{true: "running", false: "queued"}[kind == "start"]
		}
	case "thinking", "progress":
		if text == "" {
			return nil
		}
		next.Status = keptStatus(base.GetStatus())
		if kind == "thinking" {
			base.thinking = pushTail(base.thinking, text, thinkingTail)
		} else {
			base.notes = pushTail(base.notes, text, notesTail)
		}
	case "tool":
		next.Status = keptStatus(base.GetStatus())
		base.tools = pushTail(base.tools, toolLine(cmp.Or(event.GetTool(), "delegate_task"), cmp.Or(event.GetPreview(), text)), toolsTail)
	case "complete":
		next.Status = statusOf(event.GetStatus(), "completed")
		next.Summary = cmp.Or(event.GetSummary(), text, base.GetSummary())
		if event.GetDurationSeconds() == 0 {
			next.DurationSeconds = base.GetDurationSeconds()
		}
	}
	base.Subagent = next
	if kept != nil {
		k := slices.IndexFunc(kept.GetSubagents(), func(a *agentv1.Subagent) bool { return a.GetId() == id })
		kept.Subagents[k] = next
		if kind == "complete" {
			return kept
		}
		return nil
	}
	if i < 0 {
		s.agents = append(s.agents, base)
		slices.SortStableFunc(s.agents, func(a, b *agentProgress) int {
			return cmp.Or(cmp.Compare(a.GetDepth(), b.GetDepth()), cmp.Compare(a.GetTaskIndex(), b.GetTaskIndex()))
		})
	}
	return nil
}

// The fields of the event over what was known, a field the event leaves at its zero kept as it was
func merged(base, event *agentv1.Subagent) *agentv1.Subagent {
	next := proto.Clone(base).(*agentv1.Subagent)
	if event.GetDelegationId() != "" {
		next.DelegationId = event.GetDelegationId()
	}
	if event.GetParentId() != "" {
		next.ParentId = event.GetParentId()
	}
	if event.GetSessionId() != "" {
		next.SessionId = event.GetSessionId()
	}
	if event.GetDepth() != 0 {
		next.Depth = event.GetDepth()
	}
	if event.GetTaskIndex() != 0 {
		next.TaskIndex = event.GetTaskIndex()
	}
	if event.GetTaskCount() != 0 {
		next.TaskCount = event.GetTaskCount()
	}
	if event.GetGoal() != "" {
		next.Goal = event.GetGoal()
	}
	if event.GetModel() != "" {
		next.Model = event.GetModel()
	}
	if event.GetRole() != "" {
		next.Role = event.GetRole()
	}
	if event.GetToolCount() != 0 {
		next.ToolCount = event.GetToolCount()
	}
	if event.GetDurationSeconds() != 0 {
		next.DurationSeconds = event.GetDurationSeconds()
	}
	for _, pair := range []struct {
		to   *int64
		from int64
	}{{&next.InputTokens, event.GetInputTokens()}, {&next.OutputTokens, event.GetOutputTokens()},
		{&next.ReasoningTokens, event.GetReasoningTokens()}} {
		if pair.from != 0 {
			*pair.to = pair.from
		}
	}
	if event.GetApiCalls() != 0 {
		next.ApiCalls = event.GetApiCalls()
	}
	if event.GetCost() != 0 {
		next.Cost = event.GetCost()
	}
	if event.GetFilesRead() != nil {
		next.FilesRead = event.GetFilesRead()
	}
	if event.GetFilesWritten() != nil {
		next.FilesWritten = event.GetFilesWritten()
	}
	if event.GetOutputTail() != nil {
		next.OutputTail = event.GetOutputTail()
	}
	if event.GetStarted() != nil && next.Started == nil {
		next.Started = event.GetStarted()
	}
	if event.GetTranscript() != "" {
		next.Transcript = event.GetTranscript()
	}
	next.Kind = event.GetKind()
	return next
}

// The fan-out of the turn that ended kept as a finished tree, newest first; nil when it started no subagent
func (s *spawnState) finish(session string) *agentv1.SpawnTree {
	if len(s.agents) == 0 {
		return nil
	}
	tree := &agentv1.SpawnTree{SessionId: session, Finished: timestamppb.Now(), Label: spawnLabel(s.agents)}
	for _, a := range s.agents {
		if tree.Started == nil || a.GetStarted() != nil && a.GetStarted().AsTime().Before(tree.Started.AsTime()) {
			tree.Started = a.GetStarted()
		}
		tree.Subagents = append(tree.Subagents, a.Subagent)
	}
	s.agents = nil
	s.trees = append([]*agentv1.SpawnTree{tree}, s.trees...)[:min(len(s.trees)+1, treesKept)]
	return tree
}

// Label of a fan-out: its first two goals at the top, else how many agents it ran
func spawnLabel(agents []*agentProgress) string {
	known := map[string]bool{}
	for _, a := range agents {
		known[a.GetId()] = true
	}
	var top []string
	for _, a := range agents {
		if len(top) < 2 && (a.GetParentId() == "" || !known[a.GetParentId()]) {
			top = append(top, cmp.Or(a.GetGoal(), "subagent"))
		}
	}
	if len(top) > 0 {
		return strings.Join(top, " · ")
	}
	return fmt.Sprintf("%d subagents", len(agents))
}

func pushTail(list []string, text string, keep int) []string {
	if len(list) > 0 && list[len(list)-1] == text {
		return list
	}
	list = append(list, text)
	return list[max(0, len(list)-keep):]
}

// Line of a subagent's tool step: the tool and what it was given
func toolLine(tool, preview string) string {
	if preview = strings.Join(strings.Fields(preview), " "); preview == "" {
		return tool
	}
	return tool + "  " + preview
}

var subagentStatuses = []string{"completed", "error", "failed", "interrupted", "queued", "running", "timeout"}

func statusOf(status, fallback string) string {
	if lowered := strings.ToLower(status); slices.Contains(subagentStatuses, lowered) {
		return lowered
	}
	return fallback
}

func finalStatus(status string) bool {
	return status != "" && status != "running" && status != "queued"
}

func keptStatus(status string) string {
	if finalStatus(status) {
		return status
	}
	return "running"
}

func inFlight(status string) bool { return status == "running" || status == "queued" }

// One subagent with the subagents it started and the counts of its subtree
type spawnNode struct {
	agent    *agentv1.Subagent
	children []*spawnNode
	agg      spawnAggregate
}

// Counts rolled up over a subtree
type spawnAggregate struct {
	active, descendants, depth, tools int
	input, output                     int64
	duration                          float64
}

func (a *spawnAggregate) add(child spawnAggregate) {
	a.tools += child.tools
	a.duration += child.duration
	a.descendants += child.descendants + 1
	a.active += child.active
	a.depth = max(a.depth, child.depth+1)
	a.input += child.input
	a.output += child.output
}

// Tree of the subagents by the parent each names, those whose parent is not among them at the top
func spawnTree(agents []*agentv1.Subagent) []*spawnNode {
	known := map[string]bool{}
	for _, a := range agents {
		known[a.GetId()] = true
	}
	byParent := map[string][]*agentv1.Subagent{}
	for _, a := range agents {
		parent := ""
		if known[a.GetParentId()] {
			parent = a.GetParentId()
		}
		byParent[parent] = append(byParent[parent], a)
	}
	for _, bucket := range byParent {
		slices.SortStableFunc(bucket, func(x, y *agentv1.Subagent) int {
			return cmp.Or(cmp.Compare(x.GetDepth(), y.GetDepth()), cmp.Compare(x.GetTaskIndex(), y.GetTaskIndex()))
		})
	}
	path := map[string]bool{}
	var build func(a *agentv1.Subagent) *spawnNode
	build = func(a *agentv1.Subagent) *spawnNode {
		path[a.GetId()] = true
		node := &spawnNode{agent: a}
		node.agg = spawnAggregate{tools: int(a.GetToolCount()), duration: a.GetDurationSeconds(), input: a.GetInputTokens(), output: a.GetOutputTokens()}
		if inFlight(a.GetStatus()) {
			node.agg.active = 1
		}
		for _, kid := range byParent[a.GetId()] {
			if !path[kid.GetId()] {
				child := build(kid)
				node.children = append(node.children, child)
				node.agg.add(child.agg)
			}
		}
		delete(path, a.GetId())
		return node
	}
	var out []*spawnNode
	for _, a := range byParent[""] {
		out = append(out, build(a))
	}
	return out
}

func spawnTotals(tree []*spawnNode) spawnAggregate {
	var totals spawnAggregate
	for _, node := range tree {
		totals.add(node.agg)
	}
	return totals
}

// Nodes in the order the tree shows them, each after its parent
func flatten(tree []*spawnNode) []*spawnNode {
	var out []*spawnNode
	var walk func(nodes []*spawnNode)
	walk = func(nodes []*spawnNode) {
		for _, n := range nodes {
			out = append(out, n)
			walk(n.children)
		}
	}
	walk(tree)
	return out
}

// Agents at each level of the tree, the top level first
func widths(tree []*spawnNode) []int {
	var out []int
	var walk func(nodes []*spawnNode, depth int)
	walk = func(nodes []*spawnNode, depth int) {
		if len(nodes) == 0 {
			return
		}
		for len(out) <= depth {
			out = append(out, 0)
		}
		out[depth] += len(nodes)
		for _, n := range nodes {
			walk(n.children, depth+1)
		}
	}
	walk(tree, 0)
	return out
}

// Bars of the counts, one per level
func sparkline(values []int) string {
	if len(values) < 2 {
		return ""
	}
	bars := []rune("▁▂▃▄▅▆▇█")
	peak := slices.Max(values)
	var out strings.Builder
	for _, v := range values {
		out.WriteRune(bars[min(len(bars)-1, int(math.Round(float64(v)/float64(max(peak, 1))*float64(len(bars)-1))))])
	}
	return out.String()
}

func spawnTokens(n int64) string {
	switch {
	case n <= 0:
		return "0"
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 10_000:
		return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "k"
	}
	return strconv.FormatInt(int64(math.Round(float64(n)/1000)), 10) + "k"
}

// Seconds as 42s, 3m or 3m 5s
func spawnDuration(seconds float64) string {
	if seconds < 60 {
		return strconv.Itoa(int(math.Round(max(0, seconds)))) + "s"
	}
	m := math.Floor(seconds / 60)
	s := math.Round(seconds - m*60)
	if s == 0 {
		return strconv.Itoa(int(m)) + "m"
	}
	return strconv.Itoa(int(m)) + "m " + strconv.Itoa(int(s)) + "s"
}

// Seconds the agent ran: its final duration, or the time since it started while it runs
func elapsed(a *agentv1.Subagent, now time.Time) (float64, bool) {
	if a.GetDurationSeconds() > 0 {
		return a.GetDurationSeconds(), true
	}
	if a.GetStarted() != nil && inFlight(a.GetStatus()) {
		return max(0, now.Sub(a.GetStarted().AsTime()).Seconds()), true
	}
	return 0, false
}

// Read-out of the fan-out for the status line: the pause, the depth and the widest level against their caps with those running beyond it,
// marked once a cap is reached; empty with no subagent and no pause
func spawnHUD(agents []*agentv1.Subagent, limits *agentv1.SubagentsResponse) string {
	paused := limits.GetPaused()
	tree := spawnTree(agents)
	totals := spawnTotals(tree)
	if totals.descendants == 0 && !paused {
		return ""
	}
	maxDepth, maxWidth := int(limits.GetMaxSpawnDepth()), int(limits.GetMaxConcurrentChildren())
	depth, widest := max(0, totals.depth), 0
	for _, width := range widths(tree) {
		widest = max(widest, width)
	}
	var pieces []string
	if paused {
		pieces = append(pieces, "paused")
	}
	full := false
	if totals.descendants > 0 {
		label := "d" + strconv.Itoa(depth)
		if maxDepth > 0 {
			label += "/" + strconv.Itoa(maxDepth)
			full = depth >= maxDepth
		}
		pieces = append(pieces, label)
		if totals.active > 0 {
			label = "⚡" + strconv.Itoa(widest)
			if maxWidth > 0 {
				label += "/" + strconv.Itoa(maxWidth)
				full = full || widest >= maxWidth
			}
			if extra := totals.active - widest; extra > 0 {
				label += "+" + strconv.Itoa(extra)
			}
			pieces = append(pieces, label)
		}
	}
	text := strings.Join(pieces, " ")
	if full {
		text = "⚠ " + text
	}
	return text
}

// Reads the session's delegation caps and pause for the status line, as a fan-out starts
func (c *chat) delegationLimits() tea.Cmd {
	if c.id == "" || c.limiting {
		return nil
	}
	c.limiting = true
	kit, session := c.kit, c.id
	return tui.Async(func() func() tea.Cmd {
		response, err := c.desk.agents.Subagents(kit.Env.Ctx, &agentv1.SubagentsRequest{SessionId: session})
		return func() tea.Cmd {
			c.limiting = false
			if err == nil {
				c.limits = response
			}
			return nil
		}
	})
}

// Totals as one line: depth, agents, tools, time, tokens and how many run
func spawnSummary(totals spawnAggregate) string {
	pieces := []string{"d" + strconv.Itoa(max(0, totals.depth)), render.Count(totals.descendants, "agent")}
	if totals.tools > 0 {
		pieces = append(pieces, render.Count(totals.tools, "tool"))
	}
	if totals.duration > 0 {
		pieces = append(pieces, spawnDuration(totals.duration))
	}
	if tokens := totals.input + totals.output; tokens > 0 {
		pieces = append(pieces, spawnTokens(tokens)+" tok")
	}
	if totals.active > 0 {
		pieces = append(pieces, "⚡"+strconv.Itoa(totals.active))
	}
	return strings.Join(pieces, " · ")
}

// Mark of a status and the words it is shown with
func statusGlyph(status string, t theme.Theme) string {
	switch status {
	case "running":
		return render.Cool(t.Glyphs.Live, t)
	case "queued":
		return render.Dim(t.Glyphs.Hollow, t)
	case "completed":
		return render.Good(t.Glyphs.Done, t)
	case "interrupted", "timeout":
		return render.Warm(t.Glyphs.Warn, t)
	case "failed", "error":
		return render.Bad(t.Glyphs.Fail, t)
	}
	return render.Dim("?", t)
}

func (s *spawnState) live() []*agentv1.Subagent {
	out := make([]*agentv1.Subagent, len(s.agents))
	for i, a := range s.agents {
		out[i] = a.Subagent
	}
	return out
}

// Rows of the fan-out running now in the transcript: its headline, then each subagent under its parent with its newest step
func (c *chat) spawnRows(w int) []row {
	t := c.kit.Env.Theme
	agents := c.spawn.live()
	if len(agents) == 0 {
		return nil
	}
	tree := spawnTree(agents)
	totals := spawnTotals(tree)
	head := render.Heading(fmt.Sprintf("Spawn tree (%d)", totals.descendants), t) + render.Dim("  "+spawnSummary(totals), t)
	if spark := sparkline(widths(tree)); spark != "" {
		head += render.Dim("  "+spark, t)
	}
	open := c.open("spawn", true)
	mark := t.Glyphs.Bullet
	if open {
		mark = t.Glyphs.Down
	}
	rows := []row{{}, {text: ansi.Truncate(render.Dim(mark, t)+" "+head+render.Dim("  /agents to open", t), w, t.Glyphs.Ellipsis), fold: "spawn"}}
	if !open {
		return rows
	}
	now := time.Now()
	for _, node := range flatten(tree) {
		a := node.agent
		line := strings.Repeat("  ", int(a.GetDepth())+1) + statusGlyph(a.GetStatus(), t) + " " + cmp.Or(a.GetGoal(), "subagent")
		meta := []string{a.GetStatus()}
		if a.GetToolCount() > 0 {
			meta = append(meta, render.Count(int(a.GetToolCount()), "tool"))
		}
		if seconds, ok := elapsed(a, now); ok {
			meta = append(meta, spawnDuration(seconds))
		}
		rows = append(rows, row{text: ansi.Truncate(line+render.Dim("  "+strings.Join(meta, " · "), t), w, t.Glyphs.Ellipsis)})
		if detail := c.spawn.latest(a.GetId()); detail != "" && inFlight(a.GetStatus()) {
			rows = append(rows, row{text: ansi.Truncate(render.Dim(strings.Repeat("  ", int(a.GetDepth())+2)+"↳ "+detail, t), w, t.Glyphs.Ellipsis)})
		}
	}
	return rows
}

// What the subagent did last: its newest note, output or tool step
func (s *spawnState) latest(id string) string {
	for _, a := range s.agents {
		if a.GetId() != id {
			continue
		}
		switch tail := a.GetOutputTail(); {
		case len(a.notes) > 0:
			return a.notes[len(a.notes)-1]
		case len(tail) > 0 && tail[len(tail)-1].GetPreview() != "":
			return tail[len(tail)-1].GetPreview()
		case len(a.tools) > 0:
			return a.tools[len(a.tools)-1]
		}
		return "starting"
	}
	return ""
}

// Dock rows of the subagents working now: a head line, then each with how long it runs and what it does; one line when folded
func (c *chat) agentDock(w, limit int) []string {
	t := c.kit.Env.Theme
	now := time.Now()
	var working []*agentv1.Subagent
	for _, a := range c.spawn.live() {
		if inFlight(a.GetStatus()) {
			working = append(working, a)
		}
	}
	if len(working) == 0 {
		return nil
	}
	cols := max(w-2, 8)
	if c.folded {
		line := fmt.Sprintf("▸ %s working", render.Count(len(working), "agent"))
		if detail := c.spawn.latest(working[0].GetId()); detail != "" {
			line += " · " + detail
		}
		return []string{"  " + ansi.Truncate(render.Cool(line, t), cols, t.Glyphs.Ellipsis)}
	}
	shown := working[:min(len(working), max(1, limit))]
	head := fmt.Sprintf("▾ %s working", render.Count(len(working), "agent"))
	if hidden := len(working) - len(shown); hidden > 0 {
		head += fmt.Sprintf(" · %d more", hidden)
	}
	out := []string{"  " + ansi.Truncate(render.Cool(head+" · /agents", t), cols, t.Glyphs.Ellipsis)}
	for _, a := range shown {
		took := ""
		if seconds, ok := elapsed(a, now); ok {
			took = " " + render.Dim(spawnDuration(seconds), t)
		}
		out = append(out, "  "+ansi.Truncate(statusGlyph(a.GetStatus(), t)+" "+cmp.Or(a.GetGoal(), "agent")+took, cols, t.Glyphs.Ellipsis),
			"  "+ansi.Truncate(render.Dim("  ↳ "+c.spawn.latest(a.GetId()), t), cols, t.Glyphs.Ellipsis))
	}
	return out
}
