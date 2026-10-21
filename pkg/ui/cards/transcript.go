package cards

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"image/color"
	"regexp"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/agent/hooks"
	"github.com/nickheyer/icebath/pkg/agent/subagent"
	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool/files"
	"github.com/nickheyer/icebath/pkg/tool/shell"
	"github.com/nickheyer/icebath/pkg/tool/web"
	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Lines a collapsed tool result or read shows before it is cut
const previewLines = 40

type blockKind int

const (
	bUser blockKind = iota
	bText
	bThinking
	bTool
	bApproval
	bSteer
	bCompact
	bHook
	bNote
	bError
	bChild
)

// Approval as the transcript remembers it, from a live event or a stored record
type approvalInfo struct {
	Tool    string
	CallID  string
	Pending bool
	Allowed bool
	By      string
	Verdict string
	Key     string
	Reason  string
}

// One rendered unit of the transcript
type block struct {
	kind     blockKind
	agent    string
	model    string
	round    int
	at       time.Time
	text     string
	parts    []protocol.Part
	done     bool
	call     protocol.ToolCall
	partial  string
	result   *protocol.ToolResult
	data     any
	started  time.Time
	duration time.Duration
	err      error
	approval *approvalInfo
	expanded bool
	full     bool
	cache    []string
	cacheKey string
}

type transcriptTick struct{}

// Streaming transcript of one agent lineage with tool rows, diffs, approvals, markers, search and turn jumps
type Transcript struct {
	env       *Env
	name      string
	key       string
	blocks    []*block
	turn      map[int]*block
	tools     map[string]*block
	vp        viewport.Model
	follow    bool
	w, h      int
	lines     []string
	lineBlock []int
	blockLine []int
	cursor    int
	searching bool
	query     string
	matches   int
	tick      int
	ticking   bool
	streaming bool
	running   int
	dirty     bool
	thinking  bool
	turns     int
	// Run id followed, every run when empty
	Run string
	// Agent name followed, the root lineage when empty
	Agent string
	// Accepts only root events when no agent is named
	rootOnly bool
	colors   map[string]color.Color
}

var (
	tScroll = key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "scroll"))
	tTurns  = key.NewBinding(key.WithKeys("[", "]"), key.WithHelp("[/]", "turns"))
	tOpen   = key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "expand"))
	tSearch = key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search"))
	tFollow = key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "follow"))
	tThink  = key.NewBinding(key.WithKeys("T"), key.WithHelp("T", "thinking"))
)

// Builds the transcript of the root lineage
func NewTranscript(env *Env) *Transcript {
	vp := viewport.New()
	vp.MouseWheelEnabled = true
	vp.SoftWrap = false
	vp.HighlightStyle = lipgloss.NewStyle().Background(env.Theme.Accent).Foreground(env.Theme.Bg)
	vp.SelectedHighlightStyle = lipgloss.NewStyle().Background(env.Theme.Pulse).Foreground(env.Theme.Bg)
	return &Transcript{env: env, name: "TRACE", key: "trace", turn: map[int]*block{}, tools: map[string]*block{}, vp: vp, follow: true, cursor: -1, rootOnly: true, colors: map[string]color.Color{}}
}

// Builds a transcript that follows one child agent of a run
func NewChildTranscript(env *Env, run, agentName string) *Transcript {
	tr := NewTranscript(env)
	tr.name = "TRACE " + agentName
	tr.key = "trace:" + run + "/" + agentName
	tr.Run, tr.Agent, tr.rootOnly = run, agentName, false
	return tr
}

func (tr *Transcript) Title() string       { return tr.name }
func (tr *Transcript) Glyph() string       { return "≋" }
func (tr *Transcript) Key() string         { return tr.key }
func (tr *Transcript) Init() tea.Cmd       { return nil }
func (tr *Transcript) Selectable() bool    { return true }
func (tr *Transcript) Capturing() bool     { return tr.searching }
func (tr *Transcript) MinSize() (int, int) { return 24, 4 }

func (tr *Transcript) Hints() []key.Binding {
	return []key.Binding{tScroll, tTurns, tOpen, tSearch, tFollow, tThink}
}

func (tr *Transcript) Claims() []string {
	return []string{"j", "k", "g", "G", "/", "n", "N", "[", "]", "o", ".", ",", "e", "E", "T", "f", "space", "pgup", "pgdown", "ctrl+d", "ctrl+u", "home", "end"}
}

// Every block in order
func (tr *Transcript) Blocks() int { return len(tr.blocks) }

// Reports whether the transcript shows an event of this run and agent
func (tr *Transcript) accepts(msg harness.EventMsg) bool {
	if tr.Run != "" && msg.Run != tr.Run {
		return false
	}
	ev := msg.Event
	if tr.Agent != "" {
		return ev.Agent == tr.Agent
	}
	if tr.rootOnly {
		return ev.Depth == 0
	}
	return true
}

