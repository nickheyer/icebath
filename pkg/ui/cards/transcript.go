package cards

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"image/color"
	"iter"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool/files"
	"github.com/nickheyer/icebath/pkg/tool/shell"
	"github.com/nickheyer/icebath/pkg/tool/web"
	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Lines one notch of the mouse wheel scrolls
const wheelLines = 3

// Pages the painted offset may trail the viewport by before it skips ahead, so long jumps stay short
const glideReach = 3.0

// Lines a collapsed tool result or read shows before it is cut
const previewLines = 40

// Lines above the bottom the view may sit and still follow new lines
const followSlack = 2

// Cells a floating button covered in the last view, none while it was hidden
type hotspot struct{ x, y, w int }

func (h hotspot) hit(x, y int) bool { return h.w > 0 && y == h.y && x >= h.x && x < h.x+h.w }

// Longest press that still counts as a click rather than a hold
const clickHold = 400 * time.Millisecond

// Left press on a block, its cell and time, toggling the block when released on the same cell within clickHold
type press struct {
	block, x, y int
	at          time.Time
}

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
	kind  blockKind
	agent string
	model string
	round int
	at    time.Time
	text  string
	// Sources the model attached to the text
	cites   []protocol.Citation
	parts   []protocol.Part
	done    bool
	call    protocol.ToolCall
	partial string
	result  *protocol.ToolResult
	data    any
	// Count a running tool reported done and the count it works toward, zero while it reported none
	progressDone  int
	progressTotal int
	started       time.Time
	duration      time.Duration
	err           error
	approval      *approvalInfo
	expanded      bool
	full          bool
	// Runes of a thought settled before its newest arrived, and when they arrived
	settled int
	grown   time.Time
	// Lines last rendered and the state they were rendered in, standing in while the state has moved on
	cache    []string
	cacheKey renderKey
	// Markdown of the settled paragraphs of a streaming block, keyed by their length, width and theme
	head    string
	headKey string
}

// Everything the lines of a block depend on beyond the theme, the zero key matching no rendered block
type renderKey struct {
	width, text, partial, tick, settled           int
	done, expanded, full, cursor, result, pending bool
}

// Thinking blocks churn their newest runes this long before they settle
const settleTime = 400 * time.Millisecond

// Most runes a thinking block keeps churning at once
const churnRunes = 48

// Runes of the text settled by now, the newest still churning, all of them when no arrival was timed
func (b *block) resolved(now time.Time) int {
	n := utf8.RuneCountInString(b.text)
	if b.grown.IsZero() || now.IsZero() {
		return n
	}
	age := now.Sub(b.grown)
	if age < 0 {
		return n
	}
	pending := max(n-b.settled, 0)
	done := int(float64(pending) * min(float64(age)/float64(settleTime), 1))
	return min(n, max(b.settled+done, n-churnRunes, 0))
}

// Reports whether the newest runes of a thought are still churning
func (b *block) churning(now time.Time) bool {
	return b.kind == bThinking && b.resolved(now) < utf8.RuneCountInString(b.text)
}

// Interval between repaints of spinners, elapsed times and churning thoughts
const transcriptTickEvery = 120 * time.Millisecond

// Streaming transcript of one agent lineage with tool rows, diffs, approvals, markers, search and turn jumps
type Transcript struct {
	env    *Env
	name   string
	key    string
	blocks []*block
	turn   map[int]*block
	tools  map[string]*block
	vp     scroll
	follow bool
	// Line painted at the top, gliding after the viewport offset
	shown anim.Value
	glide bool
	w, h  int
	lines []string
	// Lines handed to the view, the lines with every match lit while a search is on
	painted []string
	// Count of times the painted lines changed, keying the view cache
	gen       int
	view      string
	viewKey   viewKey
	lineBlock []int
	blockLine []int
	cursor    int
	search    searchBar
	found     []match
	current   int
	seek      bool
	themed    string
	tick      int
	timer     ticker
	streaming bool
	running   int
	dirty     bool
	// Loads thinking from the session, every thinking block left out while off
	thinking bool
	turns    int
	// Blocks the last layout left with lines from an earlier state, rendered a few per frame
	stale int
	// Whether the view was drawn since the last frame, so only a card on the stage asks for the next
	viewed bool
	// Run id followed, every run when empty
	Run string
	// Agent name followed, the root lineage when empty
	Agent string
	// Accepts only root events when no agent is named
	rootOnly bool
	// Actions whose keys cap the scrollbar
	pages [2]string
	// Whether the last view had room for the scrollbar
	bar bool
	// Scroll to latest button as last drawn
	latest hotspot
	// Press awaiting its release, nil between clicks
	pressed *press
}

// Everything the rendered view depends on beyond the painted lines
type viewKey struct {
	w, h, offset, gen int
	theme, up, down   string
	follow            bool
}

// One search match on a transcript line, in cells
type match struct {
	line, start, end int
}

// Builds the transcript of the root lineage
func NewTranscript(env *Env) *Transcript {
	return &Transcript{env: env, name: "TRACE", key: "trace", turn: map[int]*block{}, tools: map[string]*block{}, follow: true, cursor: -1, current: -1, rootOnly: true, thinking: true, pages: [2]string{"session.scroll.up", "session.scroll.down"}, shown: anim.NewValueWith(0, anim.Snappy), timer: ticker{every: transcriptTickEvery}}
}

// Builds a transcript that follows one child agent of a run
func NewChildTranscript(env *Env, run, agentName string) *Transcript {
	tr := NewTranscript(env)
	tr.name = "TRACE " + agentName
	tr.key = "trace:" + run + "/" + agentName
	tr.Run, tr.Agent, tr.rootOnly = run, agentName, false
	tr.pages = [2]string{"transcript.page.up", "transcript.page.down"}
	return tr
}

func (tr *Transcript) Title() string       { return tr.name }
func (tr *Transcript) Glyph() string       { return tr.env.Theme.Glyphs.Trace }
func (tr *Transcript) Key() string         { return tr.key }
func (tr *Transcript) Selectable() bool    { return true }
func (tr *Transcript) MinSize() (int, int) { return 24, 4 }

