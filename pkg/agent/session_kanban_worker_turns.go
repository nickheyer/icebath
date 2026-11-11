package agent

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
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
func RunWorkerTurns(ctx context.Context, a WorkerTurnAgent, frame *pyjson.Object, history []map[string]any) (map[string]any, error) {
	scoped, err := gateway.WorkerTurnScope(ctx, frame)
	if err != nil {
		return nil, err
	}
	ArmSurfaceTurn(scoped, a, nil, false)
	return runWorkerTurns(scoped, a, frame, history)
}

// Chat turn in the session workspace, or a kanban attempt whose exit code is booked on the board
func runWorkerTurns(ctx context.Context, a WorkerTurnAgent, frame *pyjson.Object, history []map[string]any) (map[string]any, error) {
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
		if author, ok := frame.GetOr("turn_author", nil).(*pyjson.Object); ok {
			o.TurnAuthor, _ = pyjson.Plain(author).(map[string]any)
		}
		var result map[string]any
		err := gateway.WorkerWorkspace(ctx, a.Env(), frame, func(taskID string) error {
			o.TaskID = taskID
			var runErr error
			result, runErr = a.RunConversationContent(ctx, frame.GetOr("text", nil), o)
			return runErr
		})
		return result, err
	}
	code, lastOutput := 1, ""
	result, runErr := runTaskTurns(ctx, a, frame, history, kctx)
	if runErr == nil {
		code = kanban.TurnExitCode(result, kanban.TurnExitOptions{KanbanWorker: true})
		if result != nil {
			text := ""
			if final := result["final_response"]; pyrepr.Truthy(final) {
				text = pyrepr.Str(final)
			}
			lastOutput = pystr.Slice(text, -500, pystr.Len(text))
		}
	} else {
		rateLimited, terminal, _ := kanban.CredentialFailureFlags(runErr)
		code = kanban.TurnExitCode(nil, kanban.TurnExitOptions{KanbanWorker: true, CredentialsRateLimited: rateLimited, CredentialsTerminal: terminal})
	}
	if err := gateway.RecordWorkerResult(ctx, kctx, code, lastOutput); err != nil {
		return nil, err
	}
	return result, runErr
}

// Kanban attempt with its preloaded skills, in place provider recovery under the bound claim, then the goal loop
func runTaskTurns(ctx context.Context, a WorkerTurnAgent, frame *pyjson.Object, history []map[string]any, kctx any) (map[string]any, error) {
	var itemErr error
	item := func(key string) any {
		if itemErr != nil {
			return nil
		}
		v, err := gateway.KanbanContextItem(kctx, key)
		itemErr = err
		return v
	}
	rawSkills := item("skills")
	if itemErr != nil {
		return nil, itemErr
	}
	var identifiers []string
	switch list := rawSkills.(type) {
	case []any:
		for _, raw := range list {
			identifiers = append(identifiers, pyrepr.Str(raw))
		}
	case nil:
	default:
		return nil, &workerStoreError{"TypeError", "'" + pyrepr.TypeName(rawSkills) + "' object is not iterable"}
	}
	prompt, loaded, missing := skills.NewScope(home.Dir(ctx), nil).BuildPreloadedSkillsPrompt(ctx, identifiers, a.SessionID(), nil)
	if len(missing) > 0 && len(loaded) == 0 {
		return nil, &workerStoreError{"ValueError", "kanban_skills_unavailable"}
	}
	cardContext := item("context")
	if itemErr != nil {
		return nil, itemErr
	}
	var parts []string
	for i, part := range []any{prompt, cardContext, frame.GetOr("text", nil)} {
		if !pyrepr.Truthy(part) {
			continue
		}
		text, isText := part.(string)
		if !isText {
			return nil, &workerStoreError{"TypeError", "sequence item " + pyrepr.Str(int64(i)) + ": expected str instance, " + pyrepr.TypeName(part) + " found"}
		}
		parts = append(parts, text)
	}
	taskTurn := RunConversationOptions{TurnAuthor: gateway.KanbanTurnAuthor}
	first := taskTurn
	first.ConversationHistory = history
	result, err := a.RunConversationContent(ctx, strings.Join(parts, "\n\n"), first)
	if err != nil {
		return nil, err
	}
	sessionConversation := func() ([]map[string]any, error) {
		return a.SessionStore().GetMessagesAsConversation(ctx, a.SessionID(), state.ConversationOptions{})
	}
	var recoverErr error
	recoverTurn := func(nudge string) {
		if recoverErr != nil {
			return
		}
		var prior []map[string]any
		if messages, ok := result["messages"].([]map[string]any); ok && len(messages) > 0 {
			prior = messages
		} else if messages, ok := result["messages"].([]any); ok && len(messages) > 0 {
			for _, m := range messages {
				if row, isRow := m.(map[string]any); isRow {
					prior = append(prior, row)
				}
			}
		}
		if prior == nil {
			prior, recoverErr = sessionConversation()
			if recoverErr != nil {
				return
			}
		}
		next := taskTurn
		next.ConversationHistory = prior
		result, recoverErr = a.RunConversationContent(ctx, nudge, next)
	}
	taskID := pyrepr.Str(item("task_id"))
	if itemErr != nil {
		return nil, itemErr
	}
	_, err = kanban.RecoverFailedKanbanTurnsWith(ctx, recoverTurn, func() any {
		if result == nil {
			return nil
		}
		return result
	}, kanban.RecoveryOptions{TaskID: &taskID, ClaimCheck: func() (bool, error) { return gateway.BoundClaimIsLive(ctx, kctx) }})
	if err == nil {
		err = recoverErr
	}
	if err != nil {
		return nil, err
	}
	goalMode := item("goal_mode")
	if itemErr != nil {
		return nil, itemErr
	}
	if !pyrepr.Truthy(goalMode) || kanban.TurnExitCode(result, kanban.TurnExitOptions{KanbanWorker: true}) != 0 {
		return result, nil
	}
	return runKanbanGoalTurns(ctx, a, kctx, taskID, result, sessionConversation, taskTurn)
}

