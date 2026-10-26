package cards

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Facts of one session shown as a table or a tree with filters, evidence links and a markdown export
type Findings struct {
	env      *Env
	store    *facts.Store
	session  string
	rows     []facts.Fact
	depth    map[string]int
	sel      int
	tree     bool
	kind     string
	tag      string
	status   string
	bar      searchBar
	expanded bool
	pane     detailPane
	top      int
	start    int
	w, h     int
}

// Builds the findings card over the facts store of the session
func NewFindings(env *Env, store *facts.Store, sessionID string) *Findings {
	f := &Findings{env: env, store: store, session: sessionID, depth: map[string]int{}}
	f.refresh()
	return f
}

func (f *Findings) Title() string    { return "FINDINGS" }
func (f *Findings) Glyph() string    { return f.env.Theme.Glyphs.Findings }
func (f *Findings) Key() string      { return "findings:" + f.session }
func (f *Findings) Init() tea.Cmd    { return nil }
func (f *Findings) Selectable() bool { return true }

// Keymap contexts in force, the filter bar while it takes typing and the detail keys while a detail shows
func (f *Findings) Contexts() []string {
	if f.bar.Active() {
		return f.bar.Contexts()
	}
	if f.detailShown() {
		return []string{keys.Findings, keys.Detail, keys.Filter, keys.List, keys.Panel}
	}
	return []string{keys.Findings, keys.Filter, keys.List, keys.Panel}
}

// Reports whether the detail of the selected fact is open
func (f *Findings) detailShown() bool { return f.expanded && f.sel < len(f.rows) }

// Facts shown after filtering, in display order
func (f *Findings) Rows() []facts.Fact { return f.rows }

// Rereads the store
func (f *Findings) Reload() { f.refresh() }

func (f *Findings) refresh() {
	f.rows = f.rows[:0]
	f.depth = map[string]int{}
	if f.store == nil {
		return
	}
	q := facts.Query{Kind: f.kind, Tag: f.tag, Status: f.status, Text: f.bar.Query()}
	if f.tree {
		var walk func(nodes []facts.Node, d int)
		walk = func(nodes []facts.Node, d int) {
			for _, n := range nodes {
				if f.matches(n.Fact, q) {
					f.rows = append(f.rows, n.Fact)
					f.depth[n.Fact.ID] = d
				}
				walk(n.Children, d+1)
			}
		}
		walk(f.store.Tree(), 0)
	} else {
		f.rows = append(f.rows, f.store.List(q)...)
	}
	if f.sel >= len(f.rows) {
		f.sel = max(0, len(f.rows)-1)
	}
}

func (f *Findings) matches(fact facts.Fact, q facts.Query) bool {
	for _, got := range f.store.List(q) {
		if got.ID == fact.ID {
			return true
		}
	}
	return false
}

var statuses = []string{"", facts.StatusOpen, facts.StatusResolved, facts.StatusRetracted}

// Filter values to cycle through, none first and then the names counted, sorted
func choices(counts map[string]int) []string {
	out := make([]string, 0, len(counts)+1)
	for name := range counts {
		out = append(out, name)
	}
	sort.Strings(out)
	return append([]string{""}, out...)
}

func cycle(list []string, cur string) string {
	for i, s := range list {
		if s == cur {
			return list[(i+1)%len(list)]
		}
	}
	return list[0]
}

func (f *Findings) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.FactMsg:
		if msg.Session != f.session {
			return f, nil
		}
		f.refresh()
		if msg.Change.Op == facts.OpAdd {
			return f, wm.RequestAttention(f, wm.Pulse)
		}
		return f, nil
	case wm.FocusMsg:
		if !msg.Focused {
			f.bar.Accept()
		}
	case wm.ActionMsg:
		return f, f.act(msg.ID)
	case tea.KeyPressMsg:
		if f.bar.Active() && msg.Text != "" {
			f.bar.Type(msg.Text)
			f.refresh()
		}
	case tea.PasteMsg:
		if f.bar.Active() {
			f.bar.Type(msg.Content)
			f.refresh()
		}
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return f, nil
		}
		if msg.Y == f.h-1 && f.bar.Shown() {
			f.bar.Open()
			return f, nil
		}
		if f.detailShown() && f.pane.click(msg.X, msg.Y, f.w, f.env) {
			return f, nil
		}
		f.bar.Accept()
		if row := msg.Y - f.top - render.TableHead + f.start; msg.Y >= f.top+render.TableHead && row < len(f.rows) {
			if row == f.sel {
				f.expanded = !f.expanded
			}
			f.sel = row
		}
	case tea.MouseWheelMsg:
		step := 1
		if msg.Button == tea.MouseWheelUp {
			step = -1
		}
		if f.detailShown() && f.pane.wheel(msg.Y, step) {
			return f, nil
		}
		f.sel = min(max(f.sel+step, 0), max(0, len(f.rows)-1))
	}
	return f, nil
}

