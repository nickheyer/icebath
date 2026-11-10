package kanban

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var resumeGuardLog = slog.Default().With("logger", "hermes_cli.kanban_resume_guard")

// Session store able to classify a session as a Kanban worker transcript
type kanbanOwnershipProbe interface {
	IsKanbanOwnedSession(ctx context.Context, sessionID string) (bool, error)
}

// True when this process holds a dispatcher granted Kanban task and may continue a worker transcript
func dispatcherOwnedRun() bool {
	return pystr.Strip(tools.GetSessionEnv(nil, "ICEBATH_KANBAN_TASK", "")) != ""
}

// Refusal text when the session is a Kanban worker transcript an ordinary session must not resume, else empty
func KanbanResumeRefusal(ctx context.Context, db any, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	if dispatcherOwnedRun() {
		return ""
	}
	probe, ok := db.(kanbanOwnershipProbe)
	if rv := reflect.ValueOf(db); !ok || (rv.Kind() == reflect.Pointer && rv.IsNil()) {
		return ""
	}
	owned, err := probe.IsKanbanOwnedSession(ctx, sessionID)
	if err != nil {
		resumeGuardLog.Warn(fmt.Sprintf("kanban resume probe failed for %s (proceeding unclassified)", sessionID), "exc_info", err.Error())
		return ""
	}
	if !owned {
		return ""
	}
	return i18n.T(ctx, "cli.resume.kanban_owned", "", map[string]any{"session_id": sessionID})
}
