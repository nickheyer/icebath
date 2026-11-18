package tui

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap contexts of the spawn tree window and of the list of saved trees
const (
	spawnTreeContext  = "tui.spawntree"
	spawnTreesContext = "tui.spawntrees"
)

// Clock of the window while agents run and while a finished tree shows, and the poll of a live transcript
const (
	spawnTickLive   = 500 * time.Millisecond
	spawnTickReplay = 300 * time.Millisecond
	spawnTailPoll   = 1500 * time.Millisecond
)

// Sort and filter modes in the order the s and f keys cycle them
var (
	spawnSortOrder   = []string{"depthFirst", "toolsDesc", "durationDesc", "status"}
	spawnFilterOrder = []string{"all", "running", "failed", "leaf"}
	spawnSortKeys    = map[string]string{"depthFirst": "hubs.agents.sort.depthFirst", "toolsDesc": "hubs.agents.sort.toolsDesc", "durationDesc": "hubs.agents.sort.durationDesc", "status": "hubs.agents.sort.status"}
	spawnFilterKeys  = map[string]string{"all": "hubs.agents.filter.all", "running": "hubs.agents.filter.running", "failed": "hubs.agents.filter.failed", "leaf": "hubs.agents.filter.leaf"}
	spawnStatusKeys  = map[string]string{"completed": "hubs.agents.status.completed", "error": "hubs.agents.status.error", "failed": "hubs.agents.status.failed", "interrupted": "hubs.agents.status.interrupted", "queued": "hubs.agents.status.queued", "running": "hubs.agents.status.running", "timeout": "hubs.agents.status.timeout"}
	spawnStatusRank  = map[string]int{"error": 0, "failed": 0, "interrupted": 1, "timeout": 1, "running": 2, "queued": 3, "completed": 4}
)

// Key names of the palette actions the window manager fires at an overlay in place of the keys
var spawnActionKeys = map[string]string{"palette.pick": "enter", "palette.up": "up", "palette.down": "down", "palette.page.up": "pgup",
	"palette.page.down": "pgdown", "palette.left": "left", "palette.right": "right", "palette.home": "home", "palette.end": "end",
	"palette.clear": "ctrl+u", "palette.delete.back": "backspace", "palette.delete.forward": "delete", "palette.delete.word": "ctrl+w",
	"tui.spawntree.back": "esc"}

// Back key of the spawn tree and load key of the saved trees, registered with the keymap of the app
func registerSpawnTreeKeys(km *keys.Map) {
	for name, doc := range map[string]string{spawnTreeContext: "spawn tree window", spawnTreesContext: "saved spawn trees"} {
		if keys.ContextDocs[name] == "" {
			if err := keys.AddContext(name, doc); err != nil {
				panic(err)
			}
		}
	}
	km.Register(keys.Action{ID: "tui.spawntree.back", Context: spawnTreeContext, Label: "back", Doc: "step back from a node to the tree, or close the spawn tree", Default: []string{"esc"}})
	km.Register(keys.Action{ID: "tui.spawntrees.load", Context: spawnTreesContext, Label: "load", Doc: "replay the saved spawn tree", Default: []string{"enter"}})
}

// Counts rolled up over the subtree of a node
type spawnAggregate struct {
	activeCount      int
	descendantCount  int
	maxDepthFromHere int
	totalTools       int
	hotness          float64
	inputTokens      float64
	outputTokens     float64
	totalDuration    float64
}

// One subagent with its children and the counts of its subtree
type spawnNode struct {
	item     subagentProgress
	children []*spawnNode
	agg      spawnAggregate
}

// Value of an optional number, zero when absent
func orZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// Whether a subagent still runs or waits to start
func isRunningStatus(status string) bool { return status == "running" || status == "queued" }

// Spawn tree of a flat list, parents found by id and siblings in depth and spawn order
func buildSpawnTree(items []subagentProgress) []*spawnNode {
	const root = "\x00root"
	known := map[string]bool{}
	for _, it := range items {
		known[it.ID] = true
	}
	byParent := map[string][]subagentProgress{}
	for _, it := range items {
		key := root
		if it.ParentID != "" && known[it.ParentID] {
			key = it.ParentID
		}
		byParent[key] = append(byParent[key], it)
	}
	for _, bucket := range byParent {
		sort.SliceStable(bucket, func(i, j int) bool {
			if bucket[i].Depth != bucket[j].Depth {
				return bucket[i].Depth < bucket[j].Depth
			}
			return bucket[i].Index < bucket[j].Index
		})
	}
	onPath := map[string]bool{}
	var build func(it subagentProgress) *spawnNode
	build = func(it subagentProgress) *spawnNode {
		onPath[it.ID] = true
		node := &spawnNode{item: it}
		for _, kid := range byParent[it.ID] {
			if !onPath[kid.ID] {
				node.children = append(node.children, build(kid))
			}
		}
		delete(onPath, it.ID)
		node.agg = aggregateSpawn(it, node.children)
		return node
	}
	var out []*spawnNode
	for _, it := range byParent[root] {
		out = append(out, build(it))
	}
	return out
}

// Counts of a node and its children, hotness the tools per second over the subtree
func aggregateSpawn(it subagentProgress, children []*spawnNode) spawnAggregate {
	agg := spawnAggregate{totalTools: it.ToolCount, totalDuration: orZero(it.DurationSeconds), inputTokens: orZero(it.InputTokens), outputTokens: orZero(it.OutputTokens)}
	if isRunningStatus(it.Status) {
		agg.activeCount = 1
	}
	for _, child := range children {
		agg.add(child.agg)
	}
	if agg.totalDuration > 0 {
		agg.hotness = float64(agg.totalTools) / agg.totalDuration
	}
	return agg
}

// Adds the counts of a child subtree, one more descendant and one more level below
func (a *spawnAggregate) add(child spawnAggregate) {
	a.totalTools += child.totalTools
	a.totalDuration += child.totalDuration
	a.descendantCount += child.descendantCount + 1
	a.activeCount += child.activeCount
	a.maxDepthFromHere = max(a.maxDepthFromHere, child.maxDepthFromHere+1)
	a.inputTokens += child.inputTokens
	a.outputTokens += child.outputTokens
}

// Counts over every root of a tree
func spawnTreeTotals(tree []*spawnNode) spawnAggregate {
	var totals spawnAggregate
	for _, node := range tree {
		totals.add(node.agg)
	}
	return totals
}

// Number of agents at each depth, the top level first
func spawnWidthByDepth(tree []*spawnNode) []int {
	var widths []int
	var walk func(nodes []*spawnNode, depth int)
	walk = func(nodes []*spawnNode, depth int) {
		if len(nodes) == 0 {
			return
		}
		for len(widths) <= depth {
			widths = append(widths, 0)
		}
		widths[depth] += len(nodes)
		for _, n := range nodes {
			walk(n.children, depth+1)
		}
	}
	walk(tree, 0)
	return widths
}