// Starts a fresh repaint loop in place of one whose tick was lost while the card was closed
func (tr *Transcript) Init() tea.Cmd {
	tr.timer.reset()
	return tr.ensureTick()
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

func (tr *Transcript) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
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
			if msg.Progress.Done > 0 || msg.Progress.Total > 0 {
				b.progressDone, b.progressTotal = msg.Progress.Done, msg.Progress.Total
			}
			b.cacheKey = renderKey{}
			tr.dirty = true
		}
		return tr, nil
	case tickMsg:
		if !tr.timer.owns(msg) {
			return tr, nil
		}
		tr.tick++
		if tr.animating() {
			tr.dirty = true
		}
		return tr, tr.ensureTick()
	case wm.FocusMsg:
		if !msg.Focused {
			tr.search.Accept()
		}
		return tr, nil
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelDown:
			tr.ScrollBy(wheelLines)
		case tea.MouseWheelUp:
			tr.ScrollBy(-wheelLines)
		}
		return tr, nil
	case tea.MouseClickMsg:
		tr.pressed = nil
		if msg.Button != tea.MouseLeft {
			return tr, nil
		}
		if tr.latest.hit(msg.X, msg.Y) {
			tr.Follow()
			return tr, nil
		}
		if tr.gutterShown() && msg.X > tr.w {
			tr.clickGutter(msg.Y)
			return tr, nil
		}
		line := tr.vp.YOffset() + msg.Y
		if line >= 0 && line < len(tr.lineBlock) {
			tr.cursor = tr.lineBlock[line]
			tr.dirty = true
			tr.pressed = &press{block: tr.cursor, x: msg.X, y: msg.Y, at: time.Now()}
		}
		return tr, nil
	case tea.MouseReleaseMsg:
		p := tr.pressed
		tr.pressed = nil
		if p == nil || msg.X != p.x || msg.Y != p.y || time.Since(p.at) > clickHold {
			return tr, nil
		}
		line := tr.vp.YOffset() + msg.Y
		if line >= 0 && line < len(tr.lineBlock) && tr.lineBlock[line] == p.block {
			return tr, tr.toggle(p.block)
		}
		return tr, nil
	}
	return tr, nil
}

// Reports whether a recent thought still has runes churning
func (tr *Transcript) settling() bool {
	now := time.Now()
	for i := len(tr.blocks) - 1; i >= 0 && i >= len(tr.blocks)-8; i-- {
		if tr.blocks[i].churning(now) {
			return true
		}
	}
	return false
}

// Reports whether something on screen changes with every tick, a stream, a running tool or a churning thought
func (tr *Transcript) animating() bool {
	return tr.streaming || tr.running > 0 || tr.settling()
}

// Schedules the next repaint while anything animates and none is pending
func (tr *Transcript) ensureTick() tea.Cmd {
	if !tr.animating() {
		return nil
	}
	return tr.timer.start()
}

// Glides the painted offset after the viewport, jumping there when motion is off, and reports whether the view moved
func (tr *Transcript) Animate(dt float64) bool {
	target := float64(tr.vp.YOffset())
	tr.shown.Set(target)
	if dt <= 0 {
		changed := tr.shown.Pos != target
		tr.shown.Snap()
		tr.glide = false
		return changed || tr.wantsFrame()
	}
	if !tr.glide {
		tr.glide = true
		tr.shown.Snap()
		return tr.wantsFrame()
	}
	if reach := float64(max(tr.h, 1)) * glideReach; math.Abs(tr.shown.Pos-target) > reach {
		tr.shown.Pos = target + math.Copysign(reach, tr.shown.Pos-target)
	}
	return tr.shown.Step(dt) || tr.wantsFrame()
}

// Reports whether blocks wait to be rendered, once per view so a card off the stage asks for no frames
func (tr *Transcript) wantsFrame() bool {
	if tr.stale == 0 || !tr.viewed {
		return false
	}
	tr.viewed = false
	return true
}

// Line painted at the top, the viewport offset while the view is not gliding
func (tr *Transcript) paintedOffset() int {
	if !tr.glide {
		return tr.vp.YOffset()
	}
	return min(max(int(math.Round(tr.shown.Pos)), 0), tr.vp.maxOffset())
}

// Scrolls by n lines, down when positive
func (tr *Transcript) ScrollBy(n int) {
	if n > 0 {
		tr.vp.ScrollDown(n)
	} else {
		tr.vp.ScrollUp(-n)
	}
	tr.follow = tr.vp.NearBottom(followSlack)
}

// Scrolls a page down when d is positive and up otherwise
func (tr *Transcript) Page(d int) {
	if d > 0 {
		tr.vp.PageDown()
	} else {
		tr.vp.PageUp()
	}
	tr.follow = tr.vp.NearBottom(followSlack)
}

// Scrolls to the first line
func (tr *Transcript) Top() {
	tr.vp.GotoTop()
	tr.follow = tr.vp.NearBottom(followSlack)
}

// Follows new lines or holds the view still
func (tr *Transcript) ToggleFollow() {
	if tr.follow {
		tr.follow = false
		return
	}
	tr.Follow()
}

// Carries out a search bar action, live matching as the query changes
func (tr *Transcript) SearchAction(id string) {
	switch id {
	case "search.accept":
		tr.search.Accept()
	case "search.close":
		tr.search.Close()
		tr.applySearch()
	case "search.next":
		tr.NextMatch(1)
	case "search.prev":
		tr.NextMatch(-1)
	default:
		if tr.search.Action(id) {
			tr.applySearch()
		}
	}
}

// Types text into the search bar and matches it at once
func (tr *Transcript) SearchType(text string) {
	tr.search.Type(text)
	tr.applySearch()
}

// Searches again from the top of the view, every block rendered first so no match hides in stale lines
func (tr *Transcript) applySearch() {
	tr.current = -1
	tr.seek = true
	if tr.stale > 0 && tr.w > 0 {
		tr.relayout(tr.w, 0)
	}
	if !tr.dirty {
		tr.settle()
	}
}

// Finds the matches, picks one at or below the view top when none is current and shows it after a search
func (tr *Transcript) settle() {
	tr.find()
	if tr.current < 0 {
		top := tr.vp.YOffset()
		for i, m := range tr.found {
			if m.line >= top {
				tr.current = i
				break
			}
		}
		if tr.current < 0 && len(tr.found) > 0 {
			tr.current = 0
		}
	}
	tr.paint()
	if tr.seek {
		tr.seek = false
		tr.showMatch()
	}
}

// Matches of the query on the plain text of every line
func (tr *Transcript) find() {
	tr.found = tr.found[:0]
	q := tr.search.Query()
	if q == "" {
		return
	}
	for i, l := range tr.lines {
		plain := ansi.Strip(l)
		for _, r := range findAll(plain, q) {
			tr.found = append(tr.found, match{line: i, start: ansi.StringWidth(plain[:r[0]]), end: ansi.StringWidth(plain[:r[1]])})
		}
	}
	if tr.current >= len(tr.found) {
		tr.current = len(tr.found) - 1
	}
}