// Goal loop of a settled kanban attempt, each continuation turn reading the session's stored conversation
func runKanbanGoalTurns(ctx context.Context, a WorkerTurnAgent, kctx any, taskID string, result map[string]any,
	sessionConversation func() ([]map[string]any, error), taskTurn RunConversationOptions) (map[string]any, error) {
	field := func(key string) (any, error) { return gateway.KanbanContextItem(kctx, key) }
	rawRun, err := field("run_id")
	if err != nil {
		return nil, err
	}
	var runID *int64
	if rawRun != nil {
		n, err := pyint.Int64(rawRun)
		if err != nil {
			return nil, err
		}
		runID = &n
	}
	goalText, err := field("goal_text")
	if err != nil {
		return nil, err
	}
	rawMax, err := field("goal_max_turns")
	if err != nil {
		return nil, err
	}
	maxTurns := 0
	if pyrepr.Truthy(rawMax) {
		n, err := pyint.Int64(rawMax)
		if err != nil {
			return nil, err
		}
		maxTurns = int(n)
	}
	firstResponse := ""
	if final := result["final_response"]; pyrepr.Truthy(final) {
		firstResponse = pyrepr.Str(final)
	}
	_, err = goals.RunKanbanGoalLoop(ctx, goals.GoalLoopOptions{
		TaskID: taskID, GoalText: pyrepr.Str(goalText), MaxTurns: maxTurns, FirstResponse: firstResponse,
		RunTurn: func(prompt string) (any, error) {
			history, err := sessionConversation()
			if err != nil {
				return nil, err
			}
			next := taskTurn
			next.ConversationHistory = history
			result, err = a.RunConversationContent(ctx, prompt, next)
			if err != nil {
				return nil, err
			}
			if final, present := result["final_response"]; present {
				return final, nil
			}
			return "", nil
		},
		TaskStatus: func() (*string, error) {
			var status *string
			err := kanban.ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error {
				var err error
				status, err = kanban.GoalRunStatus(ctx, conn, taskID, runID)
				return err
			})
			return status, err
		},
		Block: func(reason string) error {
			return kanban.ConnectClosing(ctx, "", nil, func(conn state.SQLiteConn) error {
				return kanban.BlockTask(ctx, conn, taskID, reason, runID)
			})
		},
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