// Nodes in visit order, each parent before its children
func flattenSpawnTree(tree []*spawnNode) []*spawnNode {
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

// Ids of every descendant of a node, the node itself left out
func spawnDescendantIDs(node *spawnNode) []string {
	var ids []string
	for _, n := range flattenSpawnTree(node.children) {
		ids = append(ids, n.item.ID)
	}
	return ids
}

// Highest tools per second of any node in the tree
func spawnPeakHotness(tree []*spawnNode) float64 {
	peak := 0.0
	for _, n := range flattenSpawnTree(tree) {
		peak = math.Max(peak, n.agg.hotness)
	}
	return peak
}

// Heat bucket of a hotness against the peak of its tree, zero for a cold or idle node
func spawnHotnessBucket(hotness, peak float64, buckets int) int {
	if math.IsNaN(hotness) || math.IsInf(hotness, 0) || hotness <= 0 || peak <= 0 || buckets <= 1 {
		return 0
	}
	ratio := math.Min(1, hotness/peak)
	return min(buckets-1, max(0, int(jsRound(ratio*float64(buckets-1)))))
}

// Bar sparkline of agent counts per depth, an empty depth drawn as a space
func spawnSparkline(values []int) string {
	if len(values) == 0 {
		return ""
	}
	peak := 0
	for _, v := range values {
		peak = max(peak, v)
	}
	if peak <= 0 {
		return strings.Repeat(" ", len(values))
	}
	out := make([]rune, len(values))
	for i, v := range values {
		if v <= 0 {
			out[i] = ' '
			continue
		}
		out[i] = sparkBlocks[min(len(sparkBlocks)-1, max(0, int(math.Ceil(float64(v)/float64(peak)*float64(len(sparkBlocks)-1)))))]
	}
	return string(out)
}

// Token count as 542, 1.2k or 12k
func fmtSpawnTokens(n float64) string {
	switch {
	case math.IsNaN(n) || math.IsInf(n, 0) || n <= 0:
		return "0"
	case n < 1000:
		return jsString(jsRound(n))
	case n < 10_000:
		return jsToFixed(n/1000, 1) + "k"
	}
	return jsString(jsRound(n/1000)) + "k"
}

// Seconds as 42s, 3m or 3m 5s
func fmtSpawnDuration(seconds float64) string {
	if seconds < 60 {
		return jsString(math.Max(0, jsRound(seconds))) + "s"
	}
	m := math.Floor(seconds / 60)
	s := jsRound(seconds - m*60)
	if s == 0 {
		return jsString(m) + "m"
	}
	return jsString(m) + "m " + jsString(s) + "s"
}

// Elapsed label of a span, empty for a negative one
func fmtElapsedLabel(seconds float64) string {
	if seconds < 0 {
		return ""
	}
	return fmtSpawnDuration(seconds)
}

// Seconds an agent ran, its final duration or the time since it started while it still runs
func displayElapsedSeconds(it subagentProgress, nowMs float64) (float64, bool) {
	if it.DurationSeconds != nil {
		return *it.DurationSeconds, true
	}
	if it.StartedAt != nil && isRunningStatus(it.Status) {
		return math.Max(0, (nowMs-*it.StartedAt)/1000), true
	}
	return 0, false
}

// Totals as one line of depth, agents, tools, time, tokens and the running count
func formatSpawnSummary(totals spawnAggregate) string {
	pieces := []string{"d" + strconv.Itoa(max(0, totals.maxDepthFromHere))}
	if totals.descendantCount == 1 {
		pieces = append(pieces, tr("libText.subagentTree.agentsOne", totals.descendantCount))
	} else {
		pieces = append(pieces, tr("libText.subagentTree.agentsOther", totals.descendantCount))
	}
	switch {
	case totals.totalTools == 1:
		pieces = append(pieces, tr("libText.subagentTree.toolsOne", totals.totalTools))
	case totals.totalTools > 0:
		pieces = append(pieces, tr("libText.subagentTree.toolsOther", totals.totalTools))
	}
	if totals.totalDuration > 0 {
		pieces = append(pieces, fmtSpawnDuration(totals.totalDuration))
	}
	if tokens := totals.inputTokens + totals.outputTokens; tokens > 0 {
		pieces = append(pieces, tr("libText.subagentTree.tokens", fmtSpawnTokens(tokens)))
	}
	if totals.activeCount > 0 {
		pieces = append(pieces, "⚡"+strconv.Itoa(totals.activeCount))
	}
	return strings.Join(pieces, " · ")
}

// Headline of the live spawn tree in the transcript, its agent count, summary, depth sparkline and the pointer to /agents
func spawnTreeHeadline(subagents []subagentProgress, c renderCtx) string {
	tree := buildSpawnTree(subagents)
	totals := spawnTreeTotals(tree)
	suffix := formatSpawnSummary(totals)
	if spark := spawnSparkline(spawnWidthByDepth(tree)); spark != "" {
		suffix += "  " + spark
	}
	title := tr("chatBits.thinking.spawnTree") + " (" + strconv.Itoa(totals.descendantCount) + ")"
	return spawnInk(title, c.t.Color.Label, false, "") + spawnInk("  "+suffix+"  "+tr("chatBits.thinking.agentsHint"), c.t.Color.Muted, false, "")
}

// Pointer to the spawn tree beside a running delegation, empty for any other tool
func delegateMonitorHint(name string, c renderCtx) string {
	if name != "delegate_task" {
		return ""
	}
	return toneStyle(c.t.Color.StatusFg).Faint(true).Render("  " + tr("chatBits.thinking.agentsToMonitor"))
}

// Glyph and tone of an agent status, a neutral dot for a status this build does not know
func agentStatusGlyph(status string, t theme) (string, string) {
	switch status {
	case "running":
		return "●", t.Color.Accent
	case "queued", "dispatched":
		return "○", t.Color.Muted
	case "finalizing":
		return "◐", t.Color.Accent
	case "completed":
		return "✓", t.Color.StatusGood
	case "interrupted", "cancelled":
		return "■", t.Color.Warn
	case "rejected":
		return "⊘", t.Color.Warn
	case "failed":
		return "✗", t.Color.Error
	case "timeout":
		return "⌛", t.Color.Warn
	case "error":
		return "⚠", t.Color.Error
	}
	return "·", t.Color.Muted
}

// Shown word of an agent status, the status itself when the catalog has none
func agentStatusLabel(status string) string {
	if key, known := spawnStatusKeys[status]; known {
		return tr(key)
	}
	return status
}

// Rank of a status for the status sort, failures first and unknown ones with them
func spawnRank(status string) int { return spawnStatusRank[status] }

// Rows of the tree in a sort and filter, the sort ordering the roots and children keeping spawn order
func prepareSpawnRows(tree []*spawnNode, sortMode, filter string) []*spawnNode {
	if len(tree) == 0 {
		return nil
	}
	roots := append([]*spawnNode(nil), tree...)
	sort.SliceStable(roots, func(i, j int) bool {
		a, b := roots[i], roots[j]
		switch sortMode {
		case "toolsDesc":
			return b.agg.totalTools < a.agg.totalTools
		case "durationDesc":
			return b.agg.totalDuration < a.agg.totalDuration
		case "status":
			return spawnRank(a.item.Status) < spawnRank(b.item.Status)
		}
		if a.item.Depth != b.item.Depth {
			return a.item.Depth < b.item.Depth
		}
		return a.item.Index < b.item.Index
	})
	var out []*spawnNode
	for _, n := range flattenSpawnTree(roots) {
		keep := true
		switch filter {
		case "leaf":
			keep = len(n.children) == 0
		case "running":
			keep = isRunningStatus(n.item.Status)
		case "failed":
			keep = n.item.Status == "error" || n.item.Status == "failed" || n.item.Status == "interrupted" || n.item.Status == "timeout"
		}
		if keep {
			out = append(out, n)
		}
	}
	return out
}

// Row number of a node as two right aligned digits
func formatSpawnRowID(n int) string {
	text := strconv.Itoa(n + 1)
	if len(text) < 2 {
		return " " + text
	}
	return text
}

// Two finished trees of /replay-diff, the baseline and the candidate
type spawnDiffPair struct {
	baseline  spawnSnapshot
	candidate spawnSnapshot
}

// Window of the spawn tree, the live delegated agents or a finished tree, with node detail, steer and transcript
type spawnTreeCard struct {
	a            *app
	diff         *spawnDiffPair
	historyIndex int
	sortMode     int
	filterMode   int
	cursor       int
	flash        string
	now          float64
	mode         string
	scroll       int
	stick        bool
	prevLive     int
	gen          int
	steer        []rune
	steerAt      int
	steerNote    string
	steerPending bool
	tail         string
	tailID       string
	tailGen      int
	tailPending  bool
	caret        *tea.Cursor
	paneTop      int
	paneH        int
	sectionAt    map[int]string
}

// What the window shows now, the live roster or a finished tree and its rows
type spawnView struct {
	history  []spawnSnapshot
	snapshot *spawnSnapshot
	agents   []subagentProgress
	tree     []*spawnNode
	totals   spawnAggregate
	rows     []*spawnNode
	selected *spawnNode
}

// Whether a finished tree shows instead of the live turn
func (v spawnView) replay() bool { return v.snapshot != nil }

// Opens the spawn tree at a history index, zero for the live turn, or the diff of two finished trees
func (a *app) openSpawnTree(historyIndex int, diff *spawnDiffPair) tea.Cmd {
	c := &spawnTreeCard{a: a, diff: diff, mode: "list", now: a.store.nowMillis(), prevLive: len(a.store.currentAgentRoster())}
	c.historyIndex = max(0, min(len(a.store.spawnHistory), historyIndex))
	a.store.overlay.Agents = true
	return tea.Batch(a.wm.ShowOverlay(c), c.fetchDelegation(), c.tick(c.gen))
}

// Reads the delegation caps, an answer dropped when a pause or a newer read landed first
func (c *spawnTreeCard) fetchDelegation() tea.Cmd {
	before := c.a.store.delegation.UpdatedAt
	return gatewayCall(c.a.gw, "delegation.status", map[string]any{}, func(v any, err error) tea.Cmd {
		switch {
		case err != nil:
			c.flash = tr("hubs.list.errorLine", rpcErrorMessage(err))
		case c.a.store.delegation.UpdatedAt == before:
			c.a.store.applyDelegationStatus(asRPCResult(v))
		}
		return nil
	})
}

// Next beat of the window clock, stopped once the window left the screen
func (c *spawnTreeCard) tick(gen int) tea.Cmd {
	every := spawnTickLive
	if c.view().replay() {
		every = spawnTickReplay
	}
	return tea.Tick(every, func(time.Time) tea.Msg {
		return asyncDoneMsg{run: func() tea.Cmd {
			if gen != c.gen || c.a.wm.Overlay() != wm.Card(c) {
				return nil
			}
			c.now = c.a.store.nowMillis()
			return c.tick(gen)
		}}
	})
}

// State of the window now, the snapshot of a history index or the one that just finished
func (c *spawnTreeCard) view() spawnView {
	v := spawnView{history: c.a.store.spawnHistory}
	live := c.a.store.currentAgentRoster()
	switch {
	case c.historyIndex > 0 && c.historyIndex <= len(v.history):
		v.snapshot = &v.history[c.historyIndex-1]
	case c.historyIndex == 0 && len(live) == 0 && len(v.history) > 0:
		v.snapshot = &v.history[0]
	}
	v.agents = live
	if v.snapshot != nil {
		v.agents = v.snapshot.Subagents
	}
	v.tree = buildSpawnTree(v.agents)
	v.totals = spawnTreeTotals(v.tree)
	v.rows = prepareSpawnRows(v.tree, spawnSortOrder[c.sortMode], spawnFilterOrder[c.filterMode])
	if c.cursor < len(v.rows) {
		v.selected = v.rows[c.cursor]
	}
	return v
}

// Follows what changed outside the window, the history length, a finished turn, the cursor range and a lost steer target
func (c *spawnTreeCard) sync() {
	history := c.a.store.spawnHistory
	if c.historyIndex > len(history) {
		c.historyIndex = len(history)
	}
	live := len(c.a.store.currentAgentRoster())
	prev := c.prevLive
	c.prevLive = live
	if c.historyIndex == 0 && prev > 0 && live == 0 && len(history) > 0 {
		c.historyIndex, c.cursor, c.scroll = 1, 0, 0
		c.flash = tr("hubs.agents.flash.turnFinished")
	}
	v := c.view()
	if c.cursor >= len(v.rows) {
		c.moveCursor(max(0, len(v.rows)-1))
		v = c.view()
	}
	if c.mode == "steer" && (v.selected == nil || c.a.store.ui.Sid == "") {
		c.setMode("detail")
	}
}

// Switches the view mode with the pane back at its top, a transcript following its end
func (c *spawnTreeCard) setMode(mode string) tea.Cmd {
	if mode == c.mode {
		return nil
	}
	c.mode, c.scroll, c.stick = mode, 0, mode == "tail"
	switch mode {
	case "steer":
		c.steer, c.steerAt, c.steerNote, c.steerPending = nil, 0, "", false
	case "tail":
		c.tailGen++
		c.tailID = ""
		return c.pollTail(c.tailGen)
	}
	return nil
}

// Reads the live transcript of the selected agent now and again every poll while the transcript shows
func (c *spawnTreeCard) pollTail(gen int) tea.Cmd {
	if gen != c.tailGen || c.mode != "tail" || c.a.wm.Overlay() != wm.Card(c) {
		return nil
	}
	next := tea.Tick(spawnTailPoll, func(time.Time) tea.Msg {
		return asyncDoneMsg{run: func() tea.Cmd { return c.pollTail(gen) }}
	})
	v := c.view()
	sid := c.a.store.ui.Sid
	if v.selected == nil || sid == "" || v.replay() || c.tailPending {
		return next
	}
	id := v.selected.item.ID
	if id != c.tailID {
		c.tailID, c.tail = id, tr("hubs.agentControls.loadingTranscript")
	}
	c.tailPending = true
	fetch := gatewayCall(c.a.gw, "subagent.tail", map[string]any{"session_id": sid, "subagent_id": id}, func(raw any, err error) tea.Cmd {
		c.tailPending = false
		if gen != c.tailGen || id != c.tailID {
			return nil
		}
		r := asRPCResult(raw)
		switch {
		case err != nil:
			c.tail = tr("hubs.agentControls.transcriptRefreshFailed")
		case truthyJS(r["available"]):
			prefix := ""
			if truthyJS(r["truncated"]) {
				prefix = tr("hubs.agentControls.truncatedPrefix") + "\n"
			}
			c.tail = prefix + paramStr(r["text"])
		default:
			c.tail = tr("hubs.agentControls.transcriptUnavailable")
		}
		return nil
	})
	return tea.Batch(fetch, next)
}

func (c *spawnTreeCard) Title() string       { return tr("hubs.agents.title.spawnTree") }
func (c *spawnTreeCard) Key() string         { return "tui:agents" }
func (c *spawnTreeCard) Glyph() string       { return c.a.uiTheme().Glyphs.Agent }
func (c *spawnTreeCard) Init() tea.Cmd       { return nil }
func (c *spawnTreeCard) Raw() bool           { return true }
func (c *spawnTreeCard) Contexts() []string  { return []string{spawnTreeContext} }
func (c *spawnTreeCard) Cursor() *tea.Cursor { return c.caret }

// Room of nearly the whole screen, the tree, its timeline and the detail pane needing the width
func (c *spawnTreeCard) MinSize() (int, int) {
	w, h := c.a.wm.Size()
	return max(w-4, 40), max(h-4, 12)
}

// Closes the window, the diff it showed going with it
func (c *spawnTreeCard) close() tea.Cmd { return wm.RequestDismissOf(c) }

func (c *spawnTreeCard) Update(m tea.Msg) (wm.Card, tea.Cmd) {
	switch m := m.(type) {
	case wm.FocusMsg:
		c.a.store.overlay.Agents = m.Focused
		if !m.Focused {
			c.gen++
			c.tailGen++
		}
	case wm.ActionMsg:
		if k := spawnActionKeys[m.ID]; k != "" {
			return c, c.key(k)
		}
	case tea.KeyPressMsg:
		if c.mode == "steer" && c.diff == nil && m.Text != "" {
			c.steerInsert(m.Text)
			return c, nil
		}
		return c, c.key(m.String())
	case tea.PasteMsg:
		if c.mode == "steer" && c.diff == nil {
			c.steerInsert(strings.ReplaceAll(strings.TrimRight(m.Content, "\r\n"), "\n", " "))
		}
	case tea.MouseWheelMsg:
		switch m.Button {
		case tea.MouseWheelUp:
			return c, c.key("wheelup")
		case tea.MouseWheelDown:
			return c, c.key("wheeldown")
		}
	case tea.MouseClickMsg:
		if m.Button == tea.MouseLeft && c.diff == nil && (c.mode == "detail" || c.mode == "tail") {
			if id, hit := c.sectionAt[m.Y-c.paneTop+c.scroll]; hit && m.Y >= c.paneTop && m.Y < c.paneTop+c.paneH {
				c.toggleSection(id)
			}
		}
	}
	return c, nil
}

// Section open state the app keeps across nodes and openings, the default until the user toggled it
func (c *spawnTreeCard) sectionOpen(id string) bool {
	if open, set := c.a.spawnSections[id]; set {
		return open
	}
	return spawnSectionDefaults[id]
}

// Flips a detail section between open and folded
func (c *spawnTreeCard) toggleSection(id string) {
	if c.a.spawnSections == nil {
		c.a.spawnSections = map[string]bool{}
	}
	c.a.spawnSections[id] = !c.sectionOpen(id)
}

// Sections open until folded, the rest folded until opened
var spawnSectionDefaults = map[string]bool{"budget": true, "files": false, "toolCalls": true, "output": true, "progress": false, "summary": true}

// Carries out one key of the window by the mode it is in
func (c *spawnTreeCard) key(k string) tea.Cmd {
	if c.diff != nil {
		if k == "esc" || k == "q" || k == "ctrl+t" {
			return c.close()
		}
		return nil
	}
	if c.mode == "steer" {
		return c.steerKey(k)
	}
	v := c.view()
	sel := v.selected
	sid := c.a.store.ui.Sid
	switch {
	case k == "ctrl+t", k == "q":
		return c.close()
	case k == "e" && sel != nil && sid != "" && !v.replay():
		return c.setMode("steer")
	case k == "t" && sel != nil:
		return c.setMode("tail")
	case k == "d" && sel != nil:
		return c.setMode("detail")
	case k == "esc":
		if c.mode != "list" {
			return c.setMode("list")
		}
		return c.close()
	case k == "<" || k == "[" || k == "shift+,":
		c.stepHistory(1)
		return nil
	case k == ">" || k == "]" || k == "shift+.":
		c.stepHistory(-1)
		return nil
	case k == "p":
		return c.togglePause(v)
	case k == "x" && sel != nil:
		return c.killOne(v, sel.item.ID)
	case (k == "X" || k == "shift+x") && sel != nil:
		return c.killSubtree(v, sel)
	}
	if c.mode == "detail" || c.mode == "tail" {
		page := max(4, c.paneH-2)
		switch k {
		case "left", "h":
			return c.setMode("list")
		case "pgup", "ctrl+u":
			c.scrollPane(-page)
		case "pgdown", "ctrl+d":
			c.scrollPane(page)
		case "wheelup":
			c.scrollPane(-3)
		case "wheeldown":
			c.scrollPane(3)
		case "up", "k":
			c.scrollPane(-2)
		case "down", "j":
			c.scrollPane(2)
		case "g":
			c.scroll, c.stick = 0, false
		case "G", "shift+g":
			c.scroll, c.stick = math.MaxInt32, c.mode == "tail"
		}
		return nil
	}
	switch k {
	case "enter", "right", "l":
		if sel == nil {
			return nil
		}
		if k == "enter" && !v.replay() {
			return c.setMode("tail")
		}
		return c.setMode("detail")
	case "up", "k", "wheelup":
		c.moveCursor(max(0, c.cursor-1))
	case "down", "j", "wheeldown":
		c.moveCursor(min(max(0, len(v.rows)-1), c.cursor+1))
	case "g":
		c.moveCursor(0)
	case "G", "shift+g":
		c.moveCursor(max(0, len(v.rows)-1))
	case "s":
		c.sortMode = (c.sortMode + 1) % len(spawnSortOrder)
	case "f":
		c.filterMode = (c.filterMode + 1) % len(spawnFilterOrder)
	}
	return nil
}

// Puts the cursor on a row with the pane back at its top
func (c *spawnTreeCard) moveCursor(to int) {
	if to != c.cursor {
		c.cursor, c.scroll = to, 0
	}
}

// Scrolls the detail pane, scrolling up letting go of the transcript end
func (c *spawnTreeCard) scrollPane(dy int) {
	c.scroll = max(0, c.scroll+dy)
	if dy < 0 {
		c.stick = false
	}
}

// Walks the history of finished trees, up for older and down toward the live turn
func (c *spawnTreeCard) stepHistory(delta int) {
	n := len(c.a.store.spawnHistory)
	next := max(0, min(n, c.historyIndex+delta))
	if next == c.historyIndex {
		return
	}
	c.historyIndex, c.cursor, c.scroll = next, 0, 0
	if next == 0 {
		c.flash = tr("hubs.agents.flash.liveTurn")
		return
	}
	c.flash = tr("hubs.agents.flash.replay", next, n)
}

// Runs a control on the live turn, a finished tree refusing it with a notice
func (c *spawnTreeCard) guardLive(v spawnView, action func() tea.Cmd) tea.Cmd {
	if v.replay() {
		c.flash = tr("hubs.agents.flash.replayLocked")
		return nil
	}
	return action()
}

// Interrupt request of one agent of the session
func (c *spawnTreeCard) interrupt(id string, then func(r map[string]any, err error) tea.Cmd) tea.Cmd {
	return gatewayCall(c.a.gw, "subagent.interrupt", map[string]any{"session_id": c.a.store.ui.Sid, "subagent_id": id}, func(v any, err error) tea.Cmd {
		return then(asRPCResult(v), err)
	})
}

// Stops one agent and says whether it was still running
func (c *spawnTreeCard) killOne(v spawnView, id string) tea.Cmd {
	return c.guardLive(v, func() tea.Cmd {
		return c.interrupt(id, func(r map[string]any, err error) tea.Cmd {
			switch {
			case err != nil:
				c.flash = tr("hubs.agents.flash.killFailed", id)
			case truthyJS(r["found"]):
				c.flash = tr("hubs.agents.flash.killing", id)
			default:
				c.flash = tr("hubs.agents.flash.notFound", id)
			}
			return nil
		})
	})
}

// Stops an agent and every agent below it, a failed stop named in the notice
func (c *spawnTreeCard) killSubtree(v spawnView, node *spawnNode) tea.Cmd {
	return c.guardLive(v, func() tea.Cmd {
		ids := append([]string{node.item.ID}, spawnDescendantIDs(node)...)
		cmds := make([]tea.Cmd, len(ids))
		for i, id := range ids {
			cmds[i] = c.interrupt(id, func(_ map[string]any, err error) tea.Cmd {
				if err != nil {
					c.flash = tr("hubs.agents.flash.killFailed", id)
				}
				return nil
			})
		}
		if len(ids) == 1 {
			c.flash = tr("hubs.agents.flash.killingSubtreeOne", len(ids))
		} else {
			c.flash = tr("hubs.agents.flash.killingSubtreeOther", len(ids))
		}
		return tea.Batch(cmds...)
	})
}

// Pauses or resumes the spawning of new agents
func (c *spawnTreeCard) togglePause(v spawnView) tea.Cmd {
	return c.guardLive(v, func() tea.Cmd {
		return gatewayCall(c.a.gw, "delegation.pause", map[string]any{"paused": !c.a.store.delegation.Paused}, func(raw any, err error) tea.Cmd {
			if err != nil {
				c.flash = tr("hubs.agents.flash.pauseFailed")
				return nil
			}
			r := asRPCResult(raw)
			c.a.store.applyDelegationStatus(map[string]any{"paused": r["paused"]})
			if truthyJS(r["paused"]) {
				c.flash = tr("hubs.agents.flash.spawningPaused")
			} else {
				c.flash = tr("hubs.agents.flash.spawningResumed")
			}
			return nil
		})
	})
}

// Typed text into the guidance field at its caret
func (c *spawnTreeCard) steerInsert(text string) {
	if c.steerPending {
		return
	}
	add := []rune(text)
	c.steer = append(c.steer[:c.steerAt:c.steerAt], append(add, c.steer[c.steerAt:]...)...)
	c.steerAt += len(add)
}

// Editing and sending keys of the guidance field, Esc returning to the node detail
func (c *spawnTreeCard) steerKey(k string) tea.Cmd {
	switch k {
	case "esc":
		if !c.steerPending {
			return c.setMode("detail")
		}
	case "enter":
		return c.sendSteer()
	}
	if c.steerPending {
		return nil
	}
	switch k {
	case "backspace":
		if c.steerAt > 0 {
			c.steer = append(c.steer[:c.steerAt-1:c.steerAt-1], c.steer[c.steerAt:]...)
			c.steerAt--
		}
	case "delete":
		if c.steerAt < len(c.steer) {
			c.steer = append(c.steer[:c.steerAt:c.steerAt], c.steer[c.steerAt+1:]...)
		}
	case "left":
		c.steerAt = max(0, c.steerAt-1)
	case "right":
		c.steerAt = min(len(c.steer), c.steerAt+1)
	case "home":
		c.steerAt = 0
	case "end":
		c.steerAt = len(c.steer)
	case "ctrl+u":
		c.steer, c.steerAt = append([]rune(nil), c.steer[c.steerAt:]...), 0
	case "ctrl+w":
		at := c.steerAt
		for at > 0 && c.steer[at-1] == ' ' {
			at--
		}
		for at > 0 && c.steer[at-1] != ' ' {
			at--
		}
		c.steer = append(c.steer[:at:at], c.steer[c.steerAt:]...)
		c.steerAt = at
	}
	return nil
}

// Queues the typed guidance for the selected agent at its next tool boundary
func (c *spawnTreeCard) sendSteer() tea.Cmd {
	text := string(c.steer)
	sel := c.view().selected
	if trimJS(text) == "" || c.steerPending || sel == nil {
		return nil
	}
	c.steerPending = true
	return gatewayCall(c.a.gw, "subagent.steer", map[string]any{"session_id": c.a.store.ui.Sid, "subagent_id": sel.item.ID, "text": text}, func(raw any, err error) tea.Cmd {
		c.steerPending = false
		switch {
		case err != nil:
			c.steerNote = tr("hubs.agentControls.notQueuedError", rpcErrorMessage(err))
		case asRPCResult(raw)["status"] == "queued":
			c.steerNote = tr("hubs.agentControls.queued")
			c.steer, c.steerAt = nil, 0
		default:
			c.steerNote = tr("hubs.agentControls.notQueued")
		}
		return nil
	})
}

// Text in a tone, bold when asked, on the selection chip when a background is given
func spawnInk(text, tone string, bold bool, bg string) string {
	style := toneStyle(tone).Bold(bold)
	if bg != "" {
		style = style.Background(toneColor(bg))
	}
	return style.Render(text)
}

// Wrapped lines of a lead and a body, the lead keeping its own tone and weight on the first line
func spawnWrapLead(lead, leadTone string, leadBold bool, body, bodyTone string, bodyBold bool, w int) []string {
	var out []string
	for i, line := range strings.Split(ansi.Wrap(lead+body, max(w, 8), ""), "\n") {
		if rest, cut := strings.CutPrefix(line, lead); i == 0 && cut {
			out = append(out, spawnInk(lead, leadTone, leadBold, "")+spawnInk(rest, bodyTone, bodyBold, ""))
			continue
		}
		out = append(out, spawnInk(line, bodyTone, bodyBold, ""))
	}
	return out
}

func (c *spawnTreeCard) View(w, h int) string {
	c.sync()
	c.caret = nil
	c.sectionAt = nil
	if c.diff != nil {
		return c.diffView(w, h)
	}
	t := c.a.store.ui.Theme
	v := c.view()
	head := []string{c.headerLine(v, w), ""}
	foot := c.footerLines(v, w)
	bodyH := max(1, h-len(head)-len(foot))
	var body []string
	switch {
	case c.mode == "steer" && v.selected != nil && c.a.store.ui.Sid != "":
		body = c.steerLines(v.selected.item.ID, w, len(head))
	case len(v.rows) == 0:
		for _, line := range strings.Split(ansi.Wrap(tr("hubs.agents.empty"), max(w, 8), ""), "\n") {
			body = append(body, spawnInk(line, t.Color.Muted, false, ""))
		}
		body = append(body, c.processLines(w)...)
	case c.mode == "list":
		body = c.listLines(v, w, bodyH)
	default:
		body = c.paneLines(v, w, bodyH, len(head))
	}
	if len(body) > bodyH {
		body = body[:bodyH]
	}
	for len(body) < bodyH {
		body = append(body, "")
	}
	return strings.Join(append(append(head, body...), foot...), "\n")
}

// Title of the live turn or the finished tree, then its summary, depth sparkline, caps and model mix
func (c *spawnTreeCard) headerLine(v spawnView, w int) string {
	t := c.a.store.ui.Theme
	d := c.a.store.delegation
	title, tone := tr("hubs.agents.title.spawnTree"), t.Color.Primary
	if d.Paused {
		title += tr("hubs.agents.title.pausedSuffix")
	}
	if v.replay() {
		tone = t.Color.Border
		title = tr("hubs.agents.title.lastTurn")
		if c.historyIndex > 0 {
			title = tr("hubs.agents.title.replay", c.historyIndex, len(v.history))
		}
		title += tr("hubs.agents.title.finishedAt", time.UnixMilli(int64(v.snapshot.FinishedAt)).In(clockLocation()).Format("15:04:05"))
	}
	meta := []string{formatSpawnSummary(v.totals)}
	if spark := spawnSparkline(spawnWidthByDepth(v.tree)); spark != "" {
		meta = append(meta, spark)
	}
	if d.MaxSpawnDepth != nil && *d.MaxSpawnDepth != 0 {
		children := "?"
		if d.MaxConcurrentChildren != nil {
			children = jsString(*d.MaxConcurrentChildren)
		}
		meta = append(meta, tr("hubs.agents.caps", *d.MaxSpawnDepth, children))
	}
	if mix := spawnModelMix(v.agents); mix != "" {
		meta = append(meta, "· "+mix)
	}
	line := spawnInk(title, tone, true, "") + spawnInk("   "+strings.Join(meta, "  "), t.Color.Muted, false, "")
	return ansi.Truncate(line, w, "…")
}

// Four most used models of the agents with their counts, inherit for an agent on the session model
func spawnModelMix(agents []subagentProgress) string {
	counts := map[string]int{}
	var order []string
	for _, it := range agents {
		key := tr("hubs.agents.inheritModel")
		if it.Model != "" {
			parts := strings.Split(it.Model, "/")
			key = parts[len(parts)-1]
		}
		if _, seen := counts[key]; !seen {
			order = append(order, key)
		}
		counts[key]++
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	var parts []string
	for _, key := range order[:min(4, len(order))] {
		parts = append(parts, key+"×"+strconv.Itoa(counts[key]))
	}
	return strings.Join(parts, " · ")
}

// Footer of the keys of the mode, the notice of the last action between them
func (c *spawnTreeCard) footerLines(v spawnView, w int) []string {
	t := c.a.store.ui.Theme
	controls := tr("hubs.agents.hint.controlsLocked")
	if !v.replay() {
		verb := tr("hubs.agents.hint.pause")
		if c.a.store.delegation.Paused {
			verb = tr("hubs.agents.hint.resume")
		}
		controls = tr("hubs.agents.hint.controls", verb)
	}
	footer := tr("hubs.agents.hint.footerLive")
	if v.replay() {
		footer = tr("hubs.agents.hint.footerReplay")
	}
	lines := []string{"", spawnInk(truncateEnd(footer, w), t.Color.Accent, false, "")}
	if c.flash != "" {
		lines = append(lines, spawnInk(truncateEnd(c.flash, w), t.Color.Accent, false, ""))
	}
	hint := tr("hubs.agents.hint.detail", controls)
	if c.mode == "list" {
		nav := tr("hubs.agents.hint.listNavLive")
		if v.replay() {
			nav = tr("hubs.agents.hint.listNavReplay")
		}
		history := ""
		if len(v.history) > 0 {
			history = tr("hubs.agents.hint.history", c.historyIndex, len(v.history))
		}
		hint = tr("hubs.agents.hint.list", nav, controls, tr(spawnSortKeys[spawnSortOrder[c.sortMode]]), tr(spawnFilterKeys[spawnFilterOrder[c.filterMode]]), history)
	}
	return append(lines, spawnInk(truncateEnd(hint, w), t.Color.Muted, false, ""))
}

// Timeline, rows around the cursor and the background processes of the list mode
func (c *spawnTreeCard) listLines(v spawnView, w, bodyH int) []string {
	_, termH := c.a.wm.Size()
	if c.flash != "" {
		termH--
	}
	var gantt []string
	if termH >= 32 {
		gantt = c.ganttLines(v.rows, w, min(4, len(v.rows)))
	}
	procs := c.processLines(w)
	rowsH := max(1, bodyH-len(gantt)-len(procs))
	start := max(0, min(max(0, len(v.rows)-rowsH), c.cursor-rowsH/2))
	peak := spawnPeakHotness(v.tree)
	out := append([]string{}, gantt...)
	for i := start; i < min(len(v.rows), start+rowsH); i++ {
		out = append(out, c.rowLine(v.rows[i], i, i == c.cursor, peak, w))
	}
	return append(out, procs...)
}

// Gantt strip of when each row ran against the span of the whole tree, the rows around the cursor
func (c *spawnTreeCard) ganttLines(flat []*spawnNode, cols, maxRows int) []string {
	t := c.a.store.ui.Theme
	type span struct {
		idx          int
		node         *spawnNode
		startAt, end float64
	}
	var spans []span
	for idx, n := range flat {
		started, ended := c.now, c.now
		if n.item.StartedAt != nil {
			started = *n.item.StartedAt
			if n.item.DurationSeconds != nil {
				ended = started + *n.item.DurationSeconds*1000
			}
		}
		if ended >= started {
			spans = append(spans, span{idx: idx, node: n, startAt: started, end: ended})
		}
	}
	if len(spans) == 0 {
		return nil
	}
	first, last := spans[0].startAt, spans[0].end
	for _, s := range spans {
		first, last = math.Min(first, s.startAt), math.Max(last, s.end)
	}
	whole := math.Max(1, last-first)
	seconds := (last - first) / 1000
	barW := max(10, cols-5-10)
	startIdx := max(0, min(max(0, len(spans)-maxRows), c.cursor-maxRows/2))
	bar := func(from, to float64) string {
		s := int(math.Floor((from - first) / whole * float64(barW)))
		e := min(barW, int(math.Ceil((to-first)/whole*float64(barW))))
		fill := max(1, e-s)
		return strings.Repeat(" ", s) + strings.Repeat("█", fill) + strings.Repeat(" ", max(0, barW-s-fill))
	}
	window := ""
	if len(spans) > maxRows {
		window = "  (" + strconv.Itoa(startIdx+1) + "-" + strconv.Itoa(min(len(spans), startIdx+maxRows)) + "/" + strconv.Itoa(len(spans)) + ")"
	}
	lines := []string{spawnInk(truncateEnd(tr("hubs.agents.timeline")+" · "+fmtElapsedLabel(math.Max(0, seconds))+window, cols), t.Color.Muted, false, "")}
	for _, s := range spans[startIdx:min(len(spans), startIdx+maxRows)] {
		active := s.idx == c.cursor
		_, statusTone := agentStatusGlyph(s.node.item.Status, t)
		accent, barTone := t.Color.Muted, statusTone
		if active {
			accent, barTone = t.Color.Accent, t.Color.Accent
		}
		line := spawnInk(formatSpawnRowID(s.idx)+"  ", accent, active, "") + spawnInk(bar(s.startAt, s.end), barTone, false, "")
		if sec, ok := displayElapsedSeconds(s.node.item, c.now); ok && fmtElapsedLabel(sec) != "" {
			line += spawnInk("   "+fmtElapsedLabel(sec), accent, false, "")
		}
		lines = append(lines, ansi.Truncate(line, cols, "…"))
	}
	step := 10
	if seconds < 20 && barW > 20 {
		step = 5
	}
	ruler, labels := make([]rune, barW), make([]rune, barW)
	for i := range ruler {
		ruler[i], labels[i] = '─', ' '
		switch {
		case i > 0 && i%10 == 0:
			ruler[i] = '┼'
		case i > 0 && i%5 == 0:
			ruler[i] = '·'
		}
	}
	for pos := 0; pos < barW; pos += step {
		secs := float64(pos) / float64(barW) * seconds
		label := jsToFixed(secs, 1) + "s"
		switch {
		case pos == 0:
			label = "0"
		case secs >= 1:
			label = jsString(jsRound(secs)) + "s"
		}
		for j, r := range []rune(label) {
			if pos+j < barW {
				labels[pos+j] = r
			}
		}
	}
	dim := toneStyle(t.Color.Muted).Faint(true)
	lines = append(lines, ansi.Truncate(dim.Render("    "+string(ruler)), cols, "…"))
	if seconds > 0 {
		lines = append(lines, ansi.Truncate(dim.Render("    "+string(labels)), cols, "…"))
	}
	return append(lines, "")
}

// Model, elapsed time and tokens of a row, right aligned beside it
func (c *spawnTreeCard) rowMeta(it subagentProgress) string {
	var parts []string
	if it.Model != "" {
		segments := strings.Split(it.Model, "/")
		parts = append(parts, segments[len(segments)-1])
	}
	if sec, ok := displayElapsedSeconds(it, c.now); ok && fmtElapsedLabel(sec) != "" {
		parts = append(parts, fmtElapsedLabel(sec))
	}
	if tokens := orZero(it.InputTokens) + orZero(it.OutputTokens); tokens > 0 {
		parts = append(parts, tr("libText.subagentTree.tokens", fmtSpawnTokens(tokens)))
	}
	return strings.Join(parts, " · ")
}

// One tree row, its number, depth, heat mark, status, goal, tool and child counts and the tool it runs
func (c *spawnTreeCard) rowLine(n *spawnNode, index int, active bool, peak float64, w int) string {
	t := c.a.store.ui.Theme
	it := n.item
	glyph, tone := agentStatusGlyph(it.Status, t)
	heat := []string{t.Color.Border, t.Color.Accent, t.Color.Primary, t.Color.Warn, t.Color.Error}
	bucket := spawnHotnessBucket(n.agg.hotness, peak, len(heat))
	meta := c.rowMeta(it)
	metaW := 0
	if meta != "" && w >= 60 {
		metaW = ansi.StringWidth(meta) + 2
	} else {
		meta = ""
	}
	goal := it.Goal
	if goal == "" {
		goal = tr("hubs.agents.subagentFallback")
	}
	goal = compactPreview(goal, max(8, w-28-it.Depth*2-metaW))
	counts := ""
	if n.agg.totalTools > 0 {
		counts += " ·" + strconv.Itoa(n.agg.totalTools) + "t"
	}
	if len(n.children) > 0 {
		counts += " ·" + strconv.Itoa(len(n.children)) + "↓"
	}
	if it.Status == "running" && len(it.Tools) > 0 {
		line := it.Tools[len(it.Tools)-1]
		if paren := strings.Index(line, "("); paren > 0 {
			line = line[:paren]
		}
		if short := trimJS(line); short != "" {
			counts += " · " + compactPreview(short, 14)
		}
	}
	bg, fg := "", t.Color.Text
	if active {
		bg, fg = t.Color.SelectionBg, t.Color.Accent
	}
	ink := func(text, own string) string {
		if active {
			own = fg
		}
		return spawnInk(text, own, active, bg)
	}
	left := ink(" ", fg) + ink(formatSpawnRowID(index)+" ", t.Color.Muted) + ink(strings.Repeat("  ", max(0, it.Depth)), fg)
	if bucket >= 2 {
		left += ink("▍", heat[bucket])
	}
	left += ink(glyph, tone) + ink(" "+goal, fg) + ink(counts, t.Color.Muted)
	left = ansi.Truncate(left, w-metaW, "…")
	line := left
	if meta != "" {
		line += ink(strings.Repeat(" ", max(1, w-ansi.StringWidth(left)-ansi.StringWidth(meta))), fg) + ink(meta, t.Color.Muted)
	} else if active {
		line += ink(strings.Repeat(" ", max(0, w-ansi.StringWidth(left))), fg)
	}
	return line
}

// Background processes of the session under the tree, running ones with their time
func (c *spawnTreeCard) processLines(w int) []string {
	t := c.a.store.ui.Theme
	rows := c.a.store.currentProcessRows(c.now)
	if len(rows) == 0 {
		return nil
	}
	running := 0
	for _, r := range rows {
		if r.Status == "running" {
			running++
		}
	}
	var parts []string
	if running > 0 {
		parts = append(parts, tr("hubs.agentsPanel.running", running))
	}
	if done := len(rows) - running; done > 0 {
		parts = append(parts, tr("hubs.agentsPanel.done", done))
	}
	lines := []string{"", spawnInk(truncateEnd(tr("hubs.agents.processes")+" · "+strings.Join(parts, " · "), w), t.Color.Accent, true, "")}
	for _, r := range rows {
		glyph, tone := processGlyph(r.Status, t)
		activity := r.Detail
		if r.Status == "running" {
			activity = fmtSpawnDuration(r.ElapsedSeconds) + " · " + r.Detail
		}
		command := compactPreview(r.Command, max(8, w-2-ansi.StringWidth(activity)-6))
		line := spawnInk(glyph+" ", tone, false, "") + spawnInk(command, t.Color.Text, false, "") + spawnInk(" · "+activity, t.Color.Muted, false, "")
		lines = append(lines, ansi.Truncate(line, w, "…"))
	}
	return lines
}

// Scrolled pane of the node detail or its live transcript, a scrollbar beside it when it overflows
func (c *spawnTreeCard) paneLines(v spawnView, w, bodyH, top int) []string {
	t := c.a.store.ui.Theme
	contentW := max(8, w-2)
	var lines []string
	sections := map[int]string{}
	switch {
	case v.selected != nil && c.mode == "tail" && c.a.store.ui.Sid != "" && !v.replay():
		text := c.tail
		if c.tailID != v.selected.item.ID {
			text = tr("hubs.agentControls.loadingTranscript")
		}
		lines = append(lines, spawnInk(tr("hubs.agentControls.transcriptTitle"), t.Color.Accent, true, ""))
		for _, para := range strings.Split(text, "\n") {
			for _, line := range strings.Split(ansi.Wrap(para, contentW, ""), "\n") {
				lines = append(lines, spawnInk(line, t.Color.Text, false, ""))
			}
		}
	case v.selected != nil:
		lines, sections = c.detailLines(v.selected, contentW)
	}
	lines = append(lines, "", "", "", "")
	limit := max(0, len(lines)-bodyH)
	if c.mode == "tail" && !c.stick && c.scroll >= limit {
		c.stick = true
	}
	if c.mode == "tail" && c.stick {
		c.scroll = limit
	}
	c.scroll = max(0, min(c.scroll, limit))
	c.paneTop, c.paneH, c.sectionAt = top, bodyH, sections
	out := append([]string{}, lines[c.scroll:min(len(lines), c.scroll+bodyH)]...)
	for len(out) < bodyH {
		out = append(out, "")
	}
	if len(lines) > bodyH && w > 4 {
		scrollbarRows(out, c.scroll, len(lines), w, t, c.a.uiTheme())
	}
	return out
}

// Detail of a node, its fields and the budget, files, tool calls, output, progress and summary sections
func (c *spawnTreeCard) detailLines(n *spawnNode, w int) ([]string, map[int]string) {
	t := c.a.store.ui.Theme
	it, agg := n.item, n.agg
	glyph, tone := agentStatusGlyph(it.Status, t)
	sections := map[int]string{}
	lead := ""
	if it.ID != "" {
		lead = "#" + it.ID + " "
	}
	var lines []string
	for i, line := range strings.Split(ansi.Wrap(lead+glyph+" "+it.Goal, max(w, 8), ""), "\n") {
		if rest, cut := strings.CutPrefix(line, lead+glyph); i == 0 && cut {
			lines = append(lines, spawnInk(lead, t.Color.Accent, true, "")+spawnInk(glyph, tone, true, "")+spawnInk(rest, t.Color.Text, true, ""))
			continue
		}
		lines = append(lines, spawnInk(line, t.Color.Text, true, ""))
	}
	lines = append(lines, "")
	field := func(name, value string) {
		lines = append(lines, ansi.Truncate(spawnInk(name+" · ", t.Color.Label, false, "")+spawnInk(value, t.Color.Text, false, ""), w, "…"))
	}
	field(tr("hubs.agents.detail.depth"), strconv.Itoa(it.Depth)+" · "+agentStatusLabel(it.Status))
	if it.Model != "" {
		field(tr("hubs.agents.detail.model"), it.Model)
	}
	if len(it.Toolsets) > 0 {
		field(tr("hubs.agents.detail.toolsets"), strings.Join(it.Toolsets, ", "))
	}
	field(tr("hubs.agents.detail.tools"), tr("hubs.agents.detail.toolsValue", it.ToolCount, agg.totalTools))
	subtree := tr("hubs.agents.detail.subtreeValueOther", agg.descendantCount, agg.maxDepthFromHere, agg.activeCount)
	if agg.descendantCount == 1 {
		subtree = tr("hubs.agents.detail.subtreeValueOne", agg.descendantCount, agg.maxDepthFromHere, agg.activeCount)
	}
	field(tr("hubs.agents.detail.subtree"), subtree)
	if d := orZero(it.DurationSeconds); d != 0 {
		elapsed := ""
		if d > 0 {
			elapsed = fmtSpawnDuration(d)
		}
		field(tr("hubs.agents.detail.elapsed"), elapsed)
	}
	if it.Iteration != nil {
		field(tr("hubs.agents.detail.iteration"), jsString(*it.Iteration))
	}
	if orZero(it.APICalls) != 0 {
		field(tr("hubs.agents.detail.apiCalls"), jsString(*it.APICalls))
	}
	section := func(id, title string, count int, counted bool) bool {
		open := c.sectionOpen(id)
		mark := "▸ "
		if open {
			mark = "▾ "
		}
		if counted {
			title += " (" + strconv.Itoa(count) + ")"
		}
		lines = append(lines, "")
		sections[len(lines)] = id
		lines = append(lines, ansi.Truncate(spawnInk(mark, t.Color.Accent, false, "")+spawnInk(title, t.Color.Label, false, ""), w, "…"))
		return open
	}
	input, output := orZero(it.InputTokens), orZero(it.OutputTokens)
	local := input + output
	if local > 0 && section("budget", tr("hubs.agents.section.budget"), 0, false) {
		value := tr("hubs.agents.detail.tokensValue", fmtSpawnTokens(input), fmtSpawnTokens(output))
		if orZero(it.ReasoningTokens) != 0 {
			value += tr("hubs.agents.detail.reasoningSuffix", fmtSpawnTokens(*it.ReasoningTokens))
		}
		field(tr("hubs.agents.detail.tokens"), value)
		if below := agg.inputTokens + agg.outputTokens - local; below > 0 {
			field(tr("hubs.agents.detail.subtreeTokens"), "+"+fmtSpawnTokens(below))
		}
	}
	if files := len(it.FilesRead) + len(it.FilesWritten); files > 0 && section("files", tr("hubs.agents.section.files"), files, true) {
		for _, p := range it.FilesWritten[:min(8, len(it.FilesWritten))] {
			lines = append(lines, ansi.Truncate(spawnInk("+"+p, t.Color.StatusGood, false, ""), w, "…"))
		}
		for _, p := range it.FilesRead[:min(8, len(it.FilesRead))] {
			lines = append(lines, ansi.Truncate(spawnInk("·", t.Color.Muted, false, "")+spawnInk(" "+p, t.Color.Text, false, ""), w, "…"))
		}
		if over := max(0, len(it.FilesRead)-8) + max(0, len(it.FilesWritten)-8); over > 0 {
			lines = append(lines, spawnInk(tr("hubs.agents.detail.filesMore", over), t.Color.Muted, false, ""))
		}
	}
	tools := it.Tools
	if len(tools) == 0 {
		for _, e := range it.OutputTail {
			if e.Tool != "" {
				tools = append(tools, e.Tool)
			}
		}
	}
	if len(tools) > 0 && section("toolCalls", tr("hubs.agents.section.toolCalls"), len(tools), true) {
		for _, line := range tools {
			lines = append(lines, spawnWrapLead("·", t.Color.Muted, false, " "+line, t.Color.Text, false, w)...)
		}
	}
	if len(it.OutputTail) > 0 && section("output", tr("hubs.agents.section.output"), len(it.OutputTail), true) {
		for _, e := range it.OutputTail {
			leadTone, bodyTone := t.Color.Accent, t.Color.Text
			if e.IsError {
				leadTone, bodyTone = t.Color.Error, t.Color.Error
			}
			lines = append(lines, spawnWrapLead(e.Tool, leadTone, true, " "+e.Preview, bodyTone, false, w)...)
		}
	}
	if len(it.Notes) > 0 && section("progress", tr("hubs.agents.section.progress"), len(it.Notes), true) {
		for _, note := range it.Notes[max(0, len(it.Notes)-6):] {
			lines = append(lines, spawnWrapLead("·", t.Color.Label, false, " "+note, t.Color.Text, false, w)...)
		}
	}
	if it.Summary != "" && section("summary", tr("hubs.agents.section.summary"), 0, false) {
		for _, para := range strings.Split(it.Summary, "\n") {
			for _, line := range strings.Split(ansi.Wrap(para, max(w, 8), ""), "\n") {
				lines = append(lines, spawnInk(line, t.Color.Text, false, ""))
			}
		}
	}
	return lines, sections
}

// Guidance form of the selected agent, the caret placed in its field
func (c *spawnTreeCard) steerLines(id string, w, top int) []string {
	t := c.a.store.ui.Theme
	lines := []string{spawnInk(truncateEnd(tr("hubs.agentControls.steerTitle", id), w), t.Color.Accent, true, "")}
	for _, line := range strings.Split(ansi.Wrap(tr("hubs.agentControls.steerIntro"), max(w, 8), ""), "\n") {
		lines = append(lines, spawnInk(line, t.Color.Muted, false, ""))
	}
	lines = append(lines, "")
	room := max(1, w-6)
	before := string(c.steer[:c.steerAt])
	shown, caretX := string(c.steer), ansi.StringWidth(before)
	if caretX >= room {
		cut := caretX - room + 1
		shown = ansi.TruncateLeft(shown, cut, "")
		caretX -= cut
	}
	shown = ansi.Truncate(shown, room, "")
	if !c.steerPending {
		c.caret = tea.NewCursor(2+caretX, top+len(lines))
	}
	lines = append(lines, spawnInk("❯ ", t.Color.Accent, false, "")+spawnInk(shown, t.Color.Text, false, ""))
	note := c.steerNote
	if c.steerPending {
		note = tr("hubs.agentControls.queueing")
	}
	lines = append(lines, spawnInk(truncateEnd(note, w), t.Color.Muted, false, ""))
	return append(lines, spawnInk(truncateEnd(tr("hubs.agentControls.steerHint"), w), t.Color.Muted, false, ""))
}

// Line of one metric of the diff, both values and the signed change between them
func spawnDiffMetric(name string, a, b float64, format func(float64) string) string {
	d := b - a
	sign := ""
	switch {
	case d > 0:
		sign = "+"
	case d < 0:
		sign = "-"
	}
	change := format(math.Abs(d))
	if change == "" {
		change = "0"
	}
	return name + ": " + format(a) + " → " + format(b) + "  (" + sign + change + ")"
}

// Two finished trees side by side with the change of agents, tools, depth, duration and tokens
func (c *spawnTreeCard) diffView(w, h int) string {
	t := c.a.store.ui.Theme
	pair := c.diff
	before, after := spawnTreeTotals(buildSpawnTree(pair.baseline.Subagents)), spawnTreeTotals(buildSpawnTree(pair.candidate.Subagents))
	paneW := max(1, (w-4)/2)
	left := c.diffPane(tr("hubs.agents.diff.baseline"), pair.baseline, before, paneW)
	right := c.diffPane(tr("hubs.agents.diff.candidate"), pair.candidate, after, paneW)
	lines := []string{spawnInk(tr("hubs.agents.diff.title"), t.Color.Border, true, ""), spawnInk(tr("hubs.agents.diff.subtitle"), t.Color.Muted, false, ""), ""}
	for i := 0; i < max(len(left), len(right)); i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		lines = append(lines, render.Fit(l, paneW, c.a.uiTheme())+"  "+r)
	}
	round := func(n float64) string { return jsString(jsRound(n)) }
	tokens := func(x spawnAggregate) float64 { return x.inputTokens + x.outputTokens }
	lines = append(lines, "", "", spawnInk(tr("hubs.agents.diff.delta"), t.Color.Accent, true, ""))
	for _, metric := range []string{
		spawnDiffMetric(tr("hubs.agents.diff.agents"), float64(before.descendantCount), float64(after.descendantCount), round),
		spawnDiffMetric(tr("hubs.agents.diff.tools"), float64(before.totalTools), float64(after.totalTools), round),
		spawnDiffMetric(tr("hubs.agents.diff.depth"), float64(before.maxDepthFromHere), float64(after.maxDepthFromHere), round),
		spawnDiffMetric(tr("hubs.agents.diff.duration"), before.totalDuration, after.totalDuration, func(n float64) string { return jsToFixed(n, 1) + "s" }),
		spawnDiffMetric(tr("hubs.agents.diff.tokens"), tokens(before), tokens(after), fmtSpawnTokens),
	} {
		lines = append(lines, spawnInk(metric, t.Color.Text, false, ""))
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], w, "…")
	}
	return strings.Join(lines[:min(len(lines), max(h, 1))], "\n")
}

