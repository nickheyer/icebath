package app

import (
	"encoding/json"
	"fmt"
	"strings"

	todotools "github.com/nickheyer/icebath/internal/plugins/defaults/tools/todo/tools"
	toolsapp "github.com/nickheyer/icebath/internal/tools/app"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Depth past which subtasks are indented no further
const indents = 4

type views struct{}

func Register(registry *toolsapp.Registry) {
	registry.Register(views{})
}

// The task list shows as a checklist, once for each reply, its newest; each call says what it did to the list
func (views) Views() []toolsapp.View {
	return []toolsapp.View{{Tool: todotools.Name, Line: line, Result: checklist, Latest: true}}
}

type arguments struct {
	Todos json.RawMessage `json:"todos"`
	Merge bool            `json:"merge"`
}

// Whether the call wrote the list, whether it merged into it, and how many items it gave
func (a arguments) written() (bool, bool, int) {
	var todos []json.RawMessage
	if len(a.Todos) == 0 || string(a.Todos) == "null" {
		return false, false, 0
	}
	json.Unmarshal(a.Todos, &todos)
	return true, a.Merge, len(todos)
}

// What the call does or did to the list, with the items done of all once it is known
func line(call toolsapp.Call) string {
	var args arguments
	json.Unmarshal([]byte(call.Arguments), &args)
	written, merge, count := args.written()
	if call.Result == "" {
		switch {
		case !written:
			return "reading the task list"
		case merge:
			return "updating " + render.Count(count, "task")
		}
		return "planning " + render.Count(count, "task")
	}
	var result todotools.Result
	json.Unmarshal([]byte(call.Result), &result)
	total, done := result.Summary.Total, result.Summary.Completed
	progress := fmt.Sprintf("%d of %s done", done, render.Count(total, "task"))
	switch {
	case !written && total > 0:
		return progress
	case !written:
		return "no tasks yet"
	case merge && total > 0 && done > 0:
		return "updated, " + progress
	case merge:
		return "updated " + render.Count(count, "task")
	case total > 0 && done > 0:
		return progress
	}
	return render.Count(count, "task")
}

// The list as a checklist under a head counting the items still open, folded once every item is settled and the reply is over; a reply
// that ended with items open says so
func checklist(call toolsapp.Call, width int, look theme.Theme) *toolsapp.Shown {
	var result todotools.Result
	if json.Unmarshal([]byte(call.Result), &result) != nil || len(result.Todos) == 0 {
		return nil
	}
	open := 0
	for _, item := range result.Todos {
		if item.Status == "pending" || item.Status == "in_progress" {
			open++
		}
	}
	settled := open == 0
	head := render.Heading("Tasks", look) + render.Dim(fmt.Sprintf("  %d of %d open", open, len(result.Todos)), look)
	if call.Ended && open > 0 {
		head += render.Dim(" · the reply ended with "+render.Count(open, "task")+" not done", look)
	}
	shown := &toolsapp.Shown{Head: head, Open: !call.Ended || !settled}
	if settled {
		dropped := 0
		for _, item := range result.Todos {
			if item.Status == "cancelled" {
				dropped++
			}
		}
		said := render.Count(len(result.Todos), "task") + " done"
		if dropped > 0 {
			said = fmt.Sprintf("%d of %s done, %d dropped", len(result.Todos)-dropped, render.Count(len(result.Todos), "task"), dropped)
		}
		shown.Folded = render.Good(look.Glyphs.Check+" "+said, look)
	}
	for _, row := range tree(result.Todos) {
		indent := strings.Repeat("  ", min(row.depth, indents))
		mark, paint := look.Glyphs.Todo, func(text string) string { return text }
		switch row.item.Status {
		case "completed":
			mark, paint = look.Glyphs.Done, func(text string) string { return render.Good(text, look) }
		case "in_progress":
			mark, paint = look.Glyphs.Active, func(text string) string { return render.Warm(text, look) }
		case "cancelled":
			mark, paint = look.Glyphs.Skip, func(text string) string { return render.Dim(text, look) }
		}
		shown.Rows = append(shown.Rows, indent+paint(render.Truncate(mark+" "+row.item.Content, max(width-len(indent), 8), look)))
	}
	return shown
}

type node struct {
	item  todotools.Item
	depth int
}

// Items depth first, each parent before its subtasks; an item whose parent is missing, itself or in a loop at the top
func tree(items []todotools.Item) []node {
	ids := map[string]bool{}
	for _, item := range items {
		ids[item.ID] = true
	}
	kids := map[string][]todotools.Item{}
	var roots []todotools.Item
	for _, item := range items {
		if item.Parent != "" && ids[item.Parent] && item.Parent != item.ID {
			kids[item.Parent] = append(kids[item.Parent], item)
		} else {
			roots = append(roots, item)
		}
	}
	var out []node
	seen := map[string]bool{}
	var walk func(item todotools.Item, depth int)
	walk = func(item todotools.Item, depth int) {
		if seen[item.ID] {
			return
		}
		seen[item.ID] = true
		out = append(out, node{item, depth})
		for _, kid := range kids[item.ID] {
			walk(kid, depth+1)
		}
	}
	for _, root := range roots {
		walk(root, 0)
	}
	for _, item := range items {
		if !seen[item.ID] {
			seen[item.ID] = true
			out = append(out, node{item: item})
		}
	}
	return out
}
