package scenario

import (
	"testing"
)

// A board task is created, listed, shown, commented, assigned, counted and completed, each step stored in kanban.db
func TestScenarioKanbanCreateListCommentComplete(t *testing.T) {
	s := Start(t, Setup{})
	s.Run("kanban", "list").Exit(0).Has("(no matching tasks)")
	id := s.Run("kanban", "create", "Write the report", "--body", "Two pages").Exit(0).Match(`Created (t_[0-9a-f]+)  \(ready, assignee=-\)`)[1]
	s.Run("kanban", "list").Exit(0).Has(id, "ready", "(unassigned)", "Write the report")
	s.SameJSON(s.Rows("kanban.db", "SELECT id, title, body, status, assignee FROM tasks"),
		`[{"id": "`+id+`", "title": "Write the report", "body": "Two pages", "status": "ready", "assignee": null}]`)
	s.Run("kanban", "boards").Exit(0).Has("default", "Default", "ready=1", "Current board: default")
	s.Run("kanban", "stats").Exit(0).Match(`(?m)^\s*ready\s+1$`)

	s.Run("kanban", "comment", id, "Looks", "good").Exit(0).Has("Comment added to " + id)
	s.SameJSON(s.Rows("kanban.db", "SELECT task_id, body FROM task_comments"), `[{"task_id": "`+id+`", "body": "Looks good"}]`)
	s.Run("kanban", "show", id).Exit(0).Has(id, "Write the report", "Two pages", "Looks good")

	second := s.Run("kanban", "create", "Second task", "--assignee", "writer", "--priority", "3", "--json").Exit(0)
	s.HasJSON(second.Stdout, `{"title": "Second task", "assignee": "writer", "priority": 3}`)
	other, _ := Dig(second.JSON(), "id").(string)
	s.Run("kanban", "assign", id, "writer").Exit(0)
	s.SameJSON(s.Rows("kanban.db", "SELECT assignee FROM tasks WHERE id = ?", id), `[{"assignee": "writer"}]`)

	s.Run("kanban", "complete", id, "--result", "Report written").Exit(0).Has("Completed " + id)
	s.SameJSON(s.Rows("kanban.db", "SELECT status, result FROM tasks WHERE id = ?", id), `[{"status": "done", "result": "Report written"}]`)
	s.Run("kanban", "stats").Exit(0).Match(`(?m)^\s*done\s+1$`)
	s.Run("kanban", "complete", id, "--result", "again").Exit(1).Has("cannot complete " + id + " (unknown id or terminal state)")
	s.Run("kanban", "list").Exit(0).Has(other, "Second task", id, "done")
	s.Run("kanban", "list", "--status", "done").Exit(0).Has(id).Lacks(other)
}
