package kanban

import (
	"fmt"
	"iter"
	"slices"

	"github.com/nickheyer/icebath/pkg/errchain"
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

// Board columns in board order and the manual move allow list
type Workflow struct {
	Columns []Column
	Manual  ManualMoves
}

// Columns in board order as an iterator
func (w Workflow) All() iter.Seq[Column] {
	return slices.Values(w.Columns)
}

// Column keys in board order, never including archived
func (w Workflow) Keys() []string {
	out := make([]string, len(w.Columns))
	for i, c := range w.Columns {
		out[i] = c.Key
	}
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
		parts := make([]any, len(keys))
		for i, k := range keys {
			parts[i] = k
		}
		return fmt.Errorf("duplicate column keys: %s", errchain.Show(parts))
	}
	if known[Archived] {
		return fmt.Errorf("%s is reserved and cannot be a column", errchain.Show(Archived))
	}
	for _, src := range w.Manual.srcs {
		ok := known[src] || src == Archived
		for d := range w.Manual.dsts[src] {
			ok = ok && known[d]
		}
		if !ok {
			return fmt.Errorf("manual moves from %s reference unknown columns", errchain.Show(src))
		}
	}
	return nil
}

// Today's board in column order with its locked columns and its manual move matrix
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