// Hands the viewport the lines with every match lit and the current one lit brighter, keeping their colors
func (tr *Transcript) paint() {
	t := tr.env.Theme
	tr.gen++
	tr.vp.SetTotal(len(tr.lines))
	if len(tr.found) == 0 {
		tr.painted = tr.lines
		return
	}
	lit := lipgloss.NewStyle().Background(t.Accent).Foreground(t.Bg)
	hot := lipgloss.NewStyle().Background(t.Pulse).Foreground(t.Bg).Bold(true)
	out := append([]string(nil), tr.lines...)
	for i := 0; i < len(tr.found); {
		line := tr.found[i].line
		var ranges []lipgloss.Range
		for ; i < len(tr.found) && tr.found[i].line == line; i++ {
			st := lit
			if i == tr.current {
				st = hot
			}
			ranges = append(ranges, lipgloss.NewRange(tr.found[i].start, tr.found[i].end, st))
		}
		out[line] = lipgloss.StyleRanges(tr.lines[line], ranges...)
	}
	tr.painted = out
}

// Brings the current match into view
func (tr *Transcript) showMatch() {
	if tr.current < 0 || tr.current >= len(tr.found) {
		return
	}
	line := tr.found[tr.current].line
	if line < tr.vp.YOffset() || line >= tr.vp.YOffset()+tr.vp.Height() {
		tr.vp.SetYOffset(max(line-tr.vp.Height()/3, 0))
	}
	tr.follow = tr.vp.NearBottom(followSlack)
}

// Moves to the next match when d is positive and the previous otherwise, wrapping
func (tr *Transcript) NextMatch(d int) {
	n := len(tr.found)
	if n == 0 {
		return
	}
	tr.current = ((tr.current+d)%n + n) % n
	tr.paint()
	tr.showMatch()
}

// Block cursor moved by d, scrolled into view
func (tr *Transcript) MoveCursor(d int) {
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
	tr.follow = tr.vp.NearBottom(followSlack)
}

