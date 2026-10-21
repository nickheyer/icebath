package cards

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Facts shown as a table or a tree with filters, evidence links and a markdown export
type Findings struct {
	env       *Env
	rows      []facts.Fact
	depth     map[string]int
	sel       int
	tree      bool
	kind      string
	tag       string
	status    string
	query     string
	searching bool
	expanded  bool
	w, h      int
	// Directory exports are written to
	ExportDir string
}

var (
	fTree   = key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "tree"))
	fKind   = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "kind"))
	fStatus = key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "status"))
	fFilter = key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter"))
	fExport = key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "export"))
	fJSON   = key.NewBinding(key.WithKeys("E"), key.WithHelp("E", "export json"))
	fTag    = key.NewBinding(key.WithKeys("T"), key.WithHelp("T", "tag"))
	fOpen   = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "detail"))
	fRes    = key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "resolve"))
)

func NewFindings(env *Env) *Findings {
	f := &Findings{env: env, depth: map[string]int{}}
	f.refresh()
	return f
}

func (f *Findings) Title() string    { return "FINDINGS" }
func (f *Findings) Glyph() string    { return "✎" }
func (f *Findings) Key() string      { return "findings" }
func (f *Findings) Init() tea.Cmd    { return nil }
func (f *Findings) Capturing() bool  { return f.searching }
func (f *Findings) Selectable() bool { return true }
func (f *Findings) Hints() []key.Binding {
	return []key.Binding{fOpen, fTree, fKind, fTag, fStatus, fFilter, fExport, fJSON, fRes}
}

func (f *Findings) Claims() []string {
	return []string{"j", "k", "g", "G", "t", "c", "T", "s", "/", "e", "E", "x", "enter"}
}

// Facts shown after filtering, in display order
func (f *Findings) Rows() []facts.Fact { return f.rows }

// Rereads the store, for when the session behind the environment changed
func (f *Findings) Reload() { f.refresh() }

func (f *Findings) refresh() {
	f.rows = f.rows[:0]
	f.depth = map[string]int{}
	if f.env.Facts == nil {
		return
	}
	q := facts.Query{Kind: f.kind, Tag: f.tag, Status: f.status, Text: f.query}
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
		walk(f.env.Facts.Tree(), 0)
	} else {
		f.rows = append(f.rows, f.env.Facts.List(q)...)
	}
	if f.sel >= len(f.rows) {
		f.sel = max(0, len(f.rows)-1)
	}
}

