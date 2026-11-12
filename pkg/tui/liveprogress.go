package tui

import "strings"

// Item of the task list the agent keeps, a nested subtask when parent names another item
type todoItem struct {
	Content string
	ID      string
	Parent  string
	Status  string
}

// Transcript row, a role's text with the trail details a turn attached to it
type msg struct {
	CreatedAt              *float64
	Info                   map[string]any
	Kind                   string
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
	case "intro", "panel", "diff":
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
