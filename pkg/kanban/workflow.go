package kanban

import (
	"fmt"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Status outside every workflow, a filter toggle and never a board column
const Archived = "archived"

// One board column, the stable key stored in tasks.status with its display label, CLI glyph and drag flag
type Column struct {
	Key        string
	Label      string
	Icon       string
	DragTarget bool
}

// Column with the default question mark glyph that is offered as a drag target
func NewColumn(key, label string) Column {
	return Column{Key: key, Label: label, Icon: "?", DragTarget: true}
}

// Manual move allow list keyed by source column in insertion order
type ManualMoves struct {
	srcs []string
	dsts map[string]map[string]bool
}

// One source column and the destinations a human may request from it
type ManualRow struct {
	Src  string
	Dsts []string
}

// Frozen move table where a repeated source keeps its first position and its last destinations
func FrozenManual(rows ...ManualRow) ManualMoves {
	m := ManualMoves{dsts: map[string]map[string]bool{}}
	for _, row := range rows {
		if _, ok := m.dsts[row.Src]; !ok {
			m.srcs = append(m.srcs, row.Src)
		}
		set := map[string]bool{}
		for _, d := range row.Dsts {
			set[d] = true
		}
		m.dsts[row.Src] = set
	}
	return m
}

// Source columns in insertion order
func (m ManualMoves) Sources() []string { return slices.Clone(m.srcs) }

// Destinations allowed from src sorted, nil when src has no row
func (m ManualMoves) Get(src string) []string {
	set, ok := m.dsts[src]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// Board columns in board order and the manual move allow list
type Workflow struct {
	Columns []Column
	Manual  ManualMoves
}

// Column keys in board order, never including archived
func (w Workflow) Keys() []string {
	out := make([]string, len(w.Columns))
	for i, c := range w.Columns {
		out[i] = c.Key
	}
	return out
}

// First column with the key, false when none
func (w Workflow) Column(key string) (Column, bool) {
	for _, c := range w.Columns {
		if c.Key == key {
			return c, true
		}
	}
	return Column{}, false
}

// Column with the key or the ValueError naming the unknown column
func (w Workflow) require(key string) (Column, error) {
	col, ok := w.Column(key)
	if !ok {
		return Column{}, fmt.Errorf("unknown kanban workflow column %s", pyrepr.StrRepr(key))
	}
	return col, nil
}

// Reports whether a human may request src to dst, archiving always allowed and archived a valid restore source
func (w Workflow) CanMove(src, dst string) (bool, error) {
	if src != Archived {
		if _, err := w.require(src); err != nil {
			return false, err
		}
	}
	if src == dst {
		return false, nil
	}
	if dst == Archived {
		return true, nil
	}
	if _, err := w.require(dst); err != nil {
		return false, err
	}
	return w.Manual.dsts[src][dst], nil
}

// JSON shape the dashboard GET workflow route serves with sorted manual destinations
func (w Workflow) ToDict() *pyjson.Object {
	cols := make([]any, len(w.Columns))
	for i, c := range w.Columns {
		o := pyjson.NewObject()
		o.Set("key", c.Key)
		o.Set("label", c.Label)
		o.Set("icon", c.Icon)
		o.Set("drag_target", c.DragTarget)
		cols[i] = o
	}
	manual := pyjson.NewObject()
	for _, src := range w.Manual.srcs {
		dsts := w.Manual.Get(src)
		vals := make([]any, len(dsts))
		for i, d := range dsts {
			vals[i] = d
		}
		manual.Set(src, vals)
	}
	out := pyjson.NewObject()
	out.Set("columns", cols)
	out.Set("manual", manual)
	out.Set("archived", Archived)
	return out
}

// ValueError for duplicate keys, a reserved archived column or moves naming unknown columns
func (w Workflow) Validate() error {
	keys := w.Keys()
	known := map[string]bool{}
	for _, k := range keys {
		known[k] = true
	}
	if len(known) != len(keys) {
		parts := make(pyrepr.Tuple, len(keys))
		for i, k := range keys {
			parts[i] = k
		}
		return fmt.Errorf("duplicate column keys: %s", pyrepr.Repr(parts))
	}
	if known[Archived] {
		return fmt.Errorf("%s is reserved and cannot be a column", pyrepr.StrRepr(Archived))
	}
	for _, src := range w.Manual.srcs {
		ok := known[src] || src == Archived
		for d := range w.Manual.dsts[src] {
			ok = ok && known[d]
		}
		if !ok {
			return fmt.Errorf("manual moves from %s reference unknown columns", pyrepr.StrRepr(src))
		}
	}
	return nil
}

// Today's board in dashboard order, locked columns mirror the desktop and moves are the measured PATCH matrix
var DefaultWorkflow = Workflow{
	Columns: []Column{
		{"triage", "Triage", "◇", true},
		{"todo", "Todo", "◻", true},
		{"scheduled", "Scheduled", "⏱", false},
		{"ready", "Ready", "▶", true},
		{"running", "Running", "●", false},
		{"blocked", "Blocked", "⊘", true},
		{"review", "Review", "◎", false},
		{"done", "Done", "✓", true},
	},
	Manual: FrozenManual(
		ManualRow{"triage", []string{"todo", "ready"}},
		ManualRow{"todo", []string{"triage", "scheduled", "ready"}},
		ManualRow{"scheduled", []string{"triage", "todo", "ready"}},
		ManualRow{"ready", []string{"triage", "todo", "scheduled", "blocked", "review", "done"}},
		ManualRow{"running", []string{"triage", "todo", "scheduled", "ready", "blocked", "review", "done"}},
		ManualRow{"blocked", []string{"triage", "todo", "scheduled", "ready", "done"}},
		ManualRow{"review", []string{"triage", "todo", "ready", "done"}},
		ManualRow{"done", []string{"triage", "todo", "ready"}},
		ManualRow{Archived, []string{"triage", "todo", "ready"}},
	),
}

// Every value tasks.status can hold under the default workflow
var DefaultStatuses = func() map[string]bool {
	if err := DefaultWorkflow.Validate(); err != nil {
		panic(err)
	}
	out := map[string]bool{Archived: true}
	for _, k := range DefaultWorkflow.Keys() {
		out[k] = true
	}
	return out
}()