func (f *Findings) matches(fact facts.Fact, q facts.Query) bool {
	for _, got := range f.env.Facts.List(q) {
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

func (f *Findings) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.FactMsg:
		f.refresh()
		if msg.Change.Op == facts.OpAdd {
			return f, deck.RequestAttention(f, deck.Pulse)
		}
		return f, nil
	case tea.KeyPressMsg:
		if f.searching {
			switch msg.String() {
			case "enter":
				f.searching = false
			case "esc":
				f.searching = false
				f.query = ""
			case "backspace":
				if len(f.query) > 0 {
					f.query = f.query[:len(f.query)-1]
				}
			default:
				if msg.Text != "" {
					f.query += msg.Text
				}
			}
			f.refresh()
			return f, nil
		}
		switch msg.String() {
		case "j", "down":
			f.sel = min(f.sel+1, max(0, len(f.rows)-1))
		case "k", "up":
			f.sel = max(0, f.sel-1)
		case "g":
			f.sel = 0
		case "G":
			f.sel = max(0, len(f.rows)-1)
		case "t":
			f.tree = !f.tree
			f.refresh()
		case "c":
			if f.env.Facts != nil {
				f.kind = cycle(choices(f.env.Facts.Kinds()), f.kind)
				f.refresh()
			}
		case "T":
			if f.env.Facts != nil {
				f.tag = cycle(choices(f.env.Facts.Tags()), f.tag)
				f.refresh()
			}
		case "s":
			f.status = cycle(statuses, f.status)
			f.refresh()
		case "/":
			f.searching = true
			f.query = ""
		case "enter":
			f.expanded = !f.expanded
		case "e":
			return f, f.export(false)
		case "E":
			return f, f.export(true)
		case "x":
			return f, f.resolve()
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && msg.Y >= 2 && msg.Y-2 < len(f.rows) {
			f.sel = msg.Y - 2
			f.expanded = !f.expanded
		}
	}
	return f, nil
}

func (f *Findings) resolve() tea.Cmd {
	if f.sel >= len(f.rows) || f.env.Facts == nil {
		return nil
	}
	fact := f.rows[f.sel]
	status := facts.StatusResolved
	if fact.Status == facts.StatusResolved {
		status = facts.StatusOpen
	}
	if _, err := f.env.Facts.SetStatus(fact.ID, status); err != nil {
		return deck.Notify(err.Error(), deck.Error)
	}
	f.refresh()
	return deck.Notify(fact.ID+" "+status, deck.Success)
}

// Writes every fact as markdown, or as JSON, next to the project and reports where
func (f *Findings) export(asJSON bool) tea.Cmd {
	if f.env.Facts == nil {
		return nil
	}
	dir := f.ExportDir
	if dir == "" {
		dir = f.env.ProjectDir
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	stamp := f.env.now().Format("20060102-150405")
	path := filepath.Join(dir, "findings-"+stamp+".md")
	data := []byte(f.env.Facts.Markdown())
	if asJSON {
		path = filepath.Join(dir, "findings-"+stamp+".json")
		var err error
		if data, err = f.env.Facts.JSON(); err != nil {
			return func() tea.Msg { return ExportMsg{Path: path, Err: err} }
		}
	}
	err := os.WriteFile(path, data, 0o644)
	return func() tea.Msg { return ExportMsg{Path: path, Err: err} }
}

// Marks a file reference the run read or changed, found through the workspace by its resolved path
func (f *Findings) touched(ref string) string {
	if f.env.Work == nil {
		return ""
	}
	path, err := f.env.Files.Resolve(ref)
	if err != nil {
		return ""
	}
	touch, ok := f.env.Work.Get(path)
	switch {
	case !ok:
		return ""
	case touch.Change != nil:
		return render.Warm("changed", f.env.Theme)
	}
	return render.Dim("read", f.env.Theme)
}

func (f *Findings) View(w, h int) string {
	f.w, f.h = w, h
	t := f.env.Theme
	var head []string
	filters := []string{}
	if f.kind != "" {
		filters = append(filters, fmt.Sprintf("kind=%s (%d)", f.kind, f.env.Facts.Kinds()[f.kind]))
	}
	if f.tag != "" {
		filters = append(filters, fmt.Sprintf("tag=%s (%d)", f.tag, f.env.Facts.Tags()[f.tag]))
	}
	if f.status != "" {
		filters = append(filters, "status="+f.status)
	}
	if f.query != "" || f.searching {
		filters = append(filters, "/"+f.query)
	}
	view := "table"
	if f.tree {
		view = "tree"
	}
	head = append(head, render.Truncate(render.Dim(fmt.Sprintf("%d facts · %s", len(f.rows), view), t)+" "+render.Warm(strings.Join(filters, " "), t), w))
	if len(f.rows) == 0 {
		return strings.Join(append(head, "", render.Dim("nothing recorded yet", t)), "\n")
	}
	cols := []render.Column{{Title: "ID"}, {Title: "KIND"}, {Title: "TITLE", Flex: 1}, {Title: "CONF", Right: true}, {Title: "BY"}}
	rows := make([][]string, 0, len(f.rows))
	for _, fact := range f.rows {
		title := strings.Repeat("  ", f.depth[fact.ID]) + fact.Title
		mark := ""
		switch fact.Status {
		case facts.StatusResolved:
			mark = render.Good("✓ ", t)
		case facts.StatusRetracted:
			mark = render.Dim("⊘ ", t)
		}
		rows = append(rows, []string{fact.ID, fact.Kind, mark + title, render.Percent(fact.Confidence), fact.Agent})
	}
	table := render.Table(cols, rows, w, f.sel, t)
	lines := append(head, strings.Split(table, "\n")...)
	if f.expanded && f.sel < len(f.rows) {
		fact := f.rows[f.sel]
		lines = append(lines, "")
		if fact.Body != "" {
			lines = append(lines, strings.Split(render.Wrap(fact.Body, w), "\n")...)
		}
		if len(fact.Tags) > 0 {
			lines = append(lines, render.KV("tags", strings.Join(fact.Tags, " "), t))
		}
		for _, e := range fact.Evidence {
			ref := e.Ref
			switch e.Kind {
			case "file":
				ref = render.FileLink(e.Ref, e.Line, t)
				if mark := f.touched(e.Ref); mark != "" {
					ref += " " + mark
				}
			case "url":
				ref = render.Link(e.Ref, e.Ref, t)
			}
			line := "  " + render.Dim(e.Kind, t) + " " + ref
			if e.Quote != "" {
				line += " " + render.Dim(render.Truncate(fmt.Sprintf("%q", e.Quote), w/2), t)
			}
			lines = append(lines, render.Truncate(line, w))
		}
		lines = append(lines, render.Dim("recorded "+fact.Time.Local().Format(time.DateTime), t))
	}
	if len(lines) > h {
		start := 0
		if f.sel+2 >= h {
			start = f.sel + 3 - h
		}
		lines = lines[start:min(len(lines), start+h)]
	}
	return strings.Join(lines, "\n")
}

func (f *Findings) Status() string {
	if f.env.Facts == nil {
		return ""
	}
	open := len(f.env.Facts.List(facts.Query{Status: facts.StatusOpen}))
	return fmt.Sprintf("%d open", open)
}