// One side of the diff, its label, the tree label, its summary and up to eight top level agents
func (c *spawnTreeCard) diffPane(label string, snap spawnSnapshot, totals spawnAggregate, w int) []string {
	t := c.a.store.ui.Theme
	lines := []string{
		spawnInk(truncateEnd(label, w), t.Color.Text, true, ""),
		spawnInk(truncateEnd(snap.Label, w), t.Color.Muted, false, ""),
		"",
		spawnInk(truncateEnd(formatSpawnSummary(totals), w), t.Color.Muted, false, ""),
		"",
	}
	top := topLevelSubagents(snap.Subagents)
	for _, s := range top[:min(8, len(top))] {
		glyph, tone := agentStatusGlyph(s.Status, t)
		goal := s.Goal
		if goal == "" {
			goal = tr("hubs.agents.subagentFallback")
		}
		lines = append(lines, spawnInk(glyph, tone, false, "")+spawnInk(" "+truncateEnd(goal, max(1, w-2)), t.Color.Muted, false, ""))
	}
	return lines
}

// Saved spawn trees of the session newest first, enter replaying the one under the cursor
func (a *app) openSavedSpawnTrees(entries []any) tea.Cmd {
	type savedTree struct{ when, count, label, path string }
	rows := make([]savedTree, 0, len(entries))
	for _, raw := range entries {
		e, _ := raw.(map[string]any)
		row := savedTree{when: "?", count: jsTemplate(e, "count"), label: paramStr(e["label"]), path: paramStr(e["path"])}
		if finished, isNum := e["finished_at"].(float64); isNum && finished != 0 {
			row.when = time.UnixMilli(int64(finished * 1000)).In(clockLocation()).Format("2006-01-02 15:04:05")
		}
		if row.label == "" {
			row.label = tr("slashCmd.ops.replay.subagentsLabel", row.count)
		}
		rows = append(rows, row)
	}
	list := cards.NewList(a.cardEnv(), cards.ListSpec{
		Title: tr("slashCmd.ops.replay.archivedTitle"), Glyph: a.toolkit.Glyphs.Agent, Key: "tui:spawntrees", Context: spawnTreesContext, Primary: "tui.spawntrees.load",
		Columns: []render.Column{{Title: tr("slashCmd.ops.replay.columnFinished")}, {Title: tr("slashCmd.ops.replay.columnAgents"), Right: true},
			{Title: tr("slashCmd.ops.replay.columnLabel"), Flex: 2}, {Title: tr("slashCmd.ops.replay.columnPath"), Flex: 3}},
		Rows: func() [][]string {
			out := make([][]string, len(rows))
			for i, r := range rows {
				out[i] = []string{r.when, r.count + "×", strings.ReplaceAll(r.label, "\n", " "), r.path}
			}
			return out
		},
		Empty: func() string { return tr("slashCmd.ops.replay.noneOnDisk") },
		Actions: map[string]func(l *cards.List, i int) tea.Cmd{
			"tui.spawntrees.load": func(l *cards.List, i int) tea.Cmd {
				return tea.Batch(wm.RequestDismissOf(l), a.loadSpawnTree(rows[i].path, a.events.rpc))
			},
		},
	})
	return a.wm.ShowOverlay(list)
}

// Loads a saved spawn tree into the history and replays it, an empty snapshot refused
func (a *app) loadSpawnTree(path string, call func(method string, params map[string]any, then func(map[string]any) tea.Cmd) tea.Cmd) tea.Cmd {
	return call("spawn_tree.load", map[string]any{"path": path}, func(r map[string]any) tea.Cmd {
		if r == nil {
			return nil
		}
		if subagents, _ := r["subagents"].([]any); len(subagents) == 0 {
			a.sys(tr("slashCmd.ops.replay.snapshotEmpty"))
			return nil
		}
		a.store.pushDiskSnapshot(r, path)
		return a.openSpawnTree(1, nil)
	})
}