// Moves to the previous or next user or assistant turn boundary
func (tr *Transcript) JumpTurn(d int) {
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

// Expands or folds the block under the cursor, or opens the child agent the block stands for
func (tr *Transcript) Toggle() tea.Cmd {
	if tr.cursor < 0 {
		tr.cursor = tr.nearest()
	}
	return tr.toggle(tr.cursor)
}

func (tr *Transcript) toggle(i int) tea.Cmd {
	if i < 0 || i >= len(tr.blocks) {
		return nil
	}
	b := tr.blocks[i]
	switch b.kind {
	case bThinking, bCompact, bError:
		b.expanded = !b.expanded
	case bUser, bSteer:
		b.full = !b.full
	case bTool:
		if b.err == nil && b.expanded && !b.full {
			b.full = true
		} else {
			b.expanded, b.full = !b.expanded, false
		}
	case bChild:
		id := b.call.ID
		if id == "" {
			return nil
		}
		return func() tea.Msg { return OpenCallMsg{CallID: id} }
	default:
		return nil
	}
	b.cacheKey = renderKey{}
	tr.dirty = true
	return nil
}

func (tr *Transcript) expandAll(on bool) {
	for _, b := range tr.blocks {
		if b.kind == bTool {
			b.expanded, b.full = on, false
			b.cacheKey = renderKey{}
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
			b.cacheKey = renderKey{}
		}
		model := ""
		if ev.Response != nil {
			model = ev.Response.Model
			for _, b := range tr.turn {
				b.model = model
			}
		}
		if text := usageNote(model, ev.Usage); text != "" {
			tr.add(&block{kind: bNote, agent: ev.Agent, at: at, text: text, done: true})
		}
		if ev.Response != nil {
			if text := ev.Response.StopText(); text != "" {
				tr.add(&block{kind: bNote, agent: ev.Agent, at: at, text: text, done: true})
			}
		}
		tr.dirty = true
	case agent.EventToolStart:
		b := tr.toolBlock(ev.Call, ev.Agent, ev.Round, at)
		b.started = at
		b.partial = ""
		b.cacheKey = renderKey{}
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
		b.cacheKey = renderKey{}
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
		b.cacheKey = renderKey{}
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
		b.cacheKey = renderKey{}
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

// Rebuilds the streaming turn from the response so far, its blocks keyed by position among the parts shown
func (tr *Transcript) model(ev agent.Event) {
	resp := ev.Model.Response
	rebuilt := tr.misaligned(resp.Content)
	if rebuilt {
		tr.dropTurn()
	}
	at, j := -1, 0
	for i, p := range resp.Content {
		if !tr.keeps(p) {
			continue
		}
		if i == ev.Model.Index {
			at = j
		}
		b, ok := tr.turn[j]
		switch v := p.(type) {
		case protocol.Text:
			if !ok {
				b = tr.add(&block{kind: bText, agent: ev.Agent, round: ev.Round, at: ev.Time, model: resp.Model})
				tr.turn[j] = b
			}
			if b.text != v.Text || len(b.cites) != len(v.Citations) {
				b.text, b.cites = v.Text, v.Citations
				b.cacheKey = renderKey{}
				tr.dirty = true
			}
		case protocol.Thinking:
			if !ok {
				b = tr.add(&block{kind: bThinking, agent: ev.Agent, round: ev.Round, at: ev.Time, started: ev.Time, expanded: true})
				tr.turn[j] = b
			}
			text := v.Text
			if v.Redacted && text == "" {
				text = "[redacted]"
			}
			if b.text != text {
				b.settled = b.resolved(ev.Time)
				b.grown = ev.Time
				b.text = text
				b.cacheKey = renderKey{}
				tr.dirty = true
			}
		case protocol.ToolCall:
			if !ok {
				b = tr.toolBlock(v, ev.Agent, ev.Round, ev.Time)
				tr.turn[j] = b
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
			b.cacheKey = renderKey{}
			tr.dirty = true
		}
		j++
	}
	if rebuilt {
		for pos, b := range tr.turn {
			if pos < j-1 {
				b.done = true
				b.cacheKey = renderKey{}
			}
		}
	}
	switch ev.Model.Type {
	case protocol.EventTextEnd, protocol.EventThinkingEnd, protocol.EventToolCallEnd:
		if b, ok := tr.turn[at]; ok {
			b.done = true
			if ev.Model.Type == protocol.EventToolCallEnd {
				b.partial = ""
			}
			if ev.Model.Type == protocol.EventThinkingEnd && !b.started.IsZero() && !ev.Time.IsZero() {
				b.duration = ev.Time.Sub(b.started)
			}
			b.cacheKey = renderKey{}
		}
	}
}

// Reports whether a part of a streaming turn gets a block, thinking only while it is shown
func (tr *Transcript) keeps(p protocol.Part) bool {
	_, thinking := p.(protocol.Thinking)
	return tr.thinking || !thinking
}

// Kind of block a part of a streaming turn gets, negative for a part that gets none
func kindOf(p protocol.Part) blockKind {
	switch p.(type) {
	case protocol.Text:
		return bText
	case protocol.Thinking:
		return bThinking
	case protocol.ToolCall:
		return bTool
	}
	return -1
}

// Reports whether a block of the streaming turn is of another kind than the part now at its position
func (tr *Transcript) misaligned(parts []protocol.Part) bool {
	j := 0
	for _, p := range parts {
		if !tr.keeps(p) {
			continue
		}
		if b, ok := tr.turn[j]; ok && b.kind != kindOf(p) {
			return true
		}
		j++
	}
	return false
}

// Drops every block of the streaming turn so the next event builds them again from its response
func (tr *Transcript) dropTurn() {
	gone := map[*block]bool{}
	for _, b := range tr.turn {
		gone[b] = true
	}
	tr.blocks = slices.DeleteFunc(tr.blocks, func(b *block) bool { return gone[b] })
	for id, b := range tr.tools {
		if gone[b] {
			delete(tr.tools, id)
		}
	}
	tr.turn = map[int]*block{}
	tr.cursor = min(tr.cursor, len(tr.blocks)-1)
	tr.dirty = true
}

// Time a thinking block took, or has taken so far while it streams
func (tr *Transcript) thought(b *block) time.Duration {
	if b.duration > 0 || b.started.IsZero() || b.done {
		return b.duration
	}
	return time.Since(b.started)
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
			b.cacheKey = renderKey{}
			tr.dirty = true
			return
		}
	}
	tr.add(&block{kind: bApproval, agent: ev.Agent, at: ev.Time, approval: info, done: !info.Pending})
}

// Rebuilds the blocks from stored entries and records, merged by sequence, a run in progress keeping its streaming state
func (tr *Transcript) Load(entries []session.Entry, records []session.Record) {
	tr.blocks = nil
	tr.turn = map[int]*block{}
	tr.tools = map[string]*block{}
	tr.turns = 0
	tr.lineBlock, tr.blockLine = nil, nil
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
				tr.add(&block{kind: bText, agent: e.Agent, model: e.Model, at: e.Time, text: v.Text, cites: v.Citations, done: true})
			case protocol.Thinking:
				if !tr.thinking {
					continue
				}
				text := v.Text
				if v.Redacted && text == "" {
					text = "[redacted]"
				}
				tr.add(&block{kind: bThinking, agent: e.Agent, at: e.Time, text: text, done: true, expanded: true})
			case protocol.ToolCall:
				b := tr.add(&block{kind: bTool, agent: e.Agent, at: e.Time, call: v, started: e.Time})
				if v.ID != "" {
					tr.tools[v.ID] = b
				}
			}
		}
		if text := usageNote(e.Model, e.Usage); text != "" {
			tr.add(&block{kind: bNote, agent: e.Agent, at: e.Time, text: text, done: true})
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

// Color of the agent speaking in a block, the primary color across the root lineage
func (tr *Transcript) speakerColor(b *block) color.Color {
	if tr.rootOnly {
		return tr.env.Theme.Ice
	}
	return tr.agentColor(b.agent)
}

// Color that stands for an agent, stable across renders
func (tr *Transcript) agentColor(name string) color.Color {
	t := tr.env.Theme
	if name == "" {
		return t.Ice
	}
	palette := []color.Color{t.Ice, t.Pulse, t.Alive, t.Accent}
	h := fnv.New32a()
	h.Write([]byte(name))
	return palette[int(h.Sum32())%len(palette)]
}

// Time one layout pass spends rendering blocks out of view, their last lines standing in past it
const layoutBudget = 5 * time.Millisecond

// Empty rows standing in for a block not yet rendered
var blank = make([]string, 64)

// Lays the blocks out at width, those in view rendered now and the rest within the limit, zero meaning all
func (tr *Transcript) relayout(width int, limit time.Duration) {
	first, last := tr.inView()
	anchor := -1
	if first < len(tr.blockLine) {
		anchor = tr.blockLine[first]
	}
	now := time.Now()
	deadline := now.Add(limit)
	tr.stale = 0
	for i := range outward(first, last, len(tr.blocks)) {
		b := tr.blocks[i]
		key := tr.stateKey(i, b, width, now)
		if b.cache != nil && b.cacheKey == key {
			continue
		}
		if (i < first || i > last) && limit > 0 && !time.Now().Before(deadline) {
			tr.stale++
			continue
		}
		tr.renderBlock(i, b, width, key)
	}
	tr.lines = tr.lines[:0]
	tr.lineBlock = tr.lineBlock[:0]
	tr.blockLine = tr.blockLine[:0]
	for i, b := range tr.blocks {
		tr.blockLine = append(tr.blockLine, len(tr.lines))
		rows := tr.height(b)
		for range rows {
			tr.lineBlock = append(tr.lineBlock, i)
		}
		if b.cache != nil {
			tr.lines = append(tr.lines, b.cache...)
			continue
		}
		for rows > 0 {
			n := min(rows, len(blank))
			tr.lines = append(tr.lines, blank[:n]...)
			rows -= n
		}
	}
	tr.dirty = false
	tr.vp.SetTotal(len(tr.lines))
	if tr.follow || anchor < 0 || first >= len(tr.blockLine) {
		return
	}
	if shift := tr.blockLine[first] - anchor; shift != 0 {
		tr.vp.SetYOffset(tr.vp.YOffset() + shift)
		tr.shown.Pos += float64(shift)
	}
}

// Blocks in view by the last layout, the newest that fill the view while following or before any layout
func (tr *Transcript) inView() (first, last int) {
	n := len(tr.blocks)
	h := max(tr.vp.Height(), 1)
	if tr.follow || len(tr.lineBlock) == 0 {
		rows := 0
		first = n
		for first > 0 && rows < h {
			first--
			rows += tr.height(tr.blocks[first])
		}
		return first, n - 1
	}
	top := min(tr.vp.YOffset(), len(tr.lineBlock)-1)
	bottom := min(top+h-1, len(tr.lineBlock)-1)
	return min(tr.lineBlock[top], n-1), min(tr.lineBlock[bottom], n-1)
}

// Block indexes from first through last, then alternating below and above them until every one of n is visited
func outward(first, last, n int) iter.Seq[int] {
	return func(yield func(int) bool) {
		for i := max(first, 0); i <= last && i < n; i++ {
			if !yield(i) {
				return
			}
		}
		lo, hi := min(first, n)-1, max(last, -1)+1
		for lo >= 0 || hi < n {
			if hi < n {
				if !yield(hi) {
					return
				}
				hi++
			}
			if lo >= 0 {
				if !yield(lo) {
					return
				}
				lo--
			}
		}
	}
}

// Rows a block takes, its last lines or an estimate before it is rendered
func (tr *Transcript) height(b *block) int {
	if b.cache != nil {
		return len(b.cache)
	}
	return estimate(b)
}

// Tokens one model call carried and what it cost when the model is priced, empty for a call without usage
func usageNote(model string, u protocol.Usage) string {
	if u.Context() == 0 && u.Output == 0 {
		return ""
	}
	parts := []string{"tokens in " + render.Tokens(u.Input)}
	if u.CacheRead > 0 {
		parts = append(parts, "cached "+render.Tokens(u.CacheRead))
	}
	if u.CacheWrite > 0 {
		parts = append(parts, "written "+render.Tokens(u.CacheWrite))
	}
	parts = append(parts, "out "+render.Tokens(u.Output))
	if known, ok := profiles.Describe(model); ok && known.Pricing != nil && known.Pricing.Known() {
		parts = append(parts, render.Money(known.Pricing.Cost(u)))
	}
	return strings.Join(parts, ", ")
}

// Rows a block is likely to take, standing in until it is rendered
func estimate(b *block) int {
	switch b.kind {
	case bText, bThinking, bCompact:
		return strings.Count(b.text, "\n") + 3
	case bUser, bSteer:
		rows := 2
		for _, p := range b.parts {
			if t, ok := p.(protocol.Text); ok {
				rows += strings.Count(t.Text, "\n") + 1
			}
		}
		return rows
	}
	return 2
}

// Reports whether the block paints a spinner, elapsed time or churning text and so changes with every tick
func (b *block) animated(now time.Time) bool {
	if b.kind == bTool {
		return b.result == nil && (!b.started.IsZero() || !b.done)
	}
	return !b.done || b.churning(now)
}

func (tr *Transcript) stateKey(i int, b *block, width int, now time.Time) renderKey {
	k := renderKey{width: width, text: len(b.text), partial: len(b.partial), done: b.done, expanded: b.expanded, full: b.full, cursor: tr.cursor == i, result: b.result != nil}
	if b.animated(now) {
		k.tick = tr.tick
	}
	if b.kind == bThinking {
		k.settled = b.resolved(now)
	}
	if b.approval != nil {
		k.pending = b.approval.Pending
	}
	return k
}

// Renders the block at width and keeps the lines under the key
func (tr *Transcript) renderBlock(i int, b *block, width int, key renderKey) {
	t := tr.env.Theme
	var lines []string
	mark := "  "
	if tr.cursor == i {
		mark = lipgloss.NewStyle().Foreground(t.Accent).Render(t.Glyphs.Cursor + " ")
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
		c := t.Accent
		if b.kind == bSteer {
			label = t.Glyphs.Steer + " steer"
			if b.text != "" {
				label = t.Glyphs.Steer + " " + b.text
			}
			c = t.Pulse
		}
		lines = append(lines, tr.turnHead(mark, c, lipgloss.NewStyle().Foreground(c).Bold(true).Render(label), stamp, body))
		text, extras := partsText(b.parts, t)
		shown := wrap(text)
		if !b.full && len(shown) > previewLines {
			shown = append(shown[:previewLines], "  "+render.Dim(tr.more(len(shown)-previewLines), t))
		}
		lines = append(lines, shown...)
		for _, e := range extras {
			lines = append(lines, "  "+render.Warm(e, t))
		}
	case bText:
		c := tr.speakerColor(b)
		name := b.agent
		if (tr.rootOnly || name == "") && b.model != "" {
			name = b.model
		}
		if name == "" {
			name = "agent"
		}
		head := lipgloss.NewStyle().Foreground(c).Bold(true).Render(name)
		if b.model != "" && b.model != name {
			head += " " + render.Dim(b.model, t)
		}
		if !b.done {
			head += " " + render.Cool(render.Spinner(tr.tick, t), t)
		}
		lines = append(lines, tr.turnHead(mark, c, head, stamp, body))
		for _, l := range strings.Split(tr.markdown(b, body), "\n") {
			lines = append(lines, "  "+l)
		}
		for _, l := range citationLines(b.cites, body-2, t) {
			lines = append(lines, "  "+l)
		}
	case bThinking:
		head := render.Dim(t.Glyphs.Thinking+" thinking", t)
		if d := tr.thought(b); d > 0 {
			head += render.Dim(" "+t.Glyphs.Sep+" "+render.Elapsed(d), t)
		}
		if !b.done {
			head += " " + render.Dim(render.Spinner(tr.tick, t), t)
		}
		lines = append(lines, mark+head)
		if b.expanded || !b.done {
			lines = append(lines, tr.thoughtLines(b, body)...)
		}
	case bTool:
		lines = append(lines, tr.toolLines(mark, b, body)...)
	case bApproval:
		a := b.approval
		glyph := lipgloss.NewStyle().Foreground(t.Pulse).Bold(true).Render(t.Glyphs.Flag)
		var text string
		switch {
		case a.Pending:
			text = render.Hot("approve "+a.Tool+"?", t) + " " + render.Dim(render.Spinner(tr.tick, t), t)
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
			text += " " + render.Dim(render.Truncate(a.Reason, body/2, t), t)
		}
		lines = append(lines, mark+glyph+" "+text+" "+stamp)
	case bCompact:
		head := render.Warm(t.Glyphs.Compact+" compacting", t)
		if b.done {
			switch d := b.data.(type) {
			case map[string]int:
				head = render.Warm(fmt.Sprintf("%s compacted %d messages into %d", t.Glyphs.Compact, d["before"], d["after"]), t)
			case string:
				head = render.Warm(t.Glyphs.Compact+" "+d, t)
			default:
				head = render.Warm(t.Glyphs.Compact+" compacted", t)
			}
		} else {
			head += " " + render.Dim(render.Spinner(tr.tick, t), t)
		}
		lines = append(lines, mark+head+" "+stamp)
		if b.expanded && b.text != "" {
			lines = append(lines, wrap(render.Dim(b.text, t))...)
		}
	case bHook:
		lines = append(lines, mark+render.Dim(t.Glyphs.Hook+" "+render.Truncate(b.text, body, t), t))
		if ex, ok := b.data.(agent.HookExecution); ok && strings.TrimSpace(ex.Stdout) != "" {
			for _, l := range render.Lines(render.Tail(strings.TrimSpace(ex.Stdout), 5)) {
				lines = append(lines, "    "+render.Dim(render.Truncate(l, body-2, t), t))
			}
		}
	case bNote:
		lines = append(lines, mark+render.Dim(t.Glyphs.Note+" "+render.Truncate(b.text, body, t), t))
	case bError:
		shown, cut := tr.errorLines(b.err, body, b.expanded)
		lines = append(lines, mark+render.Bad(shown[0], t)+" "+stamp)
		for _, l := range shown[1:] {
			lines = append(lines, "  "+render.Bad(l, t))
		}
		if cut != "" {
			lines = append(lines, "  "+render.Dim(cut, t))
		}
	case bChild:
		name := b.call.Name
		head := lipgloss.NewStyle().Foreground(tr.agentColor(name)).Bold(true).Render(t.Glyphs.Child + " " + name)
		if b.text != "" && !b.done {
			head += " " + render.Dim(render.Truncate(b.text, body/2, t), t)
		}
		switch {
		case !b.done:
			head += " " + render.Cool(render.Spinner(tr.tick, t)+" working", t)
		case b.err != nil:
			head += " " + render.Bad(render.Truncate(b.err.Error(), body/2, t), t)
		default:
			head += " " + render.Good("done", t)
			if b.text != "" {
				head += " " + render.Dim(render.Summary(b.text, body/2, t), t)
			}
		}
		lines = append(lines, mark+head+" "+stamp)
	}
	lines = append(lines, "")
	b.cache, b.cacheKey = lines, key
}

// Turn header with the notch and label, a rule across the middle and the stamp at the right
func (tr *Transcript) turnHead(mark string, c color.Color, label, stamp string, body int) string {
	t := tr.env.Theme
	head := lipgloss.NewStyle().Foreground(c).Bold(true).Render(t.Glyphs.Notch) + " " + label
	if fill := body - ansi.StringWidth(head) - ansi.StringWidth(stamp) - 2; fill >= 2 {
		head += " " + lipgloss.NewStyle().Foreground(t.Steel).Render(strings.Repeat(t.Glyphs.Rule, fill))
	}
	return mark + head + " " + stamp
}

// Markdown of a text block, its settled paragraphs kept from last time ahead of the open tail while it streams
func (tr *Transcript) markdown(b *block, width int) string {
	t := tr.env.Theme
	cut := 0
	if !b.done {
		cut = settledCut(b.text)
	}
	if cut <= 0 {
		return render.MarkdownText(b.text, width, t)
	}
	key := fmt.Sprintf("%d:%d:%s", cut, width, t.Name)
	if b.headKey != key {
		b.head, b.headKey = render.MarkdownText(b.text[:cut], width, t), key
	}
	tail := render.MarkdownText(b.text[cut:], width, t)
	if strings.TrimSpace(tail) == "" {
		return b.head
	}
	return b.head + "\n\n" + strings.TrimLeft(tail, "\n")
}

// Offset of the last prose paragraph after a blank line outside a code fence, zero when there is none
func settledCut(text string) int {
	cut, at, fenced, blank := 0, 0, false, false
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			fenced = !fenced
		case !fenced && blank && trimmed != "" && startsProse(trimmed):
			cut = at
		}
		blank = !fenced && strings.TrimSpace(trimmed) == ""
		at += len(line)
	}
	return cut
}

