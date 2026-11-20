//go:build !icebath_core && !icebath_noextras

package gatewayrun

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/kanban"
)

// Kanban statuses the embedded dispatcher acts on without a human
var kanbanDueStatuses = []string{"scheduled", "ready", "running", "review"}

// A kanban task on any unarchived board in a status the dispatcher acts on
func kanbanTaskBusy(ctx context.Context) (string, error) {
	boards, err := kanban.ListBoards(false)
	if err != nil {
		return "", err
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(kanbanDueStatuses)), ",")
	args := make([]any, len(kanbanDueStatuses))
	for i, s := range kanbanDueStatuses {
		args[i] = s
	}
	for _, meta := range boards {
		slugValue, _ := meta.Get("slug")
		var slug *string
		if s, ok := slugValue.(string); ok {
			slug = &s
		}
		path, err := kanban.KanbanDBPath(ctx, slug)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		found, err := func() (bool, error) {
			conn, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(2000)")
			if err != nil {
				return false, err
			}
			defer conn.Close()
			rows, err := conn.QueryContext(ctx, "SELECT 1 FROM tasks WHERE status IN ("+marks+") LIMIT 1", args...)
			if err != nil {
				return false, err
			}
			defer rows.Close()
			return rows.Next(), rows.Err()
		}()
		if err != nil {
			return "", err
		}
		if found {
			return "kanban work on board " + pyrepr.Str(slugValue), nil
		}
	}
	return "", nil
}