func (tr *Transcript) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.EventMsg:
		if !tr.accepts(msg) {
			return tr, nil
		}
		tr.Apply(msg.Event)
		return tr, tr.ensureTick()
	case harness.ProgressMsg:
		if b, ok := tr.tools[msg.Progress.CallID]; ok && b.result == nil {
			b.partial += msg.Progress.Text
			b.cacheKey = ""
			tr.dirty = true
		}
		return tr, nil
	case transcriptTick:
		tr.tick++
		if tr.streaming || tr.running > 0 {
			tr.dirty = true
			return tr, tr.tickCmd()
		}
		tr.ticking = false
		return tr, nil
	case deck.FocusMsg:
		if !msg.Focused {
			tr.searching = false
		}
		return tr, nil
	case tea.KeyPressMsg:
		return tr, tr.keyPress(msg)
	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		tr.vp, cmd = tr.vp.Update(msg)
		tr.follow = tr.vp.AtBottom()
		return tr, cmd
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			line := tr.vp.YOffset() + msg.Y
			if line >= 0 && line < len(tr.lineBlock) {
				tr.cursor = tr.lineBlock[line]
				tr.toggle(tr.cursor)
			}
		}
		return tr, nil
	}
	return tr, nil
}

func (tr *Transcript) ensureTick() tea.Cmd {
	if tr.ticking || (!tr.streaming && tr.running == 0) {
		return nil
	}
	tr.ticking = true
	return tr.tickCmd()
}

func (tr *Transcript) tickCmd() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return transcriptTick{} })
}

func (tr *Transcript) keyPress(msg tea.KeyPressMsg) tea.Cmd {
	if tr.searching {
		switch msg.String() {
		case "enter":
			tr.searching = false
			tr.applySearch()
		case "esc":
			tr.searching = false
			tr.query = ""
			tr.vp.ClearHighlights()
			tr.matches = 0
		case "backspace":
			if len(tr.query) > 0 {
				tr.query = tr.query[:len(tr.query)-1]
			}
		default:
			if msg.Text != "" {
				tr.query += msg.Text
			}
		}
		return nil
	}
	switch msg.String() {
	case "j", "down":
		tr.vp.ScrollDown(1)
	case "k", "up":
		tr.vp.ScrollUp(1)
	case "ctrl+d", "pgdown", "space":
		tr.vp.HalfPageDown()
	case "ctrl+u", "pgup":
		tr.vp.HalfPageUp()
	case "g", "home":
		tr.vp.GotoTop()
	case "G", "end":
		tr.vp.GotoBottom()
	case "/":
		tr.searching = true
		tr.query = ""
	case "n":
		tr.vp.HighlightNext()
	case "N":
		tr.vp.HighlightPrevious()
	case "[":
		tr.jumpTurn(-1)
	case "]":
		tr.jumpTurn(1)
	case ".":
		tr.moveCursor(1)
	case ",":
		tr.moveCursor(-1)
	case "o", "enter":
		if tr.cursor < 0 {
			tr.cursor = tr.nearest()
		}
		tr.toggle(tr.cursor)
	case "e":
		tr.expandAll(true)
	case "E":
		tr.expandAll(false)
	case "T":
		tr.thinking = !tr.thinking
		tr.invalidate(bThinking)
	case "f":
		tr.follow = !tr.follow
		if tr.follow {
			tr.vp.GotoBottom()
		}
	default:
		return nil
	}
	tr.follow = tr.vp.AtBottom()
	return nil
}

func (tr *Transcript) applySearch() {
	tr.vp.ClearHighlights()
	tr.matches = 0
	if tr.query == "" {
		return
	}
	re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(tr.query))
	if err != nil {
		return
	}
	content := ansi.Strip(strings.Join(tr.lines, "\n"))
	found := re.FindAllStringIndex(content, -1)
	tr.matches = len(found)
	if len(found) == 0 {
		return
	}
	plain := make([]string, len(tr.lines))
	for i, l := range tr.lines {
		plain[i] = ansi.Strip(l)
	}
	tr.vp.SetContentLines(plain)
	tr.vp.SetHighlights(found)
	tr.follow = false
}

// Block cursor moved by d, scrolled into view
func (tr *Transcript) moveCursor(d int) {
	if len(tr.blocks) == 0 {
		return
	}
	if tr.cursor < 0 {
		tr.cursor = tr.nearest()
	} else {
		tr.cursor = min(max(tr.cursor+d, 0), len(tr.blocks)-1)
	}
	tr.dirty = true
	tr.reveal(tr.cursor)
}

// Block whose first line is nearest the top of the viewport
func (tr *Transcript) nearest() int {
	y := tr.vp.YOffset()
	if y < len(tr.lineBlock) {
		return tr.lineBlock[y]
	}
	return max(0, len(tr.blocks)-1)
}

func (tr *Transcript) reveal(i int) {
	if i < 0 || i >= len(tr.blockLine) {
		return
	}
	tr.vp.SetYOffset(tr.blockLine[i])
	tr.follow = tr.vp.AtBottom()
}

// Moves to the previous or next user or assistant turn boundary
func (tr *Transcript) jumpTurn(d int) {
	if len(tr.blocks) == 0 {
		return
	}
	cur := tr.nearest()
	for i := cur + d; i >= 0 && i < len(tr.blocks); i += d {
		if tr.blocks[i].kind == bUser || (tr.blocks[i].kind == bText && tr.blocks[i].round == 1) {
			tr.cursor = i
			tr.reveal(i)
			return
		}
	}
	if d < 0 {
		tr.vp.GotoTop()
	} else {
		tr.vp.GotoBottom()
	}
}

