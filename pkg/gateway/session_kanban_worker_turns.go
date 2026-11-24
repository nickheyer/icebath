//go:build !icebath_core && !icebath_noextras

package gateway

import (
	"context"
	"os"
	"sync"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

var kanbanTurnsDoneOnce sync.Once

// KanbanTurnAuthor is the dispatcher identity a card attempt's prompts carry so memory never files them as the operator's
var KanbanTurnAuthor = map[string]any{"id": "kanban:dispatcher", "name": "Kanban dispatcher", "is_bot": true}

// Item of a kanban context, TypeError for a context that is no dict and KeyError for a missing key
func KanbanContextItem(kctx any, key string) (any, error) {
	obj, isDict := kctx.(*jsonutil.Object)
	if !isDict {
		return nil, &PyError{Kind: "TypeError", Msg: "the worker kanban context is " + errchain.Kind(kctx) + ", expected a mapping"}
	}
	return kanbanContextItem(obj, key)
}

// Books the one shot credential exit of a worker that died before any turn ran, nothing for a chat turn
func recordKanbanStartFailure(ctx context.Context, frame *jsonutil.Object, cause error) error {
	kctx, err := WorkerKanbanContext(frame)
	if err != nil || kctx == nil {
		return err
	}
	rateLimited, terminal, _ := kanban.CredentialFailureFlags(cause)
	code := tools.TurnExitCode(nil, tools.TurnExitOptions{KanbanWorker: true, CredentialsRateLimited: rateLimited, CredentialsTerminal: terminal})
	return RecordWorkerResult(ctx, kctx, code, "")
}

// Ends the submitter watch and appends the attempt's worker_result event while this process is still the bound worker
func RecordWorkerResult(ctx context.Context, kctx any, code int, lastOutput string) error {
	kanbanTurnsDoneOnce.Do(func() { close(kanbanTurnsDone) })
	db, err := KanbanContextItem(kctx, "db")
	if err != nil {
		return err
	}
	return kanban.ConnectClosing(ctx, anyutil.String(db), nil, func(conn state.SQLiteConn) error {
		return kanban.WriteTxn(ctx, conn, false, func() error {
			matches, err := boundWorkerMatches(ctx, conn, kctx)
			if err != nil || !matches {
				return err
			}
			taskID, err := KanbanContextItem(kctx, "task_id")
			if err != nil {
				return err
			}
			claim, err := KanbanContextItem(kctx, "claim_lock")
			if err != nil {
				return err
			}
			rawRun, err := KanbanContextItem(kctx, "run_id")
			if err != nil {
				return err
			}
			runID, err := anyutil.ToInt(rawRun)
			if err != nil {
				return err
			}
			return kanban.AppendTaskEvent(ctx, conn, anyutil.String(taskID), "worker_result", jsonutil.ObjectOf("pid", int64(os.Getpid()),
				"claim_lock", claim, "exit_code", int64(code), "last_output", lastOutput), &runID)
		})
	})
}

// Whether the run's latest worker_bound event names this process and the context's claim
func boundWorkerMatches(ctx context.Context, conn state.SQLiteConn, kctx any) (bool, error) {
	taskID, err := KanbanContextItem(kctx, "task_id")
	if err != nil {
		return false, err
	}
	runID, err := KanbanContextItem(kctx, "run_id")
	if err != nil {
		return false, err
	}
	row, err := state.ConnRow(ctx, conn, "SELECT payload FROM task_events WHERE task_id=? AND run_id=? AND kind='worker_bound' ORDER BY id DESC LIMIT 1", taskID, runID)
	if err != nil {
		return false, err
	}
	bound := jsonutil.NewObject()
	if row != nil {
		decoded, err := jsonutil.Decode(anyutil.String(row["payload"]))
		if err != nil {
			return false, err
		}
		obj, isDict := decoded.(*jsonutil.Object)
		if !isDict {
			return false, &PyError{Kind: "AttributeError", Msg: "the bound worker record is " + errchain.Kind(decoded) + ", expected a mapping"}
		}
		bound = obj
	}
	claim, err := KanbanContextItem(kctx, "claim_lock")
	if err != nil {
		return false, err
	}
	return anyutil.Equal(bound.ValueOr("pid", nil), int64(os.Getpid())) && anyutil.Equal(bound.ValueOr("claim_lock", nil), claim), nil
}

// Retry proof of a managed worker, its exact run claim live and this process the one bound to it
func BoundClaimIsLive(ctx context.Context, kctx any) (bool, error) {
	text := func(key string) (*string, error) {
		value, err := KanbanContextItem(kctx, key)
		if err != nil {
			return nil, err
		}
		s := anyutil.String(value)
		return &s, nil
	}
	taskID, err := text("task_id")
	if err != nil {
		return false, err
	}
	db, err := text("db")
	if err != nil {
		return false, err
	}
	runID, err := text("run_id")
	if err != nil {
		return false, err
	}
	claim, err := text("claim_lock")
	if err != nil {
		return false, err
	}
	live, err := kanban.WorkerClaimIsLive(ctx, kanban.ClaimProof{TaskID: taskID, DBPath: db, RunID: runID, ClaimLock: claim, SkipPid: true})
	if err != nil || !live {
		return false, err
	}
	resolved, err := pathutil.Resolve(*db)
	if err != nil {
		return false, err
	}
	uri, err := pathutil.FileURL(resolved)
	if err != nil {
		return false, err
	}
	conn, err := state.ConnectSQLite(uri+"?mode=ro", state.SQLiteConfig{Timeout: 5.0, URI: true})
	if err != nil {
		if _, isSQLite := state.SQLiteClassOf(err); isSQLite {
			return false, nil
		}
		return false, err
	}
	defer conn.Close()
	matches, err := boundWorkerMatches(ctx, conn, kctx)
	if _, isSQLite := state.SQLiteClassOf(err); err != nil && isSQLite {
		return false, nil
	}
	return matches, err
}
