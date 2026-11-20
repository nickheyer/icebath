package tui

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Item of the task list the agent keeps, a nested subtask when parent names another item
type todoItem struct {
	Content string
	ID      string
	Parent  string
	Status  string
}

// Collapsed paste of a sent prompt, the chip the row shows, the text the model received and the digest that tells two bodies apart
type pasteSection struct {
	Label  string
	Text   string
	Digest string
}

// Transcript row, a role's text with the trail details a turn attached to it
type msg struct {
	// Identity the transcript gives the row when it first lays it out, what the user opened or closed in it kept under it
	ID        uint64
	CreatedAt *float64
	Info      *tuigateway.SessionInfo
	Kind      string
	// Header of a folded result block, shown above its first lines
	Label                  string
	PanelData              *panelData
	Role                   string
	Text                   string
	Thinking               string
	IsMoaReference         bool
	IsLiveReasoning        bool
	ThinkingTokens         int
	ToolTokens             int
	Tools                  []string
	Todos                  []todoItem
	TodoIncomplete         bool
	TodoCollapsedByDefault bool
	// Image files of the gateway host drawn under the row, the revision telling two pictures at one path apart, the rows each may take when not the default
	Images        []string
	ImageRevision string
	ImageRows     int
	// Bodies behind the paste chips of a sent prompt, each a section the row unfolds
	Pastes []pasteSection
}

// Task list item at its nesting depth
type todoRow struct {
	item  todoItem
	depth int
}

// Items in depth first order with parents before their children, an item whose parent is missing, itself or part of a cycle at the root
func todoTree(todos []todoItem) []todoRow {
	ids := map[string]bool{}
	for _, t := range todos {
		ids[t.ID] = true
	}
	kids := map[string][]todoItem{}
	var roots []todoItem
	for _, t := range todos {
		if t.Parent != "" && ids[t.Parent] && t.Parent != t.ID {
			kids[t.Parent] = append(kids[t.Parent], t)
		} else {
			roots = append(roots, t)
		}
	}
	out := make([]todoRow, 0, len(todos))
	seen := map[string]bool{}
	var walk func(item todoItem, depth int)
	walk = func(item todoItem, depth int) {
		if seen[item.ID] {
			return
		}
		seen[item.ID] = true
		out = append(out, todoRow{item: item, depth: depth})
		for _, kid := range kids[item.ID] {
			walk(kid, depth+1)
		}
	}
	for _, root := range roots {
		walk(root, 0)
	}
	for _, t := range todos {
		if !seen[t.ID] {
			seen[t.ID] = true
			out = append(out, todoRow{item: t})
		}
	}
	return out
}

// Open items, those pending or in progress
func countPendingTodos(todos []todoItem) int {
	n := 0
	for _, todo := range todos {
		if todo.Status == "in_progress" || todo.Status == "pending" {
			n++
		}
	}
	return n
}

// Whether every item is completed or cancelled, false for an empty list
func isTodoDone(todos []todoItem) bool {
	if len(todos) == 0 {
		return false
	}
	for _, todo := range todos {
		if todo.Status != "completed" && todo.Status != "cancelled" {
			return false
		}
	}
	return true
}

// Whether a row is a bare shelf of tool lines, a trail with tools and no text or reasoning
func isToolShelfMessage(m *msg) bool {
	return m != nil && m.Kind == "trail" && m.Text == "" && strings.TrimFunc(m.Thinking, isJSSpace) == "" && len(m.Tools) > 0
}

// Whether a trail row can take a shelf of tool lines
func canHoldToolShelf(m *msg) bool {
	return m != nil && m.Kind == "trail" && m.Text == "" && (strings.TrimFunc(m.Thinking, isJSSpace) != "" || len(m.Tools) > 0)
}

// Row with the source's tool lines appended to its own
func mergeToolShelfInto(target, source msg) msg {
	target.Tools = append(append([]string(nil), target.Tools...), source.Tools...)
	return target
}

// Whether a row ends the search for a shelf to merge into
func isBarrierMessage(m *msg) bool {
	if m == nil {
		return true
	}
	switch m.Kind {
	case "intro", "panel", "diff", "image":
		return true
	}
	return m.Role != "" && m.Role != "system" || m.Text != ""
}

func isToolCarryingTrail(m *msg) bool {
	return m != nil && m.Kind == "trail" && m.Text == "" && len(m.Tools) > 0
}

// Rows with a tool shelf merged into the nearest trail before a barrier, or appended
func appendToolShelfMessage(prev []msg, m msg) []msg {
	if !isToolShelfMessage(&m) {
		return append(append([]msg(nil), prev...), m)
	}
	fallback := -1
	for i := len(prev) - 1; i >= 0; i-- {
		candidate := &prev[i]
		if isToolCarryingTrail(candidate) {
			next := append([]msg(nil), prev...)
			next[i] = mergeToolShelfInto(*candidate, m)
			return next
		}
		if fallback < 0 && canHoldToolShelf(candidate) {
			fallback = i
		}
		if isBarrierMessage(candidate) {
			break
		}
	}
	if fallback >= 0 {
		next := append([]msg(nil), prev...)
		next[fallback] = mergeToolShelfInto(prev[fallback], m)
		return next
	}
	return append(append([]msg(nil), prev...), m)
}