// Reports whether a line opens plain prose rather than a list item, quote, heading, rule, table or indented block
func startsProse(line string) bool {
	r, _ := utf8.DecodeRuneInString(line)
	if unicode.IsLetter(r) || r == '"' || r == '(' || r == '`' {
		return true
	}
	return (r == '*' || r == '_') && len(line) > 1 && line[1] != ' '
}

// Text of input parts with a label for every attachment
func partsText(parts []protocol.Part, t theme.Theme) (string, []string) {
	var sb strings.Builder
	var extras []string
	for _, p := range parts {
		switch v := p.(type) {
		case protocol.Text:
			sb.WriteString(v.Text)
		case protocol.Image:
			extras = append(extras, fmt.Sprintf("%s image %s %s", t.Glyphs.Attach, v.MediaType, render.Bytes(int64(len(v.Data)))))
		case protocol.Document:
			extras = append(extras, fmt.Sprintf("%s %s %s", t.Glyphs.Attach, v.Name, render.Bytes(int64(len(v.Data)))))
		}
	}
	return sb.String(), extras
}

// Wrapped thought in dim text, its newest runes boiling in ice until they settle
func (tr *Transcript) thoughtLines(b *block, body int) []string {
	t := tr.env.Theme
	runes := []rune(b.text)
	keep := visible(runes[:min(b.resolved(time.Now()), len(runes))])
	var out []string
	for i, l := range strings.Split(render.Wrap(b.text, body), "\n") {
		lr := []rune(l)
		cut := 0
		for cut < len(lr) && keep > 0 {
			if !unicode.IsSpace(lr[cut]) {
				keep--
			}
			cut++
		}
		line := render.Dim(string(lr[:cut]), t)
		if cut < len(lr) {
			line += render.Cool(anim.Churn(string(lr[cut:]), tr.tick+i*7, t.Glyphs.Churn), t)
		}
		out = append(out, "  "+line)
	}
	return out
}