// Carries out a keymap action on the filter bar, the filters or the selected fact
func (f *Findings) act(id string) tea.Cmd {
	switch id {
	case "search.accept":
		f.bar.Accept()
	case "search.close":
		f.bar.Close()
		f.refresh()
	case "search.next":
		f.sel = listMove("list.down", f.sel, len(f.rows), 1)
	case "search.prev":
		f.sel = listMove("list.up", f.sel, len(f.rows), 1)
	case "filter.toggle":
		f.bar.Toggle()
		f.refresh()
	case "findings.tree":
		f.tree = !f.tree
		f.refresh()
	case "findings.kind":
		if f.store != nil {
			f.kind = cycle(choices(f.store.Kinds()), f.kind)
			f.refresh()
		}
	case "findings.tag":
		if f.store != nil {
			f.tag = cycle(choices(f.store.Tags()), f.tag)
			f.refresh()
		}
	case "findings.status":
		f.status = cycle(statuses, f.status)
		f.refresh()
	case "findings.detail":
		f.expanded = !f.expanded
	case "findings.export.md":
		return f.export(false)
	case "findings.export.json":
		return f.export(true)
	case "findings.resolve":
		return f.resolve()
	default:
		if f.pane.act(id) {
			return nil
		}
		if f.bar.Action(id) {
			f.refresh()
			return nil
		}
		f.sel = listMove(id, f.sel, len(f.rows), f.h-f.top-render.TableHead)
	}
	return nil
}

func (f *Findings) resolve() tea.Cmd {
	if f.sel >= len(f.rows) || f.store == nil {
		return nil
	}
	fact := f.rows[f.sel]
	status := facts.StatusResolved
	if fact.Status == facts.StatusResolved {
		status = facts.StatusOpen
	}
	if _, err := f.store.SetStatus(fact.ID, status); err != nil {
		return wm.Notify(err.Error(), wm.Error)
	}
	f.refresh()
	return wm.Notify(fact.ID+" "+status, wm.Success)
}

// Writes every fact as markdown, or as JSON, next to the project and reports where
func (f *Findings) export(asJSON bool) tea.Cmd {
	if f.store == nil {
		return nil
	}
	dir := f.env.ProjectDir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	stamp := time.Now().Format("20060102-150405")
	path := filepath.Join(dir, "findings-"+stamp+".md")
	data := []byte(f.store.Markdown())
	if asJSON {
		path = filepath.Join(dir, "findings-"+stamp+".json")
		var err error
		if data, err = f.store.JSON(); err != nil {
			return func() tea.Msg { return ExportMsg{Path: path, Err: err} }
		}
	}
	err := os.WriteFile(path, data, 0o644)
	return func() tea.Msg { return ExportMsg{Path: path, Err: err} }
}