func (tr *Transcript) toggle(i int) {
	if i < 0 || i >= len(tr.blocks) {
		return
	}
	b := tr.blocks[i]
	switch b.kind {
	case bTool, bThinking, bCompact, bUser, bSteer:
		if b.expanded && !b.full {
			b.full = true
		} else {
			b.expanded = !b.expanded
			b.full = false
		}
		b.cacheKey = ""
		tr.dirty = true
	}
}

func (tr *Transcript) expandAll(on bool) {
	for _, b := range tr.blocks {
		if b.kind == bTool {
			b.expanded, b.full = on, false
			b.cacheKey = ""
		}
	}
	tr.dirty = true
}

func (tr *Transcript) invalidate(kind blockKind) {
	for _, b := range tr.blocks {
		if b.kind == kind {
			b.cacheKey = ""
		}
	}
	tr.dirty = true
}

func (tr *Transcript) add(b *block) *block {
	tr.blocks = append(tr.blocks, b)
	tr.dirty = true
	return b
}

// Folds one agent event into the blocks
func (tr *Transcript) Apply(ev agent.Event) {
	at := ev.Time
	switch ev.Type {
	case agent.EventRunStart:
		tr.add(&block{kind: bUser, agent: ev.Agent, at: at, parts: ev.Parts, done: true})
		tr.turns++
	case agent.EventTurnStart:
		tr.turn = map[int]*block{}
		tr.streaming = true
	case agent.EventModel:
		tr.model(ev)
	case agent.EventTurnEnd:
		tr.streaming = false
		for _, b := range tr.turn {
			b.done = true
			b.cacheKey = ""
		}
		if ev.Response != nil {
			for _, b := range tr.turn {
				b.model = ev.Response.Model
			}
		}
		tr.dirty = true
	case agent.EventToolStart:
		b := tr.toolBlock(ev.Call, ev.Agent, ev.Round, at)
		b.started = at
		b.cacheKey = ""
		tr.running++
		tr.dirty = true
	case agent.EventToolEnd:
		b := tr.toolBlock(ev.Call, ev.Agent, ev.Round, at)
		res := ev.Result
		b.result = &res
		b.data = ev.Data
		b.duration = ev.Duration
		b.err = ev.Err
		b.done = true
		if b.duration == 0 && !b.started.IsZero() && !at.IsZero() {
			b.duration = at.Sub(b.started)
		}
		b.cacheKey = ""
		if tr.running > 0 {
			tr.running--
		}
		tr.dirty = true
	case agent.EventApproval:
		tr.approval(ev)
	case agent.EventSteer:
		b := tr.add(&block{kind: bSteer, agent: ev.Agent, at: at, parts: ev.Parts, text: ev.Text, done: true})
		b.expanded = true
	case agent.EventCompactStart:
		tr.add(&block{kind: bCompact, agent: ev.Agent, at: at, text: ev.Text})
	case agent.EventCompactEnd:
		b := tr.lastOf(bCompact)
		if b == nil {
			b = tr.add(&block{kind: bCompact, agent: ev.Agent, at: at})
		}
		b.text = ev.Text
		b.data = ev.Data
		b.done = true
		b.cacheKey = ""
		tr.dirty = true
	case agent.EventHook:
		tr.add(&block{kind: bHook, agent: ev.Agent, at: at, text: ev.Text, data: ev.Data, done: true})
	case agent.EventNote:
		tr.add(&block{kind: bNote, agent: ev.Agent, at: at, text: ev.Text, done: true})
	case agent.EventSubagentStart:
		tr.add(&block{kind: bChild, agent: ev.Agent, at: at, call: ev.Call, text: ev.Text, data: ev.Data})
	case agent.EventSubagentEnd:
		b := tr.childBlock(ev.Call.Name)
		if b == nil {
			b = tr.add(&block{kind: bChild, agent: ev.Agent, at: at, call: ev.Call})
		}
		b.done = true
		b.err = ev.Err
		b.data = ev.Data
		if ev.Response != nil {
			b.text = ev.Response.Text()
		}
		b.cacheKey = ""
		tr.dirty = true
	case agent.EventInterrupt:
		tr.add(&block{kind: bNote, agent: ev.Agent, at: at, text: "interrupted", done: true})
	case agent.EventRunEnd:
		tr.streaming = false
		tr.running = 0
		if ev.Err != nil {
			tr.add(&block{kind: bError, agent: ev.Agent, at: at, err: ev.Err, done: true})
		}
		tr.dirty = true
	}
}

// Rebuilds the blocks of the streaming turn from the response assembled so far
func (tr *Transcript) model(ev agent.Event) {
	resp := ev.Model.Response
	for i, p := range resp.Content {
		b, ok := tr.turn[i]
		switch v := p.(type) {
		case protocol.Text:
			if !ok {
				b = tr.add(&block{kind: bText, agent: ev.Agent, round: ev.Round, at: ev.Time, model: resp.Model})
				tr.turn[i] = b
			}
			if b.text != v.Text {
				b.text = v.Text
				b.cacheKey = ""
				tr.dirty = true
			}
		case protocol.Thinking:
			if !ok {
				b = tr.add(&block{kind: bThinking, agent: ev.Agent, round: ev.Round, at: ev.Time})
				tr.turn[i] = b
			}
			text := v.Text
			if v.Redacted && text == "" {
				text = "[redacted]"
			}
			if b.text != text {
				b.text = text
				b.cacheKey = ""
				tr.dirty = true
			}
		case protocol.ToolCall:
			if !ok {
				b = tr.toolBlock(v, ev.Agent, ev.Round, ev.Time)
				tr.turn[i] = b
			}
			if v.ID != "" && b.call.ID == "" {
				b.call = v
				tr.tools[v.ID] = b
			}
			if v.Name != "" {
				b.call.Name = v.Name
			}
			if len(v.Args) > 0 {
				b.call.Args = v.Args
			}
			if ev.Model.Type == protocol.EventToolCallDelta && ev.Model.Index == i {
				b.partial = ev.Model.Partial
			}
			b.cacheKey = ""
			tr.dirty = true
		}
	}
	switch ev.Model.Type {
	case protocol.EventTextEnd, protocol.EventThinkingEnd, protocol.EventToolCallEnd:
		if b, ok := tr.turn[ev.Model.Index]; ok {
			b.done = true
			b.cacheKey = ""
		}
	}
}

