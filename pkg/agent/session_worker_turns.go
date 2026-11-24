package agent

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Agent surface a managed worker's turns drive, the AIAgent of the child
type WorkerTurnAgent interface {
	surfaceTurnAgent
	RunConversationContent(ctx context.Context, userMessage any, o RunConversationOptions) (map[string]any, error)
	Env() *tools.Env
	SessionID() string
	SessionStore() SessionStore
}

// Runs a managed worker turn under its frame turn scope with the surface armed like the in process runner
func RunWorkerTurns(ctx context.Context, a WorkerTurnAgent, frame *jsonutil.Object, history []map[string]any) (map[string]any, error) {
	scoped, err := gateway.WorkerTurnScope(ctx, frame)
	if err != nil {
		return nil, err
	}
	ArmSurfaceTurn(scoped, a, nil, false)
	return runWorkerTurns(scoped, a, frame, history)
}

// Chat turn in the session workspace, or a kanban attempt whose exit code is booked on the board
func runWorkerTurns(ctx context.Context, a WorkerTurnAgent, frame *jsonutil.Object, history []map[string]any) (map[string]any, error) {
	kctx, err := gateway.WorkerKanbanContext(frame)
	if err != nil {
		return nil, err
	}
	if kctx == nil {
		display := gateway.WorkerDisplayKwargs(ctx)
		o := RunConversationOptions{ConversationHistory: history}
		if kind, ok := display["persist_user_display_kind"].(string); ok {
			o.PersistUserDisplayKind = kind
		}
		if meta, ok := display["persist_user_display_metadata"].(map[string]any); ok {
			o.PersistUserDisplayMetadata = meta
		}
		if author, ok := frame.ValueOr("turn_author", nil).(*jsonutil.Object); ok {
			o.TurnAuthor, _ = jsonutil.Plain(author).(map[string]any)
		}
		var result map[string]any
		err := gateway.WorkerWorkspace(ctx, a.Env(), frame, func(taskID string) error {
			o.TaskID = taskID
			var runErr error
			result, runErr = a.RunConversationContent(ctx, frame.ValueOr("text", nil), o)
			return runErr
		})
		return result, err
	}
	if kanbanAttemptTurns == nil {
		refusal := linktier.Missing("a kanban attempt", linktier.Extras)
		return nil, errors.New(refusal)
	}
	return kanbanAttemptTurns(ctx, a, frame, history, kctx)
}

// Kanban attempt turns of a managed worker, installed by the extras tier that carries kanban
var kanbanAttemptTurns func(ctx context.Context, a WorkerTurnAgent, frame *jsonutil.Object, history []map[string]any, kctx any) (map[string]any, error)