// Count of runes that are not blank
func visible(runes []rune) int {
	n := 0
	for _, r := range runes {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// Short description of a call from its arguments
func CallSummary(call protocol.ToolCall, partial string, t theme.Theme) string {
	raw := call.Args
	if len(raw) == 0 && partial != "" {
		return render.Truncate(strings.TrimSpace(partial), 60, t)
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
		if ps, ok := args["file_paths"].([]any); ok && len(ps) > 0 {
			return render.Count(len(ps), "file")
		}
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
	case files.OutlineName:
		s := str("path")
		if p := str("pattern"); p != "" {
			s += " matching " + p
		}
		return s
	case shell.ShellName:
		if d := str("description"); d != "" {
			return d
		}
		return strings.Join(strings.Fields(str("command")), " ")
	case shell.OutputName, shell.KillName:
		return str("job_id")
	case "WebFetch":
		return str("url")
	case "Agent":
		if d := str("description"); d != "" {
			return d
		}
		return render.Summary(str("prompt"), 60, t)
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

func (tr *Transcript) toolLines(mark string, b *block, body int) []string {
	t := tr.env.Theme
	glyph := render.ToolGlyph(b.call.Name, t)
	name := b.call.Name
	if name == "" {
		name = "tool"
	}
	head := lipgloss.NewStyle().Foreground(tr.speakerColor(b)).Bold(true).Render(glyph+" "+name) + " " + lipgloss.NewStyle().Foreground(t.Text).Render(render.Truncate(CallSummary(b.call, b.partial, t), body/2, t))
	var status string
	switch {
	case b.result == nil && !b.started.IsZero():
		status = render.Cool(render.Spinner(tr.tick, t)+" "+render.Elapsed(time.Since(b.started)), t)
		if p := progressText(b.progressDone, b.progressTotal); p != "" {
			status += " " + render.Dim(p, t)
		}
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
	if b.result != nil {
		status += " " + render.Dim(render.Tokens(protocol.EstimateTokens(b.result.Content))+" tok", t)
	}
	lines := []string{mark + head + " " + status}
	if b.err != nil {
		shown, cut := tr.errorLines(b.err, body-2, b.expanded)
		for _, l := range shown {
			lines = append(lines, "  "+render.Hot(l, t))
		}
		if cut != "" {
			lines = append(lines, "  "+render.Dim(cut, t))
		}
		return lines
	}
	if b.result == nil && b.partial != "" {
		tail := render.Lines(b.partial)
		if len(tail) > 6 {
			tail = tail[len(tail)-6:]
		}
		for _, l := range tail {
			lines = append(lines, "  "+render.Dim(render.Truncate(l, body-2, t), t))
		}
		return lines
	}
	if b.result == nil || !b.expanded {
		return lines
	}
	bodyLines := tr.resultLines(b, body-2)
	limit := previewLines
	if b.full {
		limit = len(bodyLines)
	}
	if len(bodyLines) > limit {
		bodyLines = append(bodyLines[:limit], render.Dim(tr.more(len(bodyLines)-limit), t))
	}
	for _, l := range bodyLines {
		lines = append(lines, "  "+l)
	}
	return lines
}

// Every wrapped line of the error once expanded, else its first line cut to width with the marker for the rest
func (tr *Transcript) errorLines(err error, width int, expanded bool) ([]string, string) {
	t := tr.env.Theme
	text := t.Glyphs.Error + " " + err.Error()
	full := strings.Split(render.Wrap(text, width), "\n")
	if expanded || len(full) == 1 {
		return full, ""
	}
	first, _, _ := strings.Cut(text, "\n")
	return []string{render.Truncate(first, width, t)}, tr.more(len(full) - 1)
}

// Cut marker naming the lines left out
func (tr *Transcript) more(n int) string {
	return fmt.Sprintf("%s %d more lines, click to show all", tr.env.Theme.Glyphs.Ellipsis, n)
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
	case files.ReadMany:
		lines := 0
		for _, f := range d.Files {
			lines += len(f.Lines)
		}
		stat := render.Count(len(d.Files), "file") + ", " + render.Count(lines, "line")
		if len(d.Skipped) > 0 {
			stat += ", " + render.Count(len(d.Skipped), "skipped")
		}
		return stat
	case files.OutlineData:
		stat := render.Count(d.Symbols, "symbol") + " in " + render.Count(d.Files, "file")
		if d.Skipped > 0 {
			stat += ", " + render.Count(d.Skipped, "skipped")
		}
		return stat
	case files.GlobData:
		return render.Count(d.Total, "match")
	case files.GrepData:
		return render.Count(d.Total, "hit") + " in " + render.Count(len(d.Files), "file")
	case shell.Run:
		return fmt.Sprintf("exit %d", d.ExitCode)
	case web.Page:
		return fmt.Sprintf("%d %s", d.Status, render.Bytes(int64(d.Bytes)))
	case agent.Child:
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
			out = append(out, render.Dim(fmt.Sprintf("%*d ", digits, d.Offset+i), t)+render.Truncate(text, width-digits-1, t))
		}
		return out
	case files.GlobData:
		var out []string
		for _, m := range d.Matches {
			out = append(out, render.Truncate(m.Path, width, t))
		}
		return out
	case files.GrepData:
		var out []string
		for _, f := range d.Files {
			out = append(out, render.Cool(render.Truncate(f.Path, width, t), t))
			for _, h := range f.Hits {
				out = append(out, render.Dim(fmt.Sprintf("  %d ", h.Line), t)+render.Truncate(h.Text, width-8, t))
			}
		}
		return out
	case shell.Run:
		var out []string
		for _, l := range render.Lines(strings.TrimRight(d.Stdout, "\n")) {
			out = append(out, render.Truncate(l, width, t))
		}
		for _, l := range render.Lines(strings.TrimRight(d.Stderr, "\n")) {
			out = append(out, render.Bad(render.Truncate(l, width, t), t))
		}
		if d.TimedOut {
			out = append(out, render.Bad("timed out", t))
		}
		if d.Exited {
			out = append(out, render.Bad("shell exited", t))
		}
		return out
	case web.Page:
		out := []string{render.KV("title", d.Title, t), render.KV("url", d.FinalURL, t)}
		for _, l := range render.Lines(d.Text) {
			out = append(out, render.Truncate(l, width, t))
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
	t := tr.env.Theme
	gw := 0
	if w >= minGutterWidth {
		gw = 2
	}
	tr.bar = gw > 0
	cw := w - gw
	if cw != tr.w || t.Name != tr.themed {
		tr.dirty = true
		for _, b := range tr.blocks {
			b.cacheKey = renderKey{}
		}
	}
	if h != tr.h {
		tr.vp.SetHeight(h)
		if tr.follow {
			tr.vp.GotoBottom()
		}
	}
	tr.w, tr.h, tr.themed = cw, h, t.Name
	tr.latest = hotspot{}
	if len(tr.blocks) == 0 {
		return tr.idle(w, h)
	}
	tr.viewed = true
	if tr.dirty || tr.stale > 0 {
		tr.relayout(cw, layoutBudget)
		tr.settle()
		if tr.follow {
			tr.vp.GotoBottom()
		}
	}
	painted := tr.paintedOffset()
	key := viewKey{w: w, h: h, offset: painted, gen: tr.gen, theme: t.Name, up: tr.env.Keys.First(tr.pages[0]), down: tr.env.Keys.First(tr.pages[1]), follow: tr.follow}
	label := ""
	if !tr.follow && h > 0 {
		label = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Accent).Bold(true).Render(" " + t.Glyphs.Down + " scroll to latest ")
		lw := ansi.StringWidth(label)
		tr.latest = hotspot{x: max(cw-lw-1, 0), y: max(h-2, 0), w: lw}
	}
	if key == tr.viewKey && tr.view != "" {
		return tr.view
	}
	rows := make([]string, 0, h)
	rows = append(rows, tr.painted[min(painted, len(tr.painted)):min(painted+h, len(tr.painted))]...)
	for len(rows) < h {
		rows = append(rows, "")
	}
	if label != "" {
		rows[tr.latest.y] = render.Fit(rows[tr.latest.y], tr.latest.x, t) + label
	}
	body := strings.Join(rows, "\n")
	if gw > 0 {
		g := tr.gutter()
		g.offset = painted
		body = g.beside(body, w, t)
	}
	tr.view, tr.viewKey = body, key
	return body
}

// Narrowest transcript that still gets a scrollbar
const minGutterWidth = 12

// Reports whether the last view drew the scrollbar
func (tr *Transcript) gutterShown() bool { return tr.bar && len(tr.blocks) > 0 }

// Scrollbar for the view, capped with the page keys and ticked at turn starts and matches
func (tr *Transcript) gutter() gutter {
	g := gutter{h: tr.h, total: len(tr.lines), offset: tr.vp.YOffset(), visible: tr.h, up: tr.env.Keys.First(tr.pages[0]), down: tr.env.Keys.First(tr.pages[1])}
	for i, b := range tr.blocks {
		if b.kind == bUser && i < len(tr.blockLine) {
			g.marks = append(g.marks, tr.blockLine[i])
		}
	}
	for _, m := range tr.found {
		g.hits = append(g.hits, m.line)
	}
	return g
}

// Pages or jumps for a click on row y of the scrollbar
func (tr *Transcript) clickGutter(y int) {
	switch hit, line := tr.gutter().hit(y); hit {
	case hitPageUp:
		tr.Page(-1)
	case hitPageDown:
		tr.Page(1)
	case hitJump:
		tr.vp.SetYOffset(line)
		tr.follow = tr.vp.NearBottom(followSlack)
	}
}

func (tr *Transcript) Status() string {
	var parts []string
	if tr.turns > 0 {
		parts = append(parts, render.Count(tr.turns, "turn"))
	}
	switch {
	case tr.streaming:
		parts = append(parts, "streaming")
	case tr.running > 0:
		parts = append(parts, render.Count(tr.running, "tool"))
	case !tr.follow:
		parts = append(parts, "paused")
	}
	if tr.search.Query() != "" {
		parts = append(parts, render.Count(len(tr.found), "match"))
	}
	return strings.Join(parts, " "+tr.env.Theme.Glyphs.Sep+" ")
}

// Reports whether typed text goes to the search line
func (tr *Transcript) Searching() bool { return tr.search.Active() }

// Opens the search bar, keeping the last query
func (tr *Transcript) Search() {
	tr.search.Open()
	tr.applySearch()
}

// Search bar row at width with the place of the current match among all of them
func (tr *Transcript) SearchView(w int, placeholder string) string {
	count := ""
	if q := tr.search.Query(); q != "" {
		count = "no match"
		if len(tr.found) > 0 {
			count = fmt.Sprintf("%d/%d", tr.current+1, len(tr.found))
		}
	}
	return tr.search.View(w, count, placeholder, tr.env.Theme)
}

// Reports whether the search bar shows, while it takes typing or holds a query
func (tr *Transcript) SearchShown() bool { return tr.search.Active() || tr.search.Query() != "" }

// Column of the terminal cursor inside the search bar
func (tr *Transcript) SearchCursor() int { return tr.search.CursorX() }

// Reports whether the viewport follows new lines
func (tr *Transcript) Following() bool { return tr.follow }

// Reports whether a turn is streaming or a tool is running
func (tr *Transcript) Busy() bool { return tr.streaming || tr.running > 0 }

// Loads thinking from the session from now on or leaves it out, in force from the next load
func (tr *Transcript) ShowThinking(on bool) { tr.thinking = on }

// Reports whether thinking is loaded from the session
func (tr *Transcript) Thinking() bool { return tr.thinking }

// Expands or folds every tool result
func (tr *Transcript) ExpandAll(on bool) { tr.expandAll(on) }

// Scrolls to the newest line and follows from there
func (tr *Transcript) Follow() {
	tr.follow = true
	tr.vp.GotoBottom()
}

// Byte ranges of every case insensitive occurrence of query in content
func findAll(content, query string) [][]int {
	if query == "" {
		return nil
	}
	hay, needle := strings.ToLower(content), strings.ToLower(query)
	if len(hay) != len(content) {
		hay, needle = content, query
	}
	var out [][]int
	for from := 0; from <= len(hay)-len(needle); {
		i := strings.Index(hay[from:], needle)
		if i < 0 {
			break
		}
		start := from + i
		out = append(out, []int{start, start + len(needle)})
		from = start + len(needle)
	}
	return out
}

// Model named as idle while the transcript is empty
func (tr *Transcript) idle(w, h int) string {
	t := tr.env.Theme
	if !tr.rootOnly {
		return emptyView(w, h, "waiting", t)
	}
	model := "model"
	if tr.env.Primary != nil {
		if id := tr.env.Primary().Model; id != "" {
			model = id
		}
	}
	line := lipgloss.NewStyle().Foreground(t.Ice).Render(model) + lipgloss.NewStyle().Foreground(t.TextDim).Render(" "+t.Glyphs.Sep+" idle")
	mark, bar := wordmark, t.Accent
	if t.Plain {
		mark, bar = wordmarkASCII, t.Text
	}
	mw := 0
	for _, l := range mark {
		mw = max(mw, ansi.StringWidth(l))
	}
	if w >= mw+4 && h >= len(mark)+3 {
		line = strings.Join(append(paintMark(mark, t.Text, bar), "", line), "\n")
	}
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, line)
}

// Wordmark in half blocks, three rows, with the bar of the logo between the words
var wordmark = []string{
	"▀█▀ █▀▀ █▀▀ │ █▀▄ █▀█ ▀█▀ █ █",
	" █  █   █▀▀ │ █▀▄ █▀█  █  █▀█",
	"▄█▄ ▀▀▀ ▀▀▀ │ ▀▀  ▀ ▀  ▀  ▀ ▀",
}

// Wordmark for terminals without block characters
var wordmarkASCII = []string{
	" _ ___ ___ ___  _ _____ _ _ ",
	"| |/ __| __| _ )/_\\_   _| || |",
	"| | (__| _|| _ \\/ _ \\| | | __ |",
	"|_|\\___|___|___/_/ \\_\\_| |_||_|",
}

// Lines with the letters painted in fg and the bar between the words in bar
func paintMark(lines []string, fg, bar color.Color) []string {
	letters := lipgloss.NewStyle().Foreground(fg)
	divider := lipgloss.NewStyle().Foreground(bar)
	out := make([]string, len(lines))
	for i, l := range lines {
		var sb strings.Builder
		for _, r := range l {
			switch r {
			case ' ':
				sb.WriteRune(r)
			case '│':
				sb.WriteString(divider.Render(string(r)))
			default:
				sb.WriteString(letters.Render(string(r)))
			}
		}
		out[i] = sb.String()
	}
	return out
}

// Count done out of the total, the share when both are known and the count alone otherwise
func progressText(done, total int) string {
	switch {
	case total > 0:
		return fmt.Sprintf("%d/%d %d%%", done, total, min(done*100/total, 100))
	case done > 0:
		return fmt.Sprint(done)
	}
	return ""
}

// One dimmed line per distinct source, titled where the model gave a title and linked where it gave a URL
func citationLines(cites []protocol.Citation, width int, t theme.Theme) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range cites {
		label := c.Title
		if label == "" {
			label = c.URL
		}
		if label == "" {
			label = c.Document
		}
		key := label + "\x00" + c.URL
		if label == "" || seen[key] {
			continue
		}
		seen[key] = true
		text := render.Truncate(label, max(width-2, 8), t)
		if c.URL != "" {
			text = render.Link(text, c.URL, t)
		} else {
			text = render.Dim(text, t)
		}
		out = append(out, render.Dim(t.Glyphs.Record, t)+" "+text)
	}
	return out
}