// Tool block for a call id, created when the model events did not announce it
func (tr *Transcript) toolBlock(call protocol.ToolCall, agentName string, round int, at time.Time) *block {
	if b, ok := tr.tools[call.ID]; ok && call.ID != "" {
		if call.Name != "" {
			b.call.Name = call.Name
		}
		if len(call.Args) > 0 {
			b.call.Args = call.Args
		}
		return b
	}
	b := tr.add(&block{kind: bTool, agent: agentName, round: round, at: at, call: call})
	if call.ID != "" {
		tr.tools[call.ID] = b
	}
	return b
}

func (tr *Transcript) childBlock(name string) *block {
	for i := len(tr.blocks) - 1; i >= 0; i-- {
		if b := tr.blocks[i]; b.kind == bChild && b.call.Name == name {
			return b
		}
	}
	return nil
}

func (tr *Transcript) lastOf(kind blockKind) *block {
	for i := len(tr.blocks) - 1; i >= 0; i-- {
		if tr.blocks[i].kind == kind {
			return tr.blocks[i]
		}
	}
	return nil
}

func (tr *Transcript) approval(ev agent.Event) {
	if ev.Approval == nil {
		return
	}
	a := ev.Approval
	info := &approvalInfo{Tool: a.Request.Call.Name, CallID: a.Request.Call.ID, Pending: a.Pending, Allowed: a.Answer.Allowed, By: a.Answer.By, Key: a.Request.Key, Reason: a.Answer.Reason}
	if a.Answer.By == "person" {
		info.Verdict = a.Answer.Verdict.String()
	}
	if a.Err != nil {
		info.Reason = a.Err.Error()
	}
	for i := len(tr.blocks) - 1; i >= 0; i-- {
		b := tr.blocks[i]
		if b.kind == bApproval && b.approval.CallID == info.CallID && b.approval.Pending {
			b.approval = info
			b.done = !info.Pending
			b.cacheKey = ""
			tr.dirty = true
			return
		}
	}
	tr.add(&block{kind: bApproval, agent: ev.Agent, at: ev.Time, approval: info, done: !info.Pending})
}

// Rebuilds the blocks from stored entries and records, merged by sequence
func (tr *Transcript) Load(entries []session.Entry, records []session.Record) {
	tr.blocks = nil
	tr.turn = map[int]*block{}
	tr.tools = map[string]*block{}
	tr.turns = 0
	type item struct {
		seq int
		e   *session.Entry
		r   *session.Record
	}
	items := make([]item, 0, len(entries)+len(records))
	for i := range entries {
		items = append(items, item{seq: entries[i].Seq, e: &entries[i]})
	}
	for i := range records {
		items = append(items, item{seq: records[i].Seq, r: &records[i]})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].seq < items[j].seq })
	for _, it := range items {
		if it.e != nil {
			tr.loadEntry(*it.e)
		} else {
			tr.loadRecord(*it.r)
		}
	}
	tr.streaming, tr.running = false, 0
	tr.dirty = true
}

func (tr *Transcript) loadEntry(e session.Entry) {
	switch e.Role {
	case protocol.RoleUser:
		if len(e.Content) == 1 {
			if t, ok := e.Content[0].(protocol.Text); ok && strings.HasPrefix(t.Text, "Summary of the earlier conversation:") {
				tr.add(&block{kind: bCompact, at: e.Time, text: t.Text, data: "compaction summary", done: true})
				return
			}
		}
		tr.add(&block{kind: bUser, agent: e.Agent, at: e.Time, parts: e.Content, done: true})
		tr.turns++
	case protocol.RoleAssistant:
		for _, p := range e.Content {
			switch v := p.(type) {
			case protocol.Text:
				if v.Text == session.Ack {
					continue
				}
				tr.add(&block{kind: bText, agent: e.Agent, model: e.Model, at: e.Time, text: v.Text, done: true})
			case protocol.Thinking:
				text := v.Text
				if v.Redacted && text == "" {
					text = "[redacted]"
				}
				tr.add(&block{kind: bThinking, agent: e.Agent, at: e.Time, text: text, done: true})
			case protocol.ToolCall:
				b := tr.add(&block{kind: bTool, agent: e.Agent, at: e.Time, call: v, started: e.Time})
				if v.ID != "" {
					tr.tools[v.ID] = b
				}
			}
		}
	case protocol.RoleTool:
		for _, p := range e.Content {
			r, ok := p.(protocol.ToolResult)
			if !ok {
				continue
			}
			b, found := tr.tools[r.CallID]
			if !found {
				b = tr.add(&block{kind: bTool, agent: e.Agent, at: e.Time, call: protocol.ToolCall{ID: r.CallID, Name: r.Name}})
			}
			res := r
			b.result = &res
			b.done = true
			if !b.started.IsZero() && !e.Time.IsZero() {
				b.duration = e.Time.Sub(b.started)
			}
		}
	}
}