func (f *Findings) View(w, h int) string {
	f.w, f.h = w, h
	t := f.env.Theme
	var lines []string
	var filters []string
	if f.kind != "" {
		filters = append(filters, fmt.Sprintf("kind=%s (%d)", f.kind, f.store.Kinds()[f.kind]))
	}
	if f.tag != "" {
		filters = append(filters, fmt.Sprintf("tag=%s (%d)", f.tag, f.store.Tags()[f.tag]))
	}
	if f.status != "" {
		filters = append(filters, "status="+f.status)
	}
	if len(filters) > 0 {
		lines = append(lines, render.Truncate(" "+render.Warm(strings.Join(filters, " "), t), w, t))
	}
	f.top = len(lines)
	foot := ""
	if f.bar.Shown() {
		foot = f.bar.View(w, render.Count(len(f.rows), "fact"), "filter", t)
	}
	body := h - len(lines)
	if foot != "" {
		body--
	}
	if len(f.rows) == 0 {
		f.pane.hide()
		empty := "no findings"
		if f.bar.Query() != "" || len(filters) > 0 {
			empty = "no match"
		}
		return footed(append(lines, strings.Split(emptyView(w, body, empty, t), "\n")...), foot, h)
	}
	cols := []render.Column{{Title: "ID"}, {Title: "KIND"}, {Title: "TITLE", Flex: 1}, {Title: "CONF", Right: true}, {Title: "BY"}}
	rows := make([][]string, 0, len(f.rows))
	for _, fact := range f.rows {
		title := strings.Repeat("  ", f.depth[fact.ID]) + fact.Title
		mark := ""
		switch fact.Status {
		case facts.StatusResolved:
			mark = render.Good(t.Glyphs.Check+" ", t)
		case facts.StatusRetracted:
			mark = render.Dim(t.Glyphs.Skip+" ", t)
		}
		rows = append(rows, []string{fact.ID, fact.Kind, mark + title, render.Percent(fact.Confidence), fact.Agent})
	}
	room := body
	if f.detailShown() {
		room = max(body/2, render.TableHead+2)
	}
	table, start := window(strings.Split(render.Table(cols, rows, w, f.sel, t), "\n"), f.sel, room)
	f.start = start
	lines = append(lines, table...)
	if !f.detailShown() {
		f.pane.hide()
		return footed(lines, foot, h)
	}
	lines = append(lines, render.Divider(w, t))
	rest := body - (len(lines) - f.top)
	if pane := f.pane.view(f.detail(f.rows[f.sel]), f.sel, len(lines), rest, w, f.env); rest > 0 {
		lines = append(lines, pane)
	}
	return footed(lines, foot, h)
}

// Body, tags, evidence and time of a fact as lines the detail pane wraps
func (f *Findings) detail(fact facts.Fact) []string {
	t := f.env.Theme
	var out []string
	if fact.Body != "" {
		out = append(out, strings.Split(fact.Body, "\n")...)
	}
	if len(fact.Tags) > 0 {
		out = append(out, render.KV("tags", strings.Join(fact.Tags, " "), t))
	}
	for _, e := range fact.Evidence {
		ref := e.Ref
		switch e.Kind {
		case "file":
			ref = render.FileLink(e.Ref, e.Line, t)
		case "url":
			ref = render.Link(e.Ref, e.Ref, t)
		}
		line := render.Dim(e.Kind, t) + " " + ref
		if e.Quote != "" {
			line += " " + render.Dim(fmt.Sprintf("%q", e.Quote), t)
		}
		out = append(out, line)
	}
	return append(out, render.Dim("recorded "+fact.Time.Local().Format(time.DateTime), t))
}

// Cursor in the filter bar along the bottom while it takes typing
func (f *Findings) Cursor() *tea.Cursor {
	if !f.bar.Active() {
		return nil
	}
	return tea.NewCursor(f.bar.CursorX(), f.h-1)
}

func (f *Findings) Status() string {
	if f.store == nil {
		return ""
	}
	open := fmt.Sprintf("%d open", len(f.store.List(facts.Query{Status: facts.StatusOpen})))
	if f.tree {
		return open + " " + f.env.Theme.Glyphs.Sep + " tree"
	}
	return open
}

// Actions the findings answer
func (f *Findings) Hints() []wm.Hint {
	if f.bar.Active() {
		return []wm.Hint{{Action: "search.accept"}, {Action: "search.next", Pair: "search.prev", Label: "move"}, {Action: "search.close"}}
	}
	out := []wm.Hint{{Action: "findings.detail"}, {Action: "findings.resolve"}, {Action: "findings.tree"}, {Action: "findings.kind"}, {Action: "findings.tag"}, {Action: "findings.status"}, {Action: "findings.export.md"}, {Action: "findings.export.json"}, {Action: "filter.toggle"}, {Action: "list.down", Pair: "list.up", Label: "move"}}
	if f.detailShown() && f.pane.overflows() {
		out = append(out, wm.Hint{Action: "detail.scroll.down", Pair: "detail.scroll.up", Label: "scroll detail"})
	}
	return out
}