func (tr *Transcript) loadRecord(r session.Record) {
	switch r.Kind {
	case "approval":
		var d struct {
			Tool    string `json:"tool"`
			CallID  string `json:"call_id"`
			Allowed bool   `json:"allowed"`
			By      string `json:"by"`
			Verdict string `json:"verdict"`
			Key     string `json:"key"`
			Reason  string `json:"reason"`
			Error   string `json:"error"`
		}
		if json.Unmarshal(r.Data, &d) != nil {
			return
		}
		info := &approvalInfo{Tool: d.Tool, CallID: d.CallID, Allowed: d.Allowed, By: d.By, Verdict: d.Verdict, Key: d.Key, Reason: d.Reason}
		if d.Error != "" {
			info.Reason = d.Error
		}
		if d.By != "person" {
			info.Verdict = ""
		}
		tr.add(&block{kind: bApproval, agent: r.Agent, at: r.Time, approval: info, done: true})
	case "compact":
		var d struct {
			Before  int    `json:"before"`
			After   int    `json:"after"`
			Summary string `json:"summary"`
		}
		if json.Unmarshal(r.Data, &d) != nil {
			return
		}
		tr.add(&block{kind: bCompact, agent: r.Agent, at: r.Time, text: d.Summary, data: map[string]int{"before": d.Before, "after": d.After}, done: true})
	case "interrupt":
		tr.add(&block{kind: bNote, agent: r.Agent, at: r.Time, text: "interrupted", done: true})
	case "run_end":
		var d struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(r.Data, &d) == nil && d.Error != "" {
			tr.add(&block{kind: bError, agent: r.Agent, at: r.Time, err: fmt.Errorf("%s", d.Error), done: true})
		}
	case facts.RecordKind:
		var c facts.Change
		if json.Unmarshal(r.Data, &c) == nil && c.Op == facts.OpAdd {
			tr.add(&block{kind: bNote, agent: r.Agent, at: r.Time, text: "recorded " + c.Fact.Kind + " " + c.Fact.ID + ": " + c.Fact.Title, done: true})
		}
	}
}

// Color that stands for an agent, stable across renders
func (tr *Transcript) agentColor(name string) color.Color {
	t := tr.env.Theme
	if name == "" {
		return t.Ice
	}
	if c, ok := tr.colors[name]; ok {
		return c
	}
	palette := []color.Color{t.Ice, t.Pulse, t.Alive, t.Accent}
	h := fnv.New32a()
	h.Write([]byte(name))
	c := palette[int(h.Sum32())%len(palette)]
	tr.colors[name] = c
	return c
}

// Lines of the whole transcript at width, deterministic for the same blocks and state
func (tr *Transcript) Lines(width int) []string {
	var out []string
	tr.lineBlock = tr.lineBlock[:0]
	tr.blockLine = tr.blockLine[:0]
	for i, b := range tr.blocks {
		lines := tr.renderBlock(i, b, width)
		tr.blockLine = append(tr.blockLine, len(out))
		for range lines {
			tr.lineBlock = append(tr.lineBlock, i)
		}
		out = append(out, lines...)
	}
	return out
}

// Reports whether the block paints a spinner or elapsed time and so changes with every tick
func (b *block) animated() bool {
	if b.kind == bTool {
		return b.result == nil && (!b.started.IsZero() || !b.done)
	}
	return !b.done
}

func (tr *Transcript) stateKey(i int, b *block, width int) string {
	k := fmt.Sprintf("%d:%d:%t:%t:%t:%t:%d", width, len(b.text), b.done, b.expanded, b.full, tr.cursor == i, len(b.partial))
	if b.animated() {
		k += fmt.Sprintf(":%d", tr.tick)
	}
	if b.kind == bThinking {
		k += fmt.Sprintf(":%t", tr.thinking)
	}
	if b.result != nil {
		k += ":r"
	}
	if b.approval != nil {
		k += fmt.Sprintf(":%t", b.approval.Pending)
	}
	return k
}

func (tr *Transcript) renderBlock(i int, b *block, width int) []string {
	key := tr.stateKey(i, b, width)
	if b.cacheKey == key && b.cache != nil {
		return b.cache
	}
	t := tr.env.Theme
	var lines []string
	mark := "  "
	if tr.cursor == i {
		mark = lipgloss.NewStyle().Foreground(t.Accent).Render("▸ ")
	}
	stamp := render.Dim(b.at.Local().Format("15:04:05"), t)
	body := max(width-2, 10)
	wrap := func(s string) []string {
		var ls []string
		for _, l := range strings.Split(render.Wrap(s, body), "\n") {
			ls = append(ls, "  "+l)
		}
		return ls
	}
	switch b.kind {
	case bUser, bSteer:
		label := "you"
		glyph := "▌"
		c := t.Accent
		if b.kind == bSteer {
			label, glyph = "steer", "↪"
			if b.text != "" {
				label = b.text
			}
			c = t.Pulse
		}
		lines = append(lines, mark+lipgloss.NewStyle().Foreground(c).Bold(true).Render(glyph+" "+label)+" "+stamp)
		text, extras := partsText(b.parts)
		shown := wrap(text)
		if !b.full && len(shown) > previewLines {
			shown = append(shown[:previewLines], "  "+render.Dim(fmt.Sprintf("… %d more lines, o to show", len(shown)-previewLines), t))
		}
		lines = append(lines, shown...)
		for _, e := range extras {
			lines = append(lines, "  "+render.Warm(e, t))
		}
	case bText:
		c := tr.agentColor(b.agent)
		name := b.agent
		if name == "" {
			name = "agent"
		}
		head := lipgloss.NewStyle().Foreground(c).Bold(true).Render("▌ " + name)
		if b.model != "" {
			head += " " + render.Dim(b.model, t)
		}
		head += " " + stamp
		if !b.done {
			head += " " + render.Cool(render.Spinner(tr.tick, t), t)
		}
		lines = append(lines, mark+head)
		for _, l := range strings.Split(render.MarkdownText(b.text, body, t), "\n") {
			lines = append(lines, "  "+l)
		}
	case bThinking:
		n := len([]rune(b.text))
		head := render.Dim(fmt.Sprintf("· thinking %s", render.Tokens(n/4)), t)
		if !b.done {
			head += " " + render.Dim(render.Spinner(tr.tick, t), t)
		}
		lines = append(lines, mark+head)
		show := tr.thinking || b.expanded
		switch {
		case show:
			for _, l := range strings.Split(render.Wrap(b.text, body), "\n") {
				lines = append(lines, "  "+render.Dim(l, t))
			}
		case !b.done:
			tail := render.Lines(render.Wrap(b.text, body))
			if len(tail) > 3 {
				tail = tail[len(tail)-3:]
			}
			for _, l := range tail {
				lines = append(lines, "  "+render.Dim(l, t))
			}
		}
	case bTool:
		lines = append(lines, tr.toolLines(mark, b, width, body)...)
	case bApproval:
		a := b.approval
		glyph := lipgloss.NewStyle().Foreground(t.Pulse).Bold(true).Render("⚑")
		var text string
		switch {
		case a.Pending:
			text = render.Hot("awaiting approval for "+a.Tool, t) + " " + render.Dim(render.Spinner(tr.tick, t), t)
		case a.Allowed:
			text = render.Good("allowed "+a.Tool, t) + " " + render.Dim("by "+a.By, t)
		default:
			text = render.Bad("denied "+a.Tool, t) + " " + render.Dim("by "+a.By, t)
		}
		if a.Verdict != "" {
			text += render.Dim(" ("+a.Verdict+")", t)
		}
		if a.Key != "" && (a.Verdict == "always" || a.Verdict == "never") {
			text += " " + render.Cool(a.Key, t)
		}
		if a.Reason != "" && !a.Allowed && !a.Pending {
			text += " " + render.Dim(render.Truncate(a.Reason, body/2), t)
		}
		lines = append(lines, mark+glyph+" "+text+" "+stamp)
	case bCompact:
		head := render.Warm("≡ compacting", t)
		if b.done {
			switch d := b.data.(type) {
			case map[string]int:
				head = render.Warm(fmt.Sprintf("≡ compacted %d messages into %d", d["before"], d["after"]), t)
			case string:
				head = render.Warm("≡ "+d, t)
			default:
				head = render.Warm("≡ compacted", t)
			}
		} else {
			head += " " + render.Dim(render.Spinner(tr.tick, t), t)
		}
		lines = append(lines, mark+head+" "+stamp)
		if b.expanded && b.text != "" {
			lines = append(lines, wrap(render.Dim(b.text, t))...)
		}
	case bHook:
		lines = append(lines, mark+render.Dim("⚙ "+render.Truncate(b.text, body), t))
		if ex, ok := b.data.(hooks.Execution); ok && strings.TrimSpace(ex.Stdout) != "" {
			for _, l := range render.Lines(render.Tail(strings.TrimSpace(ex.Stdout), 5)) {
				lines = append(lines, "    "+render.Dim(render.Truncate(l, body-2), t))
			}
		}
	case bNote:
		lines = append(lines, mark+render.Dim("· "+render.Truncate(b.text, body), t))
	case bError:
		lines = append(lines, mark+render.Bad("✗ "+render.Truncate(b.err.Error(), body), t)+" "+stamp)
	case bChild:
		name := b.call.Name
		head := lipgloss.NewStyle().Foreground(tr.agentColor(name)).Bold(true).Render("☍ " + name)
		if b.text != "" && !b.done {
			head += " " + render.Dim(render.Truncate(b.text, body/2), t)
		}
		switch {
		case !b.done:
			head += " " + render.Cool(render.Spinner(tr.tick, t)+" working", t)
		case b.err != nil:
			head += " " + render.Bad(render.Truncate(b.err.Error(), body/2), t)
		default:
			head += " " + render.Good("done", t)
			if b.text != "" {
				head += " " + render.Dim(render.Summary(b.text, body/2), t)
			}
		}
		lines = append(lines, mark+head+" "+stamp)
	}
	lines = append(lines, "")
	b.cache, b.cacheKey = lines, key
	return lines
}

// Text of input parts with a label for every attachment
func partsText(parts []protocol.Part) (string, []string) {
	var sb strings.Builder
	var extras []string
	for _, p := range parts {
		switch v := p.(type) {
		case protocol.Text:
			sb.WriteString(v.Text)
		case protocol.Image:
			extras = append(extras, fmt.Sprintf("⌦ image %s %s", v.MediaType, render.Bytes(int64(len(v.Data)))))
		case protocol.Document:
			extras = append(extras, fmt.Sprintf("⌦ %s %s", v.Name, render.Bytes(int64(len(v.Data)))))
		}
	}
	return sb.String(), extras
}

// Short description of a call from its arguments
func callSummary(call protocol.ToolCall, partial string) string {
	raw := call.Args
	if len(raw) == 0 && partial != "" {
		return render.Truncate(strings.TrimSpace(partial), 60)
	}
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil {
		return ""
	}
	str := func(k string) string {
		s, _ := args[k].(string)
		return s
	}
	switch call.Name {
	case files.ReadName:
		s := str("file_path")
		if off, ok := args["offset"].(float64); ok && off > 0 {
			s += fmt.Sprintf(" from %d", int(off))
		}
		return s
	case files.WriteName, files.EditName:
		return str("file_path")
	case files.GlobName, files.GrepName:
		s := str("pattern")
		if p := str("path"); p != "" {
			s += " in " + p
		}
		return s
	case shell.BashName:
		if d := str("description"); d != "" {
			return d
		}
		return strings.Join(strings.Fields(str("command")), " ")
	case shell.OutputName:
		return str("bash_id")
	case shell.KillName:
		return str("shell_id")
	case "WebFetch":
		return str("url")
	case "Agent":
		if d := str("description"); d != "" {
			return d
		}
		return render.Summary(str("prompt"), 60)
	case facts.ToolName:
		return str("title")
	case mission.ToolName:
		if steps, ok := args["steps"].([]any); ok {
			return fmt.Sprintf("%d steps", len(steps))
		}
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		if s, ok := args[k].(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

func (tr *Transcript) toolLines(mark string, b *block, width, body int) []string {
	t := tr.env.Theme
	glyph := render.ToolGlyph(b.call.Name)
	name := b.call.Name
	if name == "" {
		name = "tool"
	}
	head := lipgloss.NewStyle().Foreground(tr.agentColor(b.agent)).Bold(true).Render(glyph+" "+name) + " " + lipgloss.NewStyle().Foreground(t.Text).Render(render.Truncate(callSummary(b.call, b.partial), body/2))
	var status string
	switch {
	case b.result == nil && !b.started.IsZero():
		status = render.Cool(render.Spinner(tr.tick, t)+" "+render.Elapsed(tr.env.now().Sub(b.started)), t)
	case b.result == nil:
		status = render.Dim("pending", t)
		if !b.done {
			status = render.Dim(render.Spinner(tr.tick, t)+" streaming", t)
		}
	case b.err != nil:
		status = render.Hot("refused", t)
	case b.result.IsError:
		status = render.Bad("error", t)
	default:
		status = render.Good("ok", t)
	}
	if b.duration > 0 {
		status += " " + render.Dim(render.Elapsed(b.duration), t)
	}
	if stat := tr.resultStat(b); stat != "" {
		status += " " + render.Dim(stat, t)
	}
	lines := []string{mark + head + " " + status}
	if b.err != nil {
		lines = append(lines, "  "+render.Hot("✗ "+render.Truncate(b.err.Error(), body-2), t))
		return lines
	}
	if b.result == nil && b.partial != "" && !b.done {
		lines = append(lines, "  "+render.Dim(render.Truncate(b.partial, body-2), t))
		return lines
	}
	if b.result == nil && b.partial != "" {
		tail := render.Lines(b.partial)
		if len(tail) > 6 {
			tail = tail[len(tail)-6:]
		}
		for _, l := range tail {
			lines = append(lines, "  "+render.Dim(render.Truncate(l, body-2), t))
		}
		return lines
	}
	if b.result == nil || !b.expanded {
		return lines
	}
	bodyLines := tr.resultLines(b, body-2)
	if b.result.IsError && (b.call.Name == files.WriteName || b.call.Name == files.EditName) && tr.env.PreImage != nil {
		if img, ok := tr.env.PreImage.Get(b.call.ID); ok {
			bodyLines = append(bodyLines, "", render.Warm("file before the call", t))
			switch {
			case img.Missing:
				bodyLines = append(bodyLines, render.Dim("did not exist", t))
			default:
				before := render.Lines(img.Content)
				shown := before
				if len(shown) > 20 {
					shown = shown[:20]
				}
				for i, l := range shown {
					bodyLines = append(bodyLines, render.Dim(fmt.Sprintf("%3d ", i+1), t)+render.Truncate(l, body-6))
				}
				if len(before) > 20 {
					bodyLines = append(bodyLines, render.Dim(fmt.Sprintf("… %d more lines", len(before)-20), t))
				}
			}
		}
	}
	limit := previewLines
	if b.full {
		limit = len(bodyLines)
	}
	if len(bodyLines) > limit {
		bodyLines = append(bodyLines[:limit], render.Dim(fmt.Sprintf("… %d more lines, o to show all", len(bodyLines)-limit), t))
	}
	for _, l := range bodyLines {
		lines = append(lines, "  "+l)
	}
	return lines
}

// Short count shown beside a finished tool
func (tr *Transcript) resultStat(b *block) string {
	switch d := b.data.(type) {
	case files.Change:
		a, r := render.Stat(render.Diff(d.Old, d.New))
		return fmt.Sprintf("+%d -%d", a, r)
	case files.ReadData:
		if d.Total > 0 {
			return fmt.Sprintf("%d of %d lines", len(d.Lines), d.Total)
		}
	case files.GlobData:
		return fmt.Sprintf("%d matches", d.Total)
	case files.GrepData:
		return fmt.Sprintf("%d hits in %d files", d.Total, len(d.Files))
	case shell.Run:
		return fmt.Sprintf("exit %d", d.ExitCode)
	case web.Page:
		return fmt.Sprintf("%d %s", d.Status, render.Bytes(int64(d.Bytes)))
	case subagent.Child:
		return "child " + d.Name
	}
	return ""
}

// Body lines of a finished tool, structured where the data allows and the text otherwise
func (tr *Transcript) resultLines(b *block, width int) []string {
	t := tr.env.Theme
	switch d := b.data.(type) {
	case files.Change:
		return strings.Split(render.FormatDiff(render.Diff(d.Old, d.New), width, t), "\n")
	case files.ReadData:
		if d.MediaType != "" {
			return []string{render.Dim(fmt.Sprintf("image %s %s", d.MediaType, render.Bytes(d.Bytes)), t)}
		}
		var out []string
		digits := len(fmt.Sprint(d.Offset + len(d.Lines)))
		lang := render.Language(d.Path)
		code := strings.Split(render.Code(strings.Join(d.Lines, "\n"), lang, d.Path, t), "\n")
		for i := range d.Lines {
			text := d.Lines[i]
			if i < len(code) {
				text = code[i]
			}
			out = append(out, render.Dim(fmt.Sprintf("%*d ", digits, d.Offset+i), t)+render.Truncate(text, width-digits-1))
		}
		return out
	case files.GlobData:
		var out []string
		for _, m := range d.Matches {
			out = append(out, render.Truncate(m.Path, width))
		}
		return out
	case files.GrepData:
		var out []string
		for _, f := range d.Files {
			out = append(out, render.Cool(render.Truncate(f.Path, width), t))
			for _, h := range f.Hits {
				out = append(out, render.Dim(fmt.Sprintf("  %d ", h.Line), t)+render.Truncate(h.Text, width-8))
			}
		}
		return out
	case shell.Run:
		var out []string
		for _, l := range render.Lines(strings.TrimRight(d.Stdout, "\n")) {
			out = append(out, render.Truncate(l, width))
		}
		for _, l := range render.Lines(strings.TrimRight(d.Stderr, "\n")) {
			out = append(out, render.Bad(render.Truncate(l, width), t))
		}
		if d.TimedOut {
			out = append(out, render.Bad("timed out", t))
		}
		return out
	case web.Page:
		out := []string{render.KV("title", d.Title, t), render.KV("url", d.FinalURL, t)}
		for _, l := range render.Lines(d.Text) {
			out = append(out, render.Truncate(l, width))
		}
		if d.Answer != "" {
			out = append(out, "", render.Warm("answer", t))
			out = append(out, strings.Split(render.Wrap(d.Answer, width), "\n")...)
		}
		return out
	}
	var out []string
	for _, p := range b.result.Content {
		switch v := p.(type) {
		case protocol.Text:
			for _, l := range strings.Split(render.Wrap(v.Text, width), "\n") {
				if b.result.IsError {
					l = render.Bad(l, t)
				}
				out = append(out, l)
			}
		case protocol.Image:
			out = append(out, render.Dim(fmt.Sprintf("image %s %s", v.MediaType, render.Bytes(int64(len(v.Data)))), t))
		}
	}
	return out
}

func (tr *Transcript) View(w, h int) string {
	if w != tr.w || h != tr.h {
		tr.w, tr.h = w, h
		tr.vp.SetWidth(w)
		tr.vp.SetHeight(h)
		tr.dirty = true
		for _, b := range tr.blocks {
			b.cacheKey = ""
		}
	}
	if tr.dirty {
		tr.lines = tr.Lines(w)
		tr.vp.SetContentLines(tr.lines)
		if tr.query != "" && !tr.searching {
			tr.applySearch()
		}
		if tr.follow {
			tr.vp.GotoBottom()
		}
		tr.dirty = false
	}
	if tr.searching {
		t := tr.env.Theme
		prompt := lipgloss.NewStyle().Foreground(t.Accent).Render("/") + lipgloss.NewStyle().Foreground(t.Text).Render(tr.query)
		if h <= 1 {
			return prompt
		}
		tr.vp.SetHeight(h - 1)
		out := tr.vp.View() + "\n" + render.Fit(prompt, w)
		tr.vp.SetHeight(h)
		return out
	}
	return tr.vp.View()
}

func (tr *Transcript) Status() string {
	var parts []string
	if tr.turns > 0 {
		parts = append(parts, fmt.Sprintf("%d turns", tr.turns))
	}
	switch {
	case tr.streaming:
		parts = append(parts, "streaming")
	case tr.running > 0:
		parts = append(parts, fmt.Sprintf("%d tools", tr.running))
	case !tr.follow:
		parts = append(parts, "paused")
	}
	if tr.query != "" && !tr.searching {
		parts = append(parts, fmt.Sprintf("%d matches", tr.matches))
	}
	return strings.Join(parts, " · ")
}
